package collie

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func launcherAliases(t *testing.T, defs ...[2]string) []LauncherAlias {
	t.Helper()
	var out []LauncherAlias
	for _, d := range defs {
		a, ok := parseLauncher(d[0], d[1])
		if !ok {
			t.Fatalf("%s is not a launcher", d[0])
		}
		out = append(out, a)
	}
	return out
}

const (
	ccxDef   = `ANTHROPIC_BASE_URL="$ANTHROPIC_BASE_URL" ANTHROPIC_AUTH_TOKEN="` + fakeToken + `" CLAUDE_CODE_SUBAGENT_MODEL="sonnet" claude --model "claude-opus-5-5[1m]" --dangerously-skip-permissions`
	ccfDef   = `ANTHROPIC_BASE_URL="https://cf.example.com" ANTHROPIC_AUTH_TOKEN="` + fakeToken + `" claude --model "fable[1m]" --dangerously-skip-permissions`
	ccDef    = `ANTHROPIC_BASE_URL="https://cf.example.com" ANTHROPIC_AUTH_TOKEN="` + fakeToken + `" claude --dangerously-skip-permissions`
	ccDirDef = `CLAUDE_CONFIG_DIR=~/.claude-work claude`
)

// ccx as the process it starts shows it.
var ccxLaunch = &procLaunch{
	args: []string{"--model", "claude-opus-5-5[1m]", "--dangerously-skip-permissions", "--resume", "abc"},
	env:  map[string]string{"ANTHROPIC_BASE_URL": "https://gw.example.com", "ANTHROPIC_AUTH_TOKEN": fakeToken, "CLAUDE_CODE_SUBAGENT_MODEL": "sonnet", "HOME": "/Users/x"},
}

func TestLaunchedBy(t *testing.T) {
	ccxf := strings.Replace(ccxDef, "claude-opus-5-5[1m]", "claude-fable-5-2[1m]", 1)
	aliases := launcherAliases(t, [2]string{"cc", ccDef}, [2]string{"ccf", ccfDef}, [2]string{"ccx", ccxDef}, [2]string{"ccxf", ccxf}, [2]string{"ccw", ccDirDef},
		[2]string{"cx62", `command codex --model gpt-6.2-sol`}, [2]string{"codex", `command codex --model gpt-6.2-astra --no-alt-screen`})
	home := t.TempDir()
	cases := []struct {
		name string
		kind string
		l    *procLaunch
		want string
	}{
		{"ccx", "claude", ccxLaunch, "ccx"},
		// cc's flag is in ccf's too: the one that fits more wins.
		{"ccf", "claude", &procLaunch{args: []string{"--model", "fable[1m]", "--dangerously-skip-permissions"}, env: map[string]string{"ANTHROPIC_BASE_URL": "https://cf.example.com", "ANTHROPIC_AUTH_TOKEN": fakeToken}}, "ccf"},
		{"cc", "claude", &procLaunch{args: []string{"--dangerously-skip-permissions"}, env: map[string]string{"ANTHROPIC_BASE_URL": "https://cf.example.com", "ANTHROPIC_AUTH_TOKEN": fakeToken}}, "cc"},
		// The alias's flags without its variables: typed by hand.
		{"bare with flags", "claude", &procLaunch{args: []string{"--dangerously-skip-permissions"}, env: map[string]string{"ANTHROPIC_BASE_URL": "https://cf.example.com"}}, ""},
		{"bare", "claude", &procLaunch{args: []string{"--resume", "abc"}, env: map[string]string{"HOME": "/Users/x"}}, ""},
		// ~ is the home directory.
		{"config dir", "claude", &procLaunch{env: map[string]string{"CLAUDE_CONFIG_DIR": home + "/.claude-work"}}, "ccw"},
		{"other config dir", "claude", &procLaunch{env: map[string]string{"CLAUDE_CONFIG_DIR": "/elsewhere"}}, ""},
		{"codex", "codex", &procLaunch{args: []string{"--model", "gpt-6.2-sol", "resume", "id"}}, "cx62"},
		{"codex alias named codex", "codex", &procLaunch{args: []string{"--model", "gpt-6.2-astra", "--no-alt-screen"}}, "codex"},
		{"other kind", "codex", ccxLaunch, ""},
		{"unknown", "claude", nil, ""},
	}
	for _, c := range cases {
		a, choices := launchedBy(aliases, c.kind, c.l, time.Now(), home)
		got := ""
		if a != nil {
			got = a.Name
		}
		if got != c.want || choices != nil {
			t.Errorf("%s: %q %v, want %q", c.name, got, choices, c.want)
		}
	}
}

