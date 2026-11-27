package collie

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Durable state for the super-agent: the trigger inbox, threads, workers,
// the human-facing conversation, timers, source policies and settings.
// It lives in one JSON file so the router/worker sessions (compute) can be
// restarted at any time without losing what the system knows (state).

type Trigger struct {
	ID      string    `json:"id"`
	Source  string    `json:"source"` // chat | timer | worker | webhook:<name> | system
	Thread  string    `json:"thread"`
	From    string    `json:"from,omitempty"`
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
	Action  string    `json:"action"` // respond | candidate | ignore (fast-path decision)
	Status  string    `json:"status"` // queued | delivered | ignored | failed
	Targets []string  `json:"targets,omitempty"`
	// Deliveries records each target whose prompt may already be in its terminal.
	Deliveries  map[string]deliveryAttempt `json:"deliveries,omitempty"`
	LarkRequest *larkRequest               `json:"lark_request,omitempty"`
	// WaitForConvWorker keeps a dedicated extension request queued until its worker is live.
	WaitForConvWorker bool   `json:"wait_for_conv_worker,omitempty"`
	Key               string `json:"key,omitempty"` // idempotency key
	Ref               string `json:"ref,omitempty"` // related object id (worker, timer…)
}

type deliveryAttempt struct {
	Pane string    `json:"pane"`
	At   time.Time `json:"at"`
}

type Thread struct {
	ID          string    `json:"id"`
	Title       string    `json:"title,omitempty"`
	Subscribers []string  `json:"subscribers"`
	LastAt      time.Time `json:"last_at"`
	Triggers    []string  `json:"triggers"`
}

