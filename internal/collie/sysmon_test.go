package collie

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Each agent CLI words its --version differently; the number is what's kept.
func TestBinVersion(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct{ name, out, want string }{
		{"claude", "2.1.306 (Claude Code)", "2.1.306"},
		{"codex", "codex-cli 0.172.0", "0.172.0"},
		{"agy", "1.5.16", "1.5.16"},
		{"herdr", "herdr 0.9.5", "0.9.5"},
		{"none", "no version here", ""},
	} {
		bin := filepath.Join(dir, c.name)
		if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '"+c.out+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := binVersion(context.Background(), bin); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if got := binVersion(context.Background(), filepath.Join(dir, "missing")); got != "" {
		t.Errorf("missing binary: got %q", got)
	}
}
