package collie

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (s *Server) superRoutes(mux *http.ServeMux) {
	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.auth(h)) }
	api("GET /api/super", s.superSnapshot)
	api("POST /api/super/say", s.superSay)
	api("POST /api/super/reply", s.superReply)
	api("POST /api/super/reply-files", s.stageReplyFile)
	api("POST /api/super/dismiss", s.superDismiss)
	api("POST /api/super/pin", s.superPin)
	api("POST /api/super/spawn", s.superSpawn)
	api("POST /api/super/send", s.superSend)
	api("POST /api/super/report", s.superReport)
	api("POST /api/super/subscribe", s.superSubscribe)
	api("POST /api/super/stop", s.superStop)
	api("POST /api/super/answer", s.superAnswer)
	api("GET /api/super/decision", s.superDecision)
	api("POST /api/super/decide", s.superDecide)
	api("POST /api/super/decided", s.superDecided)
	api("GET /api/super/show/{id}", s.superShow)
	api("GET /api/super/list", s.superList)
	api("GET /api/super/triggers", s.superTriggers)
	api("GET /api/super/timers", s.superTimers)
	api("POST /api/super/timers", s.superAddTimer)
	api("POST /api/super/timers/{id}/delete", s.superDeleteTimer)
	api("GET /api/super/settings", s.superSettings)
	api("POST /api/super/settings", s.superSaveSettings)
	api("GET /api/super/aliases", s.superAliases)
	api("POST /api/super/seen", s.superSeen)
	api("GET /api/super/lark", s.superLark)
	api("GET /api/super/earlier", s.superEarlier)
	api("GET /api/super/search", s.superSearch)
	api("GET /api/super/memory", s.superMemory)
	api("POST /api/super/memory", s.superSaveMemory)
	api("POST /api/super/memory/delete", s.superDeleteMemory)
	api("POST /api/super/dream", s.superDream)
	api("POST /api/super/router/restart", s.superRestartRouter)
	mux.HandleFunc("POST /hooks/{source}", s.webhook)
	mux.HandleFunc("GET /dl/{ref}/{n}", s.serveDownload) // signed, no sign-in (ext_links.go)
}

// ---------------------------------------------------------------- snapshot

type routerView struct {
	Alive  bool   `json:"alive"`
	Status string `json:"status"`
	PaneID string `json:"pane_id,omitempty"`
	Kind   string `json:"kind"`
	// How full its context is (percent), and when it rotates to a fresh session.
	Context  int  `json:"context,omitempty"`
	Used     int  `json:"used,omitempty"`   // tokens in its window now
	Window   int  `json:"window,omitempty"` // of how many
	RotateAt int  `json:"rotate_at"`
	Handover bool `json:"handover,omitempty"`
	Dreaming bool `json:"dreaming,omitempty"`
	// It has already replied this turn, and is only wrapping up (memory…).
	Replied bool `json:"replied,omitempty"`
	// The model and effort it runs with ("" = the agent's own default), and
	// whether the settings have changed since it started.
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	Pending bool   `json:"pending,omitempty"`
	// The computer it runs on ("" for the hub's own) and the alias it was
	// started through; while asleep, where and how it will start.
	Machine string `json:"machine"`
	Alias   string `json:"alias,omitempty"`
}

type superView struct {
	Router  routerView `json:"router"`
	Entries []*Entry   `json:"entries"`
	Earlier bool       `json:"earlier,omitempty"` // older chat than Entries: /api/super/earlier
	// Pending: Collie's --opt and --check messages in chat:main still waiting on the human.
	Pending []PendingItem `json:"pending"`
	// Pinned: messages in chat:main manually pinned by the human until unpinned.
	Pinned  []PinnedItem `json:"pinned"`
	Workers []*Worker    `json:"workers"`
	Queued  int          `json:"queued"`
	// QueuedRouter: those waiting for Collie itself, not for a worker.
	QueuedRouter int    `json:"queued_router"`
	Paused       bool   `json:"paused"`
	WorkerTitle  string `json:"worker_title,omitempty"`
	// Workbench: the 工作台 is turned on in 接入 (its entry shows).
	Workbench bool `json:"workbench"`
	// Follow: the next time Collie set itself to look at something again.
	Follow *followView `json:"follow,omitempty"`
}

type followView struct {
	Next     time.Time `json:"next"`
	NextText string    `json:"next_text"` // what it will look at then
}

