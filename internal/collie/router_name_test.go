package collie

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHerdr is a herdr server with workspaces and agents in memory. The
// first `hidden` list calls answer empty, like a computer still reconnecting.
type fakeHerdr struct {
	mu     sync.Mutex
	ws     map[string]string // id → label
	panes  map[string]*fakePane
	next   int
	hidden int
	calls  []string
	// afterList runs once a list call has its answer, before it is sent.
	afterList func()
}

type fakePane struct {
	ws, name, kind, cwd string
	title               string
	// screen is what pane.read shows; ⇧← turns it into unfolded.
	screen, unfolded string
	sentKeys         []string
	// detection, when set, is what a "detection" read shows instead.
	detection string
	status    string // "" is idle
	// sim makes the pane an agent with an input box: its screen shows what
	// it took (said) above what is typed (box). drop counts make it lose
	// prompts or Enters; quiet ones take a message without changing state.
	sim                     bool
	box                     string
	writes                  []string // each pane.send_text, as sent
	said                    []string
	seq, completionSeq      uint64
	dropPrompts, dropEnters int
	quiet                   bool
	// hangUp: the next agent.prompt types its text, then the connection
	// drops before herdr answers (a node going away mid-call).
	hangUp bool
	// log is the agent's Claude session log, which gets what it takes;
	// staleBox keeps drawing a taken message in the input box.
	log      string
	staleBox bool
	drawn    string
}

func (p *fakePane) enter() {
	if p.dropEnters > 0 {
		p.dropEnters--
		return
	}
	if p.box == "" {
		return
	}
	if p.log != "" {
		line := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": p.box}}
		if p.status == "working" { // queued until the turn ends
			line = map[string]any{"type": "queue-operation", "operation": "enqueue", "content": p.box}
		}
		b, _ := json.Marshal(line)
		f, err := os.OpenFile(p.log, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			panic(err)
		}
		f.Write(append(b, '\n'))
		f.Close()
	}
	if p.staleBox {
		p.drawn = p.box
	}
	p.said, p.box = append(p.said, p.box), ""
	if !p.quiet {
		p.status = "working"
		p.seq++
	}
}

func (p *fakePane) simScreen() string {
	var b strings.Builder
	for _, m := range p.said {
		b.WriteString("❯ " + m + "\n")
	}
	box := p.box
	if box == "" {
		box = p.drawn
	}
	if strings.Contains(box, "\n") {
		box = fmt.Sprintf("[Pasted text #1 +%d lines]", strings.Count(box, "\n"))
	}
	// herdr reads lines without their trailing spaces: an empty box is "❯".
	b.WriteString(strings.TrimRight("────\n❯ "+box, " ") + "\n────\n")
	return b.String()
}

var errHangUp = &HerdrError{Code: "hang_up"}

func newFakeHerdr(t *testing.T) *fakeHerdr {
	return &fakeHerdr{ws: map[string]string{}, panes: map[string]*fakePane{}}
}

func (f *fakeHerdr) add(ws, label, name, kind, cwd string) string {
	f.ws[ws] = label
	pane := ws + ":p1"
	f.panes[pane] = &fakePane{ws: ws, name: name, kind: kind, cwd: cwd}
	return pane
}

func (f *fakeHerdr) nameHolder(name, except string) string {
	for id, p := range f.panes {
		if p.name == name && id != except {
			return id
		}
	}
	return ""
}

