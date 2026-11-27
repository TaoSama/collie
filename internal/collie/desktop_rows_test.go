package collie

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// codexStateFixture makes a CODEX_HOME with an empty App thread table, and
// returns a function that adds a thread updated at the given time.
func codexStateFixture(t *testing.T) (string, func(id, first string, updated time.Time)) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, ".codex", "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE threads (id TEXT, name TEXT, title TEXT, first_user_message TEXT, cwd TEXT, rollout_path TEXT,
		created_at INTEGER, created_at_ms INTEGER, updated_at INTEGER, updated_at_ms INTEGER, originator TEXT, archived INTEGER, source TEXT)`); err != nil {
		t.Fatal(err)
	}
	return home, func(id, first string, updated time.Time) {
		if _, err := db.Exec(`INSERT INTO threads VALUES (?, '', ?, ?, '/root', '', ?, ?, ?, ?, '', 0, 'vscode')`,
			id, first, first, updated.Unix(), updated.UnixMilli(), updated.Unix(), updated.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodexAppRowsSkipCollieWorkers(t *testing.T) {
	home, add := codexStateFixture(t)
	now := time.Now()
	add("worker", "[collie] 归档 devbox 上的 4 个 Codex 会话，完成后 collie report。", now)
	add("mine", "看看日志", now)
	rows := (&Server{home: home}).codexAppRows()
	if len(rows) != 1 || rows[0].session != "mine" {
		t.Fatalf("rows = %+v, want only the thread that isn't a Collie worker's", rows)
	}
}

// Workers are newer than the App's threads, and must not use up the places.
func TestCodexAppRowsWorkersTakeNoPlaces(t *testing.T) {
	home, add := codexStateFixture(t)
	now := time.Now()
	for i := range 10 {
		add(fmt.Sprintf("worker%d", i), "You are worker \"w\", managed by Collie", now.Add(-time.Duration(i)*time.Minute))
	}
	for i := range codexMax + 5 {
		add(fmt.Sprintf("mine%d", i), "看看日志", now.Add(-time.Hour-time.Duration(i)*time.Minute))
	}
	rows := (&Server{home: home}).codexAppRows()
	if len(rows) != codexMax {
		t.Fatalf("got %d rows, want %d", len(rows), codexMax)
	}
	if rows[0].session != "mine0" || rows[codexMax-1].session != fmt.Sprintf("mine%d", codexMax-1) {
		t.Fatalf("rows run %s..%s, want the newest of the App's own threads", rows[0].session, rows[codexMax-1].session)
	}
}

// An active automation's thread shows past the cap; a paused one's doesn't.
func TestCodexAppRowsKeepAutomationThreads(t *testing.T) {
	home, add := codexStateFixture(t)
	old := time.Now().Add(-30 * 24 * time.Hour)
	add("monthly", "Automation: 月度统计", old)
	for i := range codexMax {
		add(fmt.Sprintf("mine%d", i), "看看日志", time.Now())
	}
	add("paused", "Automation: 已暂停", old)
	add("stale", "很久以前", old)
	for id, status := range map[string]string{"monthly": "ACTIVE", "paused": "PAUSED"} {
		dir := filepath.Join(home, ".codex", "automations", id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		toml := fmt.Sprintf("version = 1\nid = %q\nkind = \"heartbeat\"\nstatus = %q\ntarget_thread_id = %q\n", id, status, id)
		if err := os.WriteFile(filepath.Join(dir, "automation.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rows := (&Server{home: home}).codexAppRows()
	if len(rows) != codexMax+1 || rows[codexMax].session != "monthly" {
		t.Fatalf("got %d rows ending %q, want %d recent ones and the active automation's thread", len(rows), rows[len(rows)-1].session, codexMax)
	}
}

// The apps list their old conversations too: so does Collie, up to each cap.
func TestCodexAppRowsKeepOldThreads(t *testing.T) {
	home, add := codexStateFixture(t)
	add("new", "看看日志", time.Now())
	add("old", "上个月的事", time.Now().Add(-60*24*time.Hour))
	rows := (&Server{home: home}).codexAppRows()
	if len(rows) != 2 || rows[1].session != "old" {
		t.Fatalf("rows = %+v, want the new thread and the old one", rows)
	}
}

func TestClaudeDesktopRowsKeepOldSessions(t *testing.T) {
	home := t.TempDir()
	app := filepath.Join(home, "Library", "Application Support", "Claude")
	dir := filepath.Join(app, "claude-code-sessions", "a", "b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-60 * 24 * time.Hour).UnixMilli()
	for i := range desktopMax + 2 {
		v := map[string]any{"sessionId": fmt.Sprintf("local_s%d", i), "cliSessionId": fmt.Sprintf("c%d", i), "cwd": "/root",
			"title": "旧会话", "createdAt": old, "lastActivityAt": old - int64(i)*60_000, "isArchived": i == 0}
		b, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("local_s%d.json", i)), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	claude := filepath.Join(home, ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	project := filepath.Join(claude, "projects", "test-project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(project, "c1.jsonl")
	if err := os.WriteFile(transcript, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rows := (&Server{home: home}).claudeDesktopRows([]string{app})
	if len(rows) != desktopMax || rows[0].session != "c1" {
		t.Fatalf("got %d rows starting %+v, want the %d newest unarchived ones however old", len(rows), rows, desktopMax)
	}
	if rows[0].path != transcript || rows[0].format != "claude" {
		t.Fatalf("newest transcript = %q (%s), want %q (claude)", rows[0].path, rows[0].format, transcript)
	}
}

func TestAgyDesktopRowsKeepOldConversations(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".gemini", "antigravity")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE conversation_summaries (conversation_id TEXT, title TEXT, preview TEXT, workspace_uris TEXT, status TEXT,
		not_fully_idle NUMERIC, last_modified_time DATETIME, last_user_input_time DATETIME, nesting_depth INTEGER, killed NUMERIC)`); err != nil {
		t.Fatal(err)
	}
	for i := range desktopMax + 2 {
		at := time.Now().Add(-60*24*time.Hour - time.Duration(i)*time.Minute).UTC().Format("2006-01-02 15:04:05")
		if _, err := db.Exec(`INSERT INTO conversation_summaries VALUES (?, '旧对话', '', '["file:///root"]', '', 0, ?, ?, 0, 0)`,
			fmt.Sprintf("g%d", i), at, at); err != nil {
			t.Fatal(err)
		}
	}
	rows := (&Server{home: home}).agyDesktopRows()
	if len(rows) != desktopMax || rows[0].session != "g0" {
		t.Fatalf("got %d rows, want the %d newest however old", len(rows), desktopMax)
	}
}

