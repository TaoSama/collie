package collie

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	if runtime.GOOS != "darwin" {
		t.Skip("the Claude desktop app's index is read on macOS")
	}
	home := t.TempDir()
	dir := filepath.Join(home, "Library", "Application Support", "Claude", "claude-code-sessions", "a", "b")
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
	rows := (&Server{home: home}).claudeDesktopRows()
	if len(rows) != desktopMax || rows[0].session != "c1" {
		t.Fatalf("got %d rows starting %+v, want the %d newest unarchived ones however old", len(rows), rows, desktopMax)
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

func TestAgyDesktopSessionsTakeOverWhenIdle(t *testing.T) {
	desktopCache.Lock()
	desktopCache.at, desktopCache.app = time.Now(), false
	desktopCache.rows = []desktopRow{
		{id: "ag-idle", kind: "agy", session: "idle", cwd: "/root"},
		{id: "ag-running", kind: "agy", session: "running", cwd: "/root", running: true},
		{id: "ag-waiting", kind: "agy", session: "waiting", cwd: "/root", waiting: true},
		{id: "ag-nowhere", kind: "agy", session: "nowhere"},
	}
	desktopCache.Unlock()
	t.Cleanup(forgetDesktopRows)
	got := map[string]bool{}
	for _, e := range (&Server{home: t.TempDir()}).desktopSessions(nil, nil, time.Now()) {
		got[e.Session] = e.CanTakeOver
	}
	// Only a resting conversation with a directory to resume it in: a turn the
	// App still runs has no process of its own to stop first.
	want := map[string]bool{"idle": true, "running": false, "waiting": false, "nowhere": false}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("CanTakeOver[%s] = %v, want %v", id, got[id], w)
		}
	}
}