func (f *fakeHerdr) handle(method string, params map[string]any) (any, *HerdrError) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method)
	str := func(k string) string { s, _ := params[k].(string); return s }
	taken := func(name, pane string) *HerdrError {
		if h := f.nameHolder(name, pane); h != "" {
			return &HerdrError{Code: "agent_name_taken", Message: fmt.Sprintf("agent name %s is already used; pane_id=%s", name, h)}
		}
		return nil
	}
	agentView := func(id string, p *fakePane) map[string]any {
		status := p.status
		if status == "" {
			status = "idle"
		}
		v := map[string]any{"pane_id": id, "workspace_id": p.ws, "cwd": p.cwd, "agent_status": status, "interactive_ready": true, "state_change_seq": p.seq, "completion_seq": p.completionSeq}
		if p.kind != "" {
			v["agent"] = p.kind
		}
		if p.name != "" {
			v["name"] = p.name
		}
		if p.title != "" {
			v["terminal_title"] = p.title
		}
		if p.log != "" {
			v["agent_session"] = map[string]any{"kind": "id", "value": strings.TrimSuffix(filepath.Base(p.log), ".jsonl")}
		}
		return v
	}
	switch method {
	case "workspace.list", "agent.list", "pane.list":
		hide := f.hidden > 0
		if hide {
			f.hidden--
		}
		out := []map[string]any{}
		if method == "workspace.list" && !hide {
			for id, label := range f.ws {
				out = append(out, map[string]any{"workspace_id": id, "label": label})
			}
		}
		if method != "workspace.list" && !hide {
			for id, p := range f.panes {
				if method == "pane.list" || p.kind != "" {
					out = append(out, agentView(id, p))
				}
			}
		}
		key := map[string]string{"workspace.list": "workspaces", "agent.list": "agents", "pane.list": "panes"}[method]
		if f.afterList != nil {
			f.afterList()
		}
		return map[string]any{key: out}, nil
	case "workspace.create":
		f.next++
		ws := fmt.Sprintf("wN%d", f.next)
		pane := f.add(ws, str("label"), "", "", str("cwd"))
		return map[string]any{"workspace": map[string]any{"workspace_id": ws}, "root_pane": map[string]any{"pane_id": pane}}, nil
	case "workspace.close":
		ws := str("workspace_id")
		delete(f.ws, ws)
		for id, p := range f.panes {
			if p.ws == ws {
				delete(f.panes, id)
			}
		}
		return map[string]any{}, nil
	case "pane.get":
		p := f.panes[str("pane_id")]
		if p == nil {
			return nil, &HerdrError{Code: "pane_not_found"}
		}
		return map[string]any{"pane": agentView(str("pane_id"), p)}, nil
	case "agent.get":
		p := f.panes[str("target")]
		if p == nil || p.kind == "" {
			return nil, &HerdrError{Code: "agent_not_found"}
		}
		return map[string]any{"agent": agentView(str("target"), p)}, nil
	case "pane.read":
		p := f.panes[str("pane_id")]
		if p == nil {
			return nil, &HerdrError{Code: "pane_not_found"}
		}
		if str("source") == "detection" && p.detection != "" {
			return map[string]any{"read": map[string]any{"text": p.detection}}, nil
		}
		if p.sim {
			return map[string]any{"read": map[string]any{"text": p.simScreen()}}, nil
		}
		return map[string]any{"read": map[string]any{"text": p.screen}}, nil
	case "pane.send_keys":
		p := f.panes[str("pane_id")]
		if keys, ok := params["keys"].([]any); p != nil && ok {
			for _, key := range keys {
				p.sentKeys = append(p.sentKeys, fmt.Sprint(key))
			}
		}
		if keys, _ := params["keys"].([]any); p != nil && len(keys) == 1 && keys[0] == "shift+left" && p.unfolded != "" {
			p.screen = p.unfolded
		}
		if keys, _ := params["keys"].([]any); p != nil && p.sim && len(keys) == 1 && keys[0] == "enter" {
			p.enter()
		}
		return map[string]any{}, nil
	case "pane.send_text":
		p := f.panes[str("pane_id")]
		if p == nil {
			return nil, &HerdrError{Code: "pane_not_found"}
		}
		p.writes = append(p.writes, str("text"))
		p.box += strings.TrimSuffix(strings.TrimPrefix(str("text"), "\x1b[200~"), "\x1b[201~")
		return map[string]any{}, nil
	case "agent.prompt":
		p := f.panes[str("target")]
		if p == nil || p.kind == "" {
			return nil, &HerdrError{Code: "agent_not_found"}
		}
		if p.dropPrompts > 0 {
			p.dropPrompts--
			return map[string]any{}, nil
		}
		p.box += str("text")
		p.enter()
		if p.hangUp {
			p.hangUp = false
			return nil, errHangUp
		}
		return map[string]any{}, nil
	case "agent.start":
		// Like the alias path: the agent is up before herdr refuses the name.
		p := f.panes[str("pane_id")]
		p.kind = str("kind")
		if err := taken(str("name"), str("pane_id")); err != nil {
			return nil, err
		}
		p.name = str("name")
		return map[string]any{}, nil
	case "agent.rename":
		if err := taken(str("name"), str("target")); err != nil {
			return nil, err
		}
		f.panes[str("target")].name = str("name")
		return map[string]any{}, nil
	}
	return nil, &HerdrError{Code: "unexpected_method", Message: method}
}

