package collie

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const (
	externalFirstID = "88509bd7-f8bc-4911-ace5-0361cfcf34e0"
	externalOtherID = "c6555d0f-3cf8-41ce-be1f-c72b15423be6"
)

func TestLocateExternalKeepsClaudeRegistrationWithoutTranscript(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"sessions/4242.json":                              `{"pid":4242,"sessionId":"` + externalFirstID + `"}`,
		"projects/-project/" + externalOtherID + ".jsonl": "{}\n",
	})
	s := &Server{home: t.TempDir()}
	e := &External{
		Kind: "claude", PID: 4242, Cwd: "/project", Started: time.Now(),
		launch: &procLaunch{env: map[string]string{"CLAUDE_CONFIG_DIR": root}},
		reg: &claudeReg{
			PID: 4242, SessionID: externalFirstID, Protocol: 1,
			dir: filepath.Join(root, "sessions"), Socket: filepath.Join(root, "receive.sock"),
		},
	}
	s.locateExternal(e, &proc{pid: e.PID})
	s.reachable(e)
	if e.Session != externalFirstID || e.path != "" || !e.CanSend {
		t.Fatalf("session=%q path=%q canSend=%v, want registered session waiting for its transcript", e.Session, e.path, e.CanSend)
	}
}

func TestLocateExternalUsesClaudeWrapperRegistration(t *testing.T) {
	root := t.TempDir()
	rightPath := "projects/-original/" + externalFirstID + ".jsonl"
	writeFiles(t, root, map[string]string{
		rightPath: "{}\n",
		"projects/-project/" + externalOtherID + ".jsonl": "{}\n",
	})
	s := &Server{home: t.TempDir()}
	e := &External{
		Kind: "claude", PID: 4242, Cwd: "/project", Started: time.Now(),
		launch: &procLaunch{env: map[string]string{"CLAUDE_CONFIG_DIR": root}},
		reg:    &claudeReg{PID: 4243, SessionID: externalFirstID, dir: filepath.Join(root, "sessions")},
	}
	s.locateExternal(e, &proc{pid: e.PID})
	if e.Session != externalFirstID || e.path != filepath.Join(root, rightPath) {
		t.Fatalf("session=%q path=%q, want child's registered transcript", e.Session, e.path)
	}
}

func TestLocateExternalDoesNotBindCodexBySharedDirectory(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"sessions/2026/11/26/rollout-2026-11-26T09-00-00-" + externalFirstID + ".jsonl": `{"type":"session_meta","payload":{"id":"` + externalFirstID + `","cwd":"/project"}}` + "\n",
		"sessions/2026/11/26/rollout-2026-11-26T10-00-00-" + externalOtherID + ".jsonl": `{"type":"session_meta","payload":{"id":"` + externalOtherID + `","cwd":"/project"}}` + "\n",
	})
	s := &Server{home: t.TempDir()}
	for _, pid := range []int{4242, 4243} {
		e := &External{
			Kind: "codex", PID: pid, Cwd: "/project", Started: time.Now(),
			launch: &procLaunch{env: map[string]string{"CODEX_HOME": root}},
		}
		s.locateExternal(e, &proc{pid: pid})
		if e.Session != "" || e.path != "" {
			t.Errorf("pid %d bound to session %q at %q without process evidence", pid, e.Session, e.path)
		}
	}
}

func TestLocateExternalDoesNotBindAgyBySharedDirectory(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".gemini", "antigravity-cli")
	writeFiles(t, root, map[string]string{
		"brain/" + externalFirstID + "/.system_generated/logs/transcript.jsonl": "{}\n",
		"brain/" + externalOtherID + "/.system_generated/logs/transcript.jsonl": "{}\n",
	})
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
	if _, err := db.Exec(`INSERT INTO conversation_summaries VALUES (?, '["file:///project"]', '9999-01-01'), (?, '["file:///project"]', '9999-01-01')`, externalFirstID, externalOtherID); err != nil {
		t.Fatal(err)
	}
	s := &Server{home: home}
	for _, pid := range []int{4242, 4243} {
		e := &External{Kind: "agy", PID: pid, Cwd: "/project", Started: time.Now()}
		s.locateExternal(e, &proc{pid: pid})
		if e.Session != "" || e.path != "" {
			t.Errorf("pid %d bound to session %q at %q without process evidence", pid, e.Session, e.path)
		}
	}
}

