package collie

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestLocateExternalIgnoresCodexSubagentRollouts(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process file descriptors are available on Linux and macOS")
	}
	root := t.TempDir()
	mainLog := "sessions/2026/11/26/rollout-2026-11-26T09-00-00-" + externalFirstID + ".jsonl"
	childLog := "sessions/2026/11/26/rollout-2026-11-26T09-00-01-" + externalOtherID + ".jsonl"
	writeFiles(t, root, map[string]string{
		mainLog:  `{"type":"session_meta","payload":{"id":"` + externalFirstID + `","source":"cli"}}` + "\n",
		childLog: `{"type":"session_meta","payload":{"id":"` + externalOtherID + `","source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + externalFirstID + `"}}}}}` + "\n",
	})
	for _, log := range []string{mainLog, childLog} {
		f, err := os.OpenFile(filepath.Join(root, log), os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := f.Close(); err != nil {
				t.Error(err)
			}
		})
	}

	s := &Server{home: t.TempDir()}
	e := &External{
		Kind: "codex",
		PID:  os.Getpid(),
		launch: &procLaunch{
			env: map[string]string{"CODEX_HOME": root},
		},
	}
	s.locateExternal(e, &proc{pid: e.PID})
	if e.Session != externalFirstID || e.path != filepath.Join(root, mainLog) {
		t.Fatalf("session=%q path=%q, want main rollout %q", e.Session, e.path, mainLog)
	}
}

func TestLsofWritableProcessLogsDisablesNameResolution(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	const fakeLsof = `#!/bin/sh
[ "$*" = "-nP -p 1234 -Ffan" ] || exit 1
printf '%s\n' f1 aw n/writable.jsonl f2 ar n/readonly.jsonl
`
	if err := os.WriteFile(filepath.Join(dir, "lsof"), []byte(fakeLsof), 0o700); err != nil {
		t.Fatal(err)
	}
	want := []string{"/writable.jsonl"}
	if got := lsofWritableProcessLogs(1234); !slices.Equal(got, want) {
		t.Fatalf("lsof with name resolution disabled returned %q, want %q", got, want)
	}
}
