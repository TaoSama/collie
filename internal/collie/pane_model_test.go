package collie

import (
	"slices"
	"testing"
)

func TestSwitchCommands(t *testing.T) {
	for _, c := range []struct {
		kind     string
		from, to [2]string
		want     []string
	}{
		{"codex", [2]string{"gpt-6.1-sol", "medium"}, [2]string{"gpt-6.1-sol", "high"}, []string{"/model gpt-6.1-sol --reasoning=high"}},
		{"codex", [2]string{"gpt-6.1-sol", ""}, [2]string{"gpt-6.1-mini", ""}, []string{"/model gpt-6.1-mini"}},
		{"codex", [2]string{"gpt-6.1-sol", "high"}, [2]string{"gpt-6.1-sol", "high"}, nil},
		{"codex", [2]string{"gpt-6.1-sol", "high"}, [2]string{"", "low"}, nil}, // codex's level goes with a model
		// Both would change every new session's default.
		{"claude", [2]string{"opus", "high"}, [2]string{"sonnet", "low"}, nil},
		{"antigravity", [2]string{"gemini-4-neon", "high"}, [2]string{"gemini-4-neon", "low"}, nil},
	} {
		if got := switchCommands(c.kind, c.from, c.to); !slices.Equal(got, c.want) {
			t.Errorf("switchCommands(%q, %q, %q) = %q, want %q", c.kind, c.from, c.to, got, c.want)
		}
	}
}