func TestLocateExternalUsesExplicitResumeInSharedDirectory(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".gemini", "antigravity-cli")
	agyLog := "brain/" + externalFirstID + "/.system_generated/logs/transcript.jsonl"
	codexLog := "sessions/2026/11/26/rollout-2026-11-26T09-00-00-" + externalFirstID + ".jsonl"
	writeFiles(t, root, map[string]string{
		agyLog: "{}\n",
		"brain/" + externalOtherID + "/.system_generated/logs/transcript.jsonl": "{}\n",
	})
	writeFiles(t, home, map[string]string{
		codexLog: `{"type":"session_meta","payload":{"id":"` + externalFirstID + `","cwd":"/project"}}` + "\n",
		"sessions/2026/11/26/rollout-2026-11-26T10-00-00-" + externalOtherID + ".jsonl": `{"type":"session_meta","payload":{"id":"` + externalOtherID + `","cwd":"/project"}}` + "\n",
	})
	s := &Server{home: home}
	for _, tt := range []struct {
		kind, path string
		args       []string
	}{
		{"agy", filepath.Join(root, agyLog), []string{"--conversation=" + externalFirstID}},
		{"codex", filepath.Join(home, codexLog), []string{"--model", "sample-model", "--no-alt-screen", "resume", externalFirstID}},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			e := &External{
				Kind: tt.kind, PID: 4242, Cwd: "/project", Started: time.Now(),
				launch: &procLaunch{args: tt.args, env: map[string]string{"CODEX_HOME": home}},
			}
			s.locateExternal(e, &proc{pid: e.PID})
			if e.Session != externalFirstID || e.path != tt.path {
				t.Fatalf("session=%q path=%q, want explicit resume %s at %s", e.Session, e.path, externalFirstID, tt.path)
			}
		})
	}
}

func TestLocateExternalChecksWritableCodexLog(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process file descriptors are available on Linux and macOS")
	}
	root := t.TempDir()
	firstLog := "sessions/2026/11/26/rollout-2026-11-26T09-00-00-" + externalFirstID + ".jsonl"
	otherLog := "sessions/2026/11/26/rollout-2026-11-26T10-00-00-" + externalOtherID + ".jsonl"
	writeFiles(t, root, map[string]string{
		firstLog: `{"type":"session_meta","payload":{"id":"` + externalFirstID + `","cwd":"/project"}}` + "\n",
		otherLog: `{"type":"session_meta","payload":{"id":"` + externalOtherID + `","cwd":"/project"}}` + "\n",
	})
	s := &Server{home: t.TempDir()}
	e := func() *External {
		return &External{
			Kind: "codex", PID: os.Getpid(), Cwd: "/project", Started: time.Now(),
			launch: &procLaunch{env: map[string]string{"CODEX_HOME": root}},
		}
	}
	open := func(name string, flags int) {
		t.Helper()
		f, err := os.OpenFile(filepath.Join(root, name), flags, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := f.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	open(otherLog, os.O_RDONLY)
	open(firstLog, os.O_WRONLY)
	p := &proc{pid: os.Getpid()}
	x := e()
	s.locateExternal(x, p)
	if x.Session != externalFirstID {
		t.Fatalf("session=%q, want the transcript opened for writing", x.Session)
	}
	open(otherLog, os.O_WRONLY)
	x = e()
	s.locateExternal(x, p)
	if x.Session != "" || x.path != "" {
		t.Fatalf("session=%q path=%q, want no binding with two writable logs", x.Session, x.path)
	}
}