func (s *Server) superState(ctx context.Context) superView {
	sp := s.sup
	live, _ := sp.liveAgents(withCachedLists(ctx))
	wb := s.workbenchOn() // before sp.mu: an extension's setting may ask where Collie runs
	sp.mu.Lock()
	defer sp.mu.Unlock()
	v := superView{Router: routerView{Kind: sp.st.Settings.RouterKind, Status: "asleep"}, Paused: sp.st.Settings.Paused, WorkerTitle: normalizeWorkerTitle(sp.st.Settings.WorkerTitle), Workbench: wb}
	if a, ok := live[routerName]; ok {
		v.Router = routerView{Alive: true, Status: a.Status, PaneID: a.Pane, Kind: a.Kind}
		if asked := sp.routerAskedAt; a.Status == "working" && !asked.IsZero() {
			for i := len(sp.st.Entries) - 1; i >= 0 && !sp.st.Entries[i].At.Before(asked); i-- {
				if r := sp.st.Entries[i].Role; r == "collie" || r == "user" {
					v.Router.Replied = r == "collie" // unless the human has said more since
					break
				}
			}
		}
	} else if sp.routerLaunching {
		v.Router.Status = "starting"
	}
	u := sp.routerUsage
	v.Router.Context, v.Router.Used, v.Router.Window = u.Pct(), u.Used, u.Window
	v.Router.RotateAt, v.Router.Handover = sp.rotateAtLocked(), sp.handover != nil
	v.Router.Dreaming = sp.dream != nil
	st := sp.st.Settings
	v.Router.Model, v.Router.Effort = st.RouterModel, st.RouterEffort
	v.Router.Machine, v.Router.Alias = st.RouterMachine, st.RouterAlias
	if v.Router.Alive {
		l := sp.routerLaunchLocked()
		v.Router.Machine, v.Router.Alias = l.Machine, l.Alias
		if run := sp.routerRunLocked(); run != nil {
			v.Router.Model, v.Router.Effort = run[0], run[1]
			v.Router.Pending = *run != [2]string{st.RouterModel, st.RouterEffort} || kinds[v.Router.Kind] != kinds[st.RouterKind] || l.Machine != st.RouterMachine || l.Alias != st.RouterAlias
		}
		// What its latest reply used, when it says: an alias or the agent's
		// own default may pick what the settings leave open.
		if u.Model != "" {
			v.Router.Model, v.Router.Effort = u.Model, cmp.Or(u.Effort, v.Router.Effort)
		}
	}
	if sp.dream != nil {
		v.Router.Status = "dreaming"
	}
	es := sp.st.Entries
	if len(es) > 200 {
		es = es[len(es)-200:]
		v.Earlier = true
	}
	v.Entries = append([]*Entry{}, es...)
	v.Pending = sp.pendingLocked(time.Now())
	if v.Pending == nil {
		v.Pending = []PendingItem{}
	}
	v.Pinned = sp.pinnedLocked()
	if v.Pinned == nil {
		v.Pinned = []PinnedItem{}
	}
	v.Workers = []*Worker{}
	for _, w := range sp.st.Workers {
		if !w.Ended || time.Since(w.CreatedAt) < 12*time.Hour {
			cp := *w
			v.Workers = append(v.Workers, &cp)
		}
	}
	sort.Slice(v.Workers, func(i, j int) bool { return v.Workers[i].CreatedAt.After(v.Workers[j].CreatedAt) })
	v.Follow = sp.followLocked()
	for _, t := range sp.st.Triggers {
		if t.Status == "queued" {
			v.Queued++
			if len(t.Targets) == 0 || slices.Contains(t.Targets, routerName) {
				v.QueuedRouter++
			}
		}
	}
	return v
}

func (s *Server) superSnapshot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.superState(r.Context()))
}

// ---------------------------------------------------------------- conversation

func (s *Server) superSay(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text    string  `json:"text"`
		Machine string  `json:"machine"` // where to dispatch this time, "" = the default
		Kind    string  `json:"kind"`
		Model   string  `json:"model"`
		Effort  string  `json:"effort"`
		Alias   *string `json:"alias"` // absent: the default; "": none
		Re      string  `json:"re"`    // a message of Collie's (or the human's own) this answers
	}
	if !decode(w, r, &body) || strings.TrimSpace(body.Text) == "" {
		if strings.TrimSpace(body.Text) == "" {
			writeJSON(w, 400, map[string]any{"error": "empty message"})
		}
		return
	}
	sp := s.sup
	sp.mu.Lock()
	// The composer's "派到" chip: a one-off choice the router should honour.
	st := sp.st.Settings
	var hint []string
	if m := s.machineByName(body.Machine); m != "" || body.Machine == "local" {
		if m != st.WorkerMachine {
			name := "hub"
			if m != "" {
				name = m
			}
			hint = append(hint, "--machine "+name)
		}
	}
	if body.Kind == "agy" { // the composer's name for it
		body.Kind = "antigravity"
	}
	_, known := kinds[body.Kind]
	if known && kinds[body.Kind] != kinds[st.WorkerKind] {
		hint = append(hint, "--kind "+body.Kind)
	}
	// Model and alias choices only differ from the defaults for the default kind.
	defModel, defEffort, defAlias := "", "", ""
	if !known || kinds[body.Kind] == kinds[st.WorkerKind] {
		defModel, defEffort, defAlias = st.WorkerModel, st.WorkerEffort, st.WorkerAlias
	}
	alias := defAlias
	if body.Alias != nil {
		alias = *body.Alias
	}
	// An alias runs its own model: only the effort goes with one.
	if m := strings.TrimSpace(body.Model); m != "" && m != defModel && alias == "" && modelNameRe.MatchString(m) {
		hint = append(hint, "--model "+m)
	}
	if e := strings.TrimSpace(body.Effort); e != "" && e != defEffort && modelNameRe.MatchString(e) {
		hint = append(hint, "--effort "+e)
	}
	if a := body.Alias; a != nil && *a != defAlias && (*a == "" || aliasNameRe.MatchString(*a)) {
		if *a == "" {
			hint = append(hint, "--no-alias")
		} else {
			hint = append(hint, "--alias "+*a)
		}
	}
	note := ""
	if len(hint) > 0 {
		note = "\n\n[The human picked where this runs: spawn with " + strings.Join(hint, " ") + "]"
	}
	e, t, err := sp.sayLocked(body.Text, body.Re, note)
	sp.mu.Unlock()
	if err != nil {
		queuedWriteError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"entry": e.ID, "trigger": t.ID})
}

