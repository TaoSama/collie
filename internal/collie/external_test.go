package collie

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseEtime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"05:07":      5*time.Minute + 7*time.Second,
		"01:06:25":   time.Hour + 6*time.Minute + 25*time.Second,
		"2-03:00:01": 51*time.Hour + time.Second,
	} {
		if got := parseEtime(in); got != want {
			t.Errorf("%s: %v, want %v", in, got, want)
		}
	}
}

func TestProgName(t *testing.T) {
	for want, args := range map[string][]string{
		"claude": {"/Users/x/.local/bin/claude", "--resume", "id"},
		"codex":  {"node", "/usr/lib/node_modules/@openai/codex/bin/codex.js"},
		"zsh":    {"-zsh"},
	} {
		if got := progName(args); got != want {
			t.Errorf("%v: %q, want %q", args, got, want)
		}
	}
}

// COLLIE_EXTERNAL=1 go test -run TestExternalLive -v prints what this machine finds.
func TestExternalLive(t *testing.T) {
	if os.Getenv("COLLIE_EXTERNAL") == "" {
		t.Skip("set COLLIE_EXTERNAL=1")
	}
	home, _ := os.UserHomeDir()
	s := &Server{home: home}
	s.localAliases(context.Background(), true)
	for _, e := range s.externalSessions() {
		t.Logf("%s app=%q cwd=%s start=%s session=%s alias=%q %v title=%q path=%s", e.ID, e.App, e.Cwd, e.startDir, e.Session, e.Alias, e.AliasChoices, e.Title, e.path)
	}
}

// COLLIE_TAKEOVER=<id> go test -run TestTakeOverLive -v takes that session
// over into this machine's herdr: it stops the process.
func TestTakeOverLive(t *testing.T) {
	id := os.Getenv("COLLIE_TAKEOVER")
	if id == "" {
		t.Skip("set COLLIE_TAKEOVER to an external session's id")
	}
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	s := &Server{home: home, herdr: newHerdr(filepath.Join(home, ".config/herdr/herdr.sock"), host, home)}
	r := httptest.NewRequest("POST", "/api/external/"+id+"/takeover", strings.NewReader("{}"))
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	s.takeOverExternal(w, r)
	t.Logf("%d %s", w.Code, w.Body)
}

func TestLaunchShell(t *testing.T) {
	ps := map[int]*proc{
		10: {pid: 10, ppid: 1, tty: "ttys001", name: "zsh"},
		11: {pid: 11, ppid: 10, tty: "ttys001", name: "claude"},
		20: {pid: 20, ppid: 1, tty: "??", name: "tmux"},
		21: {pid: 21, ppid: 20, tty: "ttys002", name: "claude"},
		30: {pid: 30, ppid: 1, tty: "ttys003", name: "script"},
		31: {pid: 31, ppid: 30, tty: "ttys004", name: "claude"},
	}
	for pid, want := range map[int]int{
		11: 10, // typed into zsh: resume where zsh is
		21: 0,  // started by tmux, not a shell
		31: 0,  // script's own pty is another terminal
	} {
		got := 0
		if q := launchShell(ps, ps[pid]); q != nil {
			got = q.pid
		}
		if got != want {
			t.Errorf("pid %d: shell %d, want %d", pid, got, want)
		}
	}
}

// A held message can only be confirmed on a screen: say where to find one.
func TestHeldNoteSaysWhereToConfirm(t *testing.T) {
	for _, want := range []string{"在 herdr 里接管", "「屏幕」"} {
		if !strings.Contains(heldNote, want) {
			t.Errorf("heldNote lacks %q: %s", want, heldNote)
		}
	}
}