func TestCodexSurface(t *testing.T) {
	for originator, want := range map[string]string{
		"": surfaceDesktop, "Codex Desktop": surfaceDesktop, "codex_work_desktop": surfaceDesktop,
		"codex-tui": surfaceCLI, "codex_cli_rs": surfaceCLI,
		"codex_exec": "", "some-ide": "",
	} {
		if got := codexSurface(originator); got != want {
			t.Errorf("codexSurface(%q) = %q, want %q", originator, got, want)
		}
	}
}

func TestCodexClient(t *testing.T) {
	// The CLI's threads read as herdr's CLI rows do; others by their client.
	for originator, want := range map[string]string{"codex-tui": "codex", "codex_cli_rs": "codex", "some-ide": "some-ide（Codex）"} {
		if got := codexClient(originator, ""); got != want {
			t.Errorf("codexClient(%q) = %q, want %q", originator, got, want)
		}
	}
	if got := codexClient("Codex Desktop", ""); !isCodexApp(got) {
		t.Errorf("codexClient(Codex Desktop) = %q, want the App", got)
	}
}

func TestAgySurface(t *testing.T) {
	for source, want := range map[string]string{
		"CORTEX_TRAJECTORY_SOURCE_CLI": surfaceCLI, "CORTEX_TRAJECTORY_SOURCE_VSCODE": "", "": "",
	} {
		if got := agySurface(source); got != want {
			t.Errorf("agySurface(%q) = %q, want %q", source, got, want)
		}
	}
}