// sayLocked is your message to Collie, answering its message reID if set;
// note goes to Collie only.
func (sp *Super) sayLocked(text, reID, note string) (*Entry, *Trigger, error) {
	re := sp.sayQuoteLocked(reID)
	e := &Entry{ID: sp.st.nextID("e"), Role: "user", Thread: mainThread, Text: text, At: time.Now(), Re: re}
	prompt := text
	if re != nil { // which message they mean: Collie need not guess
		said := "you said"
		if re.Who == "你" {
			said = "the human said"
		}
		prompt = "[In reply to what " + said + " earlier: \"" + clip(re.Text, 300) + "\"]\n" + prompt
	}
	t, th := sp.prepareTriggerLocked("chat", mainThread, "you", prompt+note, "", "", e.ID)
	if err := sp.commitTriggerLocked(t, th, e, nil); err != nil {
		return nil, nil, err
	}
	return e, t, nil
}

func (s *Server) superReply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text     string     `json:"text"`
		Thread   string     `json:"thread"`
		Re       string     `json:"re"`
		Options  []string   `json:"options"`
		Check    bool       `json:"check"`
		To       string     `json:"to"`
		Progress bool       `json:"progress"`
		Files    []sendFile `json:"files"`
	}
	if !decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeJSON(w, 400, map[string]any{"error": "empty reply"})
		return
	}
	if body.To != "" {
		if s.ext == nil {
			writeJSON(w, 400, map[string]any{"error": "extensions run on the hub"})
			return
		}
		c, ok := s.ext.homeConv(body.To)
		if !ok {
			writeJSON(w, 400, map[string]any{"error": "no chat of " + body.To + " with the human yet (is the extension on and signed in?)"})
			return
		}
		body.Thread = c.thread()
	}
	if draftID, ok := strings.CutPrefix(body.Thread, draftThreadPrefix); ok && s.ext != nil {
		if !s.ext.completeDraft(draftID, body.Text, "") {
			writeJSON(w, 404, map[string]any{"error": "draft not found or already finished"})
			return
		}
		s.sup.mu.Lock()
		s.sup.routerAskedAt, s.sup.routerRetries = time.Time{}, 0
		s.sup.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true, "draft": draftID})
		return
	}
	if _, _, ok := channelOf(body.Thread); ok && s.ext != nil {
		kind := s.ext.replyKind(body.Thread, body.To != "", body.Progress)
		sent, err := s.ext.sendOutFiles(body.Thread, body.Text, kind, s.sup.routerRunInfo(), "", body.Files)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		// The answer lives in that chat (and its 收件箱 view), not in the
		// chat with Collie, where its question never showed.
		if !body.Progress {
			s.sup.mu.Lock()
			s.sup.routerAskedAt, s.sup.routerRetries = time.Time{}, 0
			s.sup.mu.Unlock()
		}
		writeJSON(w, 200, map[string]any{"ok": true, "sent": sent.Ref})
		return
	}
	if len(body.Files) > 0 {
		writeJSON(w, 400, map[string]any{"error": "--file only goes to an extension conversation (ext:… thread or --to)"})
		return
	}
	e, warn := s.sup.Reply(body.Thread, body.Text, body.Re, body.Options, body.Check)
	res := map[string]any{"ok": true, "entry": e.ID}
	if warn != "" {
		res["warning"] = warn
	}
	writeJSON(w, 200, res)
}

func (s *Server) superDismiss(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &body) {
		return
	}
	id := strings.TrimSpace(body.ID)
	if id == "" {
		writeJSON(w, 400, map[string]any{"error": "id is required"})
		return
	}
	s.sup.Dismiss(id)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) superPin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID     string `json:"id"`
		Pinned bool   `json:"pinned"`
	}
	if !decode(w, r, &body) {
		return
	}
	id := strings.TrimSpace(body.ID)
	if id == "" {
		writeJSON(w, 400, map[string]any{"error": "id is required"})
		return
	}
	if err := s.sup.Pin(id, body.Pinned); err != nil {
		writeJSON(w, 404, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "pinned": body.Pinned})
}

// replyLen is a reply's length as people read it: each Chinese/Japanese/Korean
// character counts one, and so does each run of Latin text (a word).
func replyLen(s string) int {
	n, cjk := 0, 0
	for _, f := range strings.Fields(s) {
		ascii := true
		for _, r := range f {
			if r > 0x2E80 {
				cjk++
				ascii = false
			}
		}
		if ascii {
			n++
		}
	}
	return n + cjk
}

// ---------------------------------------------------------------- workers

// spawnBody is what `collie spawn` sends.
type spawnBody struct {
	Kind      string  `json:"kind"`
	Cwd       string  `json:"cwd"`
	Task      string  `json:"task"`
	Thread    string  `json:"thread"`
	Name      string  `json:"name"`
	By        string  `json:"by"`
	Machine   *string `json:"machine"`
	Model     string  `json:"model"`
	Effort    string  `json:"effort"`
	Alias     *string `json:"alias"` // absent: the default; "": none
	Subscribe *bool   `json:"subscribe"`
	Resume    string  `json:"resume"` // a stopped worker to carry on from
}

// carryOver fills in what a resumed worker keeps from old, unless asked
// otherwise, and returns the conversation to open.
func (b *spawnBody) carryOver(old *Worker) (session string) {
	// Its own computer, directory, agent and alias: that is where the conversation is.
	b.Kind, b.Cwd = old.Kind, old.Cwd
	m := old.Machine
	b.Machine = &m
	if b.Alias == nil {
		b.Alias = old.Alias
	}
	if b.Name == "" {
		b.Name = old.Name
	}
	if b.Model == "" && b.Effort == "" {
		b.Model, b.Effort = old.Model, old.Effort
	}
	if b.Thread == "" {
		b.Thread = old.Thread
	}
	return old.Session
}

