package collie

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

func fakeManifest(id, mode string) *ExtManifest {
	m := &ExtManifest{ManifestVersion: 1, ID: id, Name: id, Version: "1.0.0"}
	m.Engines.CollieProtocol = extproto.Protocol
	m.Runtime.Builtin = "fake"
	m.Contributes.Connector.ID = id
	m.Contributes.Connector.Login = "qr"
	if mode == "actions" {
		m.Contributes.Connector.Capabilities = []string{"status", "login", "actions"}
		m.Contributes.Connector.Actions = []ExtAction{
			{Name: "echo", Description: "echo", Readonly: true, Untrusted: true, Required: []string{"text"},
				Params: map[string]ExtConfigProp{"text": {Type: "string"}, "n": {Type: "integer"}, "mode": {Type: "string", Enum: []string{"a", "b"}}}},
			{Name: "write", Description: "changes something"},
			{Name: "sleep", Description: "slow", Readonly: true, Slow: true, TimeoutS: 1},
		}
		m.Defaults.Machine = machineAsk
	} else if mode == "receive" {
		m.Contributes.Connector.Capabilities = []string{"status", "login", "receive", "send"}
		m.Defaults.Handler, m.Defaults.Conversations = "collie", "owner"
	} else {
		m.Contributes.Connector.Capabilities = []string{"status", "login", "archive", "actions"}
		m.Contributes.Connector.SendAction = "larkx_send"
		m.Defaults.Handler = "collie"
	}
	m.Configuration = &ExtConfig{Type: "object", Properties: map[string]ExtConfigProp{"mode": {Type: "string", Default: mode}}}
	return m
}

func newExtTestHub(t *testing.T) (*Super, *extHost, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("COLLIE_HOME", dir)
	db, err := openDB(filepath.Join(dir, "herd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := loadState(db)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{herdr: newHerdr("", "test", dir), home: dir}
	sp := &Super{s: srv, db: db, dir: dir, bin: "collie", st: st, subs: map[chan struct{}]bool{}}
	srv.sup = sp
	h := newExtHost(srv, sp, dir)
	h.cat = map[string]*ExtManifest{"fake": fakeManifest("fake", "archive"), "fakew": fakeManifest("fakew", "receive"), "fakea": fakeManifest("fakea", "actions")}
	srv.ext = h
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		for _, id := range []string{"fake", "fakew", "fakea", "loc", "tool"} {
			h.configure(id, func(x *ExtSetting) { x.Enabled = false })
		}
		cancel()
	})
	go h.Run(ctx)
	waitFor(t, "the host running", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.ctx != nil })
	return sp, h, dir
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for until := time.Now().Add(10 * time.Second); time.Now().Before(until); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func feed(t *testing.T, dir, ext string, msgs ...fakeMsg) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "ext", ext, "data", "in.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, m := range msgs {
		if m.At == "" {
			m.At = time.Now().Format(time.RFC3339)
		}
		b, _ := json.Marshal(m)
		f.Write(append(b, '\n'))
	}
}

func sentLines(dir, ext string) []map[string]any {
	f, err := os.Open(filepath.Join(dir, "ext", ext, "data", "sent.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		json.Unmarshal(sc.Bytes(), &m)
		out = append(out, m)
	}
	return out
}

func extTriggers(sp *Super, source string) []*Trigger {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	var out []*Trigger
	for _, t := range sp.st.Triggers {
		if t.Source == source {
			out = append(out, t)
		}
	}
	return out
}

func startExt(t *testing.T, h *extHost, id string) *extRun {
	t.Helper()
	if _, err := h.configure(id, func(x *ExtSetting) { x.Enabled = true }); err != nil {
		t.Fatal(err)
	}
	var r *extRun
	waitFor(t, id+" running", func() bool { r = h.run(id); return r != nil && r.running() })
	return r
}

func login(t *testing.T, r *extRun) {
	t.Helper()
	ctx := context.Background()
	var st extproto.LoginStep
	if err := r.call(ctx, extproto.MLoginStep, map[string]any{}, &st); err != nil || st.Kind != "qr" || st.URL == "" {
		t.Fatalf("first step: %+v %v", st, err)
	}
	if err := r.call(ctx, extproto.MLoginStep, map[string]any{"step": st.Step}, &st); err != nil || !st.Done {
		t.Fatalf("second step: %+v %v", st, err)
	}
}

// The whole way through an archive extension (larkx's shape): sign-in, a
// closed chat that only shows, an opened one where only your /ai triggers,
// the answer with its footer, and that answer not triggering again.
func TestExtArchiveFlow(t *testing.T) {
	sp, h, dir := newExtTestHub(t)
	r := startExt(t, h, "fake")
	r.refreshStatus(context.Background())
	if v := h.view("fake"); v.Status == nil || v.Status.State != extproto.StateNotSignedIn {
		t.Fatalf("status before sign-in: %+v", v.Status)
	}
	login(t, r)
	r.refreshStatus(context.Background())
	if v := h.view("fake"); v.Status.State != extproto.StateOK {
		t.Fatalf("status after sign-in: %+v", v.Status)
	}

	feed(t, dir, "fake", fakeMsg{Chat: "c1", ChatName: "产品群", ID: "m1", Sender: "u2", SenderName: "张三", Text: "/ai 早"})
	waitFor(t, "the chat in the inbox", func() bool { _, ok := h.store.conv("fake:c1"); return ok })
	if c, _ := h.store.conv("fake:c1"); c.On || c.Preview == "" || c.Unread != 1 {
		t.Fatalf("a new chat is off, with a preview: %+v", c)
	}
	if len(extTriggers(sp, "ext:fake")) != 0 || len(h.store.msgs("fake:c1", time.Time{})) != 0 {
		t.Fatal("a closed chat must not trigger or keep its messages")
	}

	h.store.update("fake:c1", func(c *InboxConv) { c.On = true; c.Handed = time.Now().Add(-time.Minute) })
	feed(t, dir, "fake",
		fakeMsg{Chat: "c1", ChatName: "产品群", ID: "m2", Sender: "u2", SenderName: "张三", Text: "/ai 帮我写周报"},
		fakeMsg{Chat: "c1", ChatName: "产品群", ID: "m3", Sender: "me", SenderName: "我", Mine: true, Text: "/aide 不算"},
		fakeMsg{Chat: "c1", ChatName: "产品群", ID: "m4", Sender: "me", SenderName: "我", Mine: true, Text: "/AI：总结一下"})
	waitFor(t, "a trigger", func() bool { return len(extTriggers(sp, "ext:fake")) == 1 })
	time.Sleep(200 * time.Millisecond)
	ts := extTriggers(sp, "ext:fake")
	if len(ts) != 1 || ts[0].Thread != "ext:fake:c1" || ts[0].Ref != "m4" || !strings.HasPrefix(ts[0].Text, "[fake · 产品群 · 你") ||
		!strings.Contains(ts[0].Text, "总结一下") || !strings.Contains(ts[0].Text, "张三") {
		t.Fatalf("only my /AI triggers, with the chat as context: %+v", ts)
	}

	o, err := h.sendOut(ts[0].Thread, "今天定了三件事", extproto.KindAnswer, runInfo{Agent: "Claude Code", Model: "Opus 5.5"}, ts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the answer sent", func() bool { x := h.store.out(o.Ref); return x != nil && x.State == extproto.DeliverySent })
	sent := sentLines(dir, "fake")
	if len(sent) != 1 || sent[0]["reply_to"] != "m4" || !strings.Contains(sent[0]["text"].(string), "今天定了三件事\n\n🤖 AI 代答，仅供参考 · Claude Code · Opus 5.5 · ") {
		t.Fatalf("the answer goes under the request with the footer: %+v", sent)
	}
	if x := h.store.out(o.Ref); x.MessageID != "m-1" {
		t.Fatalf("message id read from the action's answer: %+v", x)
	}
	// Sending the same ref again does nothing: the extension runs it once.
	h.trySend(r, h.store.out(o.Ref))
	if n := len(sentLines(dir, "fake")); n != 1 {
		t.Fatalf("one ref went out %d times", n)
	}
	// The answer comes back through the archive as my own message: no new trigger.
	waitFor(t, "the answer back through the archive", func() bool { return h.store.kv("ext_cursor:fake") == "5" })
	if h.store.hasMsg("fake:c1", "m-1") || !h.store.hasMsg("fake:c1", "out:"+o.Ref) {
		t.Fatal("the answer shows once, as Collie's send, not again as a message")
	}
	if n := len(extTriggers(sp, "ext:fake")); n != 1 {
		t.Fatalf("Collie's own answer triggered again: %d triggers", n)
	}
}

// Your own trigger word works in a conversation that is off: the switch
// is about other people's messages.
func TestExtOwnTriggerSwitchOff(t *testing.T) {
	sp, h, dir := newExtTestHub(t)
	r := startExt(t, h, "fake")
	login(t, r)
	feed(t, dir, "fake",
		fakeMsg{Chat: "c1", ChatName: "群", ID: "m1", Sender: "u2", SenderName: "张三", Text: "/ai 别人的"},
		fakeMsg{Chat: "c1", ChatName: "群", ID: "m2", Sender: "me", SenderName: "我", Mine: true, Text: "随便说说"},
		fakeMsg{Chat: "c1", ChatName: "群", ID: "m3", Sender: "me", SenderName: "我", Mine: true, Text: "/ai 你好"})
	waitFor(t, "my trigger", func() bool { return len(extTriggers(sp, "ext:fake")) == 1 })
	time.Sleep(200 * time.Millisecond)
	if ts := extTriggers(sp, "ext:fake"); len(ts) != 1 || ts[0].Ref != "m3" || !strings.Contains(ts[0].Text, "你好") {
		t.Fatalf("only my /ai: %+v", ts)
	}
	if c, _ := h.store.conv("fake:c1"); c.On {
		t.Fatal("the switch stays as it was")
	}
	if h.store.hasMsg("fake:c1", "m1") || h.store.hasMsg("fake:c1", "m2") || !h.store.hasMsg("fake:c1", "m3") {
		t.Fatal("an off conversation keeps only the request")
	}
}

// A conversation's agent runs where its extension is set to run, whatever
// 设置 says for workers; an extension on the hub gives none (the hub runs no
// agents).
func TestConvMachine(t *testing.T) {
	sp, h, _ := newExtTestHub(t)
	sp.mu.Lock()
	sp.st.Settings.WorkerMachine, sp.st.Settings.RouterMachine = "macmini", "macpro"
	sp.mu.Unlock()
	h.mu.Lock()
	h.sets["fake"] = ExtSetting{ID: "fake", Machine: "devbox"}
	h.sets["fakew"] = ExtSetting{ID: "fakew"}
	h.mu.Unlock()
	if got := h.convMachine("fake"); got != "devbox" {
		t.Errorf("the extension's computer: %q", got)
	}
	if got := h.convMachine("fakew"); got != "" {
		t.Errorf("an extension on the hub: %q, want none", got)
	}
}

// A receive extension (WeChat's shape): the bound person's chat is on from
// the start and needs no trigger word; a deferred send reports later.
func TestExtReceiveFlow(t *testing.T) {
	sp, h, dir := newExtTestHub(t)
	r := startExt(t, h, "fakew")
	login(t, r)
	feed(t, dir, "fakew", fakeMsg{Chat: "u1", ChatName: "我", ID: "w1", Sender: "u1", SenderName: "我", Mine: true, Text: "看看构建"})
	waitFor(t, "a trigger", func() bool { return len(extTriggers(sp, "ext:fakew")) == 1 })
	c, _ := h.store.conv("fakew:u1")
	if !c.On || !c.Owner {
		t.Fatalf("the bound person's chat is on: %+v", c)
	}
	home, ok := h.homeConv("fakew")
	if !ok || home.ID != c.ID {
		t.Fatalf("--to fakew goes to that chat: %+v %v", home, ok)
	}
	o, err := h.sendOut(c.thread(), "DEFER 构建通过", extproto.KindAnswer, runInfo{}, "")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "deferred then sent", func() bool {
		x := h.store.out(o.Ref)
		return x != nil && x.State == extproto.DeliverySent && x.MessageID == "late"
	})
}

// Turned off and on again, the program restarts and keeps its sign-in
// (secrets live on its computer); off for good, it stops.
func TestExtRestartKeepsSignIn(t *testing.T) {
	_, h, dir := newExtTestHub(t)
	r := startExt(t, h, "fake")
	login(t, r)
	pid := h.view("fake").Proc.PID
	h.configure("fake", func(x *ExtSetting) { x.Enabled = false })
	r = startExt(t, h, "fake")
	if p := h.view("fake").Proc.PID; p == pid || p == 0 {
		t.Fatalf("a new process: %d then %d", pid, p)
	}
	waitFor(t, "signed in again", func() bool {
		r.refreshStatus(context.Background())
		v := h.view("fake")
		return v.Status != nil && v.Status.State == extproto.StateOK
	})
	b, err := os.ReadFile(filepath.Join(dir, "ext", "fake", "secrets.json"))
	if err != nil || !strings.Contains(string(b), `"fake"`) {
		t.Fatalf("secrets.json: %s %v", b, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "ext", "fake", "secrets.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("secrets.json is %v", fi.Mode().Perm())
	}
}

func TestAfterTrigger(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"/ai 总结一下", "总结一下", true}, {"  /AI：查查", "查查", true}, {"/ai", "", true}, {"/ai\n第二行", "第二行", true},
		{"/aide 不算", "", false}, {"说 /ai", "", false}, {"/a", "", false},
	} {
		got, ok := afterTrigger(c.in, "/ai")
		if got != c.want || ok != c.ok {
			t.Errorf("afterTrigger(%q) = %q %v, want %q %v", c.in, got, ok, c.want, c.ok)
		}
	}
	if triggerWord("") != "/ai" || triggerWord("有 空格") != "/ai" || triggerWord("/agent") != "/agent" {
		t.Error("triggerWord")
	}
}

