package collie

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/TaoSama/collie/internal/extproto"
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

func (sp *Super) changed() {
	sp.dirty = true
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
	if !sp.dirty {
		return
	}
	if err := saveState(sp.db, sp.st, sp.deletedTimers); err != nil {
		log.Printf("save state: %v", err)
		return
	}
	sp.st.trim() // history beyond the working set stays in SQLite
	sp.dirty, sp.deletedTimers = false, nil
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
	if e.Role == "collie" || e.Tone == "blocked" || e.Tone == "error" {
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
	if key != "" {
		for i := len(sp.st.Triggers) - 1; i >= 0 && i > len(sp.st.Triggers)-400; i-- {
			if sp.st.Triggers[i].Key == key {
				return nil
			}
		}
	}
	if thread == "" {
		thread = mainThread
	}
	pol := sp.st.policyFor(source)
	action := pol.Action
	if len(pol.Allow) > 0 && !contains(pol.Allow, from) {
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
	sp.st.Triggers = append(sp.st.Triggers, t)
	th := sp.threadLocked(thread)
	th.LastAt = t.At
	th.Triggers = append(th.Triggers, t.ID)
	sp.changed()
	return t
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
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

type liveAgent struct {
	Pane    string
	Status  string
	Ready   bool
	Kind    string
	Session string
}

var uuidRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// sessionID is what an agent's conversation is resumed by. herdr gives some
// (Codex) as the path of their log, whose name ends in the id.
func sessionID(kind, value string) string {
	if kind != "path" {
		return value
	}
	ids := uuidRe.FindAllString(filepath.Base(value), -1)
	if len(ids) == 0 {
		return ""
	}
	return ids[len(ids)-1]
}

func (sp *Super) liveAgents(ctx context.Context) (map[string]liveAgent, bool) {
	list, ok := sp.agentList(ctx)
	if !ok {
		return nil, false
	}
	return sp.byName(list), true
}

func (sp *Super) agentList(ctx context.Context) ([]Agent, bool) {
	if sp.s == nil || sp.s.herdr == nil {
		return nil, false
	}
	list, err := sp.s.herdr.Agents(ctx)
	return list, err == nil
}

// byName is the named agents in list: Collie's workers, and the Router as routerName.
func (sp *Super) byName(list []Agent) map[string]liveAgent {
	m := map[string]liveAgent{}
	router := sp.routerAgent()
	for _, a := range list {
		if a.Name == nil {
			continue
		}
		name := *a.Name
		switch name {
		case router:
			name = routerName
		case routerName:
			continue // another agent took the name the Router could not have
		}
		la := liveAgent{Pane: a.PaneID, Status: a.AgentStatus, Ready: a.Ready, Kind: deref(a.Agent)}
		if a.Session != nil {
			la.Session = sessionID(a.Session.Kind, a.Session.Value)
		}
		m[name] = la
	}
	return m
}

func (sp *Super) tick(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	sp.wb.snapshotDue() // feeds are read over the web: herdr need not be up
	// Listing every computer can take seconds. A worker spawned or done
	// starting meanwhile is missing from a list taken before it was named:
	// it isn't gone, and its brief must not go to the Router instead. Only
	// the workers already up before the list can be missing from it.
	sp.mu.Lock()
	settled := map[string]*Worker{}
	for name, w := range sp.st.Workers {
		if w.Status != "starting" {
			settled[name] = w
		}
	}
	sp.mu.Unlock()
	// One computer that hangs must not use up the tick: it is left out of
	// this list, and what is on it waits for it (away).
	listCtx, cancelList := context.WithTimeout(ctx, tickListWait)
	list, answered, ok := sp.agentsListed(listCtx)
	cancelList()
	if !ok {
		return // herdr unavailable; keep everything queued
	}
	sp.push.watch(list)
	live := sp.byName(list)
	now := time.Now()
	away := sp.awayMachines(answered, now)

	type finished struct {
		w       *Worker
		blocked bool
	}
	var fin []finished
	var dead []*Worker // the agent never started, or quit, leaving a shell
	deliveries := map[string][]*Trigger{}

	sp.mu.Lock()
	paused := sp.st.Settings.Paused
	// 1. Timers → triggers.
	for _, t := range sp.st.Timers {
		if t.Enabled && !t.NextRun.After(now) {
			at := now
			t.LastRun = &at
			t.advance(now)
			sp.ingestLocked("timer", t.Thread, t.ID, t.Text, "", t.Target, t.ID)
		}
	}
	// 2. Worker lifecycle → triggers for the router.
	for _, w := range sp.st.Workers {
		if w.Ended {
			// herdr restored its pane (a herdr restart or update): it's back.
			if a, ok := live[w.Name]; ok && w.Status == "gone" && w.PaneID != "" && a.Pane == w.PaneID {
				w.Ended, w.Status = false, a.Status
				sp.ingestLocked("worker", w.Thread, w.Name, fmt.Sprintf("worker %s is back: herdr restored its pane (status %s).", w.Name, a.Status), "", "", w.Name)
				sp.addEntryLocked("event", w.Thread, w.Name+" 回来了（herdr 恢复了它的窗格）", w.Name, "info")
				sp.changed()
			}
			continue
		}
		a, alive := live[w.Name]
		if !alive && away[w.Machine] {
			continue // its computer didn't answer: it may well be running
		}
		if !alive {
			if w.Status != "starting" && w.Status != "failed" && settled[w.Name] == w {
				w.Ended, w.Status = true, "gone"
				sp.ingestLocked("worker", w.Thread, w.Name, fmt.Sprintf("worker %s exited (its herdr pane is gone).", w.Name), "", "", w.Name)
				sp.addEntryLocked("event", w.Thread, w.Name+" 已退出", w.Name, "info")
			}
			continue
		}
		// No agent in the pane for a while: it failed to start (a broken
		// install, a bad flag) or quit. Messages for it would wait forever.
		if a.Kind == "" {
			since, seen := sp.noAgent[w.Name]
			if !seen {
				sp.noAgent[w.Name], since = now, now
			}
			if now.Sub(since) > 30*time.Second && now.Sub(w.CreatedAt) > 45*time.Second {
				delete(sp.noAgent, w.Name)
				w.Ended, w.Status, w.PaneID = true, "failed", a.Pane
				for _, t := range sp.st.Triggers {
					if t.Status == "queued" && len(t.Targets) == 1 && t.Targets[0] == w.Name {
						t.Status = "ignored"
					}
				}
				dead = append(dead, w)
				sp.changed()
			}
			continue
		}
		delete(sp.noAgent, w.Name)
		if a.Session != "" && a.Session != w.Session {
			w.Session = a.Session
			sp.dirty = true
		}
		prev := w.Status
		if a.Status == "blocked" && now.Sub(sp.lastAnswer[a.Pane]) < 2*time.Second {
			continue // give a just-answered dialog a moment to clear (or show the next one)
		}
		if a.Status != prev {
			w.Status = a.Status
			w.PaneID = a.Pane
			sp.changed()
			if prev == "blocked" {
				sp.settleAskLocked(a.Pane, "", "") // answered in its terminal, or it moved on
			}
			switch {
			case a.Status == "blocked":
				fin = append(fin, finished{w, true})
			case prev == "working" && (a.Status == "done" || a.Status == "idle"):
				if now.Sub(w.ReportAt) > 90*time.Second {
					fin = append(fin, finished{w, false})
				}
			}
		}
	}
	// 3. Route queued triggers to sessions: subscribed workers first (fast path), router otherwise.
	for _, t := range sp.st.Triggers {
		if t.Status != "queued" {
			continue
		}
		if len(t.Targets) == 0 {
			if t.Source != "worker" && t.Source != "chat" {
				if th := sp.st.Threads[t.Thread]; th != nil {
					for _, sub := range th.Subscribers {
						if w := sp.st.Workers[sub]; w != nil && !w.Ended && sub != t.From {
							t.Targets = append(t.Targets, sub)
						}
					}
				}
			}
			if len(t.Targets) == 0 {
				t.Targets = []string{routerName}
			}
		}
		for _, target := range t.Targets {
			deliveries[target] = append(deliveries[target], t)
		}
	}
	routerAlive := false
	_, routerAlive = live[routerName]
	sp.mu.Unlock()

	// Worker completion summaries need file/screen IO: do it outside the lock.
	for _, f := range fin {
		sp.workerSettled(ctx, f.w, f.blocked)
	}
	for _, w := range dead {
		sp.workerDead(ctx, w)
	}

	if paused {
		return
	}

	// 4. Deliver batches to free sessions, side by side: each one is a send
	// and a read back across the internet, with its own time.
	var sending sync.WaitGroup
	for target, list := range deliveries {
		if target == routerName && (sp.handingOver() || sp.dreaming()) {
			continue // events wait for the fresh session
		}
		a, alive := live[target]
		if !alive {
			if away[sp.targetMachine(target)] {
				continue // delivered as soon as its computer answers again
			}
			if target == routerName {
				sp.ensureRouter(ctx)
			} else {
				sp.rerouteToRouter(target, list)
			}
			continue
		}
		if target == routerName && a.Status == "blocked" {
			sp.trustOwnDir(ctx, a.Pane)
		}
		// Nothing is typed over a dialog or into an agent that is still
		// starting. Otherwise a message goes in mid-turn: Claude, Codex and agy
		// queue typed input and pick it up between steps, so corrections and
		// questions arrive now rather than after the current step. Collie's
		// other events (reports, timers) batch up while it works; the human's
		// own messages don't wait.
		if a.Status == "blocked" || (!a.Ready && a.Status == "unknown") {
			continue
		}
		if a.Status == "working" && target == routerName {
			if list = fromHuman(list); len(list) == 0 {
				continue
			}
		}
		sending.Add(1)
		go func() {
			defer sending.Done()
			dctx, cancel := context.WithTimeout(parent, deliverWait)
			defer cancel()
			sp.deliver(dctx, target, list)
		}()
	}
	sending.Wait()

	if a, ok := live[routerName]; ok && (a.Status == "done" || a.Status == "idle") {
		sp.routerSettled(ctx, a.Pane)
	}
	sp.checkHandover(ctx, live, len(deliveries[routerName]) > 0)
	sp.checkDream(ctx, live, len(deliveries[routerName]) > 0)

	// 5. Watchdog.
	sp.watchdog(ctx, live, routerAlive)
}

const (
	tickListWait = 3 * time.Second
	deliverWait  = 30 * time.Second // a send, read back, and its retries
	// awayGrace: how long a computer that doesn't answer (reconnecting,
	// hanging) keeps its workers. Past it they count as gone, as before.
	awayGrace = 2 * time.Minute
)

// agentsListed is the agent list with the computers that answered.
func (sp *Super) agentsListed(ctx context.Context) ([]Agent, map[string]bool, bool) {
	if sp.s == nil || sp.s.herdr == nil {
		return nil, nil, false
	}
	list, answered, err := sp.s.herdr.AgentsListed(ctx)
	return list, answered, err == nil
}

// awayMachines is the computers that didn't answer this list (or aren't
// connected, as after a hub restart), for less than awayGrace so far.
func (sp *Super) awayMachines(answered map[string]bool, now time.Time) map[string]bool {
	ids := map[string]bool{}
	for _, m := range sp.s.herdr.Machines() {
		ids[m.ID] = true
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	for _, w := range sp.st.Workers {
		if !w.Ended {
			ids[w.Machine] = true
		}
	}
	ids[sp.routerMachineLocked()] = true
	away := map[string]bool{}
	for id := range sp.unanswered {
		if !ids[id] || answered[id] {
			delete(sp.unanswered, id)
		}
	}
	for id := range ids {
		if answered[id] {
			continue
		}
		since, ok := sp.unanswered[id]
		if !ok {
			sp.unanswered[id], since = now, now
		}
		away[id] = now.Sub(since) < awayGrace
	}
	return away
}

// targetMachine is the computer a delivery target runs on.
func (sp *Super) targetMachine(target string) string {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if target == routerName {
		return sp.routerMachineLocked()
	}
	if w := sp.st.Workers[target]; w != nil {
		return w.Machine
	}
	return ""
}

// fromHuman keeps what the human said in the chat.
func fromHuman(list []*Trigger) []*Trigger {
	var out []*Trigger
	for _, t := range list {
		if t.Source == "chat" {
			out = append(out, t)
		}
	}
	return out
}

func (sp *Super) rerouteToRouter(target string, list []*Trigger) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	w := sp.st.Workers[target]
	if w != nil && !w.Ended && (w.Status == "starting" || time.Since(w.CreatedAt) < 2*time.Minute) {
		return // still launching
	}
	for _, t := range list {
		t.Targets = replace(t.Targets, target, routerName)
	}
	sp.changed()
}

func replace(list []string, from, to string) []string {
	out := []string{}
	for _, x := range list {
		if x == from {
			x = to
		}
		if !contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// deliver submits all pending triggers for one session as a single prompt.
func (sp *Super) deliver(ctx context.Context, target string, list []*Trigger) {
	sort.Slice(list, func(i, j int) bool { return list[i].At.Before(list[j].At) })
	if len(list) > 20 {
		list = list[:20]
	}
	sp.mu.Lock()
	fresh := target == routerName && sp.routerFresh
	words := sp.st.Settings.ReplyChars
	sp.mu.Unlock()

	var b strings.Builder
	if fresh {
		b.WriteString(sp.sessionStart())
		b.WriteString("\n")
	}
	if len(list) == 1 {
		b.WriteString("[collie] 1 new event\n")
	} else {
		fmt.Fprintf(&b, "[collie] %d new events (batched)\n", len(list))
	}
	for _, t := range list {
		fmt.Fprintf(&b, "\n#%s · %s · %s", t.ID, t.Thread, t.Source)
		if t.From != "" {
			fmt.Fprintf(&b, " · from %s", t.From)
		}
		if t.Action == "candidate" {
			b.WriteString(" · candidate (act only if a rule or memory says so; otherwise stay silent)")
		}
		fmt.Fprintf(&b, " · %s\n", t.At.Format("15:04"))
		text := strings.TrimSpace(t.Text)
		if len([]rune(text)) > 700 { // Inline & Spill
			text = string([]rune(text)[:700]) + fmt.Sprintf(" … [spilled; run `collie show %s`]", t.ID)
		}
		b.WriteString(indent(text, "  "))
		b.WriteString("\n")
	}
	if target == routerName {
		fmt.Fprintf(&b, "\nReply with `collie reply`, usually ≤%d characters.", words)
	}

	err := sp.s.herdr.Prompt(ctx, sp.herdrTarget(target), b.String())
	var he *HerdrError
	if err != nil && errors.As(err, &he) && (he.Code == "agent_blocked" || he.Code == "agent_not_ready") {
		return
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if err != nil {
		log.Printf("deliver to %s: %v", target, err)
	}
	if err != nil && (he == nil || he.Code != "prompt_unconfirmed") {
		return
	}
	if err != nil {
		// Sent and read back, retried, still not in: sending it yet again
		// could submit it twice, so it stops here, where the human sees it.
		ids := make([]string, len(list))
		for i, t := range list {
			ids[i] = "#" + t.ID
			t.Targets = remove(t.Targets, target)
			if len(t.Targets) == 0 {
				t.Status = "failed"
			}
		}
		who := target
		if target == routerName {
			who = "Collie"
		}
		sp.addEntryLocked("event", list[0].Thread, fmt.Sprintf("没能把消息送进 %s 的终端（%s）：%s", who, strings.Join(ids, " "), he.Message), target, "error")
		sp.changed()
		return
	}
	if fresh {
		sp.routerFresh = false
	}
	if target == routerName && sp.routerAskedAt.IsZero() {
		sp.routerAskedAt = time.Now() // a message mid-turn keeps the turn's start
	}
	for _, t := range list {
		t.Targets = remove(t.Targets, target)
		if len(t.Targets) == 0 {
			t.Status = "delivered"
		}
	}
	sp.changed()
}

func remove(list []string, v string) []string {
	out := list[:0:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func indent(s, p string) string {
	return p + strings.ReplaceAll(s, "\n", "\n"+p)
}

// paneTail is the last non-blank lines on a pane's screen ("" if unreadable).
func (sp *Super) paneTail(ctx context.Context, pane string) string {
	res, err := sp.s.herdr.Read(ctx, pane, "recent_unwrapped", "text", 40)
	if err != nil {
		return ""
	}
	var keep []string
	for _, l := range strings.Split(res.Text, "\n") {
		if l = strings.TrimRight(l, " "); strings.TrimSpace(l) != "" {
			keep = append(keep, l)
		}
	}
	if len(keep) > 12 {
		keep = keep[len(keep)-12:]
	}
	return strings.Join(keep, "\n")
}

// closeWorkspace closes a herdr workspace, logging a failure.
func (sp *Super) closeWorkspace(ctx context.Context, ws string) {
	if err := sp.s.herdr.Call(ctx, "workspace.close", map[string]any{"workspace_id": ws}, nil); err != nil {
		log.Printf("close workspace %s: %v", ws, err)
	}
}

// workerSettled turns a worker reaching done/blocked into a router trigger,
// carrying a short summary from the worker's transcript or screen.
// workerDead tells the router that a worker's agent isn't running, with what
// its screen says (usually the error it died with).
func (sp *Super) workerDead(ctx context.Context, w *Worker) {
	tail := sp.paneTail(ctx, w.PaneID)
	// Its screen is in the messages below; the shell it left goes.
	if w.WorkspaceID != "" {
		sp.closeWorkspace(ctx, w.WorkspaceID)
	}
	why := deathReason(tail)
	log.Printf("worker %s: no agent in its pane %s; marked failed: %s", w.Name, w.PaneID, why)
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.ingestLocked("worker", w.Thread, w.Name, fmt.Sprintf(
		"worker %s failed: %s isn't running in its pane (it never started, or it quit), so its task was not delivered. Last lines on its screen:\n%s\nFix the cause or tell the human; spawn a new worker to retry.",
		w.Name, w.Kind, tail), "", "", w.Name)
	// The Router retries it or tells the human; the spawn line shows it ended.
}

// deathReason picks the line that says why: "Error: …" over a stack frame
// like "throw new Error(".
func deathReason(screen string) string {
	var loose string
	for _, l := range strings.Split(screen, "\n") {
		t := strings.TrimSpace(l)
		low := strings.ToLower(t)
		if strings.Contains(low, "error:") || strings.HasPrefix(low, "fatal") || strings.Contains(low, "panic:") ||
			strings.Contains(low, "command not found") || strings.Contains(low, "no such file") ||
			strings.HasPrefix(low, "no conversation found") {
			return t
		}
		if loose == "" && strings.Contains(low, "error") {
			loose = t
		}
	}
	return loose
}

func (sp *Super) workerSettled(ctx context.Context, w *Worker, blocked bool) {
	// The human chose to bypass permissions in settings, so a worker's
	// "trust this folder?" (the folder Collie started it in) isn't theirs to
	// click. Anything else still goes to them.
	sp.mu.Lock()
	ask := sp.st.Settings.AskPermissions
	sp.mu.Unlock()
	if blocked && !ask && sp.answerTrust(ctx, w.PaneID, w.Cwd) {
		sp.mu.Lock()
		sp.noteAnsweredLocked(w)
		sp.mu.Unlock()
		log.Printf("worker %s: trusted its folder %s", w.Name, w.Cwd)
		return
	}
	summary := sp.lastAssistant(ctx, w)
	var dialog *Approval
	if blocked {
		if screen, err := sp.s.herdr.ReadAsk(ctx, w.PaneID); err == nil && screenAsks(screen) {
			a := parseApproval(screen)
			dialog = &a
		}
	}
	if w.Conv != "" && !blocked && sp.s.ext != nil {
		// An extension conversation's agent: its turn's last message is the
		// answer, sent back to that chat, not a report for the Router.
		// Only its own words go out: never the screen, which may be the
		// task it was given.
		text, kind := sp.lastReply(ctx, w, 0, false), extproto.KindAnswer
		if msg := sp.workerAgentError(ctx, w, text); msg != "" {
			text = "（agent 出错，没能完成：" + msg + "）"
		} else if text == "" {
			text, kind = "（没取到 agent 的回答：找不到它的会话记录。请在 Collie 里看 "+w.Name+"。）", extproto.KindProgress
		}
		if _, err := sp.s.ext.sendOut(w.Thread, text, kind, sp.workerRunInfo(w), ""); err != nil {
			log.Printf("worker %s: send the answer back: %v", w.Name, err)
		}
		return
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if blocked {
		sp.ingestLocked("worker", w.Thread, w.Name,
			fmt.Sprintf("worker %s is BLOCKED on a dialog in its terminal (the human also sees it as a card in chat). Screen:\n%s%s\nIf the answer follows from what the human asked, or is routine, answer it yourself: `collie answer %s <choice>`. Leave it to the human only when it is theirs to decide (see ROUTER.md).", w.Name, summary, choicesText(dialog), w.Name), "", "", w.Name)
		sp.addAskLocked(w, dialog)
		return
	}
	if msg := sp.workerAgentError(ctx, w, summary); msg != "" {
		// Its agent stopped it (out of quota, logged out, the API down):
		// the turn ended, the work did not.
		sp.ingestLocked("worker", w.Thread, w.Name,
			fmt.Sprintf("worker %s stopped on an error from its agent, not because it was done:\n%s\nLast message before it:\n%s\nIf this is a usage limit, a login or an outage, it won't get better by itself soon: hand the job to another kind of agent (spawn a fresh one with the brief and what this one got done; --resume only works with the same agent), then tell the human in one line what happened and who carries on.", w.Name, msg, summary), "", "", w.Name)
		log.Printf("worker %s stopped on an agent error: %s", w.Name, msg)
		return
	}
	sp.ingestLocked("worker", w.Thread, w.Name,
		fmt.Sprintf("worker %s finished a turn. Last message:\n%s", w.Name, summary), "", "", w.Name)
	sp.addEntryLocked("event", w.Thread, w.Name+" 完成了一轮", w.Name, "done")
}

// workerQuotaRe is an agent CLI saying it can't go on: a usage limit, the
// login, the API. Narrower than agentErrRe: a worker's screen is full of its
// work, and "error:" there is mostly the work's own.
var workerQuotaRe = regexp.MustCompile(`(?i)(usage limit|limit reached|hit your( \w+)? limit|reached your( \w+)? limit|quota exceeded|exceeded your( current)? quota|resource.?exhausted|daily limit|rate.?limit(ed)?\b|credit balance|out of credits|please run /login|not logged in|invalid api key|api error|overloaded|unexpected status \d{3}|try again (at|in) )`)

// workerAgentError reads the bottom of a worker's terminal after a turn:
// what its agent printed when it gave up, or "". A line that is also in its
// last message is the work talking about such things, not the agent.
func (sp *Super) workerAgentError(ctx context.Context, w *Worker, summary string) string {
	if w.PaneID == "" {
		return ""
	}
	rr, err := sp.s.herdr.Read(ctx, w.PaneID, "recent", "text", 40)
	if err != nil {
		return ""
	}
	return agentStopLine(rr.Text, summary)
}

func agentStopLine(screen, summary string) string {
	var tail []string
	for _, l := range strings.Split(screen, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			tail = append(tail, t)
		}
	}
	if len(tail) > 12 {
		tail = tail[len(tail)-12:]
	}
	for _, l := range tail {
		if workerQuotaRe.MatchString(l) && !strings.Contains(summary, strings.Trim(l, "⎿⏺●•✗✘⚠■│ ")) {
			return clip(strings.Trim(l, "⎿⏺●•✗✘⚠■│ "), 200)
		}
	}
	return ""
}

func (sp *Super) lastAssistant(ctx context.Context, w *Worker) string {
	return sp.lastReply(ctx, w, 1200, true)
}

// lastReply is w's last message, clipped to most runes (0: all of it). With
// screen, the bottom of its screen stands in when its transcript can't be
// found; without, that is "".
func (sp *Super) lastReply(ctx context.Context, w *Worker, most int, screen bool) string {
	cut := func(s string) string {
		if most > 0 {
			return clip(s, most)
		}
		return s
	}
	if pane, machine := split(w.PaneID); machine != "" {
		// Its transcript lives on that computer: ask its node.
		if c, err := sp.s.herdr.Client(machine); err == nil {
			u := "http://node/api/panes/" + url.PathEscape(pane) + "/last"
			if !screen {
				u += "?screen=0"
			}
			req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
			if resp, err := c.Do(req); err == nil {
				var out struct {
					Text string `json:"text"`
				}
				json.NewDecoder(resp.Body).Decode(&out)
				resp.Body.Close()
				if out.Text != "" {
					return cut(out.Text)
				}
			}
		}
	} else if w.PaneID != "" {
		var info struct {
			Agent agentDetail `json:"agent"`
		}
		if sp.s.herdr.Call(ctx, "agent.get", map[string]any{"target": w.Name}, &info) == nil {
			if tr := sp.s.transcriptFor(w.Name, w.PaneID, info.Agent, sp.transcripts); tr != nil {
				tr.update()
				msgs := tr.view()
				for i := len(msgs) - 1; i >= 0; i-- {
					if msgs[i].Kind == "assistant" {
						return cut(msgs[i].Text)
					}
				}
			}
		}
		if !screen {
			return ""
		}
		if res, err := sp.s.herdr.Read(ctx, w.PaneID, "detection", "text", 0); err == nil {
			return tailLines(res.Text, 14)
		}
	}
	if !screen {
		return ""
	}
	return "(no output captured)"
}

// ---------------------------------------------------------------- router

func (sp *Super) routerDir() string { return filepath.Join(sp.dir, "router") }

func (sp *Super) ensureRouter(ctx context.Context) {
	if sp.s == nil || sp.s.herdr == nil {
		return
	}
	sp.mu.Lock()
	if sp.routerLaunching || time.Since(sp.routerLaunchAt) < 45*time.Second {
		sp.mu.Unlock()
		return
	}
	sp.routerLaunching, sp.routerLaunchAt = true, time.Now()
	kind := sp.st.Settings.RouterKind
	machine := sp.routerMachineLocked()
	args := sp.routerArgsLocked()
	aliasName := sp.st.Settings.RouterAlias
	prevRun, prevLaunch := sp.routerRunLocked(), sp.routerLaunchLocked()
	sp.setRouterRun([2]string{sp.st.Settings.RouterModel, sp.st.Settings.RouterEffort})
	sp.setRouterOn(machine, aliasName)
	sp.mu.Unlock()
	// keep: no new Router started; the record goes back to the running one's.
	keep := func(name string) {
		sp.mu.Lock()
		defer sp.mu.Unlock()
		sp.routerLaunchOKLocked()
		if prevRun != nil {
			sp.setRouterRun(*prevRun)
		}
		prevLaunch.Name = name
		sp.saveRouterLaunchLocked(prevLaunch)
	}

	go func() {
		defer func() { sp.mu.Lock(); sp.routerLaunching = false; sp.mu.Unlock() }()
		lctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// Another computer's paths go into ROUTER.md: learn them before writing it.
		err := sp.syncRouter(lctx)
		if err == nil {
			if err = sp.writeRouterFiles(); err != nil {
				log.Printf("router files: %v", err)
			}
			sp.routerReadsFresh()
			err = sp.syncRouter(lctx)
		}
		// The tick that sent us here may have listed agents while the Router's
		// computer was still reconnecting (a hub restart, a node update): a
		// sync went through, so it answers now. Is the Router there after all?
		if err == nil {
			if live, ok := sp.liveAgents(lctx); ok {
				if a, alive := live[routerName]; alive {
					log.Printf("router: %s is running after all; keeping it", a.Pane)
					keep(prevLaunch.Name)
					return
				}
			}
		}
		var name string // the herdr name the new Router got, if it started
		kept := false
		if err == nil {
			sp.closeStaleRouterWorkspaces(lctx)
			alias := sp.launchAlias(lctx, machine, aliasName, kinds[kind], routerName)
			sp.noteAliasFallback(machine, aliasName, alias)
			var pane, ws string
			pane, ws, err = sp.s.startAgentAs(lctx, machine, kinds[kind], routerName, sp.routerCwd(machine), routerLabel, alias, args)
			switch {
			case nameTaken(err) && pane != "":
				name, kept, err = sp.settleNameTaken(lctx, machine, pane, ws, kinds[kind], args)
			case pane != "":
				name = routerName
			}
		} else {
			err = fmt.Errorf("没能把 Collie 的目录同步到 %s：%w", machine, err)
		}
		if kept {
			keep("") // the Router that had the name all along carries on
			return
		}
		sp.mu.Lock()
		defer sp.mu.Unlock()
		if name != "" {
			sp.setRouterNameLocked(name)
		}
		var he *HerdrError
		if err != nil && !(errors.As(err, &he) && he.Code == "agent_not_ready") {
			if msg := sp.routerLaunchFailedLocked(machine, err); msg != "" {
				sp.addEntryLocked("event", mainThread, msg, routerName, "error")
			}
			return
		}
		sp.routerLaunchOKLocked()
		sp.routerFresh = true
		sp.addEntryLocked("event", mainThread, "Collie 已唤醒（"+kind+"）", routerName, "info")
	}()
}

func (sp *Super) RestartRouter(ctx context.Context) error {
	if live, ok := sp.liveAgents(ctx); ok {
		if a, ok := live[routerName]; ok {
			var p Pane
			if err := sp.s.herdr.Call(ctx, "pane.get", map[string]any{"pane_id": a.Pane}, &struct {
				Pane *Pane `json:"pane"`
			}{&p}); err == nil && p.WorkspaceID != "" {
				sp.s.herdr.Call(ctx, "workspace.close", map[string]any{"workspace_id": p.WorkspaceID}, nil)
			}
		}
	}
	sp.mu.Lock()
	sp.routerLaunchAt = time.Time{}
	sp.mu.Unlock()
	sp.ensureRouter(ctx)
	return nil
}

// followLocked is the soonest one-off timer Collie set itself ("in 30m",
// "at …"): a follow-up. Routines like a weekday check are not one.
func (sp *Super) followLocked() *followView {
	var f *followView
	for _, t := range sp.st.Timers {
		if t.Enabled && t.Owner != "you" && (strings.HasPrefix(t.Spec, "in ") || strings.HasPrefix(t.Spec, "at ")) && t.NextRun.After(time.Now()) {
			if f == nil || t.NextRun.Before(f.Next) {
				f = &followView{Next: t.NextRun, NextText: clip(firstLine(t.Text), 80)}
			}
		}
	}
	return f
}

// routerReadFile keeps a copy of the ROUTER.md the running Collie read.
const routerReadFile = ".router-read.md"

// routerDocChanged: ROUTER.md was just written (a new build, new settings).
// The running Collie read it when its session began; if it now says
// something else, have it read it again.
func (sp *Super) routerDocChanged(dir string, doc []byte) {
	read, err := os.ReadFile(filepath.Join(dir, routerReadFile))
	if err != nil || bytes.Equal(read, doc) {
		return // no session has read it yet, or nothing changed
	}
	os.WriteFile(filepath.Join(dir, routerReadFile), doc, 0o600)
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if t := sp.rereadTrig; t != nil && t.Status == "queued" {
		return // already asked
	}
	sp.rereadTrig = sp.ingestLocked("system", mainThread, "collie", "ROUTER.md changed (a new Collie build or new settings). Re-read it now and work by it from here on. Nothing to tell the human.", "", "", "")
}

// routerReadsFresh: a new session is starting and reads ROUTER.md as it is.
func (sp *Super) routerReadsFresh() {
	dir := sp.routerDir()
	if b, err := os.ReadFile(filepath.Join(dir, "ROUTER.md")); err == nil {
		os.WriteFile(filepath.Join(dir, routerReadFile), b, 0o600)
	}
	sp.mu.Lock()
	if t := sp.rereadTrig; t != nil && t.Status == "queued" {
		t.Status = "ignored" // it reads them anyway
	}
	sp.rereadTrig = nil
	sp.mu.Unlock()
}

func (sp *Super) writeRouterFiles() error {
	dir := sp.routerDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sp.mu.Lock()
	words := sp.st.Settings.ReplyChars
	wkind := sp.st.Settings.WorkerKind
	wcwd := sp.s.tilde(sp.st.Settings.WorkerCwd)
	machine := sp.routerOnLocked()
	sp.mu.Unlock()
	tdir, tbin := sp.routerTarget(machine)
	if tdir == "" { // that computer's folder is not known yet: the first sync writes these
		sp.ensureMemory()
		return nil
	}
	layout := "   " + strings.ReplaceAll(memoryLayout, "\n", "\n   ")
	doc := strings.NewReplacer("{{LAYOUT}}", layout, "{{BIN}}", tbin, "{{WORDS}}", fmt.Sprint(words), "{{WKIND}}", wkind, "{{WCWD}}", wcwd, "{{BUDGET}}", fmt.Sprint(upFrontBudget>>10)).Replace(routerDoc)
	for _, name := range []string{"ROUTER.md", "CLAUDE.md", "AGENTS.md", "GEMINI.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(doc), 0o600); err != nil {
			return err
		}
	}
	sp.routerDocChanged(dir, []byte(doc))
	// Claude Code asks before every shell command; the router's whole job is
	// running the collie CLI and keeping memory/, so allow exactly that.
	perms, _ := json.MarshalIndent(map[string]any{"permissions": map[string]any{
		"allow": collieAllow(tbin, append(memoryRules(tdir),
			// Read-only peeks at a worker's terminal.
			"Bash(herdr agent read:*)", "Bash(herdr agent list:*)", "Bash(herdr agent get:*)", "Bash(herdr pane read:*)")...),
	}}, "", "  ")
	os.MkdirAll(filepath.Join(dir, ".claude"), 0o700)
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), perms, 0o600); err != nil {
		return err
	}
	sp.ensureMemory()
	sp.syncRouterNow()
	return nil
}

// ---------------------------------------------------------------- workers

var workerNameRe = regexp.MustCompile(`[^a-z0-9_-]+`)

func cleanWorkerName(name string) string {
	return strings.Trim(workerNameRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

// Spawn starts a worker session asynchronously (start() returns at once; the
// outcome arrives later as a trigger — the async ReAct pattern).
// model and effort are launch choices for this worker; "" falls back to the
// default worker settings when kind is the default kind, else the agent's own.
// resumeFrom is what `spawn --resume` carries over from a stopped worker:
// where it ran and the conversation to open again.
func (sp *Super) resumeFrom(name string) (*Worker, error) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	w := sp.st.Workers[name]
	switch {
	case w == nil:
		return nil, fmt.Errorf("no worker named %s", name)
	case !w.Ended:
		return nil, fmt.Errorf("%s is still running: `send` it instead", name)
	case w.Session == "":
		return nil, fmt.Errorf("%s left no conversation to resume; spawn a fresh worker", name)
	}
	c := *w
	return &c, nil
}

// SpawnSpec is what a worker starts with; "" takes the settings' default.
type SpawnSpec struct {
	Kind, Cwd, Task, Thread, Name, By, Machine, Model, Effort string
	// Alias launches it through one of the computer's shell aliases: nil for
	// the default worker alias (which goes with the default kind), "" for none.
	Alias     *string
	Subscribe bool
	Resume    string
}

func (sp *Super) Spawn(spec SpawnSpec) (*Worker, <-chan error, error) {
	kind, cwd, task, thread, name, by, machine, model, effort := spec.Kind, spec.Cwd, spec.Task, spec.Thread, spec.Name, spec.By, spec.Machine, spec.Model, spec.Effort
	subscribe, resume := spec.Subscribe, spec.Resume
	m, err := sp.s.herdr.machine(machine)
	if err != nil {
		return nil, nil, fmt.Errorf("machine %q is not connected", machine)
	}
	sp.mu.Lock()
	// takenLocked: herdr wants each agent's name its own, so one another
	// agent has (a session of the human's, an old worker's pane) goes to a
	// spare name. Called with sp.mu held.
	takenLocked := func(n string) bool {
		w := sp.st.Workers[n]
		return (w != nil && !w.Ended) || sp.s.herdr.hasName(n) || n == routerName || n == sp.routerAgentLocked()
	}
	if kind == "" {
		kind = sp.st.Settings.WorkerKind
	}
	if _, ok := kinds[kind]; !ok {
		sp.mu.Unlock()
		return nil, nil, fmt.Errorf("unknown kind %q (claude, codex, antigravity)", kind)
	}
	// Say so now, while the router can still pick another computer or kind.
	have := m.Agents
	if m.Local {
		have = localAgents(sp.s.home, false)
	}
	if have != nil && !slices.Contains(have, kinds[kind]) {
		sp.mu.Unlock()
		return nil, nil, fmt.Errorf("%s has no %s installed (it has: %s); pick another computer or kind", m.Name, kind, strings.Join(append(have, "none")[:max(len(have), 1)], ", "))
	}
	if cwd == "" && (m.ID == sp.st.Settings.WorkerMachine || (m.Local && sp.st.Settings.WorkerMachine == "")) {
		cwd = sp.st.Settings.WorkerCwd
	}
	if thread == "" {
		thread = mainThread
	}
	if name == "" {
		base := kind
		if base == "antigravity" {
			base = "agy"
		}
		for i := 1; ; i++ {
			if n := fmt.Sprintf("%s-%d", base, i); !takenLocked(n) {
				name = n
				break
			}
		}
	}
	name = cleanWorkerName(name)
	if !nameRe.MatchString(name) || name == routerName {
		sp.mu.Unlock()
		return nil, nil, fmt.Errorf("invalid worker name %q", name)
	}
	if w := sp.st.Workers[name]; w != nil && !w.Ended {
		sp.mu.Unlock()
		return nil, nil, fmt.Errorf("worker %s is still running: `collie send %s …` to give it more, or spawn under another name such as %s", name, name, spareName(name, takenLocked))
	}
	if takenLocked(name) {
		n := spareName(name, takenLocked)
		log.Printf("spawn: %q is taken in herdr; the worker is %q", name, n)
		name = n
	}
	dir := expandIn(m.Home, cwd)
	if m.Local && !isDir(dir) {
		sp.mu.Unlock()
		return nil, nil, fmt.Errorf("directory not found: %s", dir)
	}
	w := &Worker{Name: name, Kind: kind, Model: model, Effort: effort, Machine: m.ID, Cwd: dir, Task: task, Thread: thread, CreatedBy: by, CreatedAt: time.Now(), Status: "starting", Session: resume}
	sp.st.Workers[name] = w
	if subscribe {
		th := sp.threadLocked(thread)
		if !contains(th.Subscribers, name) {
			th.Subscribers = append(th.Subscribers, name)
		}
	}
	where := ""
	if !m.Local {
		where = " @ " + m.Name
	}
	verb := "派出"
	if resume != "" {
		verb = "续上"
	}
	sp.addEntryLocked("event", thread, fmt.Sprintf("%s %s（%s%s）：%s", verb, name, kind, where, clip(firstLine(task), 120)), name, "spawn")
	sp.mu.Unlock()

	// The default alias, model and effort go with the default kind. The
	// default model only with the default alias: settings take it from that
	// alias, and another one (or none) brings (or leaves) its own.
	sp.mu.Lock()
	def, aliasName := kinds[kind] == kinds[sp.st.Settings.WorkerKind], ""
	if def {
		aliasName = sp.st.Settings.WorkerAlias
	}
	if spec.Alias != nil {
		aliasName = *spec.Alias
	}
	if def && aliasName == sp.st.Settings.WorkerAlias {
		if model == "" {
			model = sp.st.Settings.WorkerModel
		}
		if effort == "" {
			effort = sp.st.Settings.WorkerEffort
		}
	}
	runModel := model
	if aliasName != "" {
		runModel = "" // the alias's own (aliasArgs drops --model); a plain fallback keeps it
	}
	w.Run, w.Alias = &[2]string{runModel, effort}, &aliasName
	ask := sp.st.Settings.AskPermissions
	sp.mu.Unlock()
	wargs := withBypass(modelArgs(kind, model, effort), kind, ask)

	// started answers whoever is still waiting on the spawn (superSpawn waits
	// a few seconds): a failure it hears it can say at once, as the spawn's
	// own error, instead of as an event after it already said "started".
	started := make(chan error)
	tell := func(err error) bool {
		select {
		case started <- err:
			return true
		default:
			return false
		}
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		bin := sp.machineBin(m)
		// codex's resume is a subcommand: it goes first, the flags after it.
		args := append(resumeArgs(kinds[kind], resume), wargs...)
		if kinds[kind] == "claude" {
			// Reporting back is part of the job; don't make it a permission prompt.
			args = append(args, "--allowedTools", strings.Join(collieAllow(bin), ","))
		}
		alias := sp.launchAlias(ctx, m.ID, aliasName, kinds[kind], name)
		pane, ws, err := sp.s.startAgentAs(ctx, m.ID, kinds[kind], name, dir, name, alias, args)
		if pane != "" {
			sp.s.herdr.RememberRun(pane, aliasModel(alias, model), effort)
		}
		var he *HerdrError
		blocked := err != nil && errors.As(err, &he) && he.Code == "agent_not_ready"
		failed, msg := err != nil && !blocked, ""
		if failed {
			if nameTaken(err) {
				sp.mu.Lock()
				err = fmt.Errorf("%w (spawn again under another name such as %s)", err, spareName(name, takenLocked))
				sp.mu.Unlock()
			}
			// The start may have used up ctx; cleaning up gets its own time.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			// What its terminal said goes with the error; the terminal itself
			// is closed, not left behind as an empty shell.
			msg = err.Error()
			if ws != "" {
				if why := deathReason(sp.paneTail(ctx, pane)); why != "" && !strings.Contains(msg, why) {
					msg += "（终端里：" + why + "）"
				}
				sp.closeWorkspace(ctx, ws)
			}
		}
		sp.mu.Lock()
		w.PaneID, w.WorkspaceID = pane, ws
		if failed {
			// The Router hears it (as the spawn's result, or as an event) and
			// retries, or tells the human; the spawn line shows it ended.
			log.Printf("worker %s failed to start: %s", name, msg)
			w.Status, w.Ended, w.Note = "failed", true, msg
			sp.changed()
			sp.mu.Unlock()
			if !tell(err) {
				sp.Ingest("worker", thread, name, fmt.Sprintf("worker %s failed to start: %s", name, msg), "", "", name)
			}
			return
		}
		w.Status = "idle"
		if blocked {
			w.Status = "blocked"
		}
		sp.changed()
		sp.mu.Unlock()
		tell(nil)
		// The task brief is delivered like any other trigger, so it waits
		// politely if the worker is stuck on a startup dialog.
		sp.Ingest("system", thread, routerName, workerBrief(bin, w), "", name, name)
	}()
	return w, started, nil
}

// choicesText lists a dialog's choices for the router, numbered as `collie
// answer` takes them.
func choicesText(a *Approval) string {
	if a == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nChoices:")
	for i, c := range a.Choices {
		fmt.Fprintf(&b, "\n  %d. %s", i+1, c.Label)
	}
	return b.String()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func workerBrief(bin string, w *Worker) string {
	return fmt.Sprintf(`You are worker %q, managed by Collie (thread %s). Your task:

%s

How this works:
- Collie relays between you and the human, through %s report "…".
- When the task is done, report the outcome: what you found or changed, and where the details are.
- When you need what only the human can give (a decision, access), or the same failure keeps coming back after a few different approaches, report "BLOCKED: <what you need> · tried: <what you tried> · I'd pick: <your choice>". Collie decides or asks them, and tells you.
- Follow-ups for your thread arrive as messages starting with [collie].`, w.Name, w.Thread, strings.TrimSpace(w.Task), bin)
}

func (sp *Super) Report(name, text string) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	w := sp.st.Workers[name]
	if w == nil {
		return fmt.Errorf("unknown worker %s", name)
	}
	w.LastReport, w.ReportAt = text, time.Now()
	sp.ingestLocked("worker", w.Thread, name, fmt.Sprintf("report from %s:\n%s", name, text), "", "", name)
	sp.addEntryLocked("event", w.Thread, name+" 回报："+clip(text, 160), name, "done")
	return nil
}

func (sp *Super) workerByPane(pane string) string {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.workerByPaneLocked(pane)
}

// ---------------------------------------------------------------- watchdog

// watchdog is the external control loop: it restarts the router when work is
// waiting and flags workers that look stuck (screen unchanged while working).
func (sp *Super) watchdog(ctx context.Context, live map[string]liveAgent, routerAlive bool) {
	sp.mu.Lock()
	stall := time.Duration(sp.st.Settings.StallMinutes) * time.Minute
	var watch []*Worker
	for _, w := range sp.st.Workers {
		if !w.Ended && w.Status == "working" {
			watch = append(watch, w)
		}
	}
	sp.mu.Unlock()
	for _, w := range watch {
		a, ok := live[w.Name]
		if !ok {
			continue
		}
		res, err := sp.s.herdr.Read(ctx, a.Pane, "detection", "text", 0)
		if err != nil {
			continue
		}
		h := progressHash(res.Text)
		m := sp.lastScreen[w.Name]
		if m.hash != h {
			sp.lastScreen[w.Name] = screenMark{hash: h, since: time.Now()}
			continue
		}
		if !m.flag && stall > 0 && time.Since(m.since) > stall {
			m.flag = true
			sp.lastScreen[w.Name] = m
			sp.Ingest("system", w.Thread, "watchdog",
				fmt.Sprintf("watchdog: worker %s has shown no visible progress for %d min while 'working'. Inspect with `collie show %s`; send guidance, stop it, or tell the human.", w.Name, int(stall.Minutes()), w.Name), "", "", w.Name)
		}
	}
	sp.watchRouter(ctx, live, stall)
}

// watchRouter catches Collie itself hanging mid-turn (a stream that never
// ends, a tool that never returns): nothing but the timer moves on its screen.
// The first time it interrupts and tells Collie to carry on; if that turn
// hangs again, it tells the human instead of looping.
func (sp *Super) watchRouter(ctx context.Context, live map[string]liveAgent, stall time.Duration) {
	a, ok := live[routerName]
	if !ok || stall <= 0 || a.Status != "working" {
		sp.mu.Lock()
		delete(sp.lastScreen, routerName)
		if ok && (a.Status == "done" || a.Status == "idle") {
			sp.routerNudged = false
		}
		sp.mu.Unlock()
		return
	}
	res, err := sp.s.herdr.Read(ctx, a.Pane, "detection", "text", 0)
	if err != nil {
		return
	}
	h := progressHash(res.Text)
	sp.mu.Lock()
	m := sp.lastScreen[routerName]
	if m.hash != h {
		sp.lastScreen[routerName] = screenMark{hash: h, since: time.Now()}
		sp.mu.Unlock()
		return
	}
	if m.flag || time.Since(m.since) < stall {
		sp.mu.Unlock()
		return
	}
	m.flag = true
	sp.lastScreen[routerName] = m
	mins := int(time.Since(m.since).Minutes())
	if sp.routerNudged {
		sp.addEntryLocked("event", mainThread, fmt.Sprintf("Collie 又卡住了（%d 分钟没有动静，自动打断过一次没好）；可以在终端里看看它，或在 自动化 → 设置 里给 Collie 换一个 agent", mins), routerName, "error")
		sp.mu.Unlock()
		return
	}
	sp.routerNudged = true
	sp.addEntryLocked("event", mainThread, fmt.Sprintf("Collie 卡住了 %d 分钟没有动静，已自动打断让它继续", mins), "", "info")
	sp.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		sp.s.herdr.Call(ctx, "pane.send_keys", map[string]any{"pane_id": a.Pane, "keys": []string{"esc"}}, nil)
		time.Sleep(2 * time.Second)
		if err := sp.s.herdr.Prompt(ctx, sp.routerAgent(), fmt.Sprintf("%s: nothing on your screen changed for %d min, so Collie interrupted it. Carry on with what you were doing; the events you had are still yours to handle. A command that hangs is a job for a worker: hand it over.", routerRetryMark, mins)); err != nil {
			log.Printf("router nudge: %v", err)
			return
		}
		sp.mu.Lock()
		if sp.routerAskedAt.IsZero() {
			sp.routerAskedAt = time.Now()
		}
		sp.mu.Unlock()
	}()
}

// progressHash fingerprints a pane's screen for the watchdog, leaving out what
// moves on its own while an agent waits: the spinner line ("✻ Thinking… (3m
// 21s · esc to interrupt)", "Working (12s • esc to interrupt)") and numbers.
func progressHash(screen string) string {
	var b strings.Builder
	for _, l := range strings.Split(screen, "\n") {
		if strings.Contains(strings.ToLower(l), "esc to interrupt") || strings.Contains(strings.ToLower(l), "esc to cancel") {
			continue
		}
		b.WriteString(strings.Map(func(r rune) rune {
			if unicode.IsDigit(r) {
				return -1
			}
			return r
		}, l))
		b.WriteByte('\n')
	}
	return fmt.Sprintf("%x", sha1.Sum([]byte(b.String())))
}

// ---------------------------------------------------------------- outbox

func (sp *Super) queueNotifyLocked(e *Entry) {
	if sp.st.Settings.NotifyWebhook == "" && !sp.st.Settings.NotifyLark && !sp.push.has() {
		return
	}
	n := notice{tone: e.Tone, text: e.Text, re: e.Re, options: e.Options, check: e.Check}
	switch {
	case e.Ask != nil: // a dialog's screen stays on the page
		n.decision, n.task, n.options, n.pane = e.ID, entryTask(e), e.Ask.Choices, e.Ask.Pane
	case isDecision(e):
		n.decision, n.task, n.summary = e.ID, entryTask(e), pendingSummary(e.Text, nil)
	case !n.needsYou() && sp.st.Settings.NotifyLevel == "quiet" && e.Role == "collie":
		return
	}
	sp.notifyBuf = append(sp.notifyBuf, n)
}

// A notice is one thing to tell you: what Collie said (and what about), or
// what needs you. One that needs you is a decision: it says whose it is and
// what it needs (line), and links to it; the text stays on the page.
type notice struct {
	tone, text string
	re         *Quote
	options    []string
	check      bool
	decision   string // the entry that announced it
	task       string // the worker it is about, or Collie
	link       string // set when it goes out
	summary    string // a --opt/--check message's first line
	pane       string // a dialog's pane
}

func (n notice) needsYou() bool {
	return n.tone == "blocked" || len(n.options) > 0 || n.check
}

// quoteLine is a Quote as one line: "你：首页加一个暗色模式…".
func (q *Quote) line() string {
	t := strings.Join(strings.Fields(q.Text), " ")
	return q.Who + "：" + clip(t, 60)
}

// quoteLocked finds what a reply's --re points at: an event Collie was given
// (#t942), or an entry. Unknown ids give nil: the reply goes out anyway.
func (sp *Super) quoteLocked(id string) *Quote {
	id = strings.TrimPrefix(strings.TrimSpace(id), "#")
	if id == "" {
		return nil
	}
	entry := func(eid string) *Quote {
		for i := len(sp.st.Entries) - 1; i >= 0; i-- {
			if e := sp.st.Entries[i]; e.ID == eid && e.Role == "user" {
				return &Quote{Entry: e.ID, Who: "你", Text: humanText(e.Text)}
			}
		}
		return nil
	}
	if q := entry(id); q != nil {
		return q
	}
	t := sp.st.trigger(id)
	if t == nil {
		return nil
	}
	switch t.Source {
	case "chat":
		if q := entry(t.Ref); q != nil {
			return q
		}
		return &Quote{Who: "你", Text: humanText(t.Text)}
	case "timer":
		return &Quote{Who: "定时", Text: t.Text}
	}
	name := t.Ref
	if name == "" {
		name = t.From
	}
	if w := sp.st.Workers[name]; w != nil {
		return &Quote{Worker: w.Name, Who: w.Name, Text: w.Task}
	}
	return nil
}

// sayQuoteLocked is a chat message the human answers: one of Collie's, or
// one of their own when a run of replies last ended on what they said.
func (sp *Super) sayQuoteLocked(id string) *Quote {
	if id == "" {
		return nil
	}
	e, _ := sp.entryLocked(id)
	switch {
	case e == nil:
		return nil
	case e.Role == "collie":
		return &Quote{Entry: e.ID, Who: "Collie", Text: clip(e.Text, 600)}
	case e.Role == "user":
		return &Quote{Entry: e.ID, Who: "你", Text: clip(humanText(e.Text), 600)}
	}
	return nil
}

// pendingMaxAge bounds unanswered quick-option prompts so forgotten topics from
// past weeks never pile up as stale badges.
const pendingMaxAge = 7 * 24 * time.Hour

// PendingItem is one Collie message in chat:main (--opt or --check) that still
// awaits the human's answer, acknowledgement or dismissal.
type PendingItem struct {
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Summary string    `json:"summary"`
	Options []string  `json:"options,omitempty"`
	Check   bool      `json:"check,omitempty"`
	Task    string    `json:"task,omitempty"` // the worker it is about, or Collie
}

// PinnedItem is one message in chat:main manually pinned by the human. Unlike
// PendingItem, pins never expire on a 7-day timer and stay until unpinned.
type PinnedItem struct {
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Summary string    `json:"summary"`
	Role    string    `json:"role"`
}

func pinnableEntry(e *Entry) bool {
	return e != nil && e.ID != "" && (e.Thread == "" || e.Thread == mainThread) && (e.Role == "collie" || e.Role == "user")
}

func (sp *Super) entryForPinLocked(id string) *Entry {
	for i := len(sp.st.Entries) - 1; i >= 0; i-- {
		if e := sp.st.Entries[i]; e.ID == id {
			if pinnableEntry(e) {
				return e
			}
			return nil
		}
	}
	if e, ok := sp.pinnedCache[id]; ok {
		return e
	}
	if sp.db != nil {
		var raw string
		var e Entry
		if sp.db.QueryRow(`SELECT data FROM entries WHERE id = ?`, id).Scan(&raw) == nil &&
			json.Unmarshal([]byte(raw), &e) == nil && pinnableEntry(&e) {
			if sp.pinnedCache == nil {
				sp.pinnedCache = map[string]*Entry{}
			}
			sp.pinnedCache[id] = &e
			return &e
		}
	}
	return nil
}

// Pin idempotently pins or unpins one chat:main message and persists it.
func (sp *Super) Pin(id string, pinned bool) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("id is required")
	}
	sp.mu.Lock()
	idx := slices.Index(sp.st.Pinned, id)
	changed := false
	if pinned {
		if idx < 0 {
			if sp.entryForPinLocked(id) == nil {
				sp.mu.Unlock()
				return errors.New("找不到这条可钉住的消息")
			}
			sp.st.Pinned = append(sp.st.Pinned, id)
			if n := len(sp.st.Pinned); n > maxPinned {
				for _, drop := range sp.st.Pinned[:n-maxPinned] {
					delete(sp.pinnedCache, drop)
				}
				sp.st.Pinned = append([]string(nil), sp.st.Pinned[n-maxPinned:]...)
			}
			changed = true
		}
	} else if idx >= 0 {
		sp.st.Pinned = slices.Delete(sp.st.Pinned, idx, idx+1)
		delete(sp.pinnedCache, id)
		changed = true
	}
	if changed {
		sp.changed()
	}
	hasDB := sp.db != nil
	sp.mu.Unlock()
	if changed && hasDB {
		sp.flush()
	}
	return nil
}

func (sp *Super) pinnedLocked() []PinnedItem {
	if len(sp.st.Pinned) == 0 {
		return []PinnedItem{}
	}
	out := make([]PinnedItem, 0, len(sp.st.Pinned))
	for _, id := range sp.st.Pinned {
		e := sp.entryForPinLocked(id)
		if e == nil {
			continue
		}
		sum := pendingSummary(e.Text, e.Options)
		if e.Role == "user" {
			sum = pendingSummary(humanText(e.Text), nil)
		}
		out = append(out, PinnedItem{
			ID:      e.ID,
			At:      e.At,
			Summary: sum,
			Role:    e.Role,
		})
	}
	return out
}

// Dismiss marks one pending message as acknowledged/ignored by the human and persists it.
func (sp *Super) Dismiss(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	sp.mu.Lock()
	changed := sp.dismissLocked(id)
	hasDB := sp.db != nil
	sp.mu.Unlock()
	if changed && hasDB {
		sp.flush()
	}
}

func (sp *Super) dismissLocked(id string) bool {
	if sp.st.Dismissed == nil {
		sp.st.Dismissed = map[string]bool{}
	}
	if sp.st.Dismissed[id] {
		return false
	}
	sp.st.Dismissed[id] = true
	sp.changed()
	return true
}

// pendingLocked computes unanswered --opt and --check Collie messages in chat:main.
// An item resolves when:
//  1. the human tapped「已看」or × (/api/super/dismiss),
//  2. the human tapped one of its options or quoted it (re.entry == e.id),
//  3. the human sent a plain unquoted message right after it with no other
//     role=="collie" reply in between (system/worker events do not interrupt).
func (sp *Super) pendingLocked(now time.Time) []PendingItem {
	es := sp.st.Entries
	var out []PendingItem
	for i, e := range es {
		if e.Role != "collie" || (e.Thread != "" && e.Thread != mainThread) || (len(e.Options) == 0 && !e.Check) || e.ID == "" {
			continue
		}
		if sp.st.Dismissed[e.ID] {
			continue
		}
		if !now.IsZero() && !e.At.IsZero() && now.Sub(e.At) > pendingMaxAge {
			continue
		}
		opts := cleanOptions(e.Options)
		if len(opts) == 0 && !e.Check {
			continue
		}
		if sp.replyLocked(i) == nil {
			out = append(out, PendingItem{
				ID:      e.ID,
				At:      e.At,
				Summary: pendingSummary(e.Text, opts),
				Options: opts,
				Check:   e.Check,
				Task:    entryTask(e),
			})
		}
	}
	return out
}

// replyLocked is your message that settles the --opt/--check message at
// Entries[i], by rules 2 and 3 above; nil while there is none.
func (sp *Super) replyLocked(i int) *Entry {
	e, otherCollie := sp.st.Entries[i], false
	for _, next := range sp.st.Entries[i+1:] {
		if next.Thread != "" && next.Thread != mainThread {
			continue
		}
		switch next.Role {
		case "user":
			reID := ""
			if next.Re != nil {
				reID = next.Re.Entry
			}
			if reID == e.ID || (reID == "" && !otherCollie) {
				return next
			}
		case "collie":
			otherCollie = true
		}
	}
	return nil
}

var (
	pendingFenceRe = regexp.MustCompile("(?s)```.*?```")
	pendingImgRe   = regexp.MustCompile(`\[image:[^\]]+\]`)
	pendingLinkRe  = regexp.MustCompile(`\[([^\]\n]+)\]\([^)\n]+\)`)
	pendingListRe  = regexp.MustCompile(`^\s*(?:[#>*•\-]+|\d+[.)])\s+`)
)

func pendingSummary(text string, opts []string) string {
	s := pendingFenceRe.ReplaceAllString(text, " ")
	s = pendingImgRe.ReplaceAllString(s, " ")
	s = pendingLinkRe.ReplaceAllString(s, "$1")
	var parts []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(pendingListRe.ReplaceAllString(l, ""))
		l = strings.TrimSpace(mdMarks.Replace(l))
		if l != "" {
			parts = append(parts, l)
		}
	}
	out := oneLine(strings.Join(parts, " "))
	if out == "" {
		out = strings.Join(opts, " / ")
	}
	if out == "" {
		out = oneLine(text)
	}
	return clip(out, 120)
}

// humanText is your message as you wrote it: without the attachment list and
// the notes the hub adds for Collie.
func humanText(t string) string {
	if i := strings.Index(t, "\n\n[The human picked"); i >= 0 {
		t = t[:i]
	}
	if i := strings.LastIndex(t, "附件：\n"); i >= 0 {
		t = strings.TrimSpace(t[:i]) + " 📎"
	}
	return strings.TrimSpace(t)
}

const (
	defaultSearchLimit = 30
	maxSearchLimit     = 50
	searchSnippetRunes = 140
	searchLeadRunes    = 36
)

var (
	searchCodeFenceRe   = regexp.MustCompile("(?s)```[^\\n`]*\\n?(.*?)(?:```|$)")
	searchLinkKeepURLRe = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\n]+)\)`)
	searchQuoteLeadRe   = regexp.MustCompile(`^\s*[#>*•]+\s+`)
	searchBulletRe      = regexp.MustCompile(`^\s*-\s+`)
)

