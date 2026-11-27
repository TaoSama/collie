package collie

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConvAliasMatching(t *testing.T) {
	sp, h, _ := newExtTestHub(t)
	h.mu.Lock()
	h.sets["fake"] = ExtSetting{ID: "fake", Machine: "devbox"}
	h.mu.Unlock()
	sp.s.aliases.put("devbox", aliasReport{Aliases: []LauncherAlias{
		{Name: "agyn", Kind: "agy"},
		{Name: "codex62", Kind: "codex"},
		{Name: "ccf", Kind: "claude"},
	}}, nil)
	sp.s.aliases.put("macmini", aliasReport{Aliases: []LauncherAlias{
		{Name: "maconly", Kind: "claude"},
	}}, nil)

	man := *h.manifest("fake")
	man.Defaults.Handler = handlerAgent

	cases := []struct {
		name      string
		handler   string
		text      string
		mention   bool
		wantAlias string
		wantHas   string
		wantNo    string
	}{
		{
			name: "hit", handler: handlerAgent, text: "/ai agyn 查一下",
			wantAlias: "agyn", wantHas: "] 查一下", wantNo: "agyn",
		},
		{
			name: "hit colon", handler: handlerAgent, text: "/ai codex62：看看代码",
			wantAlias: "codex62", wantHas: "] 看看代码", wantNo: "codex62",
		},
		{
			name: "hit newline", handler: handlerAgent, text: "/ai agyn\n第二行",
			wantAlias: "agyn", wantHas: "] 第二行", wantNo: "agyn",
		},
		{
			name: "hit bare alias", handler: handlerAgent, text: "/ai agyn",
			wantAlias: "agyn", wantHas: "只发了触发词", wantNo: "agyn",
		},
		{
			name: "case insensitive upper", handler: handlerAgent, text: "/ai AGYN 查一下",
			wantAlias: "agyn", wantHas: "] 查一下", wantNo: "AGYN",
		},
		{
			name: "case insensitive mixed", handler: handlerAgent, text: "/AI Codex62 看看",
			wantAlias: "codex62", wantHas: "] 看看", wantNo: "Codex62",
		},
		{
			name: "miss unknown word", handler: handlerAgent, text: "/ai unknown 查一下",
			wantAlias: "", wantHas: "] unknown 查一下",
		},
		{
			name: "miss prefix only", handler: handlerAgent, text: "/ai agyn2 查一下",
			wantAlias: "", wantHas: "] agyn2 查一下",
		},
		{
			name: "miss chinese", handler: handlerAgent, text: "/ai 帮我总结",
			wantAlias: "", wantHas: "] 帮我总结",
		},
		{
			name: "miss other machine alias", handler: handlerAgent, text: "/ai maconly 查一下",
			wantAlias: "", wantHas: "] maconly 查一下",
		},
		{
			name: "router mode unchanged", handler: handlerCollie, text: "/ai agyn 查一下",
			wantAlias: "", wantHas: "] agyn 查一下",
		},
		{
			name: "self mention hit", handler: handlerAgent, text: "agyn 查一下", mention: true,
			wantAlias: "agyn", wantHas: "] 查一下", wantNo: "agyn",
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chat := fmt.Sprintf("c%d", i)
			m := man
			m.Defaults.Handler = tc.handler
			_, trig, err := h.fileIncoming(&m, incomingMsg{
				Chat: chat, ChatName: "测试群", ChatKind: chatGroup,
				ID: fmt.Sprintf("m%d", i), Mine: true, Mention: tc.mention,
				Text: tc.text, At: time.Now(), Live: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if trig == nil {
				t.Fatal("expected trigger")
			}
			if trig.Alias != tc.wantAlias {
				t.Errorf("Alias = %q, want %q", trig.Alias, tc.wantAlias)
			}
			if !strings.Contains(trig.Text, tc.wantHas) {
				t.Errorf("Text = %q, want substring %q", trig.Text, tc.wantHas)
			}
			if tc.wantNo != "" && strings.Contains(trig.Text, tc.wantNo) {
				t.Errorf("Text = %q, must not contain %q", trig.Text, tc.wantNo)
			}
		})
	}
}

// fakeAliasNode simulates a remote node's herdr for alias-launched workers.
type fakeAliasNode struct {
	mu      sync.Mutex
	next    int
	panes   map[string]*fakeAliasPane
	aliases map[string]string // alias name -> herdr kind
	typed   []string          // every command line typed into a pane
}

type fakeAliasPane struct {
	ws, name, kind, status, cwd, pending string
}

func (n *fakeAliasNode) dial(context.Context) (net.Conn, error) {
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
		out, herr := n.handle(req.Method, req.Params)
		resp := map[string]any{"id": req.ID}
		if herr != nil {
			resp["error"] = herr
		} else {
			resp["result"] = out
		}
		json.NewEncoder(server).Encode(resp)
	}()
	return client, nil
}

