package collie

import (
	"strings"
	"testing"
)

// Answering your own last message in a run of replies keeps the quote, and
// tells Collie whose words they are.
func TestSayQuotesYourOwnMessage(t *testing.T) {
	sp, _, call := newPendingTestEnv(t)
	sp.mu.Lock()
	mine := sp.addEntryLocked("user", mainThread, "把分页也查一下", "", "")
	theirs := sp.addEntryLocked("collie", mainThread, "好，我派人去看", "", "")
	sp.mu.Unlock()
	last := func() (*Entry, string) {
		sp.mu.Lock()
		defer sp.mu.Unlock()
		return sp.st.Entries[len(sp.st.Entries)-1], sp.st.Triggers[len(sp.st.Triggers)-1].Text
	}
	for _, c := range []struct {
		re, who, said string
	}{
		{mine.ID, "你", "what the human said earlier"},
		{theirs.ID, "Collie", "what you said earlier"},
	} {
		if rec := call("POST", "/api/super/say", map[string]any{"text": "还有排序", "re": c.re}); rec.Code != 200 {
			t.Fatalf("%s: status %d", c.re, rec.Code)
		}
		e, prompt := last()
		if e.Re == nil || e.Re.Entry != c.re || e.Re.Who != c.who {
			t.Errorf("%s: quote = %+v", c.re, e.Re)
		}
		if !strings.HasPrefix(prompt, "[In reply to "+c.said) {
			t.Errorf("%s: prompt %q", c.re, prompt)
		}
	}
}
