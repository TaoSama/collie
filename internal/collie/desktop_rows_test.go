package collie

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexAppRowsSkipCollieWorkers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, ".codex", "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, q := range []string{
		`CREATE TABLE threads (id TEXT, name TEXT, title TEXT, first_user_message TEXT, cwd TEXT, rollout_path TEXT,
			created_at INTEGER, created_at_ms INTEGER, updated_at INTEGER, updated_at_ms INTEGER, originator TEXT, archived INTEGER, source TEXT)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	add := func(id, title, first string) {
		if _, err := db.Exec(`INSERT INTO threads VALUES (?, '', ?, ?, '/root', '', ?, ?, ?, ?, '', 0, 'vscode')`,
			id, title, first, now.Unix(), now.UnixMilli(), now.Unix(), now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	add("worker", "归档 devbox 上的 4 个 Codex 会话", "[collie] 归档 devbox 上的 4 个 Codex 会话，完成后 collie report。")
	add("mine", "看看日志", "看看日志")
	db.Close()
	rows := (&Server{home: home}).codexAppRows()
	if len(rows) != 1 || rows[0].session != "mine" {
		t.Fatalf("rows = %+v, want only the thread that isn't a Collie worker's", rows)
	}
}
