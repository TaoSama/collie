package collie

import (
	"strings"
	"testing"
)

func TestLastTurnOpen(t *testing.T) {
	pad := strings.Repeat(`{"type":"event_msg","payload":{"type":"token_count"}}`+"\n", 40000) // ~2 MB
	start := `{"type":"event_msg","payload":{"type":"task_started"}}` + "\n"
	done := `{"type":"event_msg","payload":{"type":"task_complete"}}` + "\n"
	for _, c := range []struct {
		name, log string
		want      bool
	}{
		{"start far back", start + pad, true},
		{"ended far back", start + done + pad, false},
		{"new turn after end", start + done + start + pad, true},
		{"nothing", pad, false},
	} {
		if got := lastTurnOpen(strings.NewReader(c.log), int64(len(c.log))); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestDeathReason(t *testing.T) {
	screen := "  ~  codex -m gpt\nfile:///x/codex.js:108\n  throw new Error(\n        ^\nError: Missing optional dependency @openai/codex-darwin-arm64. Reinstall Codex\n    at findCodexExecutable (file:///x)\nNode.js v22.22.0"
	if got := deathReason(screen); got != "Error: Missing optional dependency @openai/codex-darwin-arm64. Reinstall Codex" {
		t.Fatalf("got %q", got)
	}
	if got := deathReason("zsh: command not found: claude"); got != "zsh: command not found: claude" {
		t.Fatalf("got %q", got)
	}
	if got := deathReason("❯ claude --resume 0d1\nNo conversation found with session ID: 0d1\n❯"); got != "No conversation found with session ID: 0d1" {
		t.Fatalf("got %q", got)
	}
}