func searchableEntry(e *Entry, thread string) bool {
	if e == nil || e.ID == "" || (e.Role != "user" && e.Role != "collie") {
		return false
	}
	if thread == mainThread {
		return e.Thread == "" || e.Thread == mainThread
	}
	return e.Thread == thread
}

func toLowerRunes(s string) []rune {
	rs := []rune(s)
	out := make([]rune, len(rs))
	for i, r := range rs {
		out[i] = unicode.ToLower(r)
	}
	return out
}

func runesIndexFold(hayLow, needleLow []rune) int {
	n, m := len(hayLow), len(needleLow)
	if m == 0 {
		return 0
	}
	if m > n {
		return -1
	}
	first := needleLow[0]
	limit := n - m
	for i := 0; i <= limit; i++ {
		if hayLow[i] != first {
			continue
		}
		ok := true
		for j := 1; j < m; j++ {
			if hayLow[i+j] != needleLow[j] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// cleanSearchText turns raw Markdown entry text into clean single-line plain text
// while preserving code block bodies, numbers, paths, and (when includeURLs is set)
// link target URLs and quick-reply options.
func cleanSearchText(e *Entry, includeURLs bool) string {
	t := e.Text
	if e.Role == "user" {
		if i := strings.Index(t, "\n\n[The human picked"); i >= 0 {
			t = t[:i]
		}
		if i := strings.LastIndex(t, "附件：\n"); i >= 0 {
			head := strings.TrimSpace(t[:i])
			att := strings.Join(strings.Fields(t[i+len("附件：\n"):]), " ")
			if att != "" {
				t = strings.TrimSpace(head + " 📎 " + att)
			} else {
				t = head
			}
		}
	}
	t = searchCodeFenceRe.ReplaceAllString(t, " $1 ")
	if !includeURLs {
		t = pendingImgRe.ReplaceAllString(t, " ")
		t = pendingLinkRe.ReplaceAllString(t, "$1")
	} else {
		t = searchLinkKeepURLRe.ReplaceAllString(t, "$1 ($2)")
	}
	var parts []string
	for _, l := range strings.Split(t, "\n") {
		l = searchQuoteLeadRe.ReplaceAllString(l, "")
		l = searchBulletRe.ReplaceAllString(l, "")
		l = strings.TrimSpace(mdMarks.Replace(l))
		if l != "" {
			parts = append(parts, l)
		}
	}
	if includeURLs && len(e.Options) > 0 {
		parts = append(parts, strings.Join(e.Options, " / "))
	}
	out := oneLine(strings.Join(parts, " "))
	if out == "" {
		out = oneLine(e.Text)
	}
	return out
}

func matchTermsSnippet(text string, terms [][]rune) (string, bool) {
	if text == "" {
		return "", false
	}
	orig := []rune(text)
	low := make([]rune, len(orig))
	for i, r := range orig {
		low[i] = unicode.ToLower(r)
	}
	firstIdx := -1
	for ti, tr := range terms {
		idx := runesIndexFold(low, tr)
		if idx < 0 {
			return "", false
		}
		if ti == 0 {
			firstIdx = idx
		}
	}
	if len(orig) <= searchSnippetRunes {
		return text, true
	}
	start := max(0, firstIdx-searchLeadRunes)
	end := start + searchSnippetRunes
	if end > len(orig) {
		end = len(orig)
		start = max(0, end-searchSnippetRunes)
	}
	if termEnd := firstIdx + len(terms[0]); termEnd > end && termEnd <= len(orig) {
		if len(terms[0]) >= searchSnippetRunes {
			start = firstIdx
			end = min(len(orig), firstIdx+searchSnippetRunes)
		} else {
			end = termEnd
			start = max(0, end-searchSnippetRunes)
		}
	}
	out := string(orig[start:end])
	if start > 0 {
		out = "…" + strings.TrimLeft(out, " ")
	}
	if end < len(orig) {
		out = strings.TrimRight(out, " ") + "…"
	}
	return out, true
}

// Search queries full conversation history across in-memory and SQLite entries
// for case-insensitive multi-term AND matches on user/collie messages.
func (sp *Super) Search(thread, query string, limit int) SearchResult {
	if thread = strings.TrimSpace(thread); thread == "" {
		thread = mainThread
	}
	if limit <= 0 {
		limit = defaultSearchLimit
	} else if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return SearchResult{Query: "", Results: []SearchHit{}}
	}
	fields := strings.Fields(q)
	terms := make([][]rune, 0, len(fields))
	seenTerms := map[string]bool{}
	for _, f := range fields {
		lr := toLowerRunes(f)
		key := string(lr)
		if key != "" && !seenTerms[key] {
			seenTerms[key] = true
			terms = append(terms, lr)
		}
	}
	if len(terms) == 0 {
		return SearchResult{Query: "", Results: []SearchHit{}}
	}

	if sp.db != nil {
		sp.flush()
	}
	sp.mu.Lock()
	mem := make([]*Entry, len(sp.st.Entries))
	for i, e := range sp.st.Entries {
		if e != nil {
			cp := *e
			mem[i] = &cp
		}
	}
	sp.mu.Unlock()

	seenID := make(map[string]bool, len(mem))
	var candidates []*Entry
	for i := len(mem) - 1; i >= 0; i-- {
		e := mem[i]
		if e == nil || e.ID == "" || seenID[e.ID] {
			continue
		}
		seenID[e.ID] = true
		if searchableEntry(e, thread) {
			candidates = append(candidates, e)
		}
	}
	if sp.db != nil {
		for _, e := range threadEntries(sp.db, thread) {
			if e == nil || e.ID == "" || seenID[e.ID] {
				continue
			}
			seenID[e.ID] = true
			if searchableEntry(e, thread) {
				candidates = append(candidates, e)
			}
		}
	}

	type hitPair struct {
		e       *Entry
		snippet string
	}
	var hits []hitPair
	for _, e := range candidates {
		clean := cleanSearchText(e, false)
		snip, ok := matchTermsSnippet(clean, terms)
		if !ok {
			full := cleanSearchText(e, true)
			if full != clean {
				snip, ok = matchTermsSnippet(full, terms)
			}
		}
		if ok {
			hits = append(hits, hitPair{e: e, snippet: snip})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		ai, aj := hits[i].e.At, hits[j].e.At
		if !ai.Equal(aj) {
			return ai.After(aj)
		}
		return idNum(hits[i].e.ID) > idNum(hits[j].e.ID)
	})
	total := len(hits)
	hasMore := total > limit
	if hasMore {
		hits = hits[:limit]
	}
	res := make([]SearchHit, len(hits))
	for i, h := range hits {
		res[i] = SearchHit{
			ID:      h.e.ID,
			Role:    h.e.Role,
			At:      h.e.At,
			Snippet: h.snippet,
		}
	}
	return SearchResult{
		Query:   q,
		Results: res,
		Total:   total,
		HasMore: hasMore,
	}
}

func (n notice) label() string {
	switch {
	case n.needsYou():
		return "Collie · 需要你"
	case n.tone == "error":
		return "Collie · 出错了"
	}
	return "Collie"
}

// noticesText is the plain form (webhooks, and the card's fallback).
func noticesText(ns []notice) string {
	var parts []string
	for _, n := range ns {
		if n.decision != "" {
			p := n.label() + "：" + n.line()
			if n.summary != "" {
				p += "\n" + n.summary
			}
			if n.link != "" {
				p += "\n去处理：" + n.link
			}
			parts = append(parts, p)
			continue
		}
		p := n.label() + "：" + clip(n.text, 400)
		if n.re != nil {
			p = "↩ " + n.re.line() + "\n" + p
		}
		if len(n.options) > 0 {
			p += "\n可选：" + strings.Join(n.options, " / ")
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "\n\n")
}

// Seen notes that a page of Collie is open in front of you (the page says so
// every 30 seconds while it is visible).
func (sp *Super) Seen(url string) { sp.seenPage("", url, false) }

// seenPage: page is in front of you, or (away) has just gone to the
// background. You stop looking when the last page in front of you leaves,
// not a minute later: a phone put away gets its notifications at once.
func (sp *Super) seenPage(page, url string, away bool) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.seenPages == nil {
		sp.seenPages = map[string]time.Time{}
	}
	for p, at := range sp.seenPages {
		if time.Since(at) > lookingFor {
			delete(sp.seenPages, p)
		}
	}
	if url != "" {
		sp.seenURL = url
	}
	if !away {
		sp.seenAt = time.Now()
		sp.seenPages[page] = sp.seenAt
		return
	}
	if _, ok := sp.seenPages[page]; !ok {
		return
	}
	delete(sp.seenPages, page)
	sp.seenAt = time.Time{}
	for _, at := range sp.seenPages {
		if at.After(sp.seenAt) {
			sp.seenAt = at
		}
	}
}

// lookingFor: a page in front of you says so every 30 seconds.
const lookingFor = 75 * time.Second

// notifyEvery paces notifications: what comes up in between goes out together.
const notifyEvery = 20 * time.Second

// sendNotifications paces outgoing notifications: messages are coalesced and
// sent at most every 20 seconds, and not at all while you are looking at
// Collie (you see them there).
func (sp *Super) sendNotifications() {
	sp.mu.Lock()
	if len(sp.notifyBuf) == 0 && !sp.push.dialogsDue() || time.Since(sp.notifyAt) < notifyEvery {
		sp.mu.Unlock()
		return
	}
	url, toLark := sp.st.Settings.NotifyWebhook, sp.st.Settings.NotifyLark
	looking := time.Since(sp.seenAt) < lookingFor
	base := sp.linkBaseLocked()
	subject := sp.pushSubjectLocked()
	// A decision settled while it waited here (Collie answered, you did on
	// the page) is no longer news.
	var notices []notice
	var pushes []pushNote
	for _, n := range sp.notifyBuf {
		if n.decision != "" {
			if !sp.decisionOpenLocked(n.decision) {
				continue
			}
			n.link = decisionLink(base, n.decision)
		}
		if p := noticeNote(n); p != nil {
			pushes = append(pushes, *p)
		}
		notices = append(notices, n)
	}
	sp.notifyBuf, sp.notifyAt = nil, time.Now()
	sp.mu.Unlock()
	dialogs := sp.push.takeDialogs()
	if looking {
		return
	}
	if sp.push.has() && (len(pushes) > 0 || len(dialogs) > 0) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			sp.push.broadcast(ctx, subject, append(pushes, sp.dialogNotes(ctx, dialogs)...))
		}()
	}
	if len(notices) == 0 || (url == "" && !toLark) {
		return
	}
	link := ""
	if base != "" {
		link = strings.TrimRight(base, "/") + "/#/collie"
	}
	if toLark {
		if err := larkSend(notices, link); err != nil {
			log.Printf("notify lark: %v", err)
		}
	}
	if url == "" {
		return
	}
	text := noticesText(notices)
	payload := map[string]any{
		"msg_type": "text", "content": map[string]string{"text": text}, // Feishu/Lark bot
		"text": text, // Slack-style
	}
	if d := firstDecision(notices); d != nil {
		payload["link"] = d.link
	}
	body, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err != nil {
		log.Printf("notify: %v", err)
	} else {
		resp.Body.Close()
	}
}

