package collie

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// agyIndexFixture makes an Antigravity index under root, with its search
// table, and returns a function that adds a conversation (archived or not).
func agyIndexFixture(t *testing.T, root string) func(id string, at time.Time, archived bool) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE conversation_summaries (conversation_id TEXT, title TEXT, preview TEXT, workspace_uris TEXT, status TEXT,
		not_fully_idle NUMERIC, last_modified_time DATETIME, last_user_input_time DATETIME, nesting_depth INTEGER, killed NUMERIC);
		CREATE VIRTUAL TABLE conversations_fts USING fts5(conversation_id UNINDEXED, chunk_index UNINDEXED, archived UNINDEXED, title)`); err != nil {
		t.Fatal(err)
	}
	return func(id string, at time.Time, archived bool) {
		ts := at.UTC().Format("2006-01-02 15:04:05")
		if _, err := db.Exec(`INSERT INTO conversation_summaries VALUES (?, '对话', '', '["file:///root"]', '', 0, ?, ?, 0, 0)`, id, ts, ts); err != nil {
			t.Fatal(err)
		}
		// The header row (chunk_index -1) carries the archive; the chunks don't.
		a := 0
		if archived {
			a = 1
		}
		if _, err := db.Exec(`INSERT INTO conversations_fts VALUES (?, -1, ?, '对话'), (?, 0, 0, '对话')`, id, a, id); err != nil {
			t.Fatal(err)
		}
	}
}

func archiveCodexThread(t *testing.T, home, id string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, ".codex", "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE threads SET archived = 1 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
}

func TestArchivedIDsReadCodexAndBothAgyIndexes(t *testing.T) {
	home, add := codexStateFixture(t)
	now := time.Now()
	add("cx-kept", "看看日志", now)
	add("cx-gone", "看看日志", now)
	archiveCodexThread(t, home, "cx-gone")
	app := agyIndexFixture(t, filepath.Join(home, ".gemini", "antigravity"))
	app("ag-app-gone", now, true)
	app("ag-kept", now, false)
	cli := agyIndexFixture(t, filepath.Join(home, ".gemini", "antigravity-cli"))
	cli("ag-cli-gone", now, true)

	forgetArchived()
	t.Cleanup(forgetArchived)
	s := &Server{home: home}
	got := s.archivedAmong([]string{"cx-kept", "cx-gone", "ag-app-gone", "ag-kept", "ag-cli-gone", "unknown"})
	if want := []string{"cx-gone", "ag-app-gone", "ag-cli-gone"}; !slices.Equal(got, want) {
		t.Fatalf("archived = %v, want %v", got, want)
	}

	// Unarchived, it is back once the cache lets go.
	db, err := sql.Open("sqlite", filepath.Join(home, ".codex", "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE threads SET archived = 0 WHERE id = 'cx-gone'`); err != nil {
		t.Fatal(err)
	}
	forgetArchived()
	if s.archivedIDs()["cx-gone"] {
		t.Fatal("an unarchived thread still reads as archived")
	}
}

// An index without the search table (older Antigravity) archives nothing.
func TestArchivedIDsWithoutAgySearchTable(t *testing.T) {
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
	if _, err := db.Exec(`CREATE TABLE conversation_summaries (conversation_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	forgetArchived()
	t.Cleanup(forgetArchived)
	if got := (&Server{home: home}).archivedIDs(); len(got) != 0 {
		t.Fatalf("archived = %v, want none", got)
	}
}

// Archived conversations leave the app's list, without taking the places of
// the others; the CLI's index can be the one that says so.
func TestAgyDesktopRowsSkipArchived(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	app := agyIndexFixture(t, filepath.Join(home, ".gemini", "antigravity"))
	cli := agyIndexFixture(t, filepath.Join(home, ".gemini", "antigravity-cli"))
	now := time.Now()
	for i := range 3 {
		app(fmt.Sprintf("gone%d", i), now.Add(-time.Duration(i)*time.Minute), true)
	}
	app("cli-gone", now, false)
	cli("cli-gone", now, true)
	for i := range desktopMax + 1 {
		app(fmt.Sprintf("g%d", i), now.Add(-time.Hour-time.Duration(i)*time.Minute), false)
	}
	forgetArchived()
	t.Cleanup(forgetArchived)
	rows := (&Server{home: home}).agyDesktopRows()
	if len(rows) != desktopMax || rows[0].session != "g0" || rows[desktopMax-1].session != fmt.Sprintf("g%d", desktopMax-1) {
		t.Fatalf("got %d rows starting %q, want the %d newest unarchived", len(rows), rows[0].session, desktopMax)
	}
}

// The ended sessions the hub remembers drop out once archived on their
// computer: this one by its own files, another by what it last said.
func TestHistoryLeavesOutArchived(t *testing.T) {
	home, add := codexStateFixture(t)
	add("here-gone", "看看日志", time.Now())
	add("here-kept", "看看日志", time.Now())
	archiveCodexThread(t, home, "here-gone")
	forgetArchived()
	t.Cleanup(forgetArchived)

	titles.Lock()
	saved := titles.by
	titles.by = map[string]*titleCache{
		"mac":   {archived: map[string]bool{"there-gone": true}},
		"quiet": {}, // an older node: says nothing
	}
	titles.Unlock()
	t.Cleanup(func() { titles.Lock(); titles.by = saved; titles.Unlock() })

	list := []*Ended{
		{Kind: "codex", Session: "here-gone"},
		{Kind: "codex", Session: "here-kept"},
		{Kind: "codex", Machine: "mac", Session: "there-gone"},
		{Kind: "agy", Machine: "mac", Session: "there-kept"},
		{Kind: "codex", Machine: "quiet", Session: "there-gone"},
		{Kind: "claude", Machine: "mac", Session: "there-gone"}, // Claude has no archive of its own
	}
	var got []string
	for _, e := range (&Server{home: home}).withoutArchived(list) {
		got = append(got, e.Machine+"/"+e.Kind+"/"+e.Session)
	}
	want := []string{"/codex/here-kept", "mac/agy/there-kept", "quiet/codex/there-gone", "mac/claude/there-gone"}
	if !slices.Equal(got, want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
}
