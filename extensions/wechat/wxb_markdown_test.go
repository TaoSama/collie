package wechat

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// wxbCheckChunks asserts chunks rebuild source exactly, each within budget
// and each valid UTF-8 (no split surrogate pair).
func wxbCheckChunks(t *testing.T, source string, chunks []string, budget int) {
	t.Helper()
	if strings.Join(chunks, "") != source {
		t.Fatalf("budget %d: content changed", budget)
	}
	for _, c := range chunks {
		if c == "" || utf16Len(c) > budget || !utf8.ValidString(c) || strings.ContainsRune(c, utf8.RuneError) {
			t.Fatalf("budget %d: bad chunk %q", budget, c)
		}
	}
}

// from: gateway/tests/wechat/markdown.test.ts: default reply chunks use a 3800 UTF-16-unit budget and preserve full source
func TestWxbDefaultReplyChunkBudget(t *testing.T) {
	if DefaultMessageMaxChars != 3800 {
		t.Fatalf("DefaultMessageMaxChars = %d", DefaultMessageMaxChars)
	}
	source := strings.Repeat("中", DefaultMessageMaxChars-1) + "🚀" + " end\n  **原文**\n"
	chunks, err := SplitWeChatReply(source, DefaultMessageMaxChars)
	mustNoErr(t, err)
	if len(chunks) < 2 || utf16Len(chunks[0]) != DefaultMessageMaxChars-1 {
		t.Fatalf("chunks = %d, first = %d units", len(chunks), utf16Len(chunks[0]))
	}
	wxbCheckChunks(t, source, chunks, DefaultMessageMaxChars)
	emoji := strings.Repeat("🚀", DefaultMessageMaxChars/2)
	single, err := SplitWeChatReply(emoji, DefaultMessageMaxChars)
	mustNoErr(t, err)
	if len(single) != 1 || single[0] != emoji {
		t.Fatalf("exact-budget emoji text split into %d", len(single))
	}
}

// from: gateway/tests/wechat/markdown.test.ts: rejects non-integer and non-finite message budgets
func TestWxbSplitRejectsInvalidBudgets(t *testing.T) {
	// Go's int budget cannot be NaN, infinite or fractional; the remaining
	// invalid budgets are zero and negative.
	for _, budget := range []int{0, -1} {
		if _, err := SplitWeChatReply("text", budget); !errors.Is(err, errBadMaxChars) {
			t.Fatalf("budget %d: error %v", budget, err)
		}
	}
}

// from: gateway/tests/wechat/markdown.test.ts: multipart replies preserve all emoji without splitting surrogate pairs
func TestWxbSplitPreservesEmoji(t *testing.T) {
	source := strings.Repeat("状态🚀👩‍💻执行中", 20)
	for budget := 2; budget < 40; budget++ {
		chunks, err := SplitWeChatReply(source, budget)
		mustNoErr(t, err)
		wxbCheckChunks(t, source, chunks, budget)
	}
	if _, err := SplitWeChatReply("🚀", 1); !errors.Is(err, errCannotFit) {
		t.Fatalf("one unit for an emoji: %v", err)
	}
	if _, err := SplitWeChatReply("text", 0); err == nil {
		t.Fatal("zero budget accepted")
	}
	ab, err := SplitWeChatReply("ab", 1)
	mustNoErr(t, err)
	if strings.Join(ab, "|") != "a|b" {
		t.Fatalf("ab = %q", ab)
	}
}

// from: gateway/tests/wechat/markdown.test.ts: multipart replies preserve code indentation, blank lines and spaces exactly
func TestWxbSplitPreservesCode(t *testing.T) {
	source := "if ready:\n    run()\n\n    report('完成🚀')\n"
	for budget := 2; budget <= utf16Len(source)+1; budget++ {
		chunks, err := SplitWeChatReply(source, budget)
		mustNoErr(t, err)
		wxbCheckChunks(t, source, chunks, budget)
	}
	ws, err := SplitWeChatReply(" \n\t", 2)
	mustNoErr(t, err)
	if strings.Join(ws, "") != " \n\t" {
		t.Fatalf("whitespace changed: %q", ws)
	}
}
