package collie

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func isolateHistory(t *testing.T) {
	t.Helper()
	history.Lock()
	prevList, prevLoaded := history.list, history.loaded
	history.list, history.loaded = nil, false
	history.Unlock()
	t.Cleanup(func() {
		history.Lock()
		history.list, history.loaded = prevList, prevLoaded
		history.Unlock()
	})
}

func TestFinishHandoverLinksHistory(t *testing.T) {
	isolateHistory(t)
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "herd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	srv := &Server{token: "tok", home: dir}
	sp := &Super{
		s:    srv,
		db:   db,
		dir:  dir,
		st:   defaultState(),
		subs: map[chan struct{}]bool{},
		handover: &handover{
			at:      time.Date(2026, 11, 23, 16, 20, 0, 0, time.Local),
			kind:    "claude",
			session: "sess-handover-1",
			pct:     65,
		},
		routerUsage: ctxUsage{Used: 130000, Window: 200000},
	}
	srv.sup = sp

	sp.finishHandover(context.Background(), sp.handover)
	if sp.handover != nil || sp.routerUsage.Pct() != 0 {
		t.Fatalf("handover state not cleared: %+v %+v", sp.handover, sp.routerUsage)
	}
	if len(sp.st.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(sp.st.Entries))
	}
	got := sp.st.Entries[0]
	if got.Role != "event" || got.Tone != "info" {
		t.Fatalf("unexpected entry role/tone: %+v", got)
	}
	if got.Ref == routerName || !strings.HasPrefix(got.Ref, "h:") || len(got.Ref) <= 2 {
		t.Fatalf("ref = %q, want non-empty h:<id> (not %q)", got.Ref, routerName)
	}
	if got.Text != "Collie 换了一个新会话，记忆已交接，点此翻看之前的对话" {
		t.Fatalf("text = %q", got.Text)
	}

	hid := strings.TrimPrefix(got.Ref, "h:")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/history", nil)
	srv.listHistory(rec, req)
	var histResp struct {
		Sessions []*Ended `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &histResp); err != nil {
		t.Fatal(err)
	}
	if len(histResp.Sessions) != 1 || histResp.Sessions[0].ID != hid || histResp.Sessions[0].Session != "sess-handover-1" || histResp.Sessions[0].Name != routerName {
		t.Fatalf("history = %+v, want id=%s session=sess-handover-1", histResp.Sessions, hid)
	}
}

func TestReplyOptionsStoreDBAndAPI(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "herd.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Seed a pre-upgrade entry row without "options" to verify backward-compatible migration.
	oldAt := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)
	legacyJSON := `{"id":"e1","role":"collie","thread":"chat:main","text":"旧版本消息","at":"` + oldAt.Format(time.RFC3339) + `"}`
	if _, err := db.Exec(`INSERT INTO entries(id,n,at,thread,data) VALUES('e1',1,?,'chat:main',?)`, oldAt.Unix(), legacyJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO kv(k,v) VALUES('seq','1')`); err != nil {
		t.Fatal(err)
	}

	st, err := loadState(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Entries) != 1 || st.Entries[0].ID != "e1" || len(st.Entries[0].Options) != 0 {
		t.Fatalf("legacy entry load failed: %+v", st.Entries)
	}

	srv := &Server{token: "secret-test-tok", home: dir, web: fstest.MapFS{}}
	sp := &Super{s: srv, db: db, dir: dir, st: st, subs: map[chan struct{}]bool{}}
	srv.sup = sp
	mux := http.NewServeMux()
	srv.superRoutes(mux)

	call := func(method, path string, body any) *httptest.ResponseRecorder {
		var rd *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		} else {
			rd = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, rd)
		req.Header.Set("Authorization", "Bearer secret-test-tok")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// POST /api/super/reply trims blanks, deduplicates, and caps at 6 options.
	rec := call("POST", "/api/super/reply", map[string]any{
		"text":    "本地改好了，要怎么处理？",
		"options": []string{"  修并升级 ", "", "   ", "先只改本地", "修并升级", "不用了", "选项四", "选项五", "选项六", "多余第七项"},
	})
	if rec.Code != 200 {
		t.Fatalf("reply status = %d: %s", rec.Code, rec.Body.String())
	}
	var replyOut struct {
		Entry string `json:"entry"`
	}
	json.Unmarshal(rec.Body.Bytes(), &replyOut)
	wantOpts := []string{"修并升级", "先只改本地", "不用了", "选项四", "选项五", "选项六"}

	// GET /api/super returns options intact.
	snapRec := call("GET", "/api/super", nil)
	var snap superView
	if err := json.Unmarshal(snapRec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Entries) != 2 || !slices.Equal(snap.Entries[1].Options, wantOpts) {
		t.Fatalf("/api/super entries = %+v, want options %v", snap.Entries, wantOpts)
	}
	if snap.Entries[0].Options != nil {
		t.Fatalf("legacy entry options = %v, want nil", snap.Entries[0].Options)
	}

	// Clicking an option sends POST /api/super/say with re: e.id.
	sayRec := call("POST", "/api/super/say", map[string]any{"text": "修并升级", "re": replyOut.Entry})
	if sayRec.Code != 200 {
		t.Fatalf("say status = %d: %s", sayRec.Code, sayRec.Body.String())
	}
	var sayOut struct {
		Entry string `json:"entry"`
	}
	json.Unmarshal(sayRec.Body.Bytes(), &sayOut)

	// Flush to SQLite, reload fresh state, and verify round-trip persistence.
	sp.flush()
	reloaded, err := loadState(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Entries) != 3 || !slices.Equal(reloaded.Entries[1].Options, wantOpts) {
		t.Fatalf("reloaded entries = %+v, want options %v", reloaded.Entries, wantOpts)
	}
	if reloaded.Entries[2].Re == nil || reloaded.Entries[2].Re.Entry != replyOut.Entry {
		t.Fatalf("user reply quote not linked: %+v", reloaded.Entries[2].Re)
	}

	// GET /api/super/earlier returns options from SQLite history before the user reply.
	earlierRec := call("GET", "/api/super/earlier?before="+sayOut.Entry, nil)
	if earlierRec.Code != 200 {
		t.Fatalf("earlier status = %d: %s", earlierRec.Code, earlierRec.Body.String())
	}
	var earlierOut struct {
		Entries []*Entry `json:"entries"`
	}
	if err := json.Unmarshal(earlierRec.Body.Bytes(), &earlierOut); err != nil {
		t.Fatal(err)
	}
	if len(earlierOut.Entries) != 2 || !slices.Equal(earlierOut.Entries[1].Options, wantOpts) {
		t.Fatalf("/api/super/earlier entries = %+v, want options %v", earlierOut.Entries, wantOpts)
	}
}