func (n *fakeAliasNode) handle(method string, params map[string]any) (any, *HerdrError) {
	n.mu.Lock()
	defer n.mu.Unlock()
	str := func(k string) string { s, _ := params[k].(string); return s }
	switch method {
	case "workspace.create":
		n.next++
		ws := fmt.Sprintf("w%d", n.next)
		pane := ws + ":p1"
		n.panes[pane] = &fakeAliasPane{ws: ws, cwd: str("cwd")}
		return map[string]any{
			"workspace": map[string]any{"workspace_id": ws},
			"root_pane": map[string]any{"pane_id": pane},
		}, nil
	case "pane.read":
		return map[string]any{"read": map[string]any{"text": "$ "}}, nil
	case "pane.send_text":
		if p := n.panes[str("pane_id")]; p != nil {
			p.pending += str("text")
		}
		return map[string]any{}, nil
	case "pane.send_keys":
		if p := n.panes[str("pane_id")]; p != nil {
			line := strings.TrimSpace(p.pending)
			p.pending = ""
			if line != "" {
				n.typed = append(n.typed, line)
				word, _ := splitFirstWord(line)
				if kind, ok := n.aliases[word]; ok {
					p.kind, p.status = kind, "idle"
				}
			}
		}
		return map[string]any{}, nil
	case "pane.get":
		p := n.panes[str("pane_id")]
		if p == nil {
			return nil, &HerdrError{Code: "pane_not_found"}
		}
		v := map[string]any{"pane_id": str("pane_id"), "workspace_id": p.ws, "cwd": p.cwd, "agent_status": p.status, "interactive_ready": true}
		if p.kind != "" {
			v["agent"] = p.kind
		}
		if p.name != "" {
			v["name"] = p.name
		}
		return map[string]any{"pane": v}, nil
	case "agent.rename":
		if p := n.panes[str("target")]; p != nil {
			p.name = str("name")
		}
		return map[string]any{}, nil
	case "agent.start":
		if p := n.panes[str("pane_id")]; p != nil {
			p.kind, p.name, p.status = str("kind"), str("name"), "idle"
		}
		return map[string]any{}, nil
	case "workspace.close":
		return map[string]any{}, nil
	}
	return nil, &HerdrError{Code: "unexpected_method", Message: method}
}

func (n *fakeAliasNode) liveMap() map[string]liveAgent {
	n.mu.Lock()
	defer n.mu.Unlock()
	m := map[string]liveAgent{}
	for id, p := range n.panes {
		if p.name != "" {
			m[p.name] = liveAgent{Pane: id + "@devbox", Status: p.status, Ready: true, Kind: p.kind}
		}
	}
	return m
}

func (n *fakeAliasNode) lastTyped() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.typed) == 0 {
		return ""
	}
	return n.typed[len(n.typed)-1]
}

