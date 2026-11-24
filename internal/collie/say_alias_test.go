package collie

import (
	"strings"
	"testing"
)

// The composer's alias goes to the router as a spawn flag, only when it
// differs from the default the router would use anyway.
func TestSayAliasHint(t *testing.T) {
	sp, _, call := newPendingTestEnv(t)
	sp.mu.Lock()
	sp.st.Settings.WorkerKind, sp.st.Settings.WorkerAlias = "claude", "ccf"
	sp.mu.Unlock()
	last := func() string {
		sp.mu.Lock()
		defer sp.mu.Unlock()
		return sp.st.Triggers[len(sp.st.Triggers)-1].Text
	}
	for _, c := range []struct {
		body map[string]any
		want string // "" for no alias flag
	}{
		{map[string]any{"text": "a", "kind": "codex", "alias": "cx"}, "--alias cx"},
		{map[string]any{"text": "b", "kind": "claude", "alias": ""}, "--no-alias"},
		{map[string]any{"text": "c", "kind": "claude", "alias": "ccf"}, ""},
		{map[string]any{"text": "d"}, ""},
		{map[string]any{"text": "e", "alias": "bad name;rm"}, ""},
	} {
		if rec := call("POST", "/api/super/say", c.body); rec.Code != 200 {
			t.Fatalf("%v: status %d", c.body, rec.Code)
		}
		got := last()
		if c.want == "" && strings.Contains(got, "alias") {
			t.Errorf("%v: unexpected alias hint in %q", c.body, got)
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%v: %q lacks %q", c.body, got, c.want)
		}
	}
}