func TestLarkNotifyOptionsInQuietMode(t *testing.T) {
	sent := fakeLark(t)
	sp := &Super{
		st:   defaultState(),
		subs: map[chan struct{}]bool{},
	}
	sp.st.Settings.NotifyLark = true
	sp.st.Settings.NotifyLevel = "quiet"
	sp.seenAt = time.Now().Add(-5 * time.Minute)

	// Routine reply without options stays silent in quiet mode.
	sp.Reply(mainThread, "普通进展汇报", "", nil)
	sp.sendNotifications()
	if _, err := os.Stat(sent); err == nil {
		t.Fatal("plain reply notified in quiet mode")
	}

	// Reply with options notifies as an orange "needs you" card even in quiet mode.
	sp.notifyAt = time.Time{}
	sp.Reply(mainThread, "本地改好了，要现在发版升级吗？", "", []string{"修并升级", "先只改本地"})
	sp.sendNotifications()
	b, err := os.ReadFile(sent)
	if err != nil {
		t.Fatal("reply with options did not notify")
	}
	got := string(b)
	for _, want := range []string{`"template":"orange"`, "Collie · 需要你", "可选：「修并升级」 · 「先只改本地」"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

func TestCLIReplyOptParsingAndCall(t *testing.T) {
	body, err := parseReply([]string{"--re", "t12", "--opt", "修并升级", "--opt", "先只改本地", "--opt", "不用了", "要怎么处理？"}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if body["text"] != "要怎么处理？" || body["re"] != "t12" || body["thread"] != mainThread {
		t.Fatalf("unexpected body: %+v", body)
	}
	opts, _ := body["options"].([]string)
	if !slices.Equal(opts, []string{"修并升级", "先只改本地", "不用了"}) {
		t.Fatalf("options = %v", opts)
	}

	plain, err := parseReply([]string{"收到了"}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := plain["options"]; has {
		t.Fatalf("unexpected options key on plain reply: %+v", plain)
	}

	piped, err := parseReply([]string{"--opt", "确认"}, strings.NewReader("来自管道的提问"), false)
	if err != nil || piped["text"] != "来自管道的提问" {
		t.Fatalf("piped = %+v, err = %v", piped, err)
	}
	if _, err := parseReply([]string{"--opt", "确认"}, nil, true); err == nil {
		t.Fatal("expected error for missing reply text")
	}

	// End-to-end cliReply over loopback HTTP using isolated COLLIE_HOME.
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/super/reply" || r.Header.Get("Authorization") != "Bearer cli-tok" {
			http.Error(w, "bad request", 400)
			return
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"entry":"e9"}`))
	}))
	defer ts.Close()

	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "token"), []byte("cli-tok\n"), 0o600)
	os.WriteFile(filepath.Join(home, "addr"), []byte(ts.URL+"\n"), 0o600)
	t.Setenv("COLLIE_HOME", home)

	if err := cliReply([]string{"--opt", "开 PR", "--opt", "先不用", "要我开 PR 吗？"}); err != nil {
		t.Fatalf("cliReply: %v", err)
	}
	rawOpts, _ := gotBody["options"].([]any)
	if gotBody["text"] != "要我开 PR 吗？" || len(rawOpts) != 2 || rawOpts[0] != "开 PR" || rawOpts[1] != "先不用" {
		t.Fatalf("server got %+v", gotBody)
	}
	if _, hasCheck := gotBody["check"]; hasCheck {
		t.Fatalf("unexpected check field when --check not set: %+v", gotBody)
	}

	// --check standalone + compatible with --re and --thread.
	chkBody, err := parseReply([]string{"--check", "--re", "t88", "--thread", "chat:main", "有两条待你本人处理的飞书消息"}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if chkBody["check"] != true || chkBody["re"] != "t88" || chkBody["thread"] != "chat:main" || chkBody["text"] != "有两条待你本人处理的飞书消息" {
		t.Fatalf("unexpected --check body: %+v", chkBody)
	}
	if _, hasOpts := chkBody["options"]; hasOpts {
		t.Fatalf("unexpected options on --check-only reply: %+v", chkBody)
	}

	gotBody = nil
	if err := cliReply([]string{"--check", "--re", "t99", "请审阅这份报告"}); err != nil {
		t.Fatalf("cliReply --check: %v", err)
	}
	if gotBody["check"] != true || gotBody["re"] != "t99" || gotBody["text"] != "请审阅这份报告" {
		t.Fatalf("server got --check %+v", gotBody)
	}

	// routerDoc documents --check concisely alongside reply.
	if !strings.Contains(routerDoc, "[--check]") || !strings.Contains(routerDoc, "`--check`") {
		t.Fatalf("routerDoc missing --check documentation")
	}
}

func TestLarkNotifyCheckInQuietMode(t *testing.T) {
	sent := fakeLark(t)
	sp := &Super{
		st:   defaultState(),
		subs: map[chan struct{}]bool{},
	}
	sp.st.Settings.NotifyLark = true
	sp.st.Settings.NotifyLevel = "quiet"
	sp.seenAt = time.Now().Add(-5 * time.Minute)

	sp.Reply(mainThread, "发现两处异常指标待你过目", "", nil, true)
	sp.sendNotifications()
	b, err := os.ReadFile(sent)
	if err != nil {
		t.Fatal("reply with --check did not notify in quiet mode")
	}
	got := string(b)
	for _, want := range []string{`"template":"orange"`, "Collie · 需要你", "发现两处异常指标待你过目"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "可选：") {
		t.Errorf("--check without options should not render options line:\n%s", got)
	}
}
