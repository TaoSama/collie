package collie

import (
	"flag"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestParseReplyFlagsAnywhere(t *testing.T) {
	cases := []struct {
		name string
		args []string
		text string
		opts []string
		re   string
	}{
		{"flags before", []string{"--re", "t1", "--opt", "A", "--opt", "B", "要发吗？"}, "要发吗？", []string{"A", "B"}, "t1"},
		{"flags after", []string{"要发吗？", "--opt", "A", "--opt", "B", "--re", "t1"}, "要发吗？", []string{"A", "B"}, "t1"},
		{"interleaved", []string{"--opt", "A", "要", "--re=t1", "发吗？", "--opt", "B"}, "要 发吗？", []string{"A", "B"}, "t1"},
		{"dash text stays text", []string{"温度", "-5度", "->", "-", "---", "--opt", "A"}, "温度 -5度 -> - ---", []string{"A"}, ""},
		{"plain -- inside a quoted text", []string{"a -- b", "--opt", "A"}, "a -- b", []string{"A"}, ""},
		{"after -- is all text", []string{"--opt", "A", "--", "用", "--opt", "加按钮", "--re"}, "用 --opt 加按钮 --re", []string{"A"}, ""},
		{"flag value may look like a flag", []string{"--opt", "--check", "文本"}, "文本", []string{"--check"}, ""},
		{"a flag takes -- as its value", []string{"--opt", "--", "文本"}, "文本", []string{"--"}, ""},
	}
	for _, c := range cases {
		body, err := parseReply(c.args, nil, true)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		opts, _ := body["options"].([]string)
		if body["text"] != c.text || !slices.Equal(opts, c.opts) || body["re"] != c.re {
			t.Errorf("%s: got %+v", c.name, body)
		}
	}

	check, err := parseReply([]string{"请审阅", "--check", "--thread", "chat:x"}, nil, true)
	if err != nil || check["check"] != true || check["thread"] != "chat:x" || check["text"] != "请审阅" {
		t.Fatalf("trailing --check/--thread: %+v, %v", check, err)
	}
}

func TestParseReplyRefusesStrayFlags(t *testing.T) {
	for _, args := range [][]string{
		{"要发吗？ --opt 发 --opt 不发"},    // the whole command quoted as one argument
		{"--opt", "A", "说明 --re=t3"}, // --name=value inside the text
		{"收到 -check"},                // single-dash spelling too
	} {
		if _, err := parseReply(args, nil, true); err == nil || !strings.Contains(err.Error(), "after --") {
			t.Errorf("%q: want a stray-flag error, got %v", args, err)
		}
	}
	if b, err := parseReply([]string{"收到", "-check"}, nil, true); err != nil || b["check"] != true {
		t.Errorf("a separate -check is a flag: %+v, %v", b, err)
	}
	if _, err := parseReply([]string{"--", "要发吗？ --opt 发"}, nil, true); err != nil {
		t.Errorf("text after -- should go through: %v", err)
	}
	if _, err := parseReply([]string{"讲讲 --verbose 的用法"}, nil, true); err != nil {
		t.Errorf("an unknown --word in text is not a reply flag: %v", err)
	}
}

func TestParseArgsErrorsAndHelp(t *testing.T) {
	newFS := func() *flag.FlagSet {
		fs := flag.NewFlagSet("send", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("thread", "", "")
		return fs
	}
	if err := parseArgs(newFS(), []string{"w", "hi", "--thread"}); err == nil {
		t.Error("a flag missing its value should fail")
	}
	if err := parseArgs(newFS(), []string{"w", "--nope", "hi"}); err == nil {
		t.Error("an unknown flag should fail")
	}
	for _, h := range []string{"-h", "--help"} {
		if err := parseArgs(newFS(), []string{"w", h}); err != flag.ErrHelp {
			t.Errorf("%s: got %v, want flag.ErrHelp", h, err)
		}
	}
	fs := newFS()
	if err := parseArgs(fs, []string{"w1", "--thread", "chat:a", "do", "it"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fs.Args(), []string{"w1", "do", "it"}) || fs.Lookup("thread").Value.String() != "chat:a" {
		t.Errorf("send args = %q thread = %s", fs.Args(), fs.Lookup("thread").Value)
	}
}
