package collie

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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

// An active automation's thread shows past the week and past the cap; a
// paused one's doesn't.
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