func TestLaunchedBySameLaunchTwice(t *testing.T) {
	// ccsl5 is ccx under another name: the one typed, else either.
	aliases := launcherAliases(t, [2]string{"ccsl5", ccxDef}, [2]string{"ccx", ccxDef})
	home := t.TempDir()
	started := time.Unix(1795536480, 0)
	if a, choices := launchedBy(aliases, "claude", ccxLaunch, started, home); a == nil || choices != nil {
		t.Fatalf("no history: %v %v, want one of them", a, choices)
	}
	for _, c := range []struct{ file, hist string }{
		{".bash_history", "#1795536300\nccsl5\n#1795536478\nccx --allowedTools Read\n#1795536600\nccsl5\n"},
		{".zsh_history", ": 1795536300:0;ccsl5\n: 1795536479:0;cd ~/x && ccx\n"},
		// No time near the start: the one typed most.
		{".bash_history", "ccsl5\nccx --resume a\nls\nccx\n"},
	} {
		file, hist := c.file, c.hist
		os.Remove(filepath.Join(home, ".bash_history"))
		os.Remove(filepath.Join(home, ".zsh_history"))
		if err := os.WriteFile(filepath.Join(home, file), []byte(hist), 0o600); err != nil {
			t.Fatal(err)
		}
		if a, choices := launchedBy(aliases, "claude", ccxLaunch, started, home); a == nil || a.Name != "ccx" || choices != nil {
			t.Errorf("%s: %v %v, want ccx", file, a, choices)
		}
	}
}

func TestLaunchedByAsksBetweenDifferentFits(t *testing.T) {
	aliases := launcherAliases(t, [2]string{"cca", `FOO=1 claude`}, [2]string{"ccb", `BAR=2 claude`})
	l := &procLaunch{env: map[string]string{"FOO": "1", "BAR": "2"}}
	home := t.TempDir()
	started := time.Unix(1795536480, 0)
	// Typed more often is no reason to pick one of two different launches.
	if err := os.WriteFile(filepath.Join(home, ".bash_history"), []byte("ccb\nccb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, choices := launchedBy(aliases, "claude", l, started, home)
	if a == nil || !slices.Equal(choices, []string{"cca", "ccb"}) {
		t.Errorf("%v %v, want a guess and both to pick from", a, choices)
	}
	// Typed as it started is.
	if err := os.WriteFile(filepath.Join(home, ".bash_history"), []byte("ccb\n#1795536479\ncca\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if a, choices := launchedBy(aliases, "claude", l, started, home); a == nil || a.Name != "cca" || choices != nil {
		t.Errorf("%v %v, want cca", a, choices)
	}
}

// TestMain lets the test binary stand in for an agent (agentStandIn).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "ext-") && builtinExts[strings.TrimPrefix(os.Args[1], "ext-")] != nil {
		os.Exit(builtinExts[strings.TrimPrefix(os.Args[1], "ext-")](os.Args[2:])) // an extension for ext_test.go
	}
	if os.Getenv("COLLIE_STAND_IN") != "" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// An external agent started through an alias: a real process with the
// alias's variables and flags, read back the way the session list reads it.
// (macOS hides the environment of its own programs, so not sleep(1).)
func TestReadLaunchOfAliasStartedProcess(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no process environment here")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude")
	if err := os.Symlink(self, claude); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(claude, "--model", "claude-opus-5-5[1m]", "--resume", "abc")
	cmd.Env = []string{"COLLIE_STAND_IN=1", "HOME=" + dir, "CLAUDE_CODE_SUBAGENT_MODEL=sonnet", "ANTHROPIC_BASE_URL=https://gw.example.com"}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	var l *procLaunch
	for i := 0; i < 50 && (l == nil || l.env["CLAUDE_CODE_SUBAGENT_MODEL"] == ""); i++ {
		time.Sleep(20 * time.Millisecond) // until it has exec'd
		l = readLaunch(cmd.Process.Pid)
	}
	if l == nil || !slices.Equal(l.args, []string{"--model", "claude-opus-5-5[1m]", "--resume", "abc"}) || l.env["ANTHROPIC_BASE_URL"] != "https://gw.example.com" {
		t.Fatalf("read %+v", l)
	}
	aliases := launcherAliases(t, [2]string{"ccx", `ANTHROPIC_BASE_URL="$ANTHROPIC_BASE_URL" CLAUDE_CODE_SUBAGENT_MODEL=sonnet claude --model 'claude-opus-5-5[1m]'`},
		[2]string{"ccs", `CLAUDE_CODE_SUBAGENT_MODEL=haiku claude --model 'claude-opus-5-5[1m]'`}, [2]string{"cc", `claude`})
	if a, _ := launchedBy(aliases, "claude", l, time.Now(), dir); a == nil || a.Name != "ccx" {
		t.Errorf("guessed %v, want ccx", a)
	}
}

// A session an alias keeps in another config directory is found there.
func TestLocateExternalInAliasConfigDir(t *testing.T) {
	root := t.TempDir()
	const pid, id = 4242, "0b7c2f5e-1d1e-4c4b-9a55-3f2a8d7e6c11"
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "sessions"), 0o755))
	must(os.MkdirAll(filepath.Join(root, "projects", "-x"), 0o755))
	must(os.WriteFile(filepath.Join(root, "sessions", fmt.Sprint(pid)+".json"), []byte(`{"sessionId":"`+id+`"}`), 0o600))
	must(os.WriteFile(filepath.Join(root, "projects", "-x", id+".jsonl"), []byte("{}\n"), 0o600))
	s := &Server{home: t.TempDir()}
	e := &External{Kind: "claude", PID: pid, Cwd: "/x", Started: time.Now(), launch: &procLaunch{env: map[string]string{"CLAUDE_CONFIG_DIR": root}}}
	s.locateExternal(e, &proc{pid: pid})
	if e.Session != id || e.path == "" {
		t.Errorf("session %q at %q, want %s", e.Session, e.path, id)
	}
}
