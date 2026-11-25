package collie

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testWorkbench(t *testing.T) *workbench {
	t.Helper()
	home := t.TempDir()
	db, err := openDB(filepath.Join(home, "herd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return newWorkbench(db, home, filepath.Join(home, "router"))
}

func webModule(id, feed, share string) WBModule {
	return WBModule{ID: id, Title: id, Group: "fav", Kind: wbKindWeb, URL: "https://" + id + ".example.com/", Feed: feed, Share: share}
}

func TestWorkbenchNormalize(t *testing.T) {
	ok := webModule("books", "https://books.example.com/agent.md", wbShareKeep)
	cases := []struct {
		name string
		mod  func(*WBModule)
		err  string
	}{
		{"valid", func(*WBModule) {}, ""},
		{"bad id", func(m *WBModule) { m.ID = "Bad!" }, "无效"},
		{"no title", func(m *WBModule) { m.Title = " " }, "不能为空"},
		{"unknown group", func(m *WBModule) { m.Group = "nope" }, "不存在"},
		{"javascript url", func(m *WBModule) { m.URL = "javascript:alert(1)" }, "http"},
		{"userinfo url", func(m *WBModule) { m.URL = "https://u:p@x.com/" }, "http"},
		{"shared without feed", func(m *WBModule) { m.Feed = "" }, "没有数据地址"},
		{"bad share", func(m *WBModule) { m.Share = "all" }, "共享档"},
		{"unknown link", func(m *WBModule) { m.Kind, m.URL, m.Share, m.Feed = wbKindLink, "auto:nope", "", "" }, "不存在"},
		{"bad kind", func(m *WBModule) { m.Kind = "ext" }, "不支持"},
		{"every too long", func(m *WBModule) { m.Every = 1000 }, "间隔"},
	}
	for _, c := range cases {
		m := ok
		c.mod(&m)
		cfg := wbConfig{Groups: []WBGroup{{ID: "fav", Title: "收藏"}}, Modules: []WBModule{m}}
		err := cfg.normalize()
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.err)
		}
	}
	cfg := wbConfig{Groups: []WBGroup{{ID: "fav", Title: "收藏"}}, Modules: []WBModule{{ID: "a", Title: "A", Group: "fav", Kind: wbKindWeb, URL: "https://a.com"}}}
	if err := cfg.normalize(); err != nil || cfg.Modules[0].Share != wbShareOff || cfg.Modules[0].Embed != wbEmbedTab {
		t.Fatalf("defaults: %v %+v", err, cfg.Modules[0])
	}
	dup := wbConfig{Groups: []WBGroup{{ID: "fav", Title: "收藏"}}, Modules: []WBModule{ok, ok}}
	if err := dup.normalize(); err == nil {
		t.Fatal("duplicate ids accepted")
	}
}

func TestFrameAllowed(t *testing.T) {
	const origin = "https://c.example.org"
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			out.Add(kv[i], kv[i+1])
		}
		return out
	}
	cases := []struct {
		name string
		h    http.Header
		want bool
	}{
		{"no headers", h(), true},
		{"xfo deny", h("X-Frame-Options", "DENY"), false},
		{"xfo sameorigin", h("X-Frame-Options", "sameorigin"), false},
		// A login page that forbids framing, as real sites send it.
		{"none", h("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'", "X-Frame-Options", "DENY"), false},
		{"exact origin", h("Content-Security-Policy", "frame-ancestors https://c.example.org"), true},
		{"csp wins over xfo", h("Content-Security-Policy", "frame-ancestors https://c.example.org/", "X-Frame-Options", "DENY"), true},
		{"wildcard host", h("Content-Security-Policy", "frame-ancestors 'self' https://*.example.org"), true},
		{"other host", h("Content-Security-Policy", "frame-ancestors https://evil.com"), false},
		{"scheme source", h("Content-Security-Policy", "frame-ancestors https:"), true},
		{"star", h("Content-Security-Policy", "frame-ancestors *"), true},
		{"self only", h("Content-Security-Policy", "frame-ancestors 'self'"), false},
		{"other directives only", h("Content-Security-Policy", "default-src 'self'"), true},
	}
	for _, c := range cases {
		if got, why := frameAllowed(c.h, origin); got != c.want {
			t.Errorf("%s: got %v (%s), want %v", c.name, got, why, c.want)
		}
	}
	if !sameParent("https://a.example.org/x", origin) || sameParent("https://feishu.cn", origin) {
		t.Fatal("sameParent")
	}
}

func TestProbeEmbedFollowsRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			http.Redirect(w, r, "/login", http.StatusFound)
		case "/login":
			w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		case "/open":
		}
	}))
	defer srv.Close()
	c := &http.Client{Timeout: time.Second}
	p, err := probeEmbed(context.Background(), c, srv.URL+"/", "https://c.example.org")
	if err != nil || p.Embed != wbEmbedTab || !strings.Contains(p.Reason, "frame-ancestors") {
		t.Fatalf("blocked after redirect: %+v %v", p, err)
	}
	p, err = probeEmbed(context.Background(), c, srv.URL+"/open", "https://c.example.org")
	if err != nil || p.Embed != wbEmbedFrame {
		t.Fatalf("open page: %+v %v", p, err)
	}
}

func TestFetchFeed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/md":
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.Write([]byte("# 读书\n- 三体"))
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"revision":3}`))
		case "/bin":
			w.Header().Set("Content-Type", "image/png")
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if s, err := fetchFeed(ctx, srv.Client(), srv.URL+"/md", "good"); err != nil || s != "# 读书\n- 三体" {
		t.Fatalf("markdown: %q %v", s, err)
	}
	if s, err := fetchFeed(ctx, srv.Client(), srv.URL+"/json", "good"); err != nil || !strings.Contains(s, "```json\n{\n  \"revision\": 3\n}") {
		t.Fatalf("json: %q %v", s, err)
	}
	if _, err := fetchFeed(ctx, srv.Client(), srv.URL+"/md", "bad"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("unauthorized: %v", err)
	}
	if _, err := fetchFeed(ctx, srv.Client(), srv.URL+"/bin", "good"); err == nil {
		t.Fatal("binary accepted")
	}
}

func TestClipBytes(t *testing.T) {
	s := strings.Repeat("读书一行\n", 5000)
	out := clipBytes(s, 1000)
	if len(out) > 1000 || !strings.HasSuffix(out, "省略了）\n") || !strings.Contains(out, "读书一行\n") {
		t.Fatalf("clip: %d %q", len(out), out[len(out)-40:])
	}
	if clipBytes("short", 100) != "short" {
		t.Fatal("short text changed")
	}
}

func TestWBAllowed(t *testing.T) {
	shared := webModule("books", "https://x/feed", wbShareRead)
	off := webModule("books", "https://x/feed", wbShareOff)
	secret := shared
	secret.Sensitive = true
	cases := []struct {
		name            string
		m               WBModule
		human, external bool
		ok              bool
	}{
		{"agent reads shared", shared, false, false, true},
		{"agent refused off", off, false, false, false},
		{"human sends off", off, true, false, true},
		{"sensitive inside", secret, false, false, true},
		{"sensitive outside", secret, false, true, false},
	}
	for _, c := range cases {
		if err := wbAllowed(c.m, c.human, c.external); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestWorkbenchSaveTokensAndPurge(t *testing.T) {
	wb := testWorkbench(t)
	if len(wb.config().Modules) == 0 {
		t.Fatal("no default shortcuts")
	}
	books := webModule("books", "https://books.example.com/agent.md", wbShareKeep)
	books.Sensitive = true
	cfg := wbConfig{Groups: []WBGroup{{ID: "fav", Title: "收藏"}}, Modules: []WBModule{books}}
	if err := wb.save(cfg, nil); err != nil {
		t.Fatal(err)
	}
	if err := wb.setToken("books", "secret-token"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(wb.secretsPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secrets file: %v %v", fi, err)
	}
	if err := wb.setToken("nope", "x"); err == nil {
		t.Fatal("token for unknown module")
	}
	idx, _ := os.ReadFile(filepath.Join(wb.snapDir(), "index.md"))
	if !strings.Contains(string(idx), "notes/wb-private-books.md") || !strings.Contains(string(idx), "敏感") {
		t.Fatalf("index: %s", idx)
	}
	// Reloaded from the database.
	again := newWorkbench(wb.db, filepath.Dir(wb.dir), wb.router)
	if m, ok := again.module("books"); !ok || !m.Sensitive {
		t.Fatalf("reload: %+v", m)
	}
	// A snapshot and a note, then the module goes and is purged.
	os.MkdirAll(wb.modDir("books"), 0o700)
	os.WriteFile(filepath.Join(wb.modDir("books"), "2026-11-25.md"), []byte("x"), 0o600)
	notes := filepath.Join(wb.router, "memory", "notes")
	os.MkdirAll(notes, 0o700)
	os.WriteFile(filepath.Join(notes, "wb-private-books.md"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(notes, "index.md"), []byte("# Notes\n- wb-private-books.md 读书\n- agents.md\n"), 0o600)
	cfg.Modules = nil
	if err := wb.save(cfg, []string{"books"}); err != nil {
		t.Fatal(err)
	}
	if tok, _ := wb.token("books"); tok != "" {
		t.Fatal("token kept for a removed module")
	}
	if _, err := os.Stat(wb.modDir("books")); !os.IsNotExist(err) {
		t.Fatal("snapshots kept")
	}
	if b, _ := os.ReadFile(filepath.Join(notes, "index.md")); strings.Contains(string(b), "wb-private-books") || !strings.Contains(string(b), "agents.md") {
		t.Fatalf("notes index: %s", b)
	}
	if err := wb.purge("../x"); err == nil {
		t.Fatal("purge outside the folder")
	}
}

func TestWorkbenchSnapshot(t *testing.T) {
	body := "# 读书\n- 三体"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		w.Write([]byte(body))
	}))
	defer srv.Close()
	wb := testWorkbench(t)
	m := webModule("books", srv.URL+"/agent.md", wbShareKeep)
	ctx := context.Background()
	day1 := time.Date(2026, 11, 24, 9, 0, 0, 0, time.Local)
	if err := wb.snapshot(ctx, m, day1); err != nil {
		t.Fatal(err)
	}
	// The same content the next day writes nothing, so no dream is due for it.
	day2 := day1.Add(24 * time.Hour)
	if err := wb.snapshot(ctx, m, day2); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(wb.modDir("books"), "*.md"))
	if len(files) != 1 {
		t.Fatalf("unchanged content wrote %v", files)
	}
	if wb.changedSince(day1.Add(time.Hour)) {
		t.Fatal("an unchanged feed counted as new")
	}
	body = "# 读书\n- 三体\n- 沙丘"
	if err := wb.snapshot(ctx, m, day2); err != nil {
		t.Fatal(err)
	}
	if p, _ := wb.latestSnap("books"); filepath.Base(p) != "2026-11-25.md" {
		t.Fatalf("latest %s", p)
	}
	if !wb.changedSince(day1.Add(time.Hour)) || wb.changedSince(day2.Add(time.Hour)) {
		t.Fatal("changedSince")
	}
	if err := wb.prune(time.Date(2026, 12, 24, 12, 0, 0, 0, time.Local)); err != nil {
		t.Fatal(err)
	}
	if files, _ := filepath.Glob(filepath.Join(wb.modDir("books"), "*.md")); len(files) != 1 || filepath.Base(files[0]) != "2026-11-25.md" {
		t.Fatalf("prune kept %v", files)
	}
}

