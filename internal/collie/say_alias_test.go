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

// An alias runs its own model: a model picked with one goes nowhere, and the
// effort still does.
func TestAliasDropsModel(t *testing.T) {
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
		body  map[string]any
		model bool // whether --model is passed on
	}{
		{map[string]any{"text": "a", "model": "fable", "effort": "max"}, false},                // the default alias
		{map[string]any{"text": "b", "kind": "codex", "alias": "cx", "model": "gpt-6"}, false}, // another one
		{map[string]any{"text": "c", "alias": "", "model": "fable"}, true},                     // none
	} {
		if rec := call("POST", "/api/super/say", c.body); rec.Code != 200 {
			t.Fatalf("%v: status %d", c.body, rec.Code)
		}
		if got := last(); strings.Contains(got, "--model") != c.model {
			t.Errorf("%v: %q", c.body, got)
		}
	}
	if !strings.Contains(last(), "--model fable") {
		t.Errorf("no alias: %q", last())
	}

	ns := Settings{RouterAlias: "ccx", RouterModel: "fable", RouterArgs: []string{"--x"}, WorkerAlias: "ccf", WorkerModel: "opus", WorkerEffort: "high"}
	ns.aliasesOwnModels()
	if ns.RouterModel != "" || ns.RouterArgs != nil || ns.WorkerModel != "" || ns.WorkerEffort != "high" {
		t.Errorf("saved with an alias: %+v", ns)
	}
	ns = Settings{RouterModel: "fable", WorkerModel: "opus"}
	if ns.aliasesOwnModels(); ns.RouterModel != "fable" || ns.WorkerModel != "opus" {
		t.Errorf("no alias: %+v", ns)
	}
}

func TestSpawnThroughAliasRunsItsModel(t *testing.T) {
	f := newFakeHerdr(t)
	sp := newRouterTestSuper(t, f)
	sp.st.Settings.WorkerKind, sp.st.Settings.WorkerAlias, sp.st.Settings.WorkerModel = "claude", "ccx", "opus"
	w, started, err := sp.Spawn(SpawnSpec{Kind: "claude", Cwd: "/", Task: "x", Name: "aw", Model: "fable", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if w.Run == nil || *w.Run != [2]string{"", "high"} || *w.Alias != "ccx" {
		t.Errorf("run %v alias %v", w.Run, w.Alias)
	}
}
