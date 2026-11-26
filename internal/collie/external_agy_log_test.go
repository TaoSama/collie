package collie

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
			if process.agyCandidate != (tc.wantSession != "") {
				t.Errorf("candidate = %t, want %t", process.agyCandidate, tc.wantSession != "")
			}
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

func TestSameExternalAgyChecksProcessAndSession(t *testing.T) {
	home := t.TempDir()
	s := &Server{home: home}
	logPath := filepath.Join(s.agyCLIHome(), "brain", externalFirstID, agyTranscriptRelPath)
	must(t, os.MkdirAll(filepath.Dir(logPath), 0o700))
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0o600)
	must(t, err)
	t.Cleanup(func() { must(t, f.Close()) })

	pid := os.Getpid()
	start := time.Date(2026, time.November, 27, 2, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	ps := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = -p ]; then\n  printf '%%s\\n' '%s'\nelse\n  printf '%%s\\n' '%s 1 pts/1 00:01 agy'\nfi\n",
		start.Format("Mon Jan _2 15:04:05 2006"), strconv.Itoa(pid))
	must(t, os.WriteFile(filepath.Join(dir, "ps"), []byte(ps), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct {
		name, session string
		started       time.Time
		want          bool
	}{
		{"same process", externalFirstID, start, true},
		{"reused pid", externalFirstID, start.Add(-time.Second), false},
		{"changed session", externalOtherID, start, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &External{PID: pid, Session: tc.session, agyStarted: tc.started}
			if got := s.sameExternalAgy(t.Context(), e); got != tc.want {
				t.Fatalf("sameExternalAgy = %t, want %t", got, tc.want)
			}
		})
	}
}
