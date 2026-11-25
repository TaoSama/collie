package collie

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHumanAtSkipsCollieAndToolResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	log := `{"type":"summary","summary":"x"}
{"type":"user","timestamp":"2026-11-25T01:00:00Z","message":{"content":"You are worker \"w\", managed by Collie"}}
{"type":"user","timestamp":"2026-11-25T02:00:00Z","message":{"content":"fix the tests"}}
{"type":"assistant","timestamp":"2026-11-25T02:01:00Z","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}
{"type":"user","timestamp":"2026-11-25T02:02:00Z","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}
{"type":"user","timestamp":"2026-11-25T03:00:00Z","message":{"content":"[collie] 1 new event\n..."}}
`
	if err := os.WriteFile(path, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 25, 2, 0, 0, 0, time.UTC)
	if got := humanAt(path, "claude"); !got.Equal(want) {
		t.Fatalf("humanAt = %v, want %v", got, want)
	}

	// Only what the log grows by is read: the agent's own work keeps the
	// time, a new message of the human's moves it.
	appendLog(t, path, `{"type":"assistant","timestamp":"2026-11-25T04:00:00Z","message":{"content":[{"type":"text","text":"done"}]}}
`)
	if got := humanAt(path, "claude"); !got.Equal(want) {
		t.Fatalf("after the agent's reply: humanAt = %v, want %v", got, want)
	}
	appendLog(t, path, `{"type":"user","timestamp":"2026-11-25T05:00:00Z","message":{"content":"thanks"}}`) // not whole yet
	if got := humanAt(path, "claude"); !got.Equal(want) {
		t.Fatalf("with a partial line: humanAt = %v, want %v", got, want)
	}
	appendLog(t, path, "\n")
	if got, want := humanAt(path, "claude"), time.Date(2026, 11, 25, 5, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("after the human's message: humanAt = %v, want %v", got, want)
	}
}

func TestHumanAtFallsBackToStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	log := `{"timestamp":"2026-11-25T01:00:00.5Z","type":"session_meta","payload":{"id":"x"}}
{"timestamp":"2026-11-25T01:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions"}]}}
{"timestamp":"2026-11-25T01:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"[collie] 1 new event"}]}}
`
	if err := os.WriteFile(path, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 25, 1, 0, 0, 5e8, time.UTC)
	if got := humanAt(path, "codex"); !got.Equal(want) {
		t.Fatalf("humanAt = %v, want the start %v", got, want)
	}
}

func TestHumanAtAntigravity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	log := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","created_at":"2026-11-25T09:44:26Z","content":"<USER_REQUEST>\nlook at this\n</USER_REQUEST>"}
{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","created_at":"2026-11-25T09:45:00Z","content":"ok"}
`
	if err := os.WriteFile(path, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := humanAt(path, "agy"), time.Date(2026, 11, 25, 9, 44, 26, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("humanAt = %v, want %v", got, want)
	}
}

// A long run of the agent's own lines after the human's message is read
// back through, window by window.
func TestHumanAtLooksPastTheTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	var b strings.Builder
	b.WriteString(`{"type":"user","timestamp":"2026-11-25T02:00:00Z","message":{"content":"go"}}` + "\n")
	line := `{"type":"assistant","timestamp":"2026-11-25T03:00:00Z","message":{"content":[{"type":"text","text":"` + strings.Repeat("x", 1000) + `"}]}}` + "\n"
	for b.Len() < 3<<20 {
		b.WriteString(line)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := humanAt(path, "claude"), time.Date(2026, 11, 25, 2, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("humanAt = %v, want %v", got, want)
	}
}

func appendLog(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}
