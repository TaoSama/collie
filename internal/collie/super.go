package collie

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Super is Collie's super-agent: one resident Router session (the herdr agent
// named "collie") that talks to the human and dispatches work, plus isolated
// Worker sessions (other herdr agents) that do it. Everything that reaches the
// system is a Trigger in a durable inbox; Collie filters, routes, batches and
// delivers triggers whenever the target session is free.
type Super struct {
	s   *Server
	db  *sql.DB
	bin string
	dir string // ~/.config/collie

	mu            sync.Mutex
	st            *State
	dirty         bool
	deletedTimers []string
	subs          map[chan struct{}]bool

	routerLaunching bool
	rereadTrig      *Trigger // the pending "re-read ROUTER.md", if any
	routerLaunchAt  time.Time
	routerFresh     bool
	routerAskedAt   time.Time // last delivery to the router not yet followed by a reply or check
	routerRetries   int       // turns in a row that ended on an API error, each retried
	routerNudged    bool      // the watchdog interrupted this turn once already
	routerFails     int       // Router launches in a row that failed, each retried by a later tick
	routerFailTold  bool      // the human heard about this run of failures
	dreamFailAt     time.Time // the last dream that failed; the next waits a while
	routerUsage     ctxUsage
	// The model and effort the running router was launched with (unknown after a hub restart).
	routerRun      *[2]string
	routerLaunched *routerLaunch        // where and how the running router was started
	rsync          routerSync           // the router folder's mirror on that computer
	handover       *handover            // set while the router writes its memory before a fresh session
	dream          *dreamRun            // set while Collie sleeps and a dream session tidies its memory
	wb             *workbench           // the workbench: modules the human pins, and what agents may read of them
	lastTrust      map[string]time.Time // pane → when its trust dialog was last answered
	lastAnswer     map[string]time.Time // pane → when a dialog there was just answered
	lastScreen     map[string]screenMark
	transcripts    map[string]*transcript
	noAgent        map[string]time.Time // worker → since when its pane has had no agent in it
	unanswered     map[string]time.Time // machine → since when it hasn't answered the tick's list
	pinnedCache    map[string]*Entry    // older pinned entries resolved from SQLite
	heldCache      map[string]*Entry    // held pending entries, likewise
	remindLookAt   time.Time            // when to look again for the daily reminder
	notifyBuf      []notice
	notifyAt       time.Time
	seenAt         time.Time            // when a page of Collie was last open in front of you
	seenPages      map[string]time.Time // page → when it last said so, until it goes to the background
	seenURL        string               // where that page was, for the link in a notification
	push           *webPush             // the browsers notifications are pushed to
	picked         map[string]Answer    // pane → the choice a page clicked on its dialog card
}

type screenMark struct {
	hash  string
	since time.Time
	flag  bool
}

const (
	routerName  = "collie"
	routerLabel = "collie · router"
)
const mainThread = "chat:main"

func newSuper(s *Server, dir string) (*Super, error) {
	bin, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(bin); err == nil {
		bin = r
	}
	bin = stableBin(bin, dir)
	sp := &Super{
		s: s, dir: dir, bin: bin,
		subs:        map[chan struct{}]bool{},
		lastScreen:  map[string]screenMark{},
		transcripts: map[string]*transcript{},
		noAgent:     map[string]time.Time{},
		unanswered:  map[string]time.Time{},
	}
	db, err := openDB(filepath.Join(dir, "herd.db")) // ~/.collie/collie.db belongs to another app
	if err != nil {
		return nil, err
	}
	sp.db = db
	if sp.st, err = loadState(db); err != nil {
		return nil, err
	}
	sp.wb = newWorkbench(db, dir, sp.routerDir())
	if err := sp.wb.writeIndex(); err != nil {
		log.Printf("workbench: %v", err)
	}
	if sp.push, err = newWebPush(db); err != nil {
		return nil, fmt.Errorf("web push: %w", err)
	}
	return sp, nil
}

// ---------------------------------------------------------------- state plumbing

