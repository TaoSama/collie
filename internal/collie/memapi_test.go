package collie

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDropIndexLine(t *testing.T) {
	index := filepath.Join(t.TempDir(), "index.md")
	in := "# Notes\n\nOne line per file in this folder.\n- agents.md — agent 惯例\n- codex-agents.md — Codex 的\n- [机器](notes/agents.md)\n- agy.md：agy 配置\n"
	if err := os.WriteFile(index, []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dropIndexLine(index, "agents.md"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	want := "# Notes\n\nOne line per file in this folder.\n- codex-agents.md — Codex 的\n- agy.md：agy 配置\n"
	if string(b) != want {
		t.Fatalf("index =\n%s\nwant\n%s", b, want)
	}
	if err := dropIndexLine(filepath.Join(t.TempDir(), "missing.md"), "agents.md"); err != nil {
		t.Fatalf("missing index: %v", err)
	}
}

func TestMemRel(t *testing.T) {
	for path, want := range map[string]bool{
		"notes/agents.md": true, "tasks/done/x.md": true, "instructions.md": true,
		"messages.md": false, "../memory.md": false, "/etc/x.md": false, "notes/x.txt": false,
	} {
		if _, ok := memRel(path); ok != want {
			t.Errorf("memRel(%q) = %v, want %v", path, ok, want)
		}
	}
}