// ---------------------------------------------------------------- recall

// Show renders any object by id with links to related ids (chain of recall).
func (sp *Super) Show(id string) (string, bool) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	var b strings.Builder
	if t := sp.st.trigger(id); t != nil {
		fmt.Fprintf(&b, "trigger %s\nsource: %s  thread: %s  from: %s  at: %s  status: %s  action: %s\n", t.ID, t.Source, t.Thread, t.From, t.At.Format(time.RFC3339), t.Status, t.Action)
		if t.Ref != "" {
			fmt.Fprintf(&b, "ref: %s\n", t.Ref)
		}
		fmt.Fprintf(&b, "\n%s\n", t.Text)
		return b.String(), true
	}
	if th := sp.st.Threads[id]; th != nil {
		fmt.Fprintf(&b, "thread %s\nsubscribers: %s\nrecent triggers:\n", th.ID, strings.Join(th.Subscribers, ", "))
		ids := th.Triggers
		if len(ids) > 15 {
			ids = ids[len(ids)-15:]
		}
		for _, tid := range ids {
			if t := sp.st.trigger(tid); t != nil {
				fmt.Fprintf(&b, "  #%s %s %s: %s\n", t.ID, t.At.Format("01-02 15:04"), t.Source, clip(firstLine(t.Text), 90))
			}
		}
		return b.String(), true
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

// trustOwnDir answers the "do you trust this folder?" dialog for the router's
// own working directory. Collie created that directory and wrote every file in
// it, so asking the human adds friction without adding safety. Any other
// dialog (and any other directory) is left for the human.
func (sp *Super) trustOwnDir(ctx context.Context, pane string) {
	if dir := sp.routerCwd(sp.routerOn()); sp.answerTrust(ctx, pane, dir) {
		log.Printf("router: trusted its own directory %s", dir)
	}
}

func isTrustChoice(label string) bool {
	l := strings.ToLower(strings.TrimSpace(label))
	return (strings.HasPrefix(l, "yes") && strings.Contains(l, "trust")) || strings.HasPrefix(l, "trust and continue")
}

func (sp *Super) noteAnsweredLocked(w *Worker) {
	if sp.lastAnswer == nil {
		sp.lastAnswer = map[string]time.Time{}
	}
	if w != nil {
		if w.PaneID != "" {
			sp.lastAnswer[w.PaneID] = time.Now()
		}
		if w.Status == "blocked" {
			w.Status = "idle"
		}
	}
}

// answerTrust picks "Yes, I trust this folder" / "Trust and continue" when
// pane shows a folder-trust dialog for dir, the folder Collie started that
// agent in. It reports whether it answered. Nothing else is ever answered this way.
func (sp *Super) answerTrust(ctx context.Context, pane, dir string) bool {
	sp.mu.Lock()
	if sp.lastTrust == nil {
		sp.lastTrust = map[string]time.Time{}
	}
	if time.Since(sp.lastTrust[pane]) < 5*time.Second {
		sp.mu.Unlock()
		return false
	}
	sp.lastTrust[pane] = time.Now()
	sp.mu.Unlock()

	rr, err := sp.s.herdr.Read(ctx, pane, "detection", "text", 0)
	if err != nil || !trustDialogFor(rr.Text, dir, sp.s.tilde(dir)) {
		return false
	}
	for _, c := range parseApproval(rr.Text).Choices {
		if isTrustChoice(c.Label) {
			return sp.choose(ctx, pane, c) == nil
		}
	}
	return false
}

// choose answers a dialog in pane with one of its choices: cursor movement and
// Enter when the menu has a cursor, else its digit for a cursorless numbered menu.
func (sp *Super) choose(ctx context.Context, pane string, c Choice) error {
	if len(c.Keys) > 0 {
		return sp.s.herdr.Call(ctx, "pane.send_keys", map[string]any{"pane_id": pane, "keys": c.Keys}, nil)
	}
	if c.Text != "" {
		return sp.s.herdr.Call(ctx, "pane.send_text", map[string]any{"pane_id": pane, "text": c.Text}, nil)
	}
	return sp.s.herdr.Call(ctx, "pane.send_keys", map[string]any{"pane_id": pane, "keys": []string{"enter"}}, nil)
}

// pickChoice finds the choice the router named: its number as shown ("3"),
// its position, or words of its label.
func pickChoice(a Approval, want string) (Choice, bool) {
	want = strings.TrimSpace(want)
	for _, c := range a.Choices {
		if c.Text != "" && c.Text == want {
			return c, true
		}
	}
	if n, err := strconv.Atoi(want); err == nil && n >= 1 && n <= len(a.Choices) {
		return a.Choices[n-1], true
	}
	var hit []Choice
	for _, c := range a.Choices {
		if want != "" && strings.Contains(strings.ToLower(c.Label), strings.ToLower(want)) {
			hit = append(hit, c)
		}
	}
	if len(hit) == 1 {
		return hit[0], true
	}
	return Choice{}, false
}

// trustDialogFor: the screen asks whether to trust a folder, and names dir
// (as given, with ~, or by its last part: the screen may show the path a
// symlink resolves to).
func trustDialogFor(screen, dir, tilde string) bool {
	if !strings.Contains(strings.ToLower(screen), "trust") || dir == "" {
		return false
	}
	if strings.Contains(screen, dir) || strings.Contains(screen, tilde) {
		return true
	}
	return regexp.MustCompile(`(?m)/` + regexp.QuoteMeta(path.Base(dir)) + `/?\s*$`).MatchString(screen)
}

// closeStaleRouterWorkspaces removes router workspaces whose agent has exited,
// so a crashed router does not leave an orphan shell behind.
func (sp *Super) closeStaleRouterWorkspaces(ctx context.Context) {
	wss, err := sp.s.herdr.Workspaces(ctx)
	if err != nil {
		return
	}
	for _, ws := range wss {
		if ws.Label == routerLabel { // wherever it ran: there is one Collie at a time
			sp.s.herdr.Call(ctx, "workspace.close", map[string]any{"workspace_id": ws.ID}, nil)
		}
	}
}

// agentErrRe matches what an agent CLI prints when it can't run (the API
// refused, the login expired, the quota ran out), not Collie's own words about
// a task: "the backup failed twice" and "its error output" are work, not errors.
var agentErrRe = regexp.MustCompile(`(?i)(api error|please run /login|invalid api key|quota exceeded|daily limit|rate.?limit(ed)?\b|overloaded|unauthori[sz]ed|credit balance|unexpected status \d{3}|^[\W]*(⚠|■|error\b|error:))`)

// routerSettled runs once after the router finishes a turn that we prompted.
// If it went quiet without a `collie reply` and its screen shows an error
// (bad credentials, region block, quota…), surface that in the chat instead of
// leaving the human staring at silence.
func (sp *Super) routerSettled(ctx context.Context, pane string) {
	sp.mu.Lock()
	asked := sp.routerAskedAt
	if asked.IsZero() || time.Since(asked) < 4*time.Second {
		sp.mu.Unlock()
		return
	}
	sp.routerAskedAt = time.Time{}
	for i := len(sp.st.Entries) - 1; i >= 0; i-- {
		e := sp.st.Entries[i]
		if e.At.Before(asked) {
			break
		}
		if e.Role == "collie" {
			sp.routerRetries = 0
			sp.mu.Unlock()
			return
		}
	}
	sp.mu.Unlock()

	rr, err := sp.s.herdr.Read(ctx, pane, "recent", "text", 60)
	if err != nil {
		return
	}
	lines := strings.Split(rr.Text, "\n")
	// Only look at what the agent printed after our prompt, never at the event text itself.
	start := 0
	for i, l := range lines {
		if strings.Contains(l, "collie reply") || strings.Contains(l, "yourself.") || strings.Contains(l, routerRetryMark) {
			start = i + 1
		}
	}
	for i := start; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		if l == "" || !agentErrRe.MatchString(l) {
			continue
		}
		// Messages wrap in the terminal: keep following lines of the same block.
		msg := l
		for j := i + 1; j < len(lines) && j <= i+3; j++ {
			next := strings.TrimSpace(lines[j])
			if next == "" || strings.HasPrefix(next, "─") || strings.HasPrefix(next, "✻") || strings.HasPrefix(next, "Error ID") {
				break
			}
			msg += " " + next
		}
		if r := []rune(msg); len(r) > 240 {
			msg = string(r[:240]) + "…"
		}
		sp.mu.Lock()
		// Most of these pass (a region check, a busy API, a dropped
		// connection): try again before calling it an error.
		if n := sp.routerRetries; n < len(routerRetryWait) {
			sp.routerRetries++
			wait := routerRetryWait[n]
			sp.addEntryLocked("event", mainThread, fmt.Sprintf("Collie 遇到 API 错误（%s），%s后自动重试", clip(msg, 80), waitText(wait)), "", "info")
			sp.mu.Unlock()
			go sp.retryRouter(wait, msg)
			return
		}
		sp.routerRetries = 0
		text := "Collie 出错了：" + msg + "（自动重试了 " + fmt.Sprint(len(routerRetryWait)) + " 次都没好；可以在终端里让它继续，或在 自动化 → 设置 里给 Collie 换一个 agent）"
		for i := len(sp.st.Entries) - 1; i >= 0 && i >= len(sp.st.Entries)-6; i-- {
			if e := sp.st.Entries[i]; e.Text == text && time.Since(e.At) < 10*time.Minute {
				sp.mu.Unlock()
				return // same failure again; one notice is enough
			}
		}
		sp.addEntryLocked("event", mainThread, text, routerName, "error")
		sp.mu.Unlock()
		return
	}
	sp.mu.Lock()
	sp.routerRetries = 0 // a quiet turn, no error: it is working again
	sp.mu.Unlock()
}