// changed requires sp.mu to be held.
func (sp *Super) changed() {
	sp.dirty = true
	sp.notifySubscribersLocked()
}

func (sp *Super) notifySubscribersLocked() {
	for ch := range sp.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (sp *Super) subscribe() (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	sp.mu.Lock()
	sp.subs[ch] = true
	sp.mu.Unlock()
	return ch, func() { sp.mu.Lock(); delete(sp.subs, ch); sp.mu.Unlock() }
}

func (sp *Super) flush() {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if err := sp.flushLocked(); err != nil {
		log.Printf("save state: %v", err)
	}
}

func (sp *Super) flushLocked() error {
	if !sp.dirty {
		return nil
	}
	sp.holdPendingLocked(time.Now()) // what still waits on you outlives the trim
	if err := saveState(sp.db, sp.st, sp.deletedTimers); err != nil {
		return err
	}
	sp.st.trim() // history beyond the working set stays in SQLite
	sp.dirty, sp.deletedTimers = false, nil
	return nil
}

func (sp *Super) addEntryLocked(role, thread, text, ref, tone string) *Entry {
	return sp.addEntryReplyLocked(role, thread, text, ref, tone, nil, nil, false)
}

func (sp *Super) addEntryReLocked(role, thread, text, ref, tone string, re *Quote) *Entry {
	return sp.addEntryReplyLocked(role, thread, text, ref, tone, re, nil, false)
}

func (sp *Super) addEntryReplyLocked(role, thread, text, ref, tone string, re *Quote, opts []string, check bool) *Entry {
	return sp.appendEntryLocked(&Entry{ID: sp.st.nextID("e"), Role: role, Thread: thread, Text: text, At: time.Now(), Ref: ref, Tone: tone, Re: re, Options: opts, Check: check})
}

func (sp *Super) appendEntryLocked(e *Entry) *Entry {
	sp.st.Entries = append(sp.st.Entries, e)
	if e.Role == "collie" || e.Tone == "blocked" || e.Tone == "error" || e.Tone == toneRemind {
		sp.queueNotifyLocked(e)
	}
	sp.changed()
	return e
}

const maxReplyOptions = 6

// maxOptionRunes: a quick reply is sent back as the human's answer, so an
// option keeps its whole text up to this many characters, with no "…".
const maxOptionRunes = 240

func cleanOptions(raw []string) []string {
	var out []string
	for _, s := range raw {
		s = oneLine(s)
		if r := []rune(s); len(r) > maxOptionRunes {
			s = strings.TrimSpace(string(r[:maxOptionRunes]))
		}
		if s == "" || slices.Contains(out, s) {
			continue
		}
		out = append(out, s)
		if len(out) >= maxReplyOptions {
			break
		}
	}
	return out
}

// Reply posts Collie's message to a conversation thread, with optional quote,
// quick-reply buttons or --check review flag, and returns any soft warning for the CLI.
func (sp *Super) Reply(thread, text, reID string, options []string, check ...bool) (*Entry, string) {
	text = strings.TrimSpace(text)
	if thread == "" {
		thread = mainThread
	}
	opts := cleanOptions(options)
	chk := len(check) > 0 && check[0]
	sp.mu.Lock()
	limit := sp.st.Settings.ReplyChars
	re := sp.quoteLocked(reID)
	e := sp.addEntryReplyLocked("collie", thread, text, "", "", re, opts, chk)
	sp.mu.Unlock()
	var warn string
	if strings.TrimSpace(reID) != "" && re == nil {
		warn = "--re " + reID + " is not an event or message I know; the reply went out without it"
	}
	if n := replyLen(text); limit > 0 && n > limit {
		warn = fmt.Sprintf("reply is %d characters; chat replies are usually ≤%d. Fine if the human asked for detail or the result needs a list; otherwise keep the next one shorter", n, limit)
	}
	return e, warn
}

func (sp *Super) threadLocked(id string) *Thread {
	th := sp.st.Threads[id]
	if th == nil {
		th = &Thread{ID: id, Subscribers: []string{}, Triggers: []string{}}
		sp.st.Threads[id] = th
	}
	return th
}

// Ingest puts one trigger in the inbox after the fast-path policy check.
// It returns the trigger (nil when it was a duplicate).
func (sp *Super) Ingest(source, thread, from, text, key, target, ref string) *Trigger {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.ingestLocked(source, thread, from, text, key, target, ref)
}

func (sp *Super) ingestLocked(source, thread, from, text, key, target, ref string) *Trigger {
	t, th := sp.prepareTriggerLocked(source, thread, from, text, key, target, ref)
	sp.publishTriggerLocked(t, th)
	return t
}

// prepareTriggerLocked reserves an ID and builds the records before publishing
// them. Callers that acknowledge an external event commit them first.
func (sp *Super) prepareTriggerLocked(source, thread, from, text, key, target, ref string) (*Trigger, *Thread) {
	if key != "" {
		for i := len(sp.st.Triggers) - 1; i >= 0 && i > len(sp.st.Triggers)-400; i-- {
			if sp.st.Triggers[i].Key == key {
				return nil, nil
			}
		}
	}
	if thread == "" {
		thread = mainThread
	}
	pol := sp.st.policyFor(source)
	action := pol.Action
	if len(pol.Allow) > 0 && !slices.Contains(pol.Allow, from) {
		action = "ignore"
	}
	t := &Trigger{
		ID: sp.st.nextID("t"), Source: source, Thread: thread, From: from, Text: text,
		At: time.Now(), Action: action, Status: "queued", Key: key, Ref: ref,
	}
	if action == "ignore" {
		t.Status = "ignored"
	} else if target != "" {
		t.Targets = []string{target}
	}
	th := &Thread{ID: thread, Subscribers: []string{}, Triggers: []string{}}
	if current := sp.st.Threads[thread]; current != nil {
		*th = *current
		th.Triggers = slices.Clone(th.Triggers)
	}
	th.LastAt = t.At
	th.Triggers = append(th.Triggers, t.ID)
	return t, th
}

func (sp *Super) publishTriggerLocked(t *Trigger, th *Thread) {
	if t == nil {
		return
	}
	sp.st.Triggers = append(sp.st.Triggers, t)
	*sp.threadLocked(th.ID) = *th
	sp.changed()
}

// ---------------------------------------------------------------- run loop

func (sp *Super) Run(ctx context.Context) {
	// A new build may change the router's instructions: the next session
	// (a handover, a dream, a restart) reads them fresh.
	if err := sp.writeRouterFiles(); err != nil {
		log.Printf("router files: %v", err)
	}
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
	save := time.NewTicker(2 * time.Second)
	defer save.Stop()
	for {
		select {
		case <-ctx.Done():
			sp.flush()
			return
		case <-save.C:
			sp.flush()
			sp.sendNotifications()
		case <-tick.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("super tick panic: %v", r)
					}
				}()
				sp.tick(ctx)
				sp.syncRouterDue()
			}()
		}
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------------------------------------------------------------- recall