func TestAgyDesktopRowsNameTheirClient(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".gemini", "antigravity")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE conversation_summaries (conversation_id TEXT, title TEXT, preview TEXT, workspace_uris TEXT, status TEXT,
		not_fully_idle NUMERIC, last_modified_time DATETIME, last_user_input_time DATETIME, nesting_depth INTEGER, killed NUMERIC, source TEXT)`); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Format("2006-01-02 15:04:05")
	for id, source := range map[string]string{"cli": "CORTEX_TRAJECTORY_SOURCE_CLI", "app": "CORTEX_TRAJECTORY_SOURCE_VSCODE"} {
		if _, err := db.Exec(`INSERT INTO conversation_summaries VALUES (?, '对话', '', '["file:///root"]', '', 0, ?, ?, 0, 0, ?)`, id, at, at, source); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]string{}
	for _, r := range (&Server{home: home}).agyDesktopRows() {
		got[r.session] = r.app
	}
	// The CLI's reads as herdr's CLI rows do ("antigravity", like "claude"
	// and "codex"); the app's as the other apps' do.
	if got["cli"] != "antigravity" || got["app"] != "Antigravity 桌面版" {
		t.Fatalf("apps = %v, want cli: antigravity, app: Antigravity 桌面版", got)
	}
}

func TestAgyDesktopRowsSkipEmptyConversations(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".gemini", "antigravity")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE conversation_summaries (conversation_id TEXT, title TEXT, preview TEXT, step_count INTEGER, workspace_uris TEXT, status TEXT,
		not_fully_idle NUMERIC, last_modified_time DATETIME, last_user_input_time DATETIME, nesting_depth INTEGER, killed NUMERIC)`); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Format("2006-01-02 15:04:05")
	zero := "0001-01-01 00:00:00+00:00" // how the app writes a time it never set
	for _, c := range []struct {
		id    string
		steps int
		mod   string
	}{{"ok", 3, at}, {"shell", 0, zero}, {"no-steps", 0, at}, {"no-time", 3, zero}} {
		if _, err := db.Exec(`INSERT INTO conversation_summaries VALUES (?, '对话', '', ?, '["file:///root"]', '', 0, ?, ?, 0, 0)`, c.id, c.steps, c.mod, c.mod); err != nil {
			t.Fatal(err)
		}
	}
	rows := (&Server{home: home}).agyDesktopRows()
	if len(rows) != 1 || rows[0].session != "ok" {
		t.Fatalf("rows = %+v, want only the conversation with steps and a time", rows)
	}
}

func TestAgyDesktopSessionsTakeOverOnlyTheCLIs(t *testing.T) {
	desktopCache.Lock()
	desktopCache.at, desktopCache.app = time.Now(), false
	desktopCache.rows = []desktopRow{
		{id: "ag-idle", kind: "agy", surface: surfaceCLI, session: "idle", cwd: "/root"},
		{id: "ag-app", kind: "agy", session: "app", cwd: "/root"},
		{id: "ag-running", kind: "agy", surface: surfaceCLI, session: "running", cwd: "/root", running: true},
		{id: "ag-waiting", kind: "agy", surface: surfaceCLI, session: "waiting", cwd: "/root", waiting: true},
		{id: "ag-nowhere", kind: "agy", surface: surfaceCLI, session: "nowhere"},
	}
	desktopCache.Unlock()
	t.Cleanup(forgetDesktopRows)
	got := map[string]bool{}
	for _, e := range (&Server{home: t.TempDir()}).desktopSessions(nil, nil, time.Now()) {
		got[e.Session] = e.CanTakeOver
	}
	// Only a resting conversation the CLI started, with a directory to resume
	// it in: the CLI can't open the App's own, and a running turn has no
	// process here to stop first.
	want := map[string]bool{"idle": true, "app": false, "running": false, "waiting": false, "nowhere": false}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("CanTakeOver[%s] = %v, want %v", id, got[id], w)
		}
	}
}