func TestRouterOnExternal(t *testing.T) {
	sp := &Super{st: defaultState()}
	if sp.routerOnExternalLocked() {
		t.Fatal("idle router counted as external")
	}
	now := time.Now()
	sp.routerAskedAt = now
	sp.st.Triggers = []*Trigger{{ID: "t1", Thread: mainThread, Status: "delivered", At: now}}
	if sp.routerOnExternalLocked() {
		t.Fatal("chat:main counted as external")
	}
	sp.st.Triggers = append(sp.st.Triggers, &Trigger{ID: "t2", Thread: "ext:larkx:oc_1", Status: "delivered", At: now.Add(-time.Minute)})
	if !sp.routerOnExternalLocked() {
		t.Fatal("a turn with a 飞书 event is external")
	}
	sp.st.Triggers[1].At = now.Add(-2 * routerExternalWindow)
	sp.st.Triggers[0], sp.st.Triggers[1] = sp.st.Triggers[1], sp.st.Triggers[0]
	if sp.routerOnExternalLocked() {
		t.Fatal("an old outside event still counted")
	}
}

func TestWorkbenchDue(t *testing.T) {
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	wb := testWorkbench(t)
	m := webModule("books", srv.URL, wbShareKeep)
	if err := wb.save(wbConfig{Groups: []WBGroup{{ID: "fav", Title: "收藏"}}, Modules: []WBModule{m}}, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if len(wb.due(now)) != 1 {
		t.Fatal("a module never snapshotted is due")
	}
	err := wb.snapshot(context.Background(), m, now)
	if err == nil {
		t.Fatal("a failing feed snapshotted")
	}
	wb.checked["books"] = wbCheck{at: now, err: err.Error()}
	if len(wb.due(now.Add(time.Minute))) != 0 || len(wb.due(now.Add(wbSnapRetry))) != 1 {
		t.Fatal("a failure is retried an hour later, not sooner")
	}
	fail = false
	if err := wb.snapshot(context.Background(), m, now); err != nil {
		t.Fatal(err)
	}
	wb.checked["books"] = wbCheck{at: now.Add(time.Hour)}
	if len(wb.due(now.Add(2*time.Hour))) != 0 || len(wb.due(now.Add(time.Hour+wbSnapEvery))) != 1 {
		t.Fatal("a read that found nothing new waits a full interval")
	}
	// Leaving keep forgets the attempt.
	m.Share = wbShareRead
	if err := wb.save(wbConfig{Groups: []WBGroup{{ID: "fav", Title: "收藏"}}, Modules: []WBModule{m}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := wb.checked["books"]; ok {
		t.Fatal("checked kept for a module no longer kept")
	}
}