func (s *Server) superSpawn(w http.ResponseWriter, r *http.Request) {
	var body spawnBody
	if !decode(w, r, &body) {
		return
	}
	session := ""
	if body.Resume != "" {
		old, err := s.sup.resumeFrom(body.Resume)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		session = body.carryOver(old)
	}
	if strings.TrimSpace(body.Task) == "" {
		writeJSON(w, 400, map[string]any{"error": "task is required"})
		return
	}
	if !modelNameRe.MatchString(body.Model) || !modelNameRe.MatchString(body.Effort) {
		writeJSON(w, 400, map[string]any{"error": "invalid model or effort"})
		return
	}
	if body.Alias != nil && *body.Alias != "" && !aliasNameRe.MatchString(*body.Alias) {
		writeJSON(w, 400, map[string]any{"error": "invalid alias"})
		return
	}
	if body.By == "" {
		body.By = routerName
	}
	if body.Thread == "" {
		body.Thread = mainThread
	}
	// Workers subscribe to their own thread unless it is the shared main chat,
	// which always stays with the router.
	sub := body.Thread != mainThread
	if body.Subscribe != nil {
		sub = *body.Subscribe
	}
	machine := s.sup.settings().WorkerMachine
	if session != "" {
		machine = *body.Machine // an id already
	} else if body.Machine != nil {
		machine = s.machineByName(*body.Machine)
	}
	wk, started, err := s.sup.Spawn(SpawnSpec{Kind: body.Kind, Cwd: body.Cwd, Task: body.Task, Thread: body.Thread, Name: body.Name, By: body.By,
		Machine: machine, Model: body.Model, Effort: body.Effort, Alias: body.Alias, Subscribe: sub, Resume: session})
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	// Most failures (the computer's herdr down, a bad directory) show in the
	// first seconds: say them now, so nobody hears "started" first.
	status := "starting"
	select {
	case err := <-started:
		if err != nil {
			writeJSON(w, 502, map[string]any{"error": "worker " + wk.Name + " failed to start: " + err.Error()})
			return
		}
		status = "started"
	case <-time.After(8 * time.Second):
	}
	res := map[string]any{"worker": wk.Name, "kind": wk.Kind, "status": status, "thread": wk.Thread, "cwd": wk.Cwd,
		"note": "its agent is starting; you will get a [collie] event when it reports, finishes, gets blocked or fails. Tell the human it is on its way, not that it is done"}
	if wk.cwdFallback != "" {
		res["cwd_fallback"] = wk.cwdFallback
	}
	if body.Model != "" && wk.Alias != nil && *wk.Alias != "" {
		res["model_ignored"] = fmt.Sprintf("alias %s starts it with the alias's own model, so --model %s was ignored; spawn with --no-alias to pick a model", *wk.Alias, body.Model)
	}
	if body.Name != "" && wk.Name != cleanWorkerName(body.Name) {
		res["renamed"] = fmt.Sprintf("%q was taken, so this worker is %s: use that name to send to it, stop it or resume it", body.Name, wk.Name)
	}
	writeJSON(w, 202, res)
}

func (s *Server) superSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		To     string `json:"to"`
		Text   string `json:"text"`
		Thread string `json:"thread"`
	}
	if !decode(w, r, &body) {
		return
	}
	sp := s.sup
	sp.mu.Lock()
	wk := sp.st.Workers[body.To]
	if wk == nil || wk.Ended {
		sp.mu.Unlock()
		writeJSON(w, 404, map[string]any{"error": "no running worker named " + body.To})
		return
	}
	if body.Thread == "" {
		body.Thread = wk.Thread
	}
	t, err := sp.ingestDurableLocked("router", body.Thread, routerName, body.Text, "", body.To, "")
	sp.mu.Unlock()
	if err != nil {
		queuedWriteError(w, err)
		return
	}
	writeJSON(w, 202, map[string]any{"trigger": t.ID, "note": "queued; delivered when the worker is free"})
}

func (s *Server) superReport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Worker string `json:"worker"`
		Pane   string `json:"pane"`
		Text   string `json:"text"`
	}
	if !decode(w, r, &body) {
		return
	}
	name := body.Worker
	if name == "" && body.Pane != "" {
		// A worker on another computer reports through that computer's token.
		if _, d := s.identity(r); d != nil && d.Kind == "machine" {
			body.Pane = join(body.Pane, d.ID)
		}
		name = s.sup.workerByPane(body.Pane)
	}
	if name == "" {
		writeJSON(w, 400, map[string]any{"error": "could not tell which worker you are; pass --worker <name>"})
		return
	}
	if err := s.sup.Report(name, strings.TrimSpace(body.Text)); err != nil {
		if errors.Is(err, errUnknownWorker) {
			writeJSON(w, 404, map[string]any{"error": err.Error()})
		} else {
			queuedWriteError(w, err)
		}
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) superSubscribe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Worker string `json:"worker"`
		Thread string `json:"thread"`
		Off    bool   `json:"off"`
	}
	if !decode(w, r, &body) {
		return
	}
	sp := s.sup
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.st.Workers[body.Worker] == nil || body.Thread == "" {
		writeJSON(w, 400, map[string]any{"error": "worker and thread are required"})
		return
	}
	if body.Thread == mainThread && !body.Off {
		writeJSON(w, 400, map[string]any{"error": "chat:main always routes to the router"})
		return
	}
	th := sp.threadLocked(body.Thread)
	if body.Off {
		th.Subscribers = remove(th.Subscribers, body.Worker)
	} else if !slices.Contains(th.Subscribers, body.Worker) {
		th.Subscribers = append(th.Subscribers, body.Worker)
	}
	sp.changed()
	writeJSON(w, 200, map[string]any{"thread": th.ID, "subscribers": th.Subscribers})
}

