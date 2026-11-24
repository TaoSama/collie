package collie

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAliasRuns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	os.Unsetenv("CLAUDE_CONFIG_DIR")
	os.Unsetenv("CODEX_HOME")
	write := func(f, s string) {
		os.MkdirAll(filepath.Dir(filepath.Join(home, f)), 0o700)
		os.WriteFile(filepath.Join(home, f), []byte(s), 0o600)
	}
	write(".claude/settings.json", `{"model": "fable", "env": {"ANTHROPIC_DEFAULT_HAIKU_MODEL": "claude-haiku-5-5"}}`)
	write("work/settings.json", `{"model": "opus[1m]"}`)
	write(".codex/config.toml", "model = \"gpt-6.2-sol\"\n[profiles.fast]\nmodel = 'gpt-6-luna'\n[profiles.bare]\nmodel_reasoning_effort = \"low\"\n")
	for _, c := range []struct{ name, value, want string }{
		{"ccf", `claude --model fable[1m]`, "fable[1m]"},                                                           // pinned
		{"cc", `ANTHROPIC_BASE_URL=x claude`, "fable"},                                                             // the settings it reads
		{"ccw", `CLAUDE_CONFIG_DIR=~/work claude`, "opus[1m]"},                                                     // its own config directory
		{"ccv", `CLAUDE_CONFIG_DIR="$WORK" claude`, "fable"},                                                       // a variable: unknown, the usual one
		{"ccx", `ANTHROPIC_DEFAULT_SONNET_MODEL=claude-opus-5-5 claude --model sonnet[1m]`, "claude-opus-5-5[1m]"}, // remapped family
		{"cch", `claude --model haiku`, "claude-haiku-5-5"},                                                        // remapped by the settings
		{"cx", `codex`, "gpt-6.2-sol"},                                                                             // config.toml
		{"cxp", `codex -p fast`, "gpt-6-luna"},                                                                     // its profile's
		{"cxb", `codex --profile=bare`, "gpt-6.2-sol"},                                                             // a profile without a model
		{"cxm", `codex -m gpt-6-astra -p fast`, "gpt-6-astra"},                                                     // pinned wins
		{"ag", `agy`, ""}, // the agent's own default
	} {
		a, ok := parseLauncher(c.name, c.value)
		if !ok {
			t.Fatalf("%s: not a launcher", c.name)
		}
		if got := aliasRuns(home, &a); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if got := aliasModel(&LauncherAlias{Model: "sonnet", Runs: "claude-opus-5-5"}, "x"); got != "claude-opus-5-5" {
		t.Errorf("aliasModel: %q", got)
	}
}