// routerRetryWait: how long to wait before each retry of a turn that ended
// on an API error.
var routerRetryWait = []time.Duration{20 * time.Second, time.Minute, 3 * time.Minute}

// routerRetryMark starts the retry prompt; the error scan reads from there.
const routerRetryMark = "[collie] Your last turn stopped"

func waitText(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	return fmt.Sprintf("%d 分钟", int(d.Minutes()))
}

// retryRouter nudges Collie to carry on after an API error, unless something
// else has got it going meanwhile (your next message, a worker's event).
func (sp *Super) retryRouter(wait time.Duration, msg string) {
	time.Sleep(wait)
	sp.mu.Lock()
	busy := !sp.routerAskedAt.IsZero() || sp.routerLaunching
	sp.mu.Unlock()
	if busy {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sp.s.herdr.Prompt(ctx, sp.routerAgent(), routerRetryMark+" on an error from your model's API ("+msg+"). It may have passed: carry on with what you were doing; the events you had are still yours to handle."); err != nil {
		log.Printf("router retry: %v", err)
		return
	}
	sp.mu.Lock()
	if sp.routerAskedAt.IsZero() {
		sp.routerAskedAt = time.Now() // so this turn is checked like any other
	}
	sp.mu.Unlock()
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

// routerArgsLocked: the extra launch flags plus the chosen model and effort.
func (sp *Super) routerArgsLocked() []string {
	st := sp.st.Settings
	args := append(append([]string{}, st.RouterArgs...), modelArgs(st.RouterKind, st.RouterModel, st.RouterEffort)...)
	return withBypass(args, st.RouterKind, st.AskPermissions)
}

// The model and effort the router was launched with, kept across hub restarts.
func (sp *Super) setRouterRun(run [2]string) {
	sp.routerRun = &run
	b, _ := json.Marshal(run)
	sp.db.Exec(`INSERT INTO kv(k,v) VALUES('router_run',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, string(b))
}

// routerLaunch: where and through which alias the router was last started,
// kept across hub restarts. Older hubs stored the bare machine id.
type routerLaunch struct {
	Machine string `json:"machine"`
	Alias   string `json:"alias,omitempty"`
	Name    string `json:"name,omitempty"` // its herdr name when not routerName
}

// setRouterOn keeps the Router's herdr name: the running one is still known
// by it until a new launch has named its own.
func (sp *Super) setRouterOn(machine, alias string) {
	sp.saveRouterLaunchLocked(routerLaunch{Machine: machine, Alias: alias, Name: sp.routerLaunchLocked().Name})
}

func (sp *Super) saveRouterLaunchLocked(l routerLaunch) {
	sp.routerLaunched = &l
	if sp.db == nil {
		return
	}
	b, err := json.Marshal(sp.routerLaunched)
	if err == nil {
		_, err = sp.db.Exec(`INSERT INTO kv(k,v) VALUES('router_on',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, string(b))
	}
	if err != nil {
		log.Printf("router_on: %v", err)
	}
}

func (sp *Super) routerLaunchLocked() routerLaunch {
	if sp.routerLaunched == nil {
		var l routerLaunch
		var raw string
		if sp.db != nil {
			if err := sp.db.QueryRow(`SELECT v FROM kv WHERE k='router_on'`).Scan(&raw); err != nil && !errors.Is(err, sql.ErrNoRows) {
				log.Printf("router_on: %v", err)
			}
		}
		if json.Unmarshal([]byte(raw), &l) != nil {
			l.Machine = raw
		}
		sp.routerLaunched = &l
	}
	return *sp.routerLaunched
}

func (sp *Super) routerOnLocked() string { return sp.routerLaunchLocked().Machine }

// noteAliasFallback: the alias asked for could not be used and the agent
// starts plainly; the record says so, so Collie shows as waiting for a restart.
func (sp *Super) noteAliasFallback(machine, asked string, used *LauncherAlias) {
	if asked == "" || used != nil {
		return
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.setRouterOn(machine, "")
}

func (sp *Super) routerRunLocked() *[2]string {
	if sp.routerRun == nil {
		var raw string
		var run [2]string
		if sp.db.QueryRow(`SELECT v FROM kv WHERE k='router_run'`).Scan(&raw) == nil && json.Unmarshal([]byte(raw), &run) == nil {
			sp.routerRun = &run
		}
	}
	return sp.routerRun
}