func (s *Server) superStop(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Worker string `json:"worker"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.stopWorker(r.Context(), body.Worker); err != nil {
		writeJSON(w, 404, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

var errUnknownWorker = errors.New("unknown worker")

// stopWorker ends a worker's turn and closes its pane.
func (s *Server) stopWorker(ctx context.Context, name string) error {
	sp := s.sup
	sp.mu.Lock()
	wk := sp.st.Workers[name]
	sp.mu.Unlock()
	if wk == nil {
		return errUnknownWorker
	}
	if wk.Session == "" && wk.PaneID != "" {
		// herdr does not always know (Antigravity): ask the pane's machine.
		local, m := split(wk.PaneID)
		if ps := s.titlesFor(m, ctx)[local]; ps.Session != "" {
			sp.mu.Lock()
			wk.Session = ps.Session
			sp.mu.Unlock()
		}
	}
	if wk.PaneID != "" {
		// Esc stops the turn it is on, the way you would before closing it:
		// Codex's turn lives in a shared daemon and would carry on without
		// its window.
		s.herdr.SendKeys(ctx, wk.PaneID, "esc")
		time.Sleep(700 * time.Millisecond)
	}
	if kinds[wk.Kind] == "codex" && wk.Session != "" {
		// Its turn runs in Codex's shared daemon and outlives the terminal.
		if err := s.stopCodexTurn(ctx, wk.Machine, wk.Session); err != nil {
			log.Printf("stop %s: interrupt its codex turn: %v", wk.Name, err)
		}
	}
	if wk.WorkspaceID != "" {
		s.herdr.Call(ctx, "workspace.close", map[string]any{"workspace_id": wk.WorkspaceID}, nil)
	}
	sp.mu.Lock()
	wk.Ended, wk.Status = true, "gone"
	for _, th := range sp.st.Threads {
		th.Subscribers = remove(th.Subscribers, wk.Name)
	}
	sp.addEntryLocked("event", wk.Thread, wk.Name+" 已停止", wk.Name, "info")
	sp.mu.Unlock()
	return nil
}

func (s *Server) superShow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.sup.flush() // a thread's listing reads the DB
	if out, ok := s.sup.Show(id, idNum(r.URL.Query().Get("before"))); ok {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, out)
		return
	}
	http.Error(w, "nothing found for "+id+" (ids look like t12, a thread like chat:main, or a worker name)", 404)
}

func (s *Server) superList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, s.sup.List())
}

func (s *Server) superTriggers(w http.ResponseWriter, r *http.Request) {
	sp := s.sup
	sp.flush() // thread counts come from the DB
	sp.mu.Lock()
	ts := sp.st.Triggers
	if len(ts) > 120 {
		ts = ts[len(ts)-120:]
	}
	out := make([]*Trigger, 0, len(ts))
	for i := len(ts) - 1; i >= 0; i-- {
		cp := *ts[i]
		cp.Text = clip(cp.Text, 600)
		cp.LarkText = ""
		out = append(out, &cp)
	}
	ths := make([]Thread, 0, len(sp.st.Threads))
	for _, th := range sp.st.Threads {
		cp := *th
		cp.Subscribers = append([]string(nil), th.Subscribers...)
		cp.Triggers = append([]string(nil), th.Triggers...)
		ths = append(ths, cp)
	}
	sp.mu.Unlock()
	sort.Slice(ths, func(i, j int) bool { return ths[i].LastAt.After(ths[j].LastAt) })
	if len(ths) > 40 {
		ths = ths[:40]
	}
	// Memory keeps only a thread's latest ids; the DB has them all.
	ids := make([]string, len(ths))
	for i, th := range ths {
		ids[i] = th.ID
	}
	counts, err := threadTriggerCounts(sp.db, ids)
	if err != nil {
		log.Printf("count thread triggers: %v", err)
		counts = map[string]int{}
	}
	result := map[string]any{"triggers": out, "threads": ths, "counts": counts}
	if master, device := s.identity(r); s.ext != nil && (master || device != nil && device.Kind == "browser") {
		result["lark_threads"] = s.larkThreadSummaries(ths)
	}
	writeJSON(w, 200, result)
}

// ---------------------------------------------------------------- timers

func (s *Server) superTimers(w http.ResponseWriter, r *http.Request) {
	sp := s.sup
	sp.mu.Lock()
	defer sp.mu.Unlock()
	writeJSON(w, 200, map[string]any{"timers": sortedTimers(sp.st.Timers)})
}

func sortedTimers(ts []*Timer) []*Timer {
	out := slices.Clone(ts)
	endAt := func(t *Timer) time.Time {
		if t.LastRun != nil && !t.LastRun.IsZero() {
			return *t.LastRun
		}
		return t.NextRun
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Enabled != b.Enabled {
			return a.Enabled
		}
		if a.Enabled {
			if a.NextRun.IsZero() != b.NextRun.IsZero() {
				return !a.NextRun.IsZero()
			}
			if !a.NextRun.Equal(b.NextRun) {
				return a.NextRun.Before(b.NextRun)
			}
			return idNum(a.ID) < idNum(b.ID)
		}
		ta, tb := endAt(a), endAt(b)
		if ta.IsZero() != tb.IsZero() {
			return !ta.IsZero()
		}
		if !ta.Equal(tb) {
			return ta.After(tb)
		}
		return idNum(a.ID) > idNum(b.ID)
	})
	return out
}

func (s *Server) superAddTimer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Spec   string `json:"spec"`
		Text   string `json:"text"`
		Thread string `json:"thread"`
		Target string `json:"target"`
		Owner  string `json:"owner"`
	}
	if !decode(w, r, &body) {
		return
	}
	next, err := parseSpec(body.Spec, time.Now())
	if err != nil || strings.TrimSpace(body.Text) == "" {
		msg := "text is required"
		if err != nil {
			msg = err.Error()
		}
		writeJSON(w, 400, map[string]any{"error": msg})
		return
	}
	if body.Thread == "" {
		body.Thread = mainThread
	}
	sp := s.sup
	sp.mu.Lock()
	t := &Timer{ID: sp.st.nextID("m"), Spec: strings.TrimSpace(body.Spec), Thread: body.Thread, Target: body.Target,
		Text: body.Text, NextRun: next, Enabled: true, Owner: body.Owner}
	sp.st.Timers = append(sp.st.Timers, t)
	sp.changed()
	sp.mu.Unlock()
	writeJSON(w, 200, t)
}

func (s *Server) superDeleteTimer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sp := s.sup
	sp.mu.Lock()
	defer sp.mu.Unlock()
	for i, t := range sp.st.Timers {
		if t.ID == id {
			sp.st.Timers = append(sp.st.Timers[:i], sp.st.Timers[i+1:]...)
			sp.deletedTimers = append(sp.deletedTimers, id)
			sp.changed()
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
	writeJSON(w, 404, map[string]any{"error": "no timer " + id})
}

// ---------------------------------------------------------------- settings

func (s *Server) hookKey() string {
	h := sha256.Sum256([]byte("collie-hooks:" + s.token))
	return hex.EncodeToString(h[:12])
}

func (s *Server) superSettings(w http.ResponseWriter, r *http.Request) {
	sp := s.sup
	dir, bin := sp.routerTarget(sp.routerOn())
	if dir == "" {
		dir = sp.routerDir() // that computer not synced with yet
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	st := sp.st.Settings
	st.WorkerTitle = normalizeWorkerTitle(st.WorkerTitle)
	writeJSON(w, 200, map[string]any{
		"settings": st, "policies": sp.st.Policies,
		"hook_key": s.hookKey(), "router_dir": dir, "bin": bin,
	})
}

// superSeen: a page of Collie is open in front of you. Its origin goes in
// the notifications' link.
func (s *Server) superSeen(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL  string `json:"url"`
		Page string `json:"page"` // which page, so it can say it went to the background
		Away bool   `json:"away"`
		Idle int    `json:"idle"` // seconds since you last used it
	}
	if !decode(w, r, &body) {
		return
	}
	if !strings.HasPrefix(body.URL, "http://") && !strings.HasPrefix(body.URL, "https://") || len(body.URL) > 200 {
		body.URL = ""
	}
	s.sup.seenPage(clip(body.Page, 40), body.URL, body.Away, time.Duration(max(body.Idle, 0))*time.Second)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// superEarlier is Collie's chat before a given entry, a page at a time,
// oldest first: what the page shows when you scroll back past what it has.
// Optional `until=<entryID>` jumps straight down to a target message (plus a
// small context cushion before it) in a single round-trip.
func (s *Server) superEarlier(w http.ResponseWriter, r *http.Request) {
	before := idNum(r.URL.Query().Get("before"))
	if before <= 0 {
		writeJSON(w, 400, map[string]any{"error": "before?"})
		return
	}
	sp := s.sup
	sp.flush() // what memory has but the DB not yet
	const page = 120
	var es []*Entry
	var more bool
	if until := idNum(r.URL.Query().Get("until")); until > 0 && until < before {
		es, more = entryHistoryUntil(sp.db, mainThread, before, until, 24, 10000)
	} else {
		es = entryHistory(sp.db, mainThread, before, page+1)
		more = len(es) > page
		if more {
			es = es[:page]
		}
	}
	if es == nil {
		es = []*Entry{}
	}
	for i, j := 0, len(es)-1; i < j; i, j = i+1, j-1 {
		es[i], es[j] = es[j], es[i]
	}
	writeJSON(w, 200, map[string]any{"entries": es, "more": more})
}

// superSearch searches full chat:main history for user/collie messages.
func (s *Server) superSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	thread := r.URL.Query().Get("thread")
	limit := defaultSearchLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	writeJSON(w, 200, s.sup.Search(thread, q, limit))
}

// superLark says whom the hub's lark-cli would write to.
func (s *Server) superLark(w http.ResponseWriter, r *http.Request) {
	_, _, who, problem := lark.get(r.URL.Query().Has("fresh"))
	writeJSON(w, 200, map[string]any{"ready": problem == "", "who": who, "problem": problem})
}

func (s *Server) superSaveSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Settings *Settings `json:"settings"`
		Policies []Policy  `json:"policies"`
	}
	if !decode(w, r, &body) {
		return
	}
	sp := s.sup
	sp.mu.Lock()
	if body.Settings != nil {
		ns := *body.Settings
		if _, ok := kinds[ns.RouterKind]; !ok {
			ns.RouterKind = sp.st.Settings.RouterKind
		}
		if _, ok := kinds[ns.WorkerKind]; !ok {
			ns.WorkerKind = sp.st.Settings.WorkerKind
		}
		if ns.NotifyLevel != "normal" {
			ns.NotifyLevel = "quiet"
		}
		ns.WorkerTitle = normalizeWorkerTitle(ns.WorkerTitle)
		for _, p := range []*string{&ns.RouterModel, &ns.RouterEffort, &ns.WorkerModel, &ns.WorkerEffort} {
			if *p = strings.TrimSpace(*p); !modelNameRe.MatchString(*p) {
				*p = ""
			}
		}
		for _, p := range []*string{&ns.RouterAlias, &ns.WorkerAlias} {
			if *p = strings.TrimSpace(*p); !aliasNameRe.MatchString(*p) {
				*p = ""
			}
		}
		ns.aliasesOwnModels()
		ns.RouterMachine = s.machineByName(ns.RouterMachine)
		ns.migrateReply()
		if ns.DreamAt != "off" {
			var h, m int
			if _, err := fmt.Sscanf(ns.DreamAt, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
				ns.DreamAt = ""
			} else {
				ns.DreamAt = fmt.Sprintf("%02d:%02d", h, m)
			}
		}
		if ns.RotatePct > 95 || (ns.RotatePct > 0 && ns.RotatePct < 20) {
			ns.RotatePct = 0
		} else if ns.RotatePct < 0 {
			ns.RotatePct = -1
		}
		ns.WorkerMachine = s.machineByName(ns.WorkerMachine)
		// Kept as written, "~" unless given: it is resolved on the computer
		// the work runs on, against that computer's home, so it still fits
		// when the work moves to another computer.
		if ns.WorkerCwd = strings.TrimSpace(ns.WorkerCwd); ns.WorkerMachine == "" {
			ns.WorkerCwd = s.tilde(ns.WorkerCwd)
		}
		if ns.WorkerCwd == "" {
			ns.WorkerCwd = "~"
		}
		sp.st.Settings = ns
	}
	if body.Policies != nil {
		valid := []Policy{}
		for _, p := range body.Policies {
			if p.Source != "" && (p.Action == "respond" || p.Action == "candidate" || p.Action == "ignore") {
				valid = append(valid, p)
			}
		}
		sp.st.Policies = valid
	}
	sp.changed()
	settings, policies := sp.st.Settings, sp.st.Policies
	sp.mu.Unlock()

	if body.Policies != nil && s.ext != nil {
		s.ext.pushSettings() // an ignored source has its requests no longer marked 在做了
	}
	if body.Settings != nil {
		go func() { // ROUTER.md quotes some settings (reply length, where workers go)
			if err := sp.writeRouterFiles(); err != nil {
				log.Printf("router files: %v", err)
			}
		}()
	}
	writeJSON(w, 200, map[string]any{"settings": settings, "policies": policies})
}

func (s *Server) superRestartRouter(w http.ResponseWriter, r *http.Request) {
	s.sup.RestartRouter(r.Context())
	writeJSON(w, 202, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- webhooks

// webhook ingests external events (GitHub, Feishu/Lark, anything that can
// POST JSON). It acknowledges the stored event; delivery happens in the inbox.
func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	want := s.hookKey()
	if subtle.ConstantTimeCompare([]byte(key), []byte(want)) != 1 && !s.authed(r) {
		writeJSON(w, 401, map[string]any{"error": "bad hook key"})
		return
	}
	src := strings.ToLower(r.PathValue("source"))
	if !nameRe.MatchString(src) {
		writeJSON(w, 400, map[string]any{"error": "bad source name"})
		return
	}
	raw, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	var payload map[string]any
	json.Unmarshal(raw, &payload)

	// Feishu/Lark URL verification handshake.
	if payload["type"] == "url_verification" {
		writeJSON(w, 200, map[string]any{"challenge": payload["challenge"]})
		return
	}

	thread, from, text, idem := genericEvent(src, payload, raw, r)
	switch {
	case r.Header.Get("X-GitHub-Event") != "":
		thread, from, text = githubEvent(r.Header.Get("X-GitHub-Event"), payload)
		idem = r.Header.Get("X-GitHub-Delivery")
	case payload["header"] != nil && payload["event"] != nil:
		thread, from, text, idem = feishuEvent(payload)
	}
	if text == "" {
		writeJSON(w, 202, map[string]any{"ok": true, "ignored": "empty event"})
		return
	}
	t, err := s.sup.ingestDurable("webhook:"+src, thread, from, text, idem, "", "")
	if err != nil {
		queuedWriteError(w, err)
		return
	}
	res := map[string]any{"ok": true}
	if t != nil {
		res["trigger"], res["action"] = t.ID, t.Action
	} else {
		res["duplicate"] = true
	}
	writeJSON(w, 202, res)
}

func queuedWriteError(w http.ResponseWriter, err error) {
	log.Printf("save queued event: %v", err)
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not save queued event"})
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return fmt.Sprintf("%.0f", x)
	}
	return ""
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func genericEvent(src string, p map[string]any, raw []byte, r *http.Request) (thread, from, text, idem string) {
	thread = r.Header.Get("X-Collie-Thread")
	if thread == "" {
		thread = str(p["thread"])
	}
	if thread == "" {
		thread = src
	}
	if !strings.Contains(thread, ":") {
		thread = src + ":" + thread
	}
	from = str(p["from"])
	text = str(p["text"])
	if text == "" {
		text = clip(string(raw), 4000)
	}
	idem = r.Header.Get("X-Idempotency-Key")
	if idem == "" {
		idem = str(p["id"])
	}
	return
}

func githubEvent(kind string, p map[string]any) (thread, from, text string) {
	repo := str(dig(p, "repository", "full_name"))
	num := str(dig(p, "issue", "number"))
	if num == "" {
		num = str(dig(p, "pull_request", "number"))
	}
	if num == "" {
		num = str(p["number"])
	}
	thread = "github:" + repo
	if num != "" {
		thread += "#" + num
	}
	from = str(dig(p, "sender", "login"))
	title := str(dig(p, "issue", "title"))
	if title == "" {
		title = str(dig(p, "pull_request", "title"))
	}
	body := str(dig(p, "comment", "body"))
	if body == "" {
		body = str(dig(p, "review", "body"))
	}
	if body == "" {
		body = str(dig(p, "issue", "body"))
	}
	if body == "" {
		body = str(dig(p, "pull_request", "body"))
	}
	url := str(dig(p, "comment", "html_url"))
	if url == "" {
		url = str(dig(p, "issue", "html_url"))
	}
	if url == "" {
		url = str(dig(p, "pull_request", "html_url"))
	}
	if kind == "check_run" || kind == "workflow_run" || kind == "check_suite" {
		name := str(dig(p, kind, "name"))
		concl := str(dig(p, kind, "conclusion"))
		branch := str(dig(p, kind, "head_branch"))
		url = str(dig(p, kind, "html_url"))
		if concl == "" {
			return thread, from, ""
		}
		return thread, from, fmt.Sprintf("GitHub %s %q on %s: %s\n%s", kind, name, branch, concl, url)
	}
	text = fmt.Sprintf("GitHub %s/%s on %s: %s\n%s\n%s", kind, str(p["action"]), thread, title, clip(body, 2500), url)
	return thread, from, strings.TrimSpace(text)
}

func feishuEvent(p map[string]any) (thread, from, text, idem string) {
	idem = str(dig(p, "header", "event_id"))
	msg, _ := dig(p, "event", "message").(map[string]any)
	if msg == nil {
		return "feishu:events", "", clip(fmt.Sprint(p["event"]), 2000), idem
	}
	chat := str(msg["chat_id"])
	root := str(msg["root_id"])
	if root == "" {
		root = str(msg["message_id"])
	}
	thread = "feishu:" + chat + ":" + root
	from = str(dig(p, "event", "sender", "sender_id", "open_id"))
	var content struct {
		Text string `json:"text"`
	}
	json.Unmarshal([]byte(str(msg["content"])), &content)
	text = content.Text
	if text == "" {
		text = "[" + str(msg["message_type"]) + " message]"
	}
	return thread, from, "Feishu message in " + chat + ":\n" + text, idem
}

// machineByName accepts a machine id or its display name ("" or "hub" = the
// hub's own machine).
func (s *Server) machineByName(n string) string {
	n = strings.TrimSpace(n)
	if n == "" || n == "hub" || n == "local" {
		return ""
	}
	for _, m := range s.herdr.Machines() {
		if m.ID == n || strings.EqualFold(m.Name, n) {
			return m.ID
		}
	}
	return n
}

// aliasesOwnModels: an alias brings its own model (the Router's, its own
// arguments too); the effort is still chosen with one.
func (ns *Settings) aliasesOwnModels() {
	if ns.RouterAlias != "" {
		ns.RouterArgs, ns.RouterModel = nil, ""
	}
	if ns.WorkerAlias != "" {
		ns.WorkerModel = ""
	}
}

// modelNameRe: model ids and effort names go onto a command line as one argument.
var modelNameRe = regexp.MustCompile(`^[A-Za-z0-9._:/@\[\]-]{0,120}$`)

// superAnswer: the router answers a dialog a worker is stuck on, by choice
// number, label words or "esc". The human sees what it picked.
func (s *Server) superAnswer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Worker string `json:"worker"`
		Choice string `json:"choice"`
	}
	if !decode(w, r, &body) {
		return
	}
	sp := s.sup
	sp.mu.Lock()
	wk := sp.st.Workers[body.Worker]
	sp.mu.Unlock()
	if wk == nil || wk.Ended || wk.PaneID == "" {
		writeJSON(w, 404, map[string]any{"error": "no running worker " + body.Worker})
		return
	}
	label, err := sp.answerPane(r.Context(), wk.PaneID, byCollie, dialogSelection{Choice: body.Choice})
	var none noChoiceError
	switch {
	case errors.Is(err, errNotAsking):
		writeJSON(w, 409, map[string]any{"error": body.Worker + " isn't showing a dialog now"})
		return
	case errors.As(err, &none):
		var list []string
		for i, c := range none.choices {
			list = append(list, fmt.Sprintf("%d. %s", i+1, c))
		}
		writeJSON(w, 400, map[string]any{"error": "no single choice matches " + strconv.Quote(body.Choice), "choices": list})
		return
	case err != nil:
		fail(w, err)
		return
	}
	sp.mu.Lock()
	sp.addEntryLocked("event", wk.Thread, "替 "+wk.Name+" 选了："+clip(label, 80), wk.Name, "info")
	sp.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "picked": label})
}