func TestAgyDesktopSessionsDoNotRouteCLIToApp(t *testing.T) {
	home := t.TempDir()
	lockedID := "11111111-1111-1111-1111-111111111111"
	presDir := filepath.Join(home, ".gemini", "antigravity-cli", "presence")
	if err := os.MkdirAll(presDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lf, err := os.OpenFile(filepath.Join(presDir, lockedID+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lf.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		lf.Close()
	})

	agyServerCache.Lock()
	agyServerCache.at = time.Now()
	agyServerCache.ls = &agyServer{addr: "127.0.0.1:1", csrf: "test-token"}
	agyServerCache.Unlock()
	t.Cleanup(forgetAgyServer)

	desktopCache.Lock()
	desktopCache.at, desktopCache.app = time.Now(), false
	desktopCache.rows = []desktopRow{
		{id: "ag-cli-idle", kind: "agy", app: appAgyCLI, surface: surfaceCLI, session: "22222222-2222-2222-2222-222222222222", cwd: "/root"},
		{id: "ag-cli-locked", kind: "agy", app: appAgyCLI, surface: surfaceCLI, session: lockedID, cwd: "/root"},
		{id: "ag-cli-run", kind: "agy", app: appAgyCLI, surface: surfaceCLI, session: "cli-run", cwd: "/root", running: true},
		{id: "ag-app-idle", kind: "agy", app: appAgy, session: "app-idle", cwd: "/root"},
	}
	desktopCache.Unlock()
	t.Cleanup(forgetDesktopRows)

	got := map[string]*External{}
	for _, e := range (&Server{home: home}).desktopSessions(nil, nil, time.Now()) {
		got[e.Session] = e
	}
	for _, tc := range []struct {
		id       string
		canSend  bool
		via      string
		readOnly string
		takeOver bool
	}{
		{"22222222-2222-2222-2222-222222222222", true, "headless", "", true},
		{lockedID, false, "", "这个会话正在 Antigravity 终端里打开，不支持从外面直接发消息", false},
		{"cli-run", false, "", "这个会话正在别处运行，Antigravity CLI 不支持排队发消息", false},
		{"app-idle", true, "app", "", false},
	} {
		e := got[tc.id]
		if e == nil || e.CanSend != tc.canSend || e.SendVia != tc.via || e.ReadOnly != tc.readOnly || e.CanTakeOver != tc.takeOver {
			t.Fatalf("session %s = %+v, want CanSend=%v SendVia=%q ReadOnly=%q CanTakeOver=%v", tc.id, e, tc.canSend, tc.via, tc.readOnly, tc.takeOver)
		}
	}
}

func TestAgyDesktopRowsReadTheCLIsIndexWithoutTheApp(t *testing.T) {
	home := t.TempDir()
	add := agyIndexFixture(t, filepath.Join(home, ".gemini", "antigravity-cli"))
	add("cli", time.Now(), false)
	add("gone", time.Now(), false) // its conversation was deleted, its row left
	agyTranscriptFixture(t, home, "cli")
	rows := (&Server{home: home}).agyDesktopRows()
	// The CLI leaves source empty in its own index: all its rows are the CLI's.
	if len(rows) != 1 || rows[0].session != "cli" || rows[0].surface != surfaceCLI || rows[0].app != appAgyCLI {
		t.Fatalf("rows = %+v, want the CLI's conversation with a log, as the CLI's", rows)
	}
}

// agyTranscriptFixture gives a conversation of the CLI's its log.
func agyTranscriptFixture(t *testing.T, home, id string) {
	t.Helper()
	logs := filepath.Join(home, ".gemini", "antigravity-cli", "brain", id, ".system_generated", "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "transcript.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAgyDesktopRowsReadOnlyTheAppsIndexWhereItIs(t *testing.T) {
	home := t.TempDir()
	agyIndexFixture(t, filepath.Join(home, ".gemini", "antigravity"))("app", time.Now(), false)
	agyIndexFixture(t, filepath.Join(home, ".gemini", "antigravity-cli"))("cli", time.Now(), false)
	rows := (&Server{home: home}).agyDesktopRows()
	if len(rows) != 1 || rows[0].session != "app" {
		t.Fatalf("rows = %+v, want the App's index alone: it lists the CLI's too", rows)
	}
}

func TestCodexDesktopSessionsAllowSendWhileRunning(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	t.Setenv("CODEX_HOME", codexHome)
	control := filepath.Join(codexHome, "app-server-control")
	if err := os.MkdirAll(control, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "app-server-control.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	codexLoadedCache.Lock()
	codexLoadedCache.at, codexLoadedCache.ids, codexLoadedCache.err = time.Now(), map[string]bool{}, nil
	codexLoadedCache.Unlock()
	t.Cleanup(func() {
		codexLoadedCache.Lock()
		codexLoadedCache.at = time.Time{}
		codexLoadedCache.Unlock()
	})
	desktopCache.Lock()
	desktopCache.at, desktopCache.app = time.Now(), false
	desktopCache.rows = []desktopRow{
		{id: "cx-idle", kind: "codex", app: appCodex, session: "idle", cwd: "/root"},
		{id: "cx-run", kind: "codex", app: appCodex, session: "run", cwd: "/root", running: true},
	}
	desktopCache.Unlock()
	t.Cleanup(forgetDesktopRows)
	got := map[string]*External{}
	for _, e := range (&Server{home: home}).desktopSessions(nil, nil, time.Now()) {
		got[e.Session] = e
	}
	for _, tc := range []struct {
		id       string
		takeOver bool
	}{{"idle", true}, {"run", false}} {
		e := got[tc.id]
		if e == nil || !e.CanSend || e.SendVia != "queue" || e.ReadOnly != "" || e.CanTakeOver != tc.takeOver {
			t.Fatalf("session %s = %+v, want CanSend=true SendVia=queue ReadOnly='' CanTakeOver=%v", tc.id, e, tc.takeOver)
		}
	}
}

func TestCodexCLIRowsAndSendViaQueueCLI(t *testing.T) {
	home, add := codexStateFixture(t)
	db, err := sql.Open("sqlite", filepath.Join(home, ".codex", "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	if _, err := db.Exec(`INSERT INTO threads VALUES ('cli-1', '', 'cli title', 'cli title', '/work', '', ?, ?, ?, ?, '', 0, 'cli')`,
		now.Unix(), now.UnixMilli(), now.Unix(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	add("desk-1", "desktop title", now.Add(-time.Minute))

	rows := (&Server{home: home}).codexAppRows()
	if len(rows) != 2 {
		t.Fatalf("codexAppRows len = %d, want 2: %+v", len(rows), rows)
	}
	if rows[0].session != "cli-1" || rows[0].app != "codex" || rows[0].surface != surfaceCLI {
		t.Fatalf("cli row = %+v, want session=cli-1 app=codex surface=cli", rows[0])
	}

	codexLoadedCache.Lock()
	codexLoadedCache.at, codexLoadedCache.ids, codexLoadedCache.err = time.Now(), map[string]bool{"cli-loaded": true}, nil
	codexLoadedCache.Unlock()
	t.Cleanup(func() {
		codexLoadedCache.Lock()
		codexLoadedCache.at = time.Time{}
		codexLoadedCache.Unlock()
	})
	desktopCache.Lock()
	desktopCache.at, desktopCache.app = time.Now(), true // even with old appUp, CLI sessions must not route to App IPC
	desktopCache.rows = []desktopRow{
		{id: "cx-cli-idle", kind: "codex", app: "codex", surface: surfaceCLI, session: "cli-idle", cwd: "/work"},
		{id: "cx-cli-run", kind: "codex", app: "codex", surface: surfaceCLI, session: "cli-run", cwd: "/work", running: true},
		{id: "cx-cli-loaded", kind: "codex", app: "codex", surface: surfaceCLI, session: "cli-loaded", cwd: "/work", running: true},
	}
	desktopCache.Unlock()
	t.Cleanup(forgetDesktopRows)

	// Without shared daemon socket: exited CLI stays read-only with manual takeover.
	got := map[string]*External{}
	for _, e := range (&Server{home: home}).desktopSessions(nil, nil, time.Now()) {
		got[e.Session] = e
	}
	if e := got["cli-idle"]; e == nil || e.CanSend || e.SendVia != "" || !e.CanTakeOver || e.ReadOnly == "" {
		t.Fatalf("cli-idle (no daemon) = %+v, want read-only with CanTakeOver=true", e)
	}
	if e := got["cli-run"]; e == nil || e.CanSend || e.SendVia != "" || e.CanTakeOver || e.ReadOnly == "" {
		t.Fatalf("cli-run = %+v, want read-only running CLI", e)
	}
	if e := got["cli-loaded"]; e == nil || !e.CanSend || e.SendVia != "queue-cli" {
		t.Fatalf("cli-loaded = %+v, want daemon CanSend=true SendVia=queue-cli", e)
	}

	// With shared daemon socket: exited CLI routes natively via queue-cli (no herdr auto-takeover).
	control := filepath.Join(home, ".codex", "app-server-control")
	if err := os.MkdirAll(control, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "app-server-control.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got = map[string]*External{}
	for _, e := range (&Server{home: home}).desktopSessions(nil, nil, time.Now()) {
		got[e.Session] = e
	}
	if e := got["cli-idle"]; e == nil || !e.CanSend || e.SendVia != "queue-cli" || !e.CanTakeOver || e.ReadOnly != "" {
		t.Fatalf("cli-idle (with daemon) = %+v, want CanSend=true SendVia=queue-cli CanTakeOver=true", e)
	}
}

func TestClaudeCLIRowsAndSendViaHeadless(t *testing.T) {
	home := t.TempDir()
	claude := filepath.Join(home, ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	proj := filepath.Join(claude, "projects", "-work")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		p := filepath.Join(proj, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const (
		cliID    = "11111111-1111-1111-1111-111111111111"
		deskID   = "22222222-2222-2222-2222-222222222222"
		workerID = "33333333-3333-3333-3333-333333333333"
	)
	write(cliID+".jsonl", `{"type":"user","cwd":"/work","timestamp":"2026-11-27T10:00:00Z","message":{"role":"user","content":"帮我看下日志"}}`+"\n")
	write(deskID+".jsonl", `{"type":"user","cwd":"/work","timestamp":"2026-11-27T10:00:00Z","message":{"role":"user","content":"桌面会话"}}`+"\n")
	write(workerID+".jsonl", `{"type":"user","cwd":"/work","timestamp":"2026-11-27T10:00:00Z","message":{"role":"user","content":"[collie] 1 new event"}}`+"\n")
	write("agent-sub123.jsonl", `{"type":"user","cwd":"/work","message":{"role":"user","content":"子代理"}}`+"\n")

	s := &Server{home: home}
	desktop := []desktopRow{{id: "cd-1", kind: "claude", app: appClaude, surface: surfaceDesktop, session: deskID, cwd: "/work"}}
	rows := s.claudeCLIRows(desktop)
	if len(rows) != 1 || rows[0].session != cliID || rows[0].surface != surfaceCLI || rows[0].app != "claude" || rows[0].cwd != "/work" || rows[0].title != "帮我看下日志" {
		t.Fatalf("claudeCLIRows = %+v, want only cliID", rows)
	}

	desktopCache.Lock()
	desktopCache.at, desktopCache.app, desktopCache.rows = time.Now(), false, rows
	desktopCache.Unlock()
	t.Cleanup(forgetDesktopRows)
	got := s.desktopSessions(nil, nil, time.Now())
	if len(got) != 1 || !got[0].CanSend || got[0].SendVia != "headless" || !got[0].CanTakeOver || got[0].ReadOnly != "" {
		t.Fatalf("desktopSessions(claude CLI) = %+v, want CanSend=true SendVia=headless CanTakeOver=true", got)
	}

	// When a live registration owns cliID, route via peer inbox (never headless).
	regs := map[int]*claudeReg{
		os.Getpid(): {PID: os.Getpid(), SessionID: cliID, Status: "idle", Socket: "/tmp/peer.sock", Protocol: 1},
	}
	got = s.desktopSessions(regs, nil, time.Now())
	if len(got) != 1 || !got[0].Live || !got[0].CanSend || got[0].SendVia != "" {
		t.Fatalf("desktopSessions(live claude CLI) = %+v, want Live=true CanSend=true SendVia=''", got)
	}
}