func TestFooter(t *testing.T) {
	got := renderFooter(defaultFooter, map[string]string{"agent": "Codex", "model": "GPT-5", "elapsed": "1分12秒"})
	if got != "🤖 AI 代答，仅供参考 · Codex · GPT-5 · 1分12秒" {
		t.Errorf("footer: %q", got)
	}
	if renderFooter("{agent} {nope}", map[string]string{"agent": "A"}) != "A {nope}" {
		t.Error("unknown names stay")
	}
	if footerPrefix(defaultFooter) != "🤖 AI 代答，仅供参考 · " || footerPrefix("{agent}") != "" {
		t.Errorf("prefix %q", footerPrefix(defaultFooter))
	}
	for d, want := range map[time.Duration]string{5 * time.Second: "5秒", 72 * time.Second: "1分12秒", 2 * time.Minute: "2分", 3700 * time.Second: "1小时1分"} {
		if got := elapsedText(d); got != want {
			t.Errorf("elapsed %v = %q, want %q", d, got, want)
		}
	}
	for id, want := range map[string]string{
		"claude-opus-5-5": "Opus 5.5", "claude-sonnet-5-5[1m]": "Sonnet 5.5", "claude-haiku-5-5-20261101": "Haiku 5.5",
		"opus": "Opus", "gemini-4-neon-high": "Gemini 4 Neon High", "gpt-6.2-astra": "GPT-6.2 Astra",
		"gpt-5.1-codex": "GPT-5.1 Codex", "my-local-model": "my-local-model", "": "",
	} {
		if got := modelLabel(id); got != want {
			t.Errorf("modelLabel(%q) = %q, want %q", id, got, want)
		}
	}
	for kind, want := range map[string]string{"claude": "Claude Code", "codex": "Codex", "agy": "Antigravity"} {
		if got := agentLabel(kind); got != want {
			t.Errorf("agentLabel(%q) = %q", kind, got)
		}
	}
	if ext, conv, ok := channelOf("ext:larkx:oc_1"); !ok || ext != "larkx" || conv != "larkx:oc_1" {
		t.Error("channelOf")
	}
	if _, _, ok := channelOf("chat:main"); ok {
		t.Error("chat:main is not an extension chat")
	}
}

// Every manifest in extensions/ passes the rules, and its directory is its id.
func TestCatalog(t *testing.T) {
	cat, err := extCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range cat {
		ids = append(ids, m.ID)
	}
	if !strings.Contains(strings.Join(ids, ","), "larkx") {
		t.Fatalf("catalog: %v", ids)
	}
}

func TestInstallPinned(t *testing.T) {
	files := map[string]string{"bin/x": "#!/bin/sh\necho hi\n", "lib/a.txt": "a"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[strings.TrimPrefix(r.URL.Path, "/pkg/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, b)
	}))
	defer srv.Close()
	m := &ExtManifest{ID: "pk", Version: "1.0.0"}
	m.Runtime.Command = json.RawMessage(`"bin/x"`)
	m.Source = &struct {
		Base  string            `json:"base"`
		Files map[string]string `json:"files"`
	}{Base: srv.URL + "/pkg/", Files: map[string]string{
		"bin/x":     "4d8a9e2d79cf9e5c3f5d1e61fd5c3f4b6a4e1ec3f7d0b7c0b51b3a1d0a2b7e25", // wrong on purpose
		"lib/a.txt": "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb",
	}}
	home := t.TempDir()
	if err := ensureInstalled(context.Background(), home, m); err == nil || !strings.Contains(err.Error(), "bin/x") {
		t.Fatalf("a wrong hash must fail: %v", err)
	}
	if _, err := os.Stat(extPackageDir(home, m)); !os.IsNotExist(err) {
		t.Fatal("a failed install leaves nothing")
	}
	m.Source.Files["bin/x"] = "e8b7a3cd4b0d76c22b5a3b0c8a8b62e7d5c3f8f8c1b1bb3a8a4e2fc0b0e0f0f0"
	sum := sha256Hex([]byte(files["bin/x"]))
	m.Source.Files["bin/x"] = sum
	if err := ensureInstalled(context.Background(), home, m); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(extPackageDir(home, m), "bin", "x"))
	if err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("the command is installed executable: %v %v", fi, err)
	}
}