type Worker struct {
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	Machine     string    `json:"machine,omitempty"` // "" = the hub's own machine
	Cwd         string    `json:"cwd"`
	Task        string    `json:"task"`
	Thread      string    `json:"thread"`
	PaneID      string    `json:"pane_id,omitempty"`
	WorkspaceID string    `json:"workspace_id,omitempty"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	Model       string    `json:"model,omitempty"`  // asked for when spawned (empty: the default)
	Effort      string    `json:"effort,omitempty"` // likewise
	// Alias is the shell alias it was launched through ("" none; nil for
	// workers from before). A resume goes through it again: an alias may
	// point the agent at another config directory, where its conversation is.
	Alias *string `json:"alias,omitempty"`
	// Run is the model and effort it was launched with, settings' defaults
	// applied; "" is the agent's own default. Nil for workers from before.
	Run        *[2]string `json:"run,omitempty"`
	Status     string     `json:"status"` // starting | working | idle | done | blocked | unknown | gone | failed
	Note       string     `json:"note,omitempty"`
	LastReport string     `json:"last_report,omitempty"`
	ReportAt   time.Time  `json:"report_at,omitempty"`
	Ended      bool       `json:"ended,omitempty"`
	// Session is its agent's conversation id, kept after it stops so a later
	// `spawn --resume` can carry on where it left off.
	Session string `json:"session,omitempty"`
	// Conv is the extension conversation it answers ("" for other workers):
	// each turn's last message goes back there instead of to the Router.
	Conv string `json:"conv,omitempty"`
	// cwdFallback says the asked-for directory wasn't there, so it started in
	// home; spawn's answer passes it on. Not kept.
	cwdFallback string
	// ResumeAt is when Collie tells it to carry on after its agent stopped
	// on an error that passes (worker_resume.go); Resumed counts those in a row.
	ResumeAt time.Time `json:"resume_at,omitempty"`
	Resumed  int       `json:"resumed,omitempty"`
}

// Entry is one line of the human-facing super-agent conversation.
type Entry struct {
	ID     string    `json:"id"`
	Role   string    `json:"role"` // user | collie | event
	Thread string    `json:"thread"`
	Text   string    `json:"text"`
	At     time.Time `json:"at"`
	Ref    string    `json:"ref,omitempty"`
	Tone   string    `json:"tone,omitempty"` // event flavour: spawn | done | blocked | info | error
	// What a reply is about, when Collie said (collie reply --re): copied at
	// the time, so it still reads right after the original is trimmed away.
	Re *Quote `json:"re,omitempty"`
	// Quick-reply choices Collie offered with its question; tapped to answer.
	Options []string `json:"options,omitempty"`
	// Marked by Collie (--check) when a reply without options still awaits the human's review or action.
	Check bool `json:"check,omitempty"`
	// The dialog a "等你确认" event is about (decisions.go).
	Ask *DialogAsk `json:"ask,omitempty"`
}

// SearchHit is one matched user/collie message in a conversation search.
type SearchHit struct {
	ID      string    `json:"id"`
	Role    string    `json:"role"`
	At      time.Time `json:"at"`
	Snippet string    `json:"snippet"`
}

// SearchResult is the response payload of GET /api/super/search.
type SearchResult struct {
	Query   string      `json:"query"`
	Results []SearchHit `json:"results"`
	Total   int         `json:"total"`
	HasMore bool        `json:"has_more"`
}

// A Quote is what a reply answers: a message of yours, a worker's work, a timer.
type Quote struct {
	Entry  string `json:"entry,omitempty"`  // your message it answers
	Worker string `json:"worker,omitempty"` // or the worker it is about
	Who    string `json:"who"`              // 你 | the worker's name | 定时
	Text   string `json:"text"`
}

type Timer struct {
	ID      string     `json:"id"`
	Spec    string     `json:"spec"` // "in 30m" | "every 2h" | "daily 09:00" | "weekdays 09:00" | "weekly mon,thu 09:00" | "at 2026-11-18 09:00"
	Thread  string     `json:"thread"`
	Target  string     `json:"target,omitempty"` // worker name; empty = router
	Text    string     `json:"text"`
	NextRun time.Time  `json:"next_run"`
	LastRun *time.Time `json:"last_run,omitempty"`
	Enabled bool       `json:"enabled"`
	Owner   string     `json:"owner,omitempty"`
}

// Policy is the deterministic fast-path filter for one trigger source.
type Policy struct {
	Source string   `json:"source"` // e.g. webhook:github, webhook:feishu, webhook:*
	Action string   `json:"action"` // respond | candidate | ignore
	Allow  []string `json:"allow,omitempty"`
}

type Settings struct {
	RouterKind string   `json:"router_kind"`
	RouterArgs []string `json:"router_args,omitempty"`
	// RouterMachine: the computer Collie itself runs on, "" for the hub.
	RouterMachine string `json:"router_machine,omitempty"`
	// Model and thinking effort, "" for the agent's own default.
	RouterModel   string `json:"router_model,omitempty"`
	RouterEffort  string `json:"router_effort,omitempty"`
	WorkerModel   string `json:"worker_model,omitempty"`
	WorkerEffort  string `json:"worker_effort,omitempty"`
	WorkerKind    string `json:"worker_kind"`
	WorkerCwd     string `json:"worker_cwd"`
	WorkerMachine string `json:"worker_machine"`
	// A launcher alias in the shell of the computer it runs on, "" for the
	// agent's own command: it brings the alias's endpoint, keys and model.
	RouterAlias   string `json:"router_alias,omitempty"`
	WorkerAlias   string `json:"worker_alias,omitempty"`
	NotifyWebhook string `json:"notify_webhook,omitempty"`
	// Also send them from the hub's lark-cli (its bot, to whoever is logged in).
	NotifyLark   bool   `json:"notify_lark,omitempty"`
	NotifyLevel  string `json:"notify_level"` // quiet | normal
	StallMinutes int    `json:"stall_minutes"`
	// How long a chat reply should usually be, in characters (an English word counts as one).
	ReplyChars int `json:"reply_chars"`
	// The old limit in words (a Chinese character counted as half a word); read once to migrate.
	ReplyWords int  `json:"reply_words,omitempty"`
	Paused     bool `json:"paused,omitempty"`
	// Collie starts itself in a fresh session (memory.md carries over) once its
	// context is this full: 0 means the default (60), -1 never.
	RotatePct int `json:"rotate_pct,omitempty"`
	// When Collie dreams (reviews recent sessions and tidies its memory):
	// "HH:MM" each night, "" for 04:00, "off" never.
	DreamAt string `json:"dream_at,omitempty"`
	// Collie and its workers run with the agent's permission checks off
	// (--dangerously-skip-permissions and the like) unless this is set.
	AskPermissions bool `json:"ask_permissions,omitempty"`
	// What the UI calls the sessions Collie started: "" or "topic" the title
	// the agent gave its session (default), "name" their worker name.
	WorkerTitle string `json:"worker_title,omitempty"`
	// Extensions (extensions/README.md §4): the word that hands a message in
	// an open conversation to its agent ("" for /ai), and the line under
	// every answer sent back ("" for the default template).
	ExtTrigger string `json:"ext_trigger,omitempty"`
	ExtFooter  string `json:"ext_footer,omitempty"`
}

const (
	workerTitleTopic = "topic"
	workerTitleName  = "name"
)

func normalizeWorkerTitle(v string) string {
	if v == workerTitleName {
		return workerTitleName
	}
	return workerTitleTopic
}

type State struct {
	Seq       int                `json:"seq"`
	Triggers  []*Trigger         `json:"triggers"`
	Threads   map[string]*Thread `json:"threads"`
	Workers   map[string]*Worker `json:"workers"`
	Entries   []*Entry           `json:"entries"`
	Dismissed map[string]bool    `json:"dismissed,omitempty"`
	Pinned    []string           `json:"pinned,omitempty"`
	// Held: --opt/--check messages still waiting on the human that fell out
	// of Entries (pending_hold.go). RemindedAt: the last daily reminder.
	Held       []string  `json:"held,omitempty"`
	RemindedAt time.Time `json:"reminded_at,omitempty"`
	Timers     []*Timer  `json:"timers"`
	Policies   []Policy  `json:"policies"`
	Settings   Settings  `json:"settings"`
}

func defaultState() *State {
	return &State{
		Threads:   map[string]*Thread{},
		Workers:   map[string]*Worker{},
		Dismissed: map[string]bool{},
		Policies: []Policy{
			{Source: "webhook:github", Action: "candidate"},
			{Source: "webhook:feishu", Action: "candidate"},
			{Source: "webhook:*", Action: "candidate"},
		},
		Settings: Settings{
			RouterKind: "claude", WorkerKind: "claude", WorkerCwd: "~",
			NotifyLevel: "quiet", StallMinutes: 12, ReplyChars: 200,
			WorkerTitle: workerTitleTopic,
		},
	}
}

func (st *State) nextID(prefix string) string {
	st.Seq++
	return prefix + strconv.Itoa(st.Seq)
}

const maxPinned = 200
const keepTriggers = 600

// keepEntries is how much of the conversation stays in memory; the rest is in SQLite.
const keepEntries = 600

func (st *State) trim() {
	if n := len(st.Triggers); n > keepTriggers {
		cutoff := n - keepTriggers
		kept := make([]*Trigger, 0, keepTriggers)
		for _, t := range st.Triggers[:cutoff] {
			if t.Status == "queued" {
				kept = append(kept, t)
			}
		}
		st.Triggers = append(kept, st.Triggers[cutoff:]...)
	}
	if n := len(st.Entries); n > keepEntries {
		st.Entries = append([]*Entry(nil), st.Entries[n-keepEntries:]...)
	}
	if n := len(st.Pinned); n > maxPinned {
		st.Pinned = append([]string(nil), st.Pinned[n-maxPinned:]...)
	}
	for _, th := range st.Threads {
		if n := len(th.Triggers); n > 60 {
			th.Triggers = append([]string(nil), th.Triggers[n-60:]...)
		}
	}
	const keepDismissed = 1000
	if len(st.Dismissed) > keepDismissed {
		ids := make([]string, 0, len(st.Dismissed))
		for id := range st.Dismissed {
			ids = append(ids, id)
		}
		// Keep the highest entry numbers so recent dismissals never fall out.
		for i := 0; i < len(ids)-1; i++ {
			for j := i + 1; j < len(ids); j++ {
				if idNum(ids[i]) > idNum(ids[j]) {
					ids[i], ids[j] = ids[j], ids[i]
				}
			}
		}
		keep := make(map[string]bool, keepDismissed)
		for _, id := range ids[len(ids)-keepDismissed:] {
			keep[id] = true
		}
		st.Dismissed = keep
	}
}

func (st *State) trigger(id string) *Trigger {
	for i := len(st.Triggers) - 1; i >= 0; i-- {
		if st.Triggers[i].ID == id {
			return st.Triggers[i]
		}
	}
	return nil
}

func (st *State) policyFor(source string) Policy {
	if source == "chat" || source == "worker" || source == "timer" || source == "system" || source == "router" {
		return Policy{Source: source, Action: "respond"}
	}
	var wildcard *Policy
	for i, p := range st.Policies {
		if p.Source == source {
			return p
		}
		if strings.HasSuffix(p.Source, "*") && strings.HasPrefix(source, strings.TrimSuffix(p.Source, "*")) {
			wildcard = &st.Policies[i]
		}
	}
	if wildcard != nil {
		return *wildcard
	}
	return Policy{Source: source, Action: "candidate"}
}

// hasPolicy: a rule in 来源策略 covers source (by name or a trailing *).
func (st *State) hasPolicy(source string) bool {
	for _, p := range st.Policies {
		if p.Source == source || strings.HasSuffix(p.Source, "*") && strings.HasPrefix(source, strings.TrimSuffix(p.Source, "*")) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- timers

// parseSpec returns the first run time for a timer spec.
func parseSpec(spec string, now time.Time) (time.Time, error) {
	f := strings.Fields(strings.TrimSpace(spec))
	if len(f) < 2 {
		return time.Time{}, fmt.Errorf("spec must be 'in 30m', 'every 2h', 'daily 09:00', 'weekdays 09:00', 'weekly mon,thu 09:00' or 'at 2026-11-18 09:00'")
	}
	switch f[0] {
	case "in", "every":
		d, err := time.ParseDuration(f[1])
		if err != nil || d < time.Minute {
			return time.Time{}, fmt.Errorf("duration must be ≥1m, like 30m or 2h")
		}
		return now.Add(d), nil
	case "daily", "weekdays", "weekly":
		days, clock, err := clockSpec(f)
		if err != nil {
			return time.Time{}, err
		}
		next := time.Date(now.Year(), now.Month(), now.Day(), clock.Hour(), clock.Minute(), 0, 0, now.Location())
		for !next.After(now) || !days[next.Weekday()] {
			next = next.AddDate(0, 0, 1)
		}
		return next, nil
	case "at":
		t, err := time.ParseInLocation("2006-01-02 15:04", strings.Join(f[1:], " "), now.Location())
		if err != nil {
			return time.Time{}, fmt.Errorf("at needs 'YYYY-MM-DD HH:MM'")
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unknown spec %q", f[0])
}

var weekdayNames = map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}

// clockSpec reads the days and time of day of a daily / weekdays / weekly spec.
func clockSpec(f []string) (map[time.Weekday]bool, time.Time, error) {
	days := map[time.Weekday]bool{}
	clockAt := 1
	switch f[0] {
	case "daily":
		for d := time.Sunday; d <= time.Saturday; d++ {
			days[d] = true
		}
	case "weekdays":
		for d := time.Monday; d <= time.Friday; d++ {
			days[d] = true
		}
	case "weekly":
		if len(f) < 3 {
			return nil, time.Time{}, fmt.Errorf("weekly needs days and HH:MM, like 'weekly mon,thu 09:00'")
		}
		for _, n := range strings.Split(strings.ToLower(f[1]), ",") {
			d, ok := weekdayNames[strings.TrimSpace(n)]
			if !ok {
				return nil, time.Time{}, fmt.Errorf("unknown day %q (mon tue wed thu fri sat sun)", n)
			}
			days[d] = true
		}
		clockAt = 2
	}
	t, err := time.Parse("15:04", f[clockAt])
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%s needs HH:MM", f[0])
	}
	return days, t, nil
}

// advance moves a fired timer to its next run; one-shot timers are disabled.
func (t *Timer) advance(now time.Time) {
	f := strings.Fields(t.Spec)
	switch f[0] {
	case "every":
		d, _ := time.ParseDuration(f[1])
		next := t.NextRun
		for !next.After(now) {
			next = next.Add(d)
		}
		t.NextRun = next
	case "daily", "weekdays", "weekly":
		days, _, err := clockSpec(f)
		if err != nil {
			t.Enabled = false
			return
		}
		next := t.NextRun
		for !next.After(now) || !days[next.Weekday()] {
			next = next.AddDate(0, 0, 1)
		}
		t.NextRun = next
	default:
		t.Enabled = false
	}
}

// migrateReply turns the old word limit into characters (it counted two
// Chinese characters as one word, so the length stays the same).
func (s *Settings) migrateReply() {
	if s.ReplyChars <= 0 {
		s.ReplyChars = 200
		if s.ReplyWords > 0 {
			s.ReplyChars = s.ReplyWords * 2
		}
	}
	s.ReplyWords = 0
}
