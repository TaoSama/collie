package collie

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocateExternalAgyFindsUniqueNewWorkspaceLog(t *testing.T) {
	for _, tc := range []struct {
		name         string
		secondLog    bool
		earlierLog   bool
		resumeAbsent bool
		shared       bool
		wantSession  string
	}{
		{name: "one new log", wantSession: externalFirstID},
		{name: "two new logs", secondLog: true},
		{name: "log predates process", earlierLog: true},
		{name: "explicit resume waits for its log", resumeAbsent: true},
		{name: "another AGY process shares the workspace", shared: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, cwd := t.TempDir(), t.TempDir()
			root := filepath.Join(home, ".gemini", "antigravity-cli")
			first := filepath.Join("brain", externalFirstID, ".system_generated", "logs", "transcript.jsonl")
			logs := map[string]string{first: "{}\n"}
			if tc.secondLog {
				logs[filepath.Join("brain", externalOtherID, ".system_generated", "logs", "transcript.jsonl")] = "{}\n"
			}
			writeFiles(t, root, logs)
			db, err := sql.Open("sqlite", filepath.Join(root, "conversation_summaries.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			})
			if _, err := db.Exec(`CREATE TABLE conversation_summaries (conversation_id TEXT, workspace_uris TEXT, last_modified_time TEXT)`); err != nil {
				t.Fatal(err)
			}
			workspaces, err := json.Marshal([]string{"file://" + cwd})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{externalFirstID, externalOtherID} {
				if _, err := db.Exec(`INSERT INTO conversation_summaries VALUES (?, ?, ?)`, id, string(workspaces), time.Now().UTC().Format(time.DateTime)); err != nil {
					t.Fatal(err)
				}
			}
			started := time.Now().Add(-time.Minute)
			if tc.earlierLog {
				directory, err := os.Stat(filepath.Join(root, "brain", externalFirstID))
				if err != nil {
					t.Fatal(err)
				}
				created := birthTime(directory)
				if created.IsZero() {
					created = directory.ModTime()
				}
				started = created.Add(time.Second)
			}
			process := &External{Kind: "agy", Cwd: cwd, Started: started, launch: &procLaunch{}, agySharedWorkspace: tc.shared}
			if tc.resumeAbsent {
				process.launch.args = []string{"--conversation", externalOtherID}
			}
			s := &Server{home: home}
			s.locateExternal(process, &proc{})
			wantPath := ""
			if tc.wantSession != "" {
				wantPath = filepath.Join(root, first)
			}
			if process.Session != tc.wantSession || process.path != wantPath {
				t.Fatalf("got session %q path %q, want %q at %q", process.Session, process.path, tc.wantSession, wantPath)
			}
		})
	}
}