// The real larkx package with its fake Feishu (no network, no account):
// COLLIE_LARKX_UPSTREAM is a folder with LarkAgentX's larkx/ at the pinned
// commit, COLLIE_LARKX_DEPS one with requirements.txt installed. It checks
// the pinned hashes and bundled files, the handshake, sign-in, received
// messages and a send whose footer goes apart.
func TestLarkxFake(t *testing.T) {
	upstream, deps := os.Getenv("COLLIE_LARKX_UPSTREAM"), os.Getenv("COLLIE_LARKX_DEPS")
	if upstream == "" || deps == "" {
		t.Skip("COLLIE_LARKX_UPSTREAM or COLLIE_LARKX_DEPS not set")
	}
	srv := httptest.NewServer(http.StripPrefix("/up/", http.FileServer(http.Dir(upstream))))
	defer srv.Close()
	cat, err := extCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var real *ExtManifest
	for _, m := range cat {
		if m.ID == "larkx" {
			cp := *m
			src := *m.Source
			src.Base = srv.URL + "/up/"
			cp.Source = &src
			real = &cp
		}
	}
	extEnvKeep = append(extEnvKeep, "COLLIE_LARKX_FAKE", "COLLIE_LARKX_DEPS")
	t.Setenv("COLLIE_LARKX_FAKE", "1")
	sp, h, dir := newExtTestHub(t)
	h.cat["larkx"] = real
	r := startExt(t, h, "larkx")
	ctx := context.Background()
	var st extproto.LoginStep
	for i := 0; i < 5 && !st.Done; i++ {
		args := map[string]any{"contribution": "larkx", "step": st.Step}
		if err := r.call(ctx, extproto.MLoginStep, args, &st); err != nil {
			t.Fatalf("login step %d: %v", i, err)
		}
		if !st.Done && (st.Kind != "qr" || st.URL == "") {
			t.Fatalf("a QR to scan: %+v", st)
		}
	}
	if !st.Done {
		t.Fatal("the fake signs in after two polls")
	}
	waitFor(t, "signed in", func() bool {
		r.refreshStatus(ctx)
		v := h.view("larkx")
		return v.Status != nil && v.Status.State == extproto.StateOK
	})
	waitFor(t, "its chats in the inbox", func() bool { return len(h.store.list("larkx")) >= 3 })
	waitFor(t, "your own /ai, taken", func() bool { return len(extTriggers(sp, "ext:larkx")) == 1 })
	if ts := extTriggers(sp, "ext:larkx"); ts[0].Ref != "m3" {
		t.Fatalf("only your own /ai triggers, in any conversation: %+v", ts)
	}
	waitFor(t, "the request marked as being worked on", func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "ext", "larkx", "data", "fake-reactions.jsonl"))
		return strings.Contains(string(b), `"message_id": "m3", "key": "OnIt"`)
	})
	waitFor(t, "rich inbox preview", func() bool { c, _ := h.store.conv("larkx:c_inbox"); return c.Preview != "" })
	conv, _ := h.store.conv("larkx:c_inbox")
	if conv.On || conv.Preview != "值班助手: [卡片] 数据库告警已恢复" || len(h.store.msgs(conv.ID, time.Time{})) != 0 {
		t.Fatalf("closed chat stores just its preview: %+v", conv)
	}
	h.store.update(conv.ID, func(c *InboxConv) { c.Preview = "值班助手: [卡片]" })
	reply := httptest.NewRecorder()
	sp.s.inboxConv(reply, httptest.NewRequest("GET", "/api/inbox/conv?id=larkx:c_inbox", nil))
	var detail struct{ Msgs []InboxMsg }
	if err := json.Unmarshal(reply.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Msgs) != 5 || detail.Msgs[4].View == nil || detail.Msgs[4].View.Title != "数据库告警已恢复" {
		t.Fatalf("history of a closed chat: %+v", detail.Msgs)
	}
	if upgraded, _ := h.store.conv(conv.ID); upgraded.Preview != conv.Preview {
		t.Fatalf("history should repair the old card placeholder: %q", upgraded.Preview)
	}
	for i, kind := range []string{"image/png", "application/octet-stream"} {
		path := detail.Msgs[i+1].View.Nodes[0].URL
		if !strings.HasPrefix(path, "/api/inbox/media?") {
			t.Fatalf("media URL does not go through hub: %q", path)
		}
		media := httptest.NewRecorder()
		sp.s.inboxMedia(media, httptest.NewRequest("GET", path, nil))
		if media.Code != 200 || media.Header().Get("Content-Type") != kind || media.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("media %d: %d %v", i, media.Code, media.Header())
		}
	}

	// Read state: Feishu's counts stand over Collie's;
	// what you read here stays read, and is marked read in Feishu once
	// read-back is on (a switch of Collie's: the program keeps running).
	waitFor(t, "@-mentioned in the group", func() bool { c, _ := h.store.conv("larkx:c_group"); return c.AtMe && c.Unread == 1 })
	r.syncRead(ctx)
	dm, _ := h.store.conv("larkx:c_dm")
	grp, _ := h.store.conv("larkx:c_group")
	if dm.Unread != 3 || grp.Unread != 2 || !grp.AtMe {
		t.Fatalf("Feishu's counts: dm %+v, group %+v", dm, grp)
	}
	readLog := filepath.Join(dir, "ext", "larkx", "data", "fake-read.jsonl")
	if h.readConvs([]string{"larkx:c_dm"}) != 1 {
		t.Fatal("read in Collie")
	}
	r.syncRead(ctx)
	if dm, _ = h.store.conv("larkx:c_dm"); dm.Unread != 0 || dm.AppUnread != 3 {
		t.Fatalf("read here, Feishu not told: %+v", dm)
	}
	if _, err := os.Stat(readLog); err == nil {
		t.Fatal("read-back is off by default")
	}
	if _, err := h.configure("larkx", func(x *ExtSetting) { x.Config = map[string]any{"region": "feishu", "read_back": true} }); err != nil {
		t.Fatal(err)
	}
	if h.run("larkx") != r {
		t.Fatal("read-back is Collie's switch: no restart")
	}
	h.readConvs([]string{"larkx:c_dm"})
	waitFor(t, "marked read in Feishu", func() bool {
		b, _ := os.ReadFile(readLog)
		c, _ := h.store.conv("larkx:c_dm")
		return strings.Contains(string(b), "c_dm") && c.AppUnread == 0 && c.ReadBase == 0
	})

	h.store.update("larkx:c_dm", func(c *InboxConv) { c.On = true; c.AskMsg = "m1"; c.AskAt = time.Now() })
	o, err := h.sendOut("ext:larkx:c_dm", "改到 **三点**", extproto.KindAnswer, runInfo{Agent: "Codex"}, "")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sent", func() bool { x := h.store.out(o.Ref); return x != nil && x.State == extproto.DeliverySent })
	if x := h.store.out(o.Ref); !strings.HasPrefix(x.MessageID, "m_sent_") {
		t.Fatalf("the message id: %+v", x)
	}
	b, err := os.ReadFile(filepath.Join(dir, "ext", "larkx", "data", "fake-posts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	b = []byte(lines[len(lines)-1]) // before it, maybe the note that its agent can't start on the hub
	var post struct {
		Text, Footer string
		ReplyTo      string `json:"reply_to"`
	}
	json.Unmarshal(b, &post)
	if post.Text != "改到 **三点**" || !strings.HasPrefix(post.Footer, "🤖 AI 代答") || post.ReplyTo != "m1" {
		t.Fatalf("the post, its footer apart: %s", b)
	}

	// Direct replies get the error at the sending surface, without also
	// notifying the owner in chat:main.
	beforeOuts := len(h.store.outs("conv = ?", "larkx:c_dm"))
	beforeEntries := len(sp.st.Entries)
	reply = httptest.NewRecorder()
	sp.s.inboxReply(reply, httptest.NewRequest("POST", "/api/inbox/reply", strings.NewReader(
		`{"id":"larkx:c_dm","text":"app_secret: B9Qm583tknWzFake"}`)))
	if reply.Code != 400 || strings.Contains(reply.Body.String(), "B9Qm583tknWzFake") {
		t.Fatal("direct reply should receive a sanitized 400")
	}
	if len(h.store.outs("conv = ?", "larkx:c_dm")) != beforeOuts || len(sp.st.Entries) != beforeEntries {
		t.Fatal("blocked direct reply should neither reach the outbox nor send a duplicate notice")
	}
	reply = httptest.NewRecorder()
	sp.s.inboxReply(reply, httptest.NewRequest("POST", "/api/inbox/reply", strings.NewReader(
		`{"id":"larkx:c_dm","text":"token: req.Token"}`)))
	if reply.Code != 200 {
		t.Fatalf("direct reply with a code reference = %d, want 200", reply.Code)
	}

	testLarkInboxActions(t, sp.s)

	// An outbox row saved before the guard was installed must not reach the
	// fake Feishu on retry. Its rejection reason contains no original text.
	pending := &OutboxItem{Ref: "oold", Conv: "larkx:c_dm", Text: "token=synthetic_07062_secret",
		Kind: extproto.KindAnswer, State: outPending, At: time.Now()}
	h.store.putOut(pending)
	h.retrySends(r)
	if got := h.store.out(pending.Ref); got == nil || got.State != extproto.DeliveryFailed || got.Reason != errLarkCredential.Error() {
		t.Fatal("legacy pending reply was not blocked on retry")
	}
	if posts, err := os.ReadFile(filepath.Join(dir, "ext", "larkx", "data", "fake-posts.jsonl")); err != nil ||
		strings.Contains(string(posts), pending.Text) {
		t.Fatal("a blocked reply reached Feishu or the fixture log could not be read")
	}

	// Restart larkx with a slow restore (unreachable_once) while a Router reply
	// is waiting in the outbox on a closed conversation with a subscribed worker:
	// the initial retry on $proc.state=running must see not_ready (not fail with
	// not_signed_in), and the outbox wake after restore must deliver it.
	if _, err := h.configure("larkx", func(x *ExtSetting) { x.Enabled = false }); err != nil {
		t.Fatal(err)
	}
	secretsFile := filepath.Join(dir, "ext", "larkx", "secrets.json")
	rawSecrets, err := os.ReadFile(secretsFile)
	if err != nil {
		t.Fatal(err)
	}
	var allSecrets map[string]string
	if err := json.Unmarshal(rawSecrets, &allSecrets); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(allSecrets["larkx"])
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(decoded, &saved); err != nil {
		t.Fatal(err)
	}
	saved["unreachable_once"] = true
	reencoded, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	allSecrets["larkx"] = base64.StdEncoding.EncodeToString(reencoded)
	if err := writePrivate(secretsFile, allSecrets); err != nil {
		t.Fatal(err)
	}
	h.store.update("larkx:c_dm", func(c *InboxConv) { c.On = false; c.Worker = "larkx-sub" })
	sp.mu.Lock()
	sp.st.Workers["larkx-sub"] = &Worker{Name: "larkx-sub", Thread: "ext:larkx:c_dm", Status: "done", Conv: "larkx:c_dm"}
	sp.threadLocked("ext:larkx:c_dm").Subscribers = []string{"larkx-sub"}
	sp.mu.Unlock()
	reply = httptest.NewRecorder()
	sp.s.superReply(reply, httptest.NewRequest("POST", "/api/super/reply", strings.NewReader(
		`{"thread":"ext:larkx:c_dm","text":"重启恢复后照常发出"}`)))
	if reply.Code != 200 {
		t.Fatalf("superReply while larkx restarting = %d: %s", reply.Code, reply.Body.String())
	}
	var sentResp struct {
		OK   bool   `json:"ok"`
		Sent string `json:"sent"`
	}
	if err := json.Unmarshal(reply.Body.Bytes(), &sentResp); err != nil || !sentResp.OK || sentResp.Sent == "" {
		t.Fatalf("unexpected superReply response: %s", reply.Body.String())
	}
	if _, err := h.configure("larkx", func(x *ExtSetting) { x.Enabled = true }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pending router reply delivered after larkx restore", func() bool {
		got := h.store.out(sentResp.Sent)
		if got != nil && got.State == extproto.DeliveryFailed {
			t.Fatalf("pending reply failed during startup restore: %+v", got)
		}
		return got != nil && got.State == extproto.DeliverySent
	})
	if posts, err := os.ReadFile(filepath.Join(dir, "ext", "larkx", "data", "fake-posts.jsonl")); err != nil ||
		!strings.Contains(string(posts), "重启恢复后照常发出") {
		t.Fatalf("restored reply did not reach fake Feishu: %v", err)
	}
}