// Show renders any object by id with links to related ids (chain of recall).
func (sp *Super) Show(id string, before int64) (string, bool) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	var b strings.Builder
	t := sp.st.trigger(id)
	if t == nil && sp.db != nil {
		var err error
		if t, err = triggerByID(sp.db, id); err != nil {
			log.Printf("show %s: %v", id, err)
		}
	}
	if t != nil {
		fmt.Fprintf(&b, "trigger %s\nsource: %s  thread: %s  from: %s  at: %s  status: %s  action: %s\n", t.ID, t.Source, t.Thread, t.From, t.At.Format(time.RFC3339), t.Status, t.Action)
		if t.Ref != "" {
			fmt.Fprintf(&b, "ref: %s\n", t.Ref)
		}
		fmt.Fprintf(&b, "\n%s\n", t.Text)
		return b.String(), true
	}
	if out, ok := sp.showThreadLocked(id, before); ok {
		return out, true
	}
	if w := sp.st.Workers[id]; w != nil {
		fmt.Fprintf(&b, "worker %s (%s)\nstatus: %s  thread: %s  pane: %s  cwd: %s\ncreated: %s by %s\n", w.Name, w.Kind, w.Status, w.Thread, w.PaneID, w.Cwd, w.CreatedAt.Format("01-02 15:04"), w.CreatedBy)
		fmt.Fprintf(&b, "\ntask:\n%s\n", w.Task)
		if w.LastReport != "" {
			fmt.Fprintf(&b, "\nlast report (%s):\n%s\n", w.ReportAt.Format("01-02 15:04"), w.LastReport)
		}
		if w.Note != "" {
			fmt.Fprintf(&b, "\nnote: %s\n", w.Note)
		}
		fmt.Fprintf(&b, "\nterminal: herdr agent read %s --source recent-unwrapped --lines 80\n", w.Name)
		return b.String(), true
	}
	return "", false
}