func TestConvAliasSwitchAndReuse(t *testing.T) {
	sp, h, dir := newExtTestHub(t)
	node := &fakeAliasNode{
		panes:   map[string]*fakeAliasPane{},
		aliases: map[string]string{"agyn": "agy", "codex62": "codex"},
	}
	sp.s.herdr.mu.Lock()
	sp.s.herdr.remote["devbox"] = &Machine{
		ID: "devbox", Name: "devbox", Home: dir, Online: true,
		Agents: []string{"claude", "codex", "agy"},
		conn:   &herdrConn{dial: node.dial},
	}
	sp.s.herdr.mu.Unlock()

	h.mu.Lock()
	h.sets["fake"] = ExtSetting{ID: "fake", Machine: "devbox"}
	h.mu.Unlock()
	sp.s.aliases.put("devbox", aliasReport{Aliases: []LauncherAlias{
		{Name: "agyn", Kind: "agy"},
		{Name: "codex62", Kind: "codex"},
	}}, nil)

	man := *h.manifest("fake")
	man.Defaults.Handler = handlerAgent
	convKey := convID("fake", "c1")

	send := func(id, text string) *Trigger {
		t.Helper()
		_, trig, err := h.fileIncoming(&man, incomingMsg{
			Chat: "c1", ChatName: "dev", ChatKind: chatGroup,
			ID: id, Mine: true, Text: text, At: time.Now(), Live: true,
		})
		if err != nil || trig == nil {
			t.Fatalf("fileIncoming(%s): %+v %v", id, trig, err)
		}
		return trig
	}

	route := func(trig *Trigger) bool {
		sp.mu.Lock()
		defer sp.mu.Unlock()
		return sp.routeExtensionLocked(trig, node.liveMap(), nil, false)
	}

	waitIdle := func() {
		waitFor(t, "worker launch to settle", func() bool {
			_, busy := h.starting.Load(convKey)
			return !busy
		})
	}

	// 1. First request with alias agyn launches an agy worker through agyn.
	t1 := send("m1", "/ai agyn 问题1")
	if route(t1) {
		t.Fatal("first request should wait for worker launch")
	}
	waitIdle()
	if !route(t1) || len(t1.Targets) != 1 {
		t.Fatalf("t1 targets = %v, want launched worker", t1.Targets)
	}
	w1Name := t1.Targets[0]
	sp.mu.Lock()
	w1 := sp.st.Workers[w1Name]
	sp.mu.Unlock()
	if w1 == nil || w1.Kind != "agy" || workerAlias(w1) != "agyn" {
		t.Fatalf("w1 = %+v, want kind=agy alias=agyn", w1)
	}
	if cmd := node.lastTyped(); !strings.HasPrefix(cmd, "agyn ") && cmd != "agyn" {
		t.Fatalf("w1 launch command = %q, want agyn ...", cmd)
	}

	// 2. Same alias (including different case) reuses existing live worker.
	t2 := send("m2", "/ai AGYN 问题2")
	if !route(t2) || !slices.Equal(t2.Targets, []string{w1Name}) {
		t.Fatalf("same alias should reuse %s, got %v", w1Name, t2.Targets)
	}
	if strings.Contains(t2.Text, "AGYN") {
		t.Fatalf("alias word was not stripped: %q", t2.Text)
	}

	// 3. No alias also reuses existing live worker.
	t3 := send("m3", "/ai 问题3")
	if !route(t3) || !slices.Equal(t3.Targets, []string{w1Name}) {
		t.Fatalf("no alias should reuse %s, got %v", w1Name, t3.Targets)
	}

	// 4. Different alias while w1 is live launches a new dedicated worker.
	t4 := send("m4", "/ai codex62 问题4")
	if route(t4) {
		t.Fatal("different alias must launch a new worker instead of reusing w1")
	}
	waitIdle()
	if !route(t4) || len(t4.Targets) != 1 {
		t.Fatalf("t4 targets = %v, want new worker", t4.Targets)
	}
	w2Name := t4.Targets[0]
	if w2Name == w1Name {
		t.Fatalf("switched worker kept old name %s", w1Name)
	}
	sp.mu.Lock()
	w2 := sp.st.Workers[w2Name]
	subs := slices.Clone(sp.st.Threads["ext:fake:c1"].Subscribers)
	sp.mu.Unlock()
	if w2 == nil || w2.Kind != "codex" || workerAlias(w2) != "codex62" || w2.Session != "" {
		t.Fatalf("w2 = %+v, want fresh kind=codex alias=codex62", w2)
	}
	if slices.Contains(subs, w1Name) || !slices.Contains(subs, w2Name) {
		t.Fatalf("thread subscribers = %v, want only newest worker %s", subs, w2Name)
	}
	if cmd := node.lastTyped(); !strings.HasPrefix(cmd, "codex62 ") && cmd != "codex62" {
		t.Fatalf("w2 launch command = %q, want codex62 ...", cmd)
	}

	// 5. Subsequent request without alias follows the newest worker w2.
	t5 := send("m5", "/ai 问题5")
	if !route(t5) || !slices.Equal(t5.Targets, []string{w2Name}) {
		t.Fatalf("follow-up should reuse newest %s, got %v", w2Name, t5.Targets)
	}

	// 6. Ended worker + same alias resumes its saved session.
	sp.mu.Lock()
	w2.Ended, w2.Status, w2.Session = true, "gone", "sess-codex-1"
	sp.mu.Unlock()
	node.mu.Lock()
	for id, p := range node.panes {
		if p.name == w2Name {
			delete(node.panes, id)
		}
	}
	node.mu.Unlock()

	t6 := send("m6", "/ai codex62 问题6")
	if route(t6) {
		t.Fatal("ended worker should launch/resume first")
	}
	waitIdle()
	if !route(t6) || len(t6.Targets) != 1 {
		t.Fatalf("t6 targets = %v, want resumed worker", t6.Targets)
	}
	if cmd := node.lastTyped(); !strings.Contains(cmd, "codex62 resume sess-codex-1") {
		t.Fatalf("resumed command = %q, want codex62 resume sess-codex-1", cmd)
	}
	w3Name := t6.Targets[0]

	// 7. Ended worker + different alias starts a fresh session (no resume).
	sp.mu.Lock()
	sp.st.Workers[w3Name].Ended = true
	sp.st.Workers[w3Name].Status = "gone"
	sp.st.Workers[w3Name].Session = "sess-codex-2"
	sp.mu.Unlock()
	node.mu.Lock()
	for id, p := range node.panes {
		if p.name == w3Name {
			delete(node.panes, id)
		}
	}
	node.mu.Unlock()

	t7 := send("m7", "/ai agyn 问题7")
	if route(t7) {
		t.Fatal("ended worker with different alias should launch fresh")
	}
	waitIdle()
	if !route(t7) || len(t7.Targets) != 1 {
		t.Fatalf("t7 targets = %v, want fresh agyn worker", t7.Targets)
	}
	if cmd := node.lastTyped(); !strings.HasPrefix(cmd, "agyn") || strings.Contains(cmd, "sess-codex-2") {
		t.Fatalf("switched command = %q, want fresh agyn without resuming sess-codex-2", cmd)
	}
}