// An action extension: a pasted key to sign in, the computer to be picked
// first, actions checked against the manifest, writes that can be turned
// off, its own wait, and the result passed on.
func TestExtActions(t *testing.T) {
	_, h, dir := newExtTestHub(t)
	if _, err := h.configure("fakea", func(x *ExtSetting) { x.Enabled = true }); err == nil {
		t.Fatal("defaults.machine ask: turning it on without picking a computer must fail")
	}
	h.configure("fakea", func(x *ExtSetting) { x.Machine, x.Chosen = "", true })
	r := startExt(t, h, "fakea")
	ctx := context.Background()
	var st extproto.LoginStep
	if err := r.call(ctx, extproto.MLoginStep, map[string]any{}, &st); err != nil || st.Kind != extproto.LoginSecret || st.Label == "" {
		t.Fatalf("a key to paste: %+v %v", st, err)
	}
	if err := r.call(ctx, extproto.MLoginStep, map[string]any{"step": st.Step, "secret": "k-123"}, &st); err != nil || !st.Done {
		t.Fatalf("pasted: %+v %v", st, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ext", "fakea", "secrets.json")); !strings.Contains(string(b), "ay0xMjM=") {
		t.Fatalf("the key is kept by the extension on its computer: %s", b)
	}
	out, err := h.do(ctx, "fakea", "echo", map[string]any{"text": "hi", "n": "3"})
	if err != nil || out.Text != "hi×3" || !out.Untrusted || len(out.Files) != 1 || len(out.Items) != 1 {
		t.Fatalf("echo: %+v %v", out, err)
	}
	if txt, _ := formatAction(mustJSON(out)); !strings.HasPrefix(txt, "（以下是第三方内容") || !strings.Contains(txt, "/tmp/x.pdf") {
		t.Fatalf("CLI text: %q", txt)
	}
	for _, c := range []struct {
		name string
		args map[string]any
	}{{"echo", map[string]any{}}, {"echo", map[string]any{"text": "x", "zzz": 1}}, {"echo", map[string]any{"text": "x", "mode": "c"}}, {"echo", map[string]any{"text": "x", "n": "x"}}, {"nope", nil}} {
		if _, err := h.do(ctx, "fakea", c.name, c.args); err == nil {
			t.Errorf("%s %v must be refused", c.name, c.args)
		}
	}
	if out, err := h.do(ctx, "fakea", "write", nil); err != nil || out.Text != "written" {
		t.Fatalf("write allowed by default: %v %v", out, err)
	}
	h.configure("fakea", func(x *ExtSetting) { x.NoWrites = true })
	if _, err := h.do(ctx, "fakea", "write", nil); !extproto.IsCode(err, errRefused) {
		t.Fatalf("writes turned off: %v", err)
	}
	started := time.Now()
	if _, err := h.do(ctx, "fakea", "sleep", nil); !extproto.IsCode(err, extproto.ErrTimeout) || time.Since(started) > 2500*time.Millisecond {
		t.Fatalf("timeout_s 1: %v after %s", err, time.Since(started))
	}
	var n int
	h.store.db.QueryRow(`SELECT COUNT(*) FROM ext_audit WHERE ext = 'fakea'`).Scan(&n)
	if n != 3 {
		t.Fatalf("audited calls: %d", n)
	}
}

// A package put in COLLIE_HOME/ext/<id>/pkg/<version>/ shows up and runs.
func TestExtLocal(t *testing.T) {
	_, h, dir := newExtTestHub(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(dir, "ext", "loc", "pkg", "1.2.0")
	os.MkdirAll(filepath.Join(pkg, "bin"), 0o755)
	os.WriteFile(filepath.Join(pkg, "bin", "run"), []byte("#!/bin/sh\nexec "+self+" ext-fake \"$@\"\n"), 0o755)
	man := `{"manifest_version":1,"id":"loc","name":"本地","version":"1.2.0","description":"d",
	  "engines":{"collie_protocol":1},"runtime":{"command":{"` + runtime.GOOS + "-" + runtime.GOARCH + `":"bin/run"}},
	  "contributes":{"connector":{"id":"loc","capabilities":["status","actions"],"actions":[{"name":"echo","description":"e","readonly":true,"params":{"text":{"type":"string"}}}]},"channel":{"format":"markdown"}},
	  "configuration":{"type":"object","properties":{"mode":{"type":"string","default":"actions"}}}}`
	os.WriteFile(filepath.Join(pkg, "collie-extension.json"), []byte(man), 0o644)
	h.refreshLocal(context.Background())
	m := h.manifest("loc")
	if m == nil || !m.Local {
		t.Fatalf("found: %+v", m)
	}
	startExt(t, h, "loc")
	if out, err := h.do(context.Background(), "loc", "echo", map[string]any{"text": "ok"}); err != nil || !strings.HasPrefix(out.Text, "ok") {
		t.Fatalf("local action: %+v %v", out, err)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// A marketplace: add it with a token, read its index, install a package
// at a pinned commit, run it from the hub's copy, uninstall and purge.
func TestExtMarket(t *testing.T) {
	_, h, dir := newExtTestHub(t)
	self, _ := os.Executable()
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	add := func(name, body string, mode int64) {
		tw.WriteHeader(&tar.Header{Name: "owner-repo-abc123/" + name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	add("exts/tool/collie-extension.json", `{"manifest_version":1,"id":"tool","name":"工具","version":"2.0.0","description":"d",
	  "engines":{"collie_protocol":1},"runtime":{"command":"bin/run"},
	  "contributes":{"connector":{"id":"tool","capabilities":["status","actions"],"actions":[{"name":"echo","description":"e","readonly":true,"params":{"text":{"type":"string"}}}]},"channel":{"format":"markdown"}},
	  "configuration":{"type":"object","properties":{"mode":{"type":"string","default":"actions"},"key":{"type":"string","x-secret":true}}}}`, 0o644)
	add("exts/tool/bin/run", "#!/bin/sh\nexec "+self+" ext-fake \"$@\"\n", 0o755)
	add("other/README.md", "not this one", 0o644)
	tw.Close()
	gz.Close()
	var auth string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		switch {
		case strings.HasSuffix(r.URL.Path, "/commits/HEAD"):
			fmt.Fprint(w, "abc123")
		case strings.HasSuffix(r.URL.Path, "/contents/collie-marketplace.json"):
			fmt.Fprint(w, `{"name":"私有扩展","extensions":[{"id":"tool","path":"exts/tool","version":"2.0.0","description":"一个工具"}]}`)
		case strings.HasSuffix(r.URL.Path, "/tarball/abc123"):
			w.Write(tgz.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	old := marketAPI
	marketAPI = gh.URL
	defer func() { marketAPI = old }()

	mk, err := h.addMarket(context.Background(), "owner/repo", "", "tok-1")
	if err != nil || mk.Commit != "abc123" || len(mk.Index) != 1 || auth != "Bearer tok-1" {
		t.Fatalf("market: %+v %v %q", mk, err, auth)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "market", "secrets.json")); !strings.Contains(string(b), "tok-1") || strings.Contains(h.store.kv("ext_markets"), "tok-1") {
		t.Fatal("the token lives in market/secrets.json, not the database")
	}
	m, err := h.installFromMarket(context.Background(), "owner/repo", "tool")
	if err != nil || m.Market == nil || m.Market.Commit != "abc123" || len(m.Market.Files) != 2 {
		t.Fatalf("install: %+v %v", m, err)
	}
	if _, err := h.setConfigSecrets(m, map[string]any{"key": "s3cret"}, nil); err != nil {
		t.Fatal(err)
	}
	h.configure("tool", func(x *ExtSetting) { x.Config = map[string]any{"key": "s3cret"} })
	if strings.Contains(h.store.kv("ext_settings"), "s3cret") {
		t.Fatal("an x-secret value never goes into the settings")
	}
	startExt(t, h, "tool")
	if out, err := h.do(context.Background(), "tool", "echo", map[string]any{"text": "hi"}); err != nil || !strings.HasPrefix(out.Text, "hi") {
		t.Fatalf("installed tool runs: %+v %v", out, err)
	}
	if v, err := h.s.extConfigSecret("tool", "key"); err != nil || v != "s3cret" {
		t.Fatalf("extConfigSecret: %q %v", v, err)
	}
	if err := h.uninstall(context.Background(), "tool", true); err != nil {
		t.Fatal(err)
	}
	if h.manifest("tool") != nil {
		t.Fatal("uninstalled")
	}
	if _, err := os.Stat(filepath.Join(dir, "ext", "tool")); !os.IsNotExist(err) {
		t.Fatal("purged its folder")
	}
	if _, err := os.Stat(filepath.Join(dir, "market", "pkgs", "tool")); !os.IsNotExist(err) {
		t.Fatal("dropped the hub's copy")
	}
}

// Only declarations (a 工作台 module): on without a program.
func TestExtDeclarative(t *testing.T) {
	_, h, _ := newExtTestHub(t)
	m := &ExtManifest{ManifestVersion: 1, ID: "decl", Name: "d", Version: "1.0.0"}
	m.Contributes.Workbench = []json.RawMessage{json.RawMessage(`{"id":"x","title":"X"}`)}
	if err := m.validate(); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.cat["decl"] = m
	h.mu.Unlock()
	h.configure("decl", func(x *ExtSetting) { x.Enabled = true })
	if h.run("decl") != nil {
		t.Fatal("no program for declarations")
	}
	if items := h.s.extWorkbench(); len(items) != 0 {
		t.Fatalf("the workbench is off: its modules are not shown: %+v", items)
	}
	turnOnWorkbench(t, h)
	if items := h.s.extWorkbench(); len(items) != 1 || items[0].Ext != "decl" {
		t.Fatalf("workbench items: %+v", items)
	}
}

// turnOnWorkbench turns the 工作台 on, as 接入 does.
func turnOnWorkbench(t *testing.T, h *extHost) {
	t.Helper()
	cat, err := extCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range cat {
		if m.ID == featureWorkbench {
			h.mu.Lock()
			h.cat[m.ID] = m
			h.mu.Unlock()
		}
	}
	if _, err := h.configure(featureWorkbench, func(x *ExtSetting) { x.Enabled = true }); err != nil {
		t.Fatal(err)
	}
	if h.run(featureWorkbench) != nil || !h.featureOn(featureWorkbench) {
		t.Fatal("on, with nothing to run")
	}
}

// A reply with a picture: the picture waits on the hub with the send and
// reaches a send-capable extension as an image.
func TestExtSendFiles(t *testing.T) {
	sp, h, dir := newExtTestHub(t)
	r := startExt(t, h, "fakew")
	login(t, r)
	feed(t, dir, "fakew", fakeMsg{Chat: "u1", ChatName: "我", ID: "w1", Sender: "u1", SenderName: "我", Mine: true, Text: "截图给我"})
	waitFor(t, "a trigger", func() bool { return len(extTriggers(sp, "ext:fakew")) == 1 })
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nxx"))
	o, err := h.sendOutFiles("ext:fakew:u1", "在这", extproto.KindAnswer, runInfo{}, "", []sendFile{{Name: "a.png", Mime: "image/png", Data: png}})
	if err != nil || len(o.Files) != 1 {
		t.Fatalf("%+v %v", o, err)
	}
	waitFor(t, "sent", func() bool { x := h.store.out(o.Ref); return x != nil && x.State == extproto.DeliverySent })
	if _, err := h.sendOutFiles("ext:fake:c1", "x", extproto.KindAnswer, runInfo{}, "", []sendFile{{Name: "a.png", Mime: "image/png", Data: png}}); err == nil {
		t.Fatal("an actions-only extension can't take files")
	}
}

// The 工作台 is off until turned on in 接入: its API refuses, ROUTER.md and
// the dream leave it out; on, all of it is back as it was.
func TestWorkbenchSwitch(t *testing.T) {
	_, h, _ := newExtTestHub(t)
	ok := h.s.wbOnly(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"ok": true}) })
	code := func() int {
		rec := httptest.NewRecorder()
		ok(rec, httptest.NewRequest("GET", "/api/wb", nil))
		return rec.Code
	}
	if c := code(); c != 409 {
		t.Fatalf("off: %d, want 409", c)
	}
	if strings.Contains(routerDoc, "wb ls") || !strings.Contains(routerWBDoc, "wb ls") || !strings.Contains(routerDoc, "{{WB}}") {
		t.Fatal("ROUTER.md's workbench lines go in only while it is on")
	}
	if strings.Contains(dreamPrompt, "wb/index.md") || !strings.Contains(dreamPrompt, "{{WBDIR}}") || !strings.Contains(dreamPrompt, "{{WBSTEP}}") {
		t.Fatal("the dream's workbench part goes in only while it is on")
	}
	turnOnWorkbench(t, h)
	if c := code(); c != 200 {
		t.Fatalf("on: %d", c)
	}
}

// A chat muted in the chat app stays muted in the inbox: its unread count
// still counts, shown without the alert colour; a message that doesn't say
// leaves it as it was.
func TestInboxMuted(t *testing.T) {
	m := fakeManifest("fake", "receive")
	yes, no := true, false
	in := incomingMsg{Chat: "c1", ChatName: "大群", ID: "1", Sender: "u2", Text: "hi", At: time.Now(), Live: true, Muted: &yes}
	c := inboxConversation(m, in, nil)
	if !c.Muted || c.Unread != 1 {
		t.Fatalf("muted, still counted: %+v", c)
	}
	in.Muted = nil
	if c = inboxConversation(m, in, &c); !c.Muted || c.Unread != 2 {
		t.Fatalf("not said: kept: %+v", c)
	}
	in.Muted = &no
	if c = inboxConversation(m, in, &c); c.Muted {
		t.Fatalf("unmuted: %+v", c)
	}
	in.Folded = &yes
	if c = inboxConversation(m, in, &c); !c.Folded {
		t.Fatalf("folded: %+v", c)
	}
}

// A line the extension writes to stderr reaches the hub's view (接入's log)
// within a second, not only with the next start or stop.
func TestExtLogsPushed(t *testing.T) {
	var buf syncBuffer
	p := &extProc{id: "x", hub: &lineWriter{w: &buf}}
	p.readStderr(strings.NewReader("INFO working 1: 0.6ms\nWARNING b\n"))
	waitFor(t, "the lines pushed", func() bool {
		s := buf.String()
		return strings.Contains(s, `"$proc.state"`) && strings.Contains(s, "INFO working 1") && strings.Contains(s, "WARNING b")
	})
	if n := strings.Count(buf.String(), `"$proc.state"`); n != 1 {
		t.Fatalf("two lines together are one push: %d", n)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// The 收件箱 leaves out chats folded in the chat app and those a switch of
// 接入 hides; the switches are Collie's: changing one restarts nothing.
func TestInboxHidden(t *testing.T) {
	cat, err := extCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var lark *ExtManifest
	for _, m := range cat {
		if m.ID == "larkx" {
			lark = m
		}
	}
	on := lark.hides(lark.configWithDefaults(nil))
	if len(on) != 5 {
		t.Fatalf("three switches, on by default, the Meego ones two rules each: %v", on)
	}
	for _, c := range []struct {
		conv  InboxConv
		rules []string
		want  bool
	}{
		{InboxConv{Name: "产品群"}, on, false},
		{InboxConv{Name: "产品群", Folded: true}, nil, true},
		{InboxConv{Name: "大群", Muted: true}, on, true},
		{InboxConv{Name: "大群", Muted: true}, nil, false},
		{InboxConv{Name: "登录页改版 - [需求同步群]"}, on, true},
		{InboxConv{Name: "[Main] 闪退 - [缺陷同步群]"}, on, true},
		{InboxConv{Name: "[Main] 闪退 - [缺陷同步群]"}, nil, false},
		// Feishu cuts a long name to 64 characters, the tag with it: the extension's tag still tells.
		{InboxConv{Name: "[N1][Main][PVT][低概率] Wi-Fi 条件任务端侧执行成功后任务卡片错误显示「当前任务服务超时」 - [缺...", Tags: []string{"meego-issue"}}, on, true},
		{InboxConv{Name: "【GM】背屏-助手设置", Tags: []string{"meego-story"}}, on, true},
		{InboxConv{Name: "【GM】背屏-助手设置", Tags: []string{"meego-story"}}, nil, false},
		{InboxConv{Name: "Nerida 业务内部UT反馈-新", Tags: []string{"other"}}, on, false},
	} {
		if got := hiddenInInbox(c.conv, c.rules); got != c.want {
			t.Errorf("%+v with %v: hidden %v, want %v", c.conv, c.rules, got, c.want)
		}
	}
	cfg := lark.configWithDefaults(nil)
	off := lark.configWithDefaults(map[string]any{"hide_muted": false})
	if !sameJSON(lark.programConfig(cfg), lark.programConfig(off)) || sameJSON(cfg, off) {
		t.Fatal("a 收件箱 switch is not the program's config")
	}
	bad := *lark
	bad.Configuration = &ExtConfig{Type: "object", Properties: map[string]ExtConfigProp{"x": {Type: "string", Hide: hideRules{hideMuted}}}}
	if bad.validate() == nil {
		t.Fatal("x-hide belongs to a boolean")
	}
	bad.Configuration = &ExtConfig{Type: "object", Properties: map[string]ExtConfigProp{"x": {Type: "boolean", Hide: hideRules{"tag:a", "tag:"}}}}
	if bad.validate() == nil {
		t.Fatal("every rule of a list is checked")
	}
	var one, list ExtConfigProp
	if json.Unmarshal([]byte(`{"x-hide":"muted"}`), &one) != nil || json.Unmarshal([]byte(`{"x-hide":["tag:a","name:b"]}`), &list) != nil ||
		!slices.Equal(one.Hide, []string{"muted"}) || !slices.Equal(list.Hide, []string{"tag:a", "name:b"}) {
		t.Fatalf("x-hide is a rule or a list: %v %v", one.Hide, list.Hide)
	}
}

// @-mentioning yourself is a request like the trigger word (someone else
// @-ing you is not), and a group topic is a conversation of its own: its
// trigger has its thread, and its answer goes under its own request.
func TestMentionAndTopics(t *testing.T) {
	sp, h, _ := newExtTestHub(t)
	m := h.manifest("fake")
	msg := func(id, sender string, owner, mention bool, kind, topic string) incomingMsg {
		return incomingMsg{Chat: "g1", ChatName: "群", ChatKind: kind, ID: id, Sender: sender, SenderName: sender,
			Mine: owner, Owner: owner, Mention: mention, Topic: topic, Text: "看下", At: time.Now(), Live: true}
	}
	for _, in := range []incomingMsg{
		msg("1", "me", true, true, "group", "1"),
		msg("2", "u2", false, true, "group", "2"),
		msg("3", "me", true, false, "group", "3"),
		msg("4", "me", true, true, "group", "1"),
	} {
		if err := h.incoming(m, in); err != nil {
			t.Fatal(err)
		}
	}
	ts := extTriggers(sp, "ext:fake")
	if len(ts) != 2 || ts[0].Ref != "1" || ts[1].Ref != "4" || ts[0].Thread != "ext:fake:g1#1" || ts[1].Thread != ts[0].Thread {
		t.Fatalf("your @ only, in topic 1's thread: %+v", ts)
	}
	if err := h.incoming(m, msg("5", "me", true, true, "group", "5")); err != nil {
		t.Fatal(err)
	}
	c, _ := h.store.conv("fake:g1")
	if c.askOf("1").AskMsg != "4" || c.askOf("5").AskMsg != "5" || c.AskMsg != "" {
		t.Fatalf("each topic its own request: %+v", c.Topics)
	}
	o, err := h.sendOut("ext:fake:g1#1", "好", extproto.KindAnswer, runInfo{}, "")
	if err != nil || o.ReplyTo != "4" {
		t.Fatalf("topic 1's answer goes under its request: %+v %v", o, err)
	}
	if _, conv, _ := channelOf("ext:fake:g1#1"); conv != "fake:g1" || topicOf("ext:fake:g1#1") != "1" || topicOf("ext:fake:g1") != "" {
		t.Fatal("a topic's thread names its chat")
	}
	// A direct chat is one conversation: a topic it reports is ignored.
	if err := h.incoming(m, incomingMsg{Chat: "d1", ChatKind: "dm", ID: "6", Sender: "me", Mine: true, Owner: true, Mention: true, Topic: "6", Text: "x", At: time.Now(), Live: true}); err != nil {
		t.Fatal(err)
	}
	if ts := extTriggers(sp, "ext:fake"); ts[len(ts)-1].Thread != "ext:fake:d1" {
		t.Fatalf("direct chat: %+v", ts[len(ts)-1])
	}
}

// 来源策略 ignoring an extension: its requests are not taken, so none is
// marked as being worked on, and the extension is told so.
func TestExtRequestsIgnored(t *testing.T) {
	sp, h, _ := newExtTestHub(t)
	if h.requestsIgnored("fake") {
		t.Fatal("taken by default")
	}
	sp.mu.Lock()
	sp.st.Policies = []Policy{{Source: "ext:fake", Action: "ignore"}}
	sp.mu.Unlock()
	if !h.requestsIgnored("fake") {
		t.Fatal("an ignored source")
	}
	sp.mu.Lock()
	sp.st.Policies = []Policy{{Source: "ext:*", Action: "respond", Allow: []string{"u2"}}}
	sp.mu.Unlock()
	if !h.requestsIgnored("fake") {
		t.Fatal("allowed others only: yours are dropped")
	}
	sp.mu.Lock()
	sp.st.Policies = []Policy{{Source: "ext:fake", Action: "ignore"}}
	sp.mu.Unlock()
	in := incomingMsg{Chat: "c1", ChatKind: "dm", ID: "1", Sender: "me", Mine: true, Owner: true, Text: "/ai 看下", At: time.Now(), Live: true}
	if err := h.incoming(h.manifest("fake"), in); err != nil {
		t.Fatal(err)
	}
	if ts := extTriggers(sp, "ext:fake"); len(ts) != 1 || ts[0].Status != "ignored" {
		t.Fatalf("ignored: %+v", ts)
	}
}

// End to end: a setting saved before the inbox switches existed (only
// region) still hides as 接入 shows them, on by default; the list the page
// gets marks muted chats and Meego sync groups hidden, the rest not.
func TestInboxHiddenEndToEnd(t *testing.T) {
	_, h, _ := newExtTestHub(t)
	cat, err := extCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range cat {
		if m.ID == "larkx" {
			h.mu.Lock()
			h.cat[m.ID] = m
			h.sets[m.ID] = ExtSetting{ID: m.ID, Machine: "devbox", Config: map[string]any{"region": "feishu"}}
			h.mu.Unlock()
		}
	}
	m := h.manifest("larkx")
	yes := true
	for _, in := range []incomingMsg{
		{Chat: "g1", ChatName: "严肃 Agent 交流群", ChatKind: "group", Muted: &yes},
		{Chat: "g2", ChatName: "手机任务自建 harness 二期 - [需求同步群]", ChatKind: "group"},
		{Chat: "g3", ChatName: "闪退 - [缺陷同步群]", ChatKind: "group"},
		{Chat: "g4", ChatName: "产品群", ChatKind: "group"},
		{Chat: "g5", ChatName: "[Genesis-M][beta][2115872568] 【应用使用问题/豆包/其他功能类/】 我在新闻界面调出豆包，让...", ChatKind: "group", Tags: []string{"meego-issue"}},
	} {
		in.ID, in.Sender, in.Text, in.At, in.Live = in.Chat+"-1", "u2", "hi", time.Now(), true
		if err := h.incoming(m, in); err != nil {
			t.Fatal(err)
		}
	}
	list := func() map[string]bool {
		rec := httptest.NewRecorder()
		h.s.inboxList(rec, httptest.NewRequest("GET", "/api/inbox", nil))
		var out struct{ Convs []convView }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		hidden := map[string]bool{}
		for _, c := range out.Convs {
			hidden[c.Name] = c.Hidden
		}
		return hidden
	}
	got := list()
	if !got["严肃 Agent 交流群"] || !got["手机任务自建 harness 二期 - [需求同步群]"] || !got["闪退 - [缺陷同步群]"] || got["产品群"] || !got["[Genesis-M][beta][2115872568] 【应用使用问题/豆包/其他功能类/】 我在新闻界面调出豆包，让..."] {
		t.Fatalf("switches never saved count as on: %v", got)
	}
	if _, err := h.configure("larkx", func(x *ExtSetting) {
		x.Config = map[string]any{"region": "feishu", "hide_muted": false, "hide_meego_story": true, "hide_meego_bug": false}
	}); err != nil {
		t.Fatal(err)
	}
	got = list()
	if got["严肃 Agent 交流群"] || !got["手机任务自建 harness 二期 - [需求同步群]"] || got["闪退 - [缺陷同步群]"] || got["[Genesis-M][beta][2115872568] 【应用使用问题/豆包/其他功能类/】 我在新闻界面调出豆包，让..."] {
		t.Fatalf("switched off: shown again: %v", got)
	}
}

// Header status slot extension: valid with only status + header_status,
// hidden while disabled, served via /api/ext/header-status while enabled,
// and restarted when an x-secret config field changes.
func TestExtHeaderStatus(t *testing.T) {
	_, h, _ := newExtTestHub(t)
	m := &ExtManifest{ManifestVersion: 1, ID: "hdr", Name: "Header Meter", Label: "Meter", Version: "1.0.0"}
	m.Engines.CollieProtocol = extproto.Protocol
	m.Runtime.Builtin = "fake"
	m.Contributes.Connector.ID = "hdr"
	m.Contributes.Connector.Capabilities = []string{"status", "header_status"}
	m.Configuration = &ExtConfig{Type: "object", Properties: map[string]ExtConfigProp{
		"mode": {Type: "string", Default: "header"},
		"key":  {Type: "string", Title: "Key", Secret: true},
	}}
	if err := m.validate(); err != nil {
		t.Fatalf("header_status manifest should validate: %v", err)
	}
	h.mu.Lock()
	h.cat["hdr"] = m
	h.mu.Unlock()
	t.Cleanup(func() { h.configure("hdr", func(x *ExtSetting) { x.Enabled = false }) })

	fetch := func(query string) []headerStatusSlot {
		rec := httptest.NewRecorder()
		h.s.extHeaderStatus(rec, httptest.NewRequest("GET", "/api/ext/header-status"+query, nil))
		if rec.Code != 200 {
			t.Fatalf("GET /api/ext/header-status%s: status %d %s", query, rec.Code, rec.Body.String())
		}
		var out struct {
			Slots []headerStatusSlot `json:"slots"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Slots
	}

	// Off by default: nothing shown in the header.
	if slots := fetch(""); len(slots) != 0 {
		t.Fatalf("disabled extension must not appear: %+v", slots)
	}

	startExt(t, h, "hdr")
	slots := fetch("?model=claude-opus-5-5&fresh=1")
	if len(slots) != 1 || slots[0].ID != "hdr" || slots[0].State != "ok" || slots[0].Status == nil {
		t.Fatalf("enabled header status: %+v", slots)
	}
	if slots[0].Status.Compact != "Fake 75%" || slots[0].Status.Subline != "model=claude-opus-5-5 fresh" {
		t.Fatalf("unexpected header status payload: %+v", slots[0].Status)
	}

	// Updating an x-secret config field restarts the extension with the new value.
	changed, err := h.setConfigSecrets(m, map[string]any{"key": "abc12345"}, nil)
	if err != nil || !changed {
		t.Fatalf("setConfigSecrets: changed=%v err=%v", changed, err)
	}
	if _, err := h.configureWithRestart("hdr", changed, func(x *ExtSetting) {
		x.Config = map[string]any{"mode": "header"}
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "restarted header status sees secret", func() bool {
		ss := fetch("?model=test")
		return len(ss) == 1 && ss[0].Status != nil && strings.Contains(ss[0].Status.Subline, "secret-len=8")
	})

	// Turn off again: disappears immediately.
	if _, err := h.configure("hdr", func(x *ExtSetting) { x.Enabled = false }); err != nil {
		t.Fatal(err)
	}
	if slots := fetch(""); len(slots) != 0 {
		t.Fatalf("disabled again: %+v", slots)
	}
}

// The app's count stands over Collie's; what you read in Collie stays read
// as more comes, until you read on in the app.
func TestInboxAppRead(t *testing.T) {
	var c InboxConv
	if !c.appRead(5, nil) || c.Unread != 5 {
		t.Fatalf("the app's count: %+v", c)
	}
	c.AtMe = true
	c.markRead()
	if c.Unread != 0 || c.AtMe || c.ReadBase != 5 {
		t.Fatalf("read in Collie: %+v", c)
	}
	if c.appRead(5, nil) {
		t.Fatal("nothing new")
	}
	c.appRead(7, nil)
	if c.Unread != 2 {
		t.Fatalf("two came since: %+v", c)
	}
	c.AtMe = true
	c.appRead(1, nil)
	if c.Unread != 0 || c.ReadBase != 1 || c.AtMe {
		t.Fatalf("read on in the app: %+v", c)
	}
	c.appRead(0, nil)
	if c.ReadBase != 0 || c.Unread != 0 {
		t.Fatalf("all read in the app: %+v", c)
	}
	c.appRead(-3, nil)
	if c.AppUnread != 0 {
		t.Fatalf("never below zero: %+v", c)
	}
	// @我 as the app keeps it: shown while unread here, gone once read here.
	yes, no := true, false
	c.appRead(4, &yes)
	if !c.AtMe {
		t.Fatalf("an unread @ in the app: %+v", c)
	}
	c.markRead()
	if c.appRead(4, &yes); c.AtMe {
		t.Fatalf("read here, the @ with it: %+v", c)
	}
	c.appRead(6, &no)
	if c.AtMe || c.Unread != 2 {
		t.Fatalf("new messages, none @-ing you: %+v", c)
	}
}

// A chat's picture comes through the hub: only from the extension's hosts,
// fetched once and kept, and the list gives the hub's address, not the CDN's.
func TestInboxAvatar(t *testing.T) {
	var hits int
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("png"))
	}))
	defer cdn.Close()
	was := avatarClient
	avatarClient = cdn.Client()
	defer func() { avatarClient = was }()
	_, h, _ := newExtTestHub(t)
	m := h.manifest("fake")
	m.Permissions.Network = []string{"127.0.0.1"}
	for _, in := range []incomingMsg{
		{Chat: "g1", ChatName: "群", ChatKind: "group", Avatar: cdn.URL + "/g1.png"},
		{Chat: "g2", ChatName: "别处", ChatKind: "group", Avatar: "https://elsewhere.example/g2.png"},
	} {
		in.ID, in.Sender, in.Text, in.At, in.Live = in.Chat+"-1", "u2", "hi", time.Now(), true
		if err := h.incoming(m, in); err != nil {
			t.Fatal(err)
		}
	}
	get := func(id string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.s.inboxAvatar(rec, httptest.NewRequest("GET", "/api/inbox/avatar?id="+id, nil))
		return rec
	}
	for range 2 {
		if rec := get("fake:g1"); rec.Code != 200 || rec.Body.String() != "png" {
			t.Fatalf("the picture: %d %q", rec.Code, rec.Body.String())
		}
	}
	if hits != 1 {
		t.Fatalf("fetched once and kept: %d fetches", hits)
	}
	if rec := get("fake:g2"); rec.Code != http.StatusForbidden {
		t.Fatalf("a host the extension doesn't name: %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	h.s.inboxList(rec, httptest.NewRequest("GET", "/api/inbox", nil))
	if body := rec.Body.String(); strings.Contains(body, cdn.URL) || !strings.Contains(body, `"/api/inbox/avatar?id=fake%3Ag1`) {
		t.Fatalf("the list names the hub's address only: %s", body)
	}
	// A sender's picture: the address the hub signed, and no other.
	path := h.s.senderAvatarPath("fake:g1", cdn.URL+"/u2.png")
	for want, p := range map[int]string{200: path, 404: strings.Replace(path, "u2.png", "u3.png", 1)} {
		rec := httptest.NewRecorder()
		h.s.inboxAvatar(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != want {
			t.Fatalf("%s: %d, want %d", p, rec.Code, want)
		}
	}
}

// The read state puts a chat's name right (an old 私聊（还不知道对方是谁）
// among them), and in recent messages Collie's answers from your account
// are told from what you wrote yourself.
func TestInboxNamesAndAnswers(t *testing.T) {
	_, h, _ := newExtTestHub(t)
	m := h.manifest("fake")
	in := incomingMsg{Chat: "c1", ChatName: "私聊（还不知道对方是谁）", ChatKind: "self", ID: "1", Sender: "me", Mine: true, Owner: true, Text: "x", At: time.Now(), Live: true}
	if err := h.incoming(m, in); err != nil {
		t.Fatal(err)
	}
	h.store.appRead("fake", map[string]extproto.ChatRead{"c1": {Name: "陆文韬（我）"}, "c9": {Name: "不在收件箱"}})
	if c, _ := h.store.conv("fake:c1"); c.Name != "陆文韬（我）" {
		t.Fatalf("put right: %q", c.Name)
	}
	h.store.appRead("fake", map[string]extproto.ChatRead{"c1": {}})
	if c, _ := h.store.conv("fake:c1"); c.Name != "陆文韬（我）" {
		t.Fatalf("no name said, the name stays: %q", c.Name)
	}
	h.store.appRead("fake", map[string]extproto.ChatRead{"c1": {Tags: []string{"bot"}}})
	h.store.appRead("fake", map[string]extproto.ChatRead{"c1": {}})
	if c, _ := h.store.conv("fake:c1"); !slices.Equal(c.Tags, []string{"bot"}) {
		t.Fatalf("tags said are kept, none said leaves them: %v", c.Tags)
	}
	yes := true
	got := mergeInboxRecent(InboxConv{ID: "fake:c1", Chat: "c1"}, nil, []extproto.Message{
		{ID: "a", ChatID: "c1", Owner: &yes, Text: "你好！\n\n🤖 AI 代答，仅供参考 · Codex · 7秒", At: "2026-11-26T12:00:00Z"},
		{ID: "b", ChatID: "c1", Owner: &yes, Text: "你好", At: "2026-11-26T12:01:00Z"},
	}, nil, defaultFooter)
	if len(got) != 2 || !got[0].Bot || got[1].Bot {
		t.Fatalf("the answer is the bot's, your own message yours: %+v", got)
	}
}

// A chat handed to AI shows its latest message: Collie's own answer moves
// the list on (the app keeps the echo of it from Collie), and so does a
// newer message the chat app has when the chat is opened.
func TestInboxLatestMessage(t *testing.T) {
	_, h, _ := newExtTestHub(t)
	m := h.manifest("fake")
	asked := time.Now().Add(-time.Minute)
	in := incomingMsg{Chat: "c1", ChatName: "我", ChatKind: "self", ID: "1", Sender: "me", Mine: true, Owner: true, Text: "/ai 你好", At: asked, Live: true}
	if err := h.incoming(m, in); err != nil {
		t.Fatal(err)
	}
	h.store.update("fake:c1", func(c *InboxConv) { c.On = true; c.AskMsg = "1"; c.AskAt = asked })
	if _, err := h.sendOut("ext:fake:c1", "你好！有什么可以帮你？", extproto.KindAnswer, runInfo{}, ""); err != nil {
		t.Fatal(err)
	}
	c, _ := h.store.conv("fake:c1")
	if c.Preview != "AI 代答: 你好！有什么可以帮你？" || !c.LastAt.After(asked) {
		t.Fatalf("the answer is the latest: %q at %v", c.Preview, c.LastAt)
	}
	before := c.LastAt
	h.store.noteLatest("fake:c1", before.Add(-time.Hour), "旧的")
	if c, _ = h.store.conv("fake:c1"); c.Preview != "AI 代答: 你好！有什么可以帮你？" {
		t.Fatalf("an older message doesn't move it back: %q", c.Preview)
	}
}

func TestInboxLatestLine(t *testing.T) {
	at := time.Date(2026, 11, 27, 0, 52, 0, 0, time.UTC)
	for _, c := range []struct {
		m    InboxMsg
		want string
	}{
		{InboxMsg{At: at, Mine: true, Text: "@缪张澍 orca t3code"}, "你: @缪张澍 orca t3code"},
		{InboxMsg{At: at, Mine: true, Bot: true, Text: "你好！\n\n🤖 AI 代答"}, "AI 代答: 你好！"},
		{InboxMsg{At: at, SenderName: "李四", Text: "评审定在周五"}, "李四: 评审定在周五"},
	} {
		if got, line := latestLine([]InboxMsg{{At: at.Add(-time.Hour), Text: "旧"}, c.m}); !got.Equal(at) || line != c.want {
			t.Errorf("%+v: %v %q, want %q", c.m, got, line, c.want)
		}
	}
	if at, line := latestLine(nil); !at.IsZero() || line != "" {
		t.Fatal("nothing: nothing")
	}
}

// A chat whose answers came before the list followed them catches up when the hub starts.
func TestInboxLatestCatchUp(t *testing.T) {
	_, h, _ := newExtTestHub(t)
	at := time.Now().Add(-time.Hour)
	c := &InboxConv{ID: "fake:c1", Ext: "fake", Chat: "c1", Name: "我", Kind: "self", LastAt: at, Preview: "你: 你好", On: true}
	if err := putInboxConv(h.store.db, c); err != nil {
		t.Fatal(err)
	}
	h.store.addMsg(InboxMsg{ID: "out:r1", Conv: c.ID, At: at.Add(time.Minute), Mine: true, Ref: "r1", Text: "你好！\n\n🤖 AI 代答，仅供参考"})
	again := newExtStore(h.store.db)
	if got, _ := again.conv(c.ID); got.Preview != "AI 代答: 你好！" || !got.LastAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("caught up: %q at %v", got.Preview, got.LastAt)
	}
}

func TestInboxEmojiURLs(t *testing.T) {
	sp, _, _ := newExtTestHub(t)
	m := InboxMsg{
		ID:   "m1",
		Conv: "larkx:c1",
		View: &extproto.MessageView{
			Kind: "post",
			Nodes: []extproto.MessageNode{{
				Kind: "paragraph",
				Children: []extproto.MessageNode{
					{Kind: "emoji", Text: "流泪", URL: "https://sf3-cn.feishucdn.com/obj/lark-reaction-cn/emoji_sob.png"},
					{Kind: "emoji", Text: "坏", URL: "javascript:alert(1)"},
				},
			}},
		},
	}
	sp.s.inboxMediaURLs("larkx:c1", &m)
	kids := m.View.Nodes[0].Children
	if !strings.HasPrefix(kids[0].URL, "/api/inbox/avatar?") || !strings.Contains(kids[0].URL, "sig=") {
		t.Fatalf("https emoji should be signed via hub avatar proxy: %q", kids[0].URL)
	}
	if kids[1].URL != "" {
		t.Fatalf("non-https emoji URL should be stripped: %q", kids[1].URL)
	}
}

func TestInboxActionSigningAndUpdatedMessage(t *testing.T) {
	sp, h, _ := newExtTestHub(t)
	m := h.manifest("fake")
	now := time.Now()
	if err := h.incoming(m, incomingMsg{
		Chat: "c1", ChatName: "云文档助手", ChatKind: "dm", ID: "m1", Sender: "bot", SenderName: "云文档助手",
		Text: "[卡片] 待审批", At: now, Live: true,
		View: &extproto.MessageView{
			Kind: "card", Title: "待审批",
			Nodes: []extproto.MessageNode{{Kind: "button", Text: "Approve", Action: "act_1"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	h.store.update("fake:c1", func(c *InboxConv) { c.On = true })
	h.store.addMsg(InboxMsg{
		ID: "m1", Conv: "fake:c1", At: now, Sender: "bot", SenderName: "云文档助手", Text: "[卡片] 待审批",
		View: &extproto.MessageView{
			Kind: "card", Title: "待审批",
			Nodes: []extproto.MessageNode{{Kind: "button", Text: "Approve", Action: "act_1"}},
		},
	})

	// Action signing on card button + calendar RSVP.
	msg := h.store.msgs("fake:c1", time.Time{})[0]
	sp.s.inboxMediaURLs("fake:c1", &msg)
	if msg.View.Nodes[0].Sig == "" {
		t.Fatal("card action button should be signed")
	}
	cal := InboxMsg{
		ID: "m2", Conv: "fake:c1",
		View: &extproto.MessageView{Kind: "calendar", Title: "评审", Action: "cal:uk1:0", Status: "accept", Conflict: true},
	}
	sp.s.inboxMediaURLs("fake:c1", &cal)
	if cal.View.Sig == "" {
		t.Fatal("calendar action should be signed")
	}

	// Bad signature rejected with 403.
	rec := httptest.NewRecorder()
	sp.s.inboxAction(rec, httptest.NewRequest("POST", "/api/inbox/action",
		strings.NewReader(`{"id":"fake:c1","message_id":"m1","kind":"card","action_id":"act_1","sig":"bad"}`)))
	if rec.Code != 403 {
		t.Fatalf("bad sig: got %d, want 403", rec.Code)
	}

	// Updated message replaces stored view in-place without increasing unread count.
	before, _ := h.store.conv("fake:c1")
	if err := h.incoming(m, incomingMsg{
		Chat: "c1", ID: "m1", Sender: "bot", SenderName: "云文档助手",
		Text: "[卡片] 已通过", At: now, Live: true, Updated: true,
		View: &extproto.MessageView{Kind: "card", Title: "已通过", Nodes: []extproto.MessageNode{{Kind: "text", Text: "You have approved."}}},
	}); err != nil {
		t.Fatal(err)
	}
	after, _ := h.store.conv("fake:c1")
	if after.Unread != before.Unread {
		t.Fatalf("updated message must not bump unread: %d -> %d", before.Unread, after.Unread)
	}
	stored := h.store.msgs("fake:c1", time.Time{})
	if len(stored) != 1 || stored[0].View == nil || stored[0].View.Title != "已通过" {
		t.Fatalf("stored message view should update in place: %+v", stored)
	}
}