func (sp *Super) List() string {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	var b strings.Builder
	b.WriteString("machines (pass the name to `collie spawn -machine`):\n")
	for _, m := range sp.s.herdr.Machines() {
		state := "online"
		if !m.Local && !m.Online {
			state = "offline"
		}
		id := m.ID
		if m.Local {
			id = "hub"
		}
		fmt.Fprintf(&b, "  %-14s %-8s %s  home=%s\n", id, state, m.Name, m.Home)
	}
	b.WriteString("workers:\n")
	names := make([]string, 0, len(sp.st.Workers))
	for n := range sp.st.Workers {
		names = append(names, n)
	}
	sort.Strings(names)
	any := false
	for _, n := range names {
		w := sp.st.Workers[n]
		if w.Ended && time.Since(w.CreatedAt) > 24*time.Hour {
			continue
		}
		any = true
		fmt.Fprintf(&b, "  %-14s %-8s %-8s thread=%s  %s\n", w.Name, w.Kind, w.Status, w.Thread, clip(firstLine(w.Task), 70))
	}
	if !any {
		b.WriteString("  (none)\n")
	}
	b.WriteString("threads:\n")
	ths := make([]*Thread, 0, len(sp.st.Threads))
	for _, th := range sp.st.Threads {
		ths = append(ths, th)
	}
	sort.Slice(ths, func(i, j int) bool { return ths[i].LastAt.After(ths[j].LastAt) })
	for i, th := range ths {
		if i >= 12 {
			break
		}
		fmt.Fprintf(&b, "  %-28s last=%s subscribers=%s\n", th.ID, th.LastAt.Format("01-02 15:04"), strings.Join(th.Subscribers, ","))
	}
	b.WriteString("timers:\n")
	for _, t := range sp.st.Timers {
		if t.Enabled {
			fmt.Fprintf(&b, "  %s %q next=%s → %s\n", t.ID, t.Spec, t.NextRun.Format("01-02 15:04"), clip(t.Text, 60))
		}
	}
	return b.String()
}

// stableBin returns a path to this binary that agents can keep calling. A
// `go run` build lives in a temp dir that vanishes on restart, so in that case
// copy the executable to ~/.collie/bin/collie and hand out that path instead.
func stableBin(exe, dir string) string {
	if !strings.Contains(exe, "go-build") {
		return exe
	}
	dst := filepath.Join(dir, "bin", "collie")
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return exe
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		return exe
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		return exe
	}
	if err := os.Rename(tmp, dst); err != nil {
		return exe
	}
	return dst
}

func (sp *Super) settings() Settings {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.st.Settings
}

// collieAllow is the Claude Code permission rule set for calling the collie
// CLI, by its absolute path and by name.
func collieAllow(bin string, extra ...string) []string {
	rules := []string{"Bash(collie:*)", "Bash(collie *)", "Read(~/.collie/uploads/**)"}
	// Claude matches the resolved path too; home is often a symlink.
	if up, err := filepath.EvalSymlinks(filepath.Join(collieDir(), "uploads")); err == nil {
		rules = append(rules, "Read(/"+up+"/**)")
	}
	if bin != "" && bin != "collie" {
		rules = append(rules, "Bash("+bin+":*)", "Bash("+bin+" *)")
	}
	return append(rules, extra...)
}
