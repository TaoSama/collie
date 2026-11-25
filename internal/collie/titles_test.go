package collie

import (
	"strings"
	"testing"
)

// cleanTitle drops only the spinner an agent shows before its topic; a
// title's own emoji and brackets stay, and long titles are cut by character.
func TestCleanTitle(t *testing.T) {
	long := strings.Repeat("长", 70)
	for in, want := range map[string]string{
		"✳ Fix the login bug": "Fix the login bug",
		"⠐ Fix the login bug": "Fix the login bug",
		"⠂  ✶ Topic":          "Topic",
		"🟢 [下载] Pacific ROM · 直连下载完成 | GPT-6.2 Sol": "🟢 [下载] Pacific ROM · 直连下载完成 | GPT-6.2 Sol",
		"[Hakimi] 多机安装 · 五机完成 | Seed Code":          "[Hakimi] 多机安装 · 五机完成 | Seed Code",
		"「引号」开头":                        "「引号」开头",
		"#123 issue":                    "#123 issue",
		"✳ Claude Code":                 "",
		"  codex ":                      "",
		long + "x":                      strings.Repeat("长", titleRunes) + " …",
		strings.Repeat("长", titleRunes): strings.Repeat("长", titleRunes),
	} {
		if got := cleanTitle(in); got != want {
			t.Errorf("cleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
