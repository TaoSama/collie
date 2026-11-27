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

func TestTokenMeterByProviderAndAntigravity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	now := time.Date(2026, 11, 27, 15, 30, 30, 0, time.Local)
	ts := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339Nano) }

	// Claude Code transcript
	appendFile(t, filepath.Join(home, ".claude/projects/-w/a.jsonl"),
		claudeUsageLine("m1", ts(-2*time.Minute), 100, 50, 800, 50))
	// Codex transcript with turn_context model + token_count
	rollout := filepath.Join(home, ".codex/sessions", now.Format("2006/01/02"), "rollout-1.jsonl")
	appendFile(t, rollout,
		`{"timestamp":"`+ts(-2*time.Minute)+`","type":"turn_context","payload":{"model":"gpt-6.2-sol"}}`+"\n"+
			`{"timestamp":"`+ts(-1*time.Minute)+`","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":400},"last_token_usage":{"input_tokens":350,"cached_input_tokens":300,"total_tokens":400}}}}`+"\n")
	// Antigravity CLI transcript (duplicate step_index must deduplicate)
	agyLog := filepath.Join(home, ".gemini/antigravity-cli/brain/conv-1/.system_generated/logs/transcript.jsonl")
	appendFile(t, agyLog,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"`+ts(-1*time.Minute)+`","model":"gemini-4-neon-high","input_tokens":200,"cache_read_tokens":1000,"output_tokens":100}`+"\n"+
			`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"`+ts(-1*time.Minute)+`","model":"gemini-4-neon-high","input_tokens":200,"cache_read_tokens":1000,"output_tokens":100}`+"\n")

	m := newTokenMeter(home)
	m.scan(now)
	st := m.todayDetail(now)
	if st.Tokens != 1000+400+1300 {
		t.Fatalf("total today tokens = %d, want %d", st.Tokens, 2700)
	}
	if len(st.ByProvider) != 3 {
		t.Fatalf("by_provider len = %d, want 3: %+v", len(st.ByProvider), st.ByProvider)
	}
	if got := st.ByProvider["claude"]; got.Tokens != 1000 || got.CacheRead != 800 || got.CacheInput != 950 {
		t.Fatalf("claude provider stats = %+v", got)
	}
	if got := st.ByProvider["codex"]; got.Tokens != 400 || got.CacheRead != 300 || got.CacheInput != 350 {
		t.Fatalf("codex provider stats = %+v", got)
	}
	if got := st.ByProvider["antigravity"]; got.Tokens != 1300 || got.Requests != 1 || got.CacheRead != 1000 || got.CacheInput != 1200 {
		t.Fatalf("antigravity provider stats = %+v", got)
	}
}