// dial answers one herdr request over a pipe, as the socket does.
func (f *fakeHerdr) dial(context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		var req struct {
			ID     string         `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(server).Decode(&req); err != nil {
			return
		}
		resp := map[string]any{"id": req.ID}
		out, err := f.handle(req.Method, req.Params)
		if err == errHangUp {
			return
		}
		if err != nil {
			resp["error"] = err
		} else {
			resp["result"] = out
		}
		json.NewEncoder(server).Encode(resp)
	}()
	return client, nil
}

func (f *fakeHerdr) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == method {
			n++
		}
	}
	return n
}

func newRouterTestSuper(t *testing.T, f *fakeHerdr) *Super {
	t.Helper()
	dir := t.TempDir()
	h := newHerdr("", "test", dir)
	h.local.conn.dial = f.dial
	db, err := openDB(filepath.Join(dir, "herd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := loadState(db)
	if err != nil {
		t.Fatal(err)
	}
	st.Settings.RouterKind, st.Settings.RouterMachine, st.Settings.RouterAlias = "claude", "", ""
	binCache.Store("claude", "/usr/bin/true") // the agent is "installed"
	srv := &Server{herdr: h, home: dir}
	h.logged = srv.promptLogged
	sp := &Super{s: srv, db: db, dir: dir, bin: "collie", st: st, subs: map[chan struct{}]bool{},
		lastScreen: map[string]screenMark{}, transcripts: map[string]*transcript{}, noAgent: map[string]time.Time{},
		unanswered: map[string]time.Time{}}
	srv.sup = sp
	return sp
}

func launchRouter(t *testing.T, sp *Super) {
	t.Helper()
	sp.ensureRouter(context.Background())
	for until := time.Now().Add(10 * time.Second); ; {
		sp.mu.Lock()
		busy := sp.routerLaunching
		sp.mu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(until) {
			t.Fatal("router launch did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func chatEvents(sp *Super) string {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	var b strings.Builder
	for _, e := range sp.st.Entries {
		b.WriteString(e.Text + "\n")
	}
	return b.String()
}

// The Router was alive but out of sight when the tick looked (its computer
// reconnecting after a hub restart): the launch finds it and starts nothing.
func TestEnsureRouterKeepsRouterBackInSight(t *testing.T) {
	f := newFakeHerdr(t)
	sp := newRouterTestSuper(t, f)
	f.add("wA", routerLabel, routerName, "claude", sp.routerCwd(""))
	sp.mu.Lock()
	sp.saveRouterLaunchLocked(routerLaunch{Alias: "old"}) // how the running one was started
	sp.mu.Unlock()
	launchRouter(t, sp)
	if n := f.count("workspace.create"); n != 0 {
		t.Errorf("started %d new Routers next to the live one", n)
	}
	sp.mu.Lock()
	l := sp.routerLaunchLocked()
	sp.mu.Unlock()
	if l.Alias != "old" {
		t.Errorf("launch record alias = %q, want the running Router's %q", l.Alias, "old")
	}
	if ev := chatEvents(sp); ev != "" {
		t.Errorf("chat got %q, want nothing", ev)
	}
}

// Out of sight until the new one asks for the name: the old Router is kept
// and the one just started is closed, so only one takes events.
func TestEnsureRouterNameTakenByLiveRouter(t *testing.T) {
	f := newFakeHerdr(t)
	sp := newRouterTestSuper(t, f)
	old := f.add("wA", routerLabel, routerName, "claude", sp.routerCwd(""))
	f.hidden = 2 // the launch's own look, then the stale-workspace sweep
	launchRouter(t, sp)
	if n := f.count("workspace.create"); n != 1 {
		t.Fatalf("workspace.create calls = %d, want 1", n)
	}
	if len(f.panes) != 1 || f.panes[old] == nil {
		t.Errorf("panes left = %v, want only the old Router %s", f.panes, old)
	}
	if ev := chatEvents(sp); strings.Contains(ev, "失败") || strings.Contains(ev, "已唤醒") {
		t.Errorf("chat got %q, want nothing", ev)
	}
	live, _ := sp.liveAgents(context.Background())
	if live[routerName].Pane != old {
		t.Errorf("Router pane = %q, want %q", live[routerName].Pane, old)
	}
}

// Another agent (not a Router) has "collie" and a worker has "collie-2": the
// new Router becomes collie-3, and Collie, the sidebar and delivery know it.
func TestEnsureRouterTakesSpareName(t *testing.T) {
	f := newFakeHerdr(t)
	sp := newRouterTestSuper(t, f)
	other := f.add("w1", "mine", routerName, "claude", "/work/elsewhere")
	f.add("w2", "collie-2", "collie-2", "codex", "/work/task")
	f.hidden = 2
	launchRouter(t, sp)
	if ev := chatEvents(sp); !strings.Contains(ev, "已唤醒") {
		t.Fatalf("chat got %q, want the Router up", ev)
	}
	var pane string
	for id, p := range f.panes {
		if p.name == "collie-3" {
			pane = id
		}
	}
	if pane == "" || f.panes[pane].cwd != sp.routerCwd("") {
		t.Fatalf("no Router named collie-3 in the Router's folder: %+v", f.panes)
	}
	if got := sp.routerAgent(); got != "collie-3" {
		t.Errorf("routerAgent() = %q, want collie-3", got)
	}
	if got := sp.herdrTarget(routerName); got != "collie-3" {
		t.Errorf("herdrTarget(routerName) = %q, want collie-3", got)
	}
	live, _ := sp.liveAgents(context.Background())
	if live[routerName].Pane != pane {
		t.Errorf("live Router pane = %q, want %q (not %q)", live[routerName].Pane, pane, other)
	}
	if _, ok := live["collie-2"]; !ok {
		t.Error("the worker collie-2 went missing")
	}
	st := sp.s.state(context.Background())
	for _, a := range st.Agents {
		if want := a.PaneID == pane; a.IsRouter != want {
			t.Errorf("pane %s is_router = %t, want %t", a.PaneID, a.IsRouter, want)
		}
	}

	// A hub restart reads the name back.
	sp2 := &Super{s: sp.s, db: sp.db, st: sp.st}
	if got := sp2.routerAgent(); got != "collie-3" {
		t.Errorf("after restart routerAgent() = %q, want collie-3", got)
	}

	// The next launch asks for "collie" again: a changed setting keeps the
	// running Router's name until then.
	sp.mu.Lock()
	sp.setRouterOn("", "")
	sp.mu.Unlock()
	if got := sp.routerAgent(); got != "collie-3" {
		t.Errorf("after setRouterOn routerAgent() = %q, want collie-3", got)
	}
}

// A dev Collie sharing herdr started a "collie" pane in a temp folder: the hub
// must not adopt it in liveAgents or state, must not close its workspace, and
// must launch its own Router in the real router folder under a spare name.
func TestRouterIgnoresSameNameInOtherFolder(t *testing.T) {
	f := newFakeHerdr(t)
	sp := newRouterTestSuper(t, f)
	dev := f.add("wDev", routerLabel, routerName, "claude", "/tmp/dev-collie/router")

	live, ok := sp.liveAgents(context.Background())
	if !ok {
		t.Fatal("liveAgents failed")
	}
	if _, found := live[routerName]; found {
		t.Fatalf("liveAgents adopted wrong-cwd agent %s as Router", dev)
	}
	st := sp.s.state(context.Background())
	for _, a := range st.Agents {
		if a.IsRouter {
			t.Errorf("state marked wrong-cwd pane %s as is_router", a.PaneID)
		}
	}

	launchRouter(t, sp)
	if f.panes[dev] == nil {
		t.Fatal("closeStaleRouterWorkspaces closed another Collie's live router workspace")
	}
	if got := sp.routerAgent(); got != "collie-2" {
		t.Fatalf("routerAgent() = %q, want collie-2", got)
	}
	live, _ = sp.liveAgents(context.Background())
	got, found := live[routerName]
	if !found || got.Pane == dev || f.panes[got.Pane].cwd != sp.routerCwd("") {
		t.Fatalf("live Router = %+v (found=%t), want new pane in %q", got, found, sp.routerCwd(""))
	}
	st = sp.s.state(context.Background())
	for _, a := range st.Agents {
		if want := a.PaneID == got.Pane; a.IsRouter != want {
			t.Errorf("pane %s is_router = %t, want %t", a.PaneID, a.IsRouter, want)
		}
	}
}

// Router identity checks the computer as well as the folder, and stays unset
// for a remote computer until its router folder has been synced.
func TestByNameChecksRouterMachineAndCwd(t *testing.T) {
	f := newFakeHerdr(t)
	sp := newRouterTestSuper(t, f)
	sp.mu.Lock()
	sp.setRouterOn("macpro", "")
	sp.mu.Unlock()
	router, claude := routerName, "claude"
	dir, wrong := "/Users/me/.collie/router", "/tmp/dev/router"
	list := []Agent{
		{Name: &router, PaneID: "w1:p1", Agent: &claude, Cwd: &dir},
		{Name: &router, PaneID: "w2:p1@other", Agent: &claude, Cwd: &dir},
		{Name: &router, PaneID: "w3:p1@macpro", Agent: &claude, Cwd: &wrong},
		{Name: &router, PaneID: "w4:p1@macpro", Agent: &claude, Cwd: &dir},
	}

	// Before first sync, routerCwd("macpro") is unknown: match nothing.
	if live := sp.byName(list); len(live) != 0 {
		t.Fatalf("before sync live = %+v, want empty", live)
	}

	sp.rsync.mu.Lock()
	sp.rsync.machine, sp.rsync.dir = "macpro", dir
	sp.rsync.mu.Unlock()
	if live := sp.byName(list); live[routerName].Pane != "w4:p1@macpro" {
		t.Fatalf("matched Router = %+v, want w4:p1@macpro", live[routerName])
	}
	if live := sp.byName(list[:3]); len(live) != 0 {
		t.Fatalf("without the real Router, live = %+v, want empty", live)
	}
}

func TestCloseStaleRouterWorkspaces(t *testing.T) {
	f := newFakeHerdr(t)
	sp := newRouterTestSuper(t, f)
	f.add("wOrphan", routerLabel, "", "", "/tmp/elsewhere")
	f.add("wOwn", routerLabel, routerName, "claude", sp.routerCwd(""))
	f.add("wOther", routerLabel, routerName, "claude", "/tmp/dev-collie/router")
	f.add("wShell", "work", "", "", sp.routerCwd(""))

	sp.closeStaleRouterWorkspaces(context.Background())

	if f.ws["wOrphan"] != "" {
		t.Error("orphan router workspace (no live agent) was not closed")
	}
	if f.ws["wOwn"] != "" {
		t.Error("own-folder router workspace was not closed")
	}
	if f.ws["wOther"] == "" {
		t.Error("other-folder live router workspace should have been kept")
	}
	if f.ws["wShell"] == "" {
		t.Error("non-router workspace should have been kept")
	}
}

func TestInboxDraftCompletedWhilePaneWasGoneIsNotSent(t *testing.T) {
	f := newDraftWorkerTest(t)
	f.pane.completionSeq = 5
	id := f.start(t)
	f.sp.tick(t.Context())
	delete(f.herdr.panes, f.worker.PaneID)
	f.sp.tick(t.Context())
	f.finish(t, "Completed draft while gone.")
	f.pane.completionSeq++
	f.herdr.panes[f.worker.PaneID] = f.pane
	f.sp.tick(t.Context()) // restore the pane in done state
	f.sp.tick(t.Context()) // do not replay the completion as an ordinary reply
	f.check(t, id, "pending", "", 0)
	f.restoreState(t)
	if f.worker.DraftTrigger != "" || f.worker.CompletionSeq != f.pane.completionSeq {
		t.Fatalf("persisted worker retained draft %q or old completion %d", f.worker.DraftTrigger, f.worker.CompletionSeq)
	}
}
