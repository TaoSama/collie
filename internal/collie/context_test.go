package collie

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContextUsage(t *testing.T) {
	dir := t.TempDir()
	cl := filepath.Join(dir, "c.jsonl")
	os.WriteFile(cl, []byte(`{"type":"assistant","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":1000,"cache_creation_input_tokens":0,"output_tokens":5}}}
{"type":"assistant","isSidechain":true,"message":{"usage":{"input_tokens":99999}}}
{"type":"user","message":{"content":"hi"}}
{"type":"assistant","message":{"usage":{"input_tokens":731,"cache_read_input_tokens":37688,"cache_creation_input_tokens":0,"output_tokens":71}}}
`), 0o600)
	if u := contextUsage(cl, "claude", 0); u.Used != 38490 || u.Window != 200000 || u.Pct() != 19 {
		t.Fatalf("claude: %+v", u)
	}
	cx := filepath.Join(dir, "x.jsonl")
	os.WriteFile(cx, []byte(`{"type":"turn_context","payload":{"cwd":"/x","model":"gpt-6.2-sol","effort":"medium"}}
{"type":"event_msg","payload":{"type":"token_count","info":null}}
{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":21500,"total_tokens":21619},"model_context_window":475000}}}
`), 0o600)
	if u := contextUsage(cx, "codex", 0); u.Used != 21619 || u.Window != 475000 || u.Pct() != 4 || u.Model != "gpt-6.2-sol" || u.Effort != "medium" {
		t.Fatalf("codex: %+v", u)
	}
}

func TestCollieBody(t *testing.T) {
	logged := "Another Claude session sent a message:\n" + peerEnvelope("只回复两个字：收到啦") + "\n\nThis came from another Claude session — not the user."
	if b, ok := collieBody(logged); !ok || b != "只回复两个字：收到啦" {
		t.Fatalf("got %q %v", b, ok)
	}
	if _, ok := collieBody("hello"); ok {
		t.Fatal("plain text taken as a peer message")
	}
}

func TestReplyLen(t *testing.T) {
	if n := replyLen("file-counter 做完了：6 个文件"); n != 8 {
		t.Fatalf("replyLen = %d", n)
	}
	s := Settings{ReplyWords: 100}
	s.migrateReply()
	if s.ReplyChars != 200 || s.ReplyWords != 0 {
		t.Fatalf("migrate = %+v", s)
	}
	s = Settings{}
	s.migrateReply()
	if s.ReplyChars != 200 {
		t.Fatalf("default = %d", s.ReplyChars)
	}
}

func TestWithBypass(t *testing.T) {
	cases := []struct {
		kind string
		ask  bool
		in   []string
		want string
	}{
		{"claude", false, []string{"--model", "opus"}, "--model opus --dangerously-skip-permissions"},
		{"antigravity", false, nil, "--dangerously-skip-permissions"},
		{"codex", false, []string{"-m", "x"}, "-m x --dangerously-bypass-approvals-and-sandbox"},
		{"claude", true, []string{"--model", "opus"}, "--model opus"},
		{"claude", false, []string{"--dangerously-skip-permissions"}, "--dangerously-skip-permissions"},
	}
	for _, c := range cases {
		if got := strings.Join(withBypass(c.in, c.kind, c.ask), " "); got != c.want {
			t.Errorf("withBypass(%v, %s, %v) = %q, want %q", c.in, c.kind, c.ask, got, c.want)
		}
	}
}

func TestCollieBodyOldHead(t *testing.T) {
	for _, h := range append([]string{peerHead}, oldPeerHeads...) {
		s := "<cross-session-message from-name=\"Collie\">\n" + h + "\n\nBlue\n</cross-session-message>"
		if b, ok := collieBody(s); !ok || b != "Blue" {
			t.Fatalf("%q -> %q", h[:20], b)
		}
	}
}
