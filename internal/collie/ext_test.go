package collie

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// The real larkx package, its fixture backend (no network, no account):
// set COLLIE_LARKX_PKG to an unpacked extensions/larkx of the pinned commit.
// It checks the pinned hashes, the handshake, sign-in, the archive and a send.
func TestLarkxFixture(t *testing.T) {
	pkg := os.Getenv("COLLIE_LARKX_PKG")
	if pkg == "" {
		t.Skip("COLLIE_LARKX_PKG not set")
	}
	srv := httptest.NewServer(http.StripPrefix("/pkg/", http.FileServer(http.Dir(pkg))))
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
			src.Base = srv.URL + "/pkg/"
			cp.Source = &src
			real = &cp
		}
	}
	extEnvKeep = append(extEnvKeep, "LARKX_PERSONA_BACKEND")
	t.Setenv("LARKX_PERSONA_BACKEND", "fixture")
	sp, h, _ := newExtTestHub(t)
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
		t.Fatal("the fixture signs in after two polls")
	}
	waitFor(t, "signed in", func() bool {
		r.refreshStatus(ctx)
		v := h.view("larkx")
		return v.Status != nil && v.Status.State == extproto.StateOK
	})
	r.signalPull()
	waitFor(t, "its chats in the inbox", func() bool { return len(h.store.list("larkx")) >= 3 })
	if len(extTriggers(sp, "ext:larkx")) != 0 {
		t.Fatal("history never triggers")
	}
	h.store.update("larkx:c_dm", func(c *InboxConv) { c.On = true; c.AskMsg = "m4"; c.AskAt = time.Now() })
	o, err := h.sendOut("ext:larkx:c_dm", "好的", extproto.KindAnswer, runInfo{Agent: "Codex"}, "")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sent through larkx_send", func() bool { x := h.store.out(o.Ref); return x != nil && x.State == extproto.DeliverySent })
	if x := h.store.out(o.Ref); x.MessageID != "m_sent_1" {
		t.Fatalf("larkx's message id: %+v", x)
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
	if err := h.setConfigSecrets(m, map[string]any{"key": "s3cret"}, nil); err != nil {
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
	if items := h.s.extWorkbench(); len(items) != 1 || items[0].Ext != "decl" {
		t.Fatalf("workbench items: %+v", items)
	}
}
