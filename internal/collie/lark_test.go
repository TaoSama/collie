package collie

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A fake lark-cli: says who is logged in, and writes what it was asked to send.
func fakeLark(t *testing.T) string {
	dir := t.TempDir()
	sent := filepath.Join(dir, "sent")
	script := `#!/bin/sh
if [ "$1" = auth ]; then echo '{"identities":{"bot":{"available":true},"user":{"openId":"ou_test","userName":"T"}}}'; exit 0; fi
printf '%s\n' "$@" >> "` + sent + `"
`
	if err := os.WriteFile(filepath.Join(dir, "lark-cli"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	binCache.Delete("lark-cli")
	lark.get(true)
	t.Cleanup(func() { binCache.Delete("lark-cli"); lark.get(true) })
	return sent
}

func TestLarkNotify(t *testing.T) {
	sent := fakeLark(t)
	sp := &Super{st: &State{Settings: Settings{NotifyLark: true, NotifyLevel: "normal"}}}

	// Looking at Collie: held back.
	sp.seenPage("", "http://hub:7788", false)
	sp.queueNotifyLocked(&Entry{Role: "collie", Text: "做完了"})
	sp.sendNotifications()
	if _, err := os.Stat(sent); err == nil {
		t.Fatal("sent while you were looking")
	}

	// Away: sent, as the bot, to the logged-in user, with a link.
	sp.seenAt = time.Now().Add(-5 * time.Minute)
	sp.notifyAt = time.Time{}
	sp.queueNotifyLocked(&Entry{Role: "collie", Text: "报告：都好了"})
	sp.sendNotifications()
	b, err := os.ReadFile(sent)
	if err != nil {
		t.Fatal("nothing sent")
	}
	got := string(b)
	wantURL := `"default_url":"http://hub:7788/?lk_mobile_jump_to_browser=true&lk_jump_to_browser=true#/collie"`
	for _, want := range []string{"--as\nbot", "--user-id\nou_test", "--msg-type\ninteractive", `"schema":"2.0"`, "报告：都好了", wantURL} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "做完了") {
		t.Error("the held-back message went out later")
	}
}

func TestLarkJumpURLAndTextFallback(t *testing.T) {
	cases := map[string]string{
		"":                                    "",
		"http://hub:7788/#/collie":            "http://hub:7788/?lk_mobile_jump_to_browser=true&lk_jump_to_browser=true#/collie",
		"http://hub:7788":                     "http://hub:7788/?lk_mobile_jump_to_browser=true&lk_jump_to_browser=true",
		"https://collie.example/app?x=1#/p/2": "https://collie.example/app?x=1&lk_mobile_jump_to_browser=true&lk_jump_to_browser=true#/p/2",
	}
	for in, want := range cases {
		got := larkJumpURL(in)
		if got != want {
			t.Fatalf("larkJumpURL(%q) = %q, want %q", in, got, want)
		}
		// Idempotent when called a second time.
		if got2 := larkJumpURL(got); got2 != want {
			t.Fatalf("second larkJumpURL(%q) = %q, want %q", got, got2, want)
		}
	}

	// Plain-text fallback when interactive card is rejected also carries jump params.
	dir := t.TempDir()
	sent := filepath.Join(dir, "sent")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = auth ]; then echo '{\"identities\":{\"bot\":{\"available\":true},\"user\":{\"openId\":\"ou_test\",\"userName\":\"T\"}}}'; exit 0; fi\n" +
		"for a in \"$@\"; do if [ \"$a\" = interactive ]; then echo 'card rejected' >&2; exit 1; fi; done\n" +
		"printf '%s\\n' \"$@\" >> \"" + sent + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "lark-cli"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	binCache.Delete("lark-cli")
	lark.get(true)
	t.Cleanup(func() { binCache.Delete("lark-cli"); lark.get(true) })

	if err := larkSend([]notice{{text: "降级纯文本"}}, "http://hub:7788/#/collie"); err != nil {
		t.Fatalf("larkSend fallback err = %v", err)
	}
	b, err := os.ReadFile(sent)
	if err != nil {
		t.Fatal(err)
	}
	wantLine := "打开 Collie：http://hub:7788/?lk_mobile_jump_to_browser=true&lk_jump_to_browser=true#/collie"
	if !strings.Contains(string(b), wantLine) {
		t.Fatalf("plain-text fallback missing %q in:\n%s", wantLine, string(b))
	}
}

func TestCardSummary(t *testing.T) {
	got := cardSummary([]notice{{text: "写完了"}, {tone: "blocked", text: "测试：**fix-flaky-test** 在等你批准 `go test`"}, {text: "### 后面的"}})
	if want := "测试：fix-flaky-test 在等你批准 go test"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := cardSummary([]notice{{text: "\n- **完成**：好了"}}); got != "完成：好了" {
		t.Errorf("got %q", got)
	}
}

func TestQuote(t *testing.T) {
	sp := &Super{st: &State{Workers: map[string]*Worker{"rn": {Name: "rn", Task: "写发布说明"}}}}
	sp.st.Entries = []*Entry{{ID: "e1", Role: "user", Text: "查一下分页\n\n附件：\n/x/a.png\n\n[The human picked where this runs: spawn with --kind codex]"}}
	sp.st.Triggers = []*Trigger{
		{ID: "t1", Source: "chat", Ref: "e1"},
		{ID: "t2", Source: "worker", From: "rn", Ref: "rn"},
		{ID: "t3", Source: "timer", Text: "每天看一眼 CI"},
	}
	cases := map[string]string{"#t1": "你：查一下分页 📎", "e1": "你：查一下分页 📎", "t2": "rn：写发布说明", "t3": "定时：每天看一眼 CI"}
	for id, want := range cases {
		q := sp.quoteLocked(id)
		if q == nil || q.line() != want {
			t.Errorf("%s: got %+v, want %q", id, q, want)
		}
	}
	if sp.quoteLocked("t99") != nil || sp.quoteLocked("") != nil {
		t.Error("unknown ids should give no quote")
	}
}

func TestSayQuote(t *testing.T) {
	sp := &Super{st: &State{Entries: []*Entry{{ID: "e1", Role: "collie", Text: "要我开 PR 吗？"}, {ID: "e2", Role: "user", Text: "先别开"}, {ID: "e3", Role: "event", Text: "w 完成了一轮"}}}}
	if q := sp.sayQuoteLocked("e1"); q == nil || q.line() != "Collie：要我开 PR 吗？" {
		t.Errorf("got %+v", q)
	}
	if q := sp.sayQuoteLocked("e2"); q == nil || q.line() != "你：先别开" {
		t.Errorf("your own message: got %+v", q)
	}
	if sp.sayQuoteLocked("e3") != nil || sp.sayQuoteLocked("") != nil || sp.sayQuoteLocked("e9") != nil {
		t.Error("only chat messages are quoted")
	}
}
