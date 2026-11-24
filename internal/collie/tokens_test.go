package collie

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func claudeUsageLine(id, ts string, in, write, read, out int) string {
	return `{"type":"assistant","timestamp":"` + ts + `","message":{"id":"` + id + `","model":"claude-opus-5-5","usage":{"input_tokens":` +
		strconv.Itoa(in) + `,"cache_creation_input_tokens":` + strconv.Itoa(write) + `,"cache_read_input_tokens":` + strconv.Itoa(read) + `,"output_tokens":` + strconv.Itoa(out) + `}}}` + "\n"
}

func codexCountLine(ts string, total, last int) string {
	return `{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":` +
		strconv.Itoa(total) + `},"last_token_usage":{"total_tokens":` + strconv.Itoa(last) + `}}}}` + "\n"
}

func appendFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestTokenMeter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	now := time.Date(2026, 11, 24, 15, 30, 30, 0, time.Local)
	ts := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339Nano) }
	main := filepath.Join(home, ".claude/projects/-w/a.jsonl")
	// One reply over three lines (one per content block), the last saying the most.
	appendFile(t, main, claudeUsageLine("m1", ts(-90*time.Second), 2, 100, 1000, 10)+
		claudeUsageLine("m1", ts(-90*time.Second), 2, 100, 1000, 10)+
		claudeUsageLine("m1", ts(-90*time.Second), 2, 100, 1000, 40)+
		`{"type":"user","message":{"content":"hi"}}`+"\n"+
		claudeUsageLine("old", now.Add(-30*time.Hour).UTC().Format(time.RFC3339Nano), 0, 0, 5, 5))
	// A fork copies the reply into another file: still once.
	appendFile(t, filepath.Join(home, ".claude/projects/-w/fork.jsonl"), claudeUsageLine("m1", ts(-90*time.Second), 2, 100, 1000, 40))
	rollout := filepath.Join(home, ".codex/sessions", now.Format("2006/01/02"), "rollout-x.jsonl")
	appendFile(t, rollout, codexCountLine(ts(-20*time.Second), 300, 300)+codexCountLine(ts(-10*time.Second), 300, 300)+
		codexCountLine(ts(-5*time.Second), 500, 200))

	m := newTokenMeter(home)
	m.scan(now)
	r := m.report(now)
	if want := int64(1142 + 500); r.Today != want {
		t.Fatalf("today = %d, want %d", r.Today, want)
	}
	if len(r.Recent) != tokenRecent || r.Recent[tokenRecent-1] != 500 || r.Recent[tokenRecent-2] != 1142 {
		t.Fatalf("recent tail = %v", r.Recent[tokenRecent-3:])
	}
	if r.PerMinute != 656 { // 1642 over two and a half minutes
		t.Fatalf("per minute = %d", r.PerMinute)
	}

	// Only what was added is read; a line not finished yet waits.
	half := claudeUsageLine("m2", ts(0), 0, 0, 0, 7)
	appendFile(t, main, half[:20])
	m.scan(now)
	if got := m.report(now).Today; got != 1642 {
		t.Fatalf("today after a half line = %d", got)
	}
	appendFile(t, main, half[20:])
	m.scan(now)
	if got := m.report(now).Today; got != 1649 {
		t.Fatalf("today after the line ended = %d", got)
	}
}

func TestTokenMeterForgets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	now := time.Now()
	appendFile(t, filepath.Join(home, ".claude/projects/-w/a.jsonl"), claudeUsageLine("m1", now.UTC().Format(time.RFC3339Nano), 0, 0, 0, 9))
	m := newTokenMeter(home)
	m.scan(now)
	later := now.Add(tokenKeep + 2*time.Minute)
	m.scan(later)
	if len(m.seen) != 0 || len(m.minutes) != 0 {
		t.Fatalf("kept %d replies, %d minutes", len(m.seen), len(m.minutes))
	}
}
