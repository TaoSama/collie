package collie

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// agyHome: a home whose agy lists these models (as `agy models` did, 1.5.17)
// and whose settings.json picks Gemini 3.8 Flash (Medium).
func agyHome(t *testing.T) string {
	t.Helper()
	agyListCache.Lock()
	old := agyListCache.list
	agyListCache.at, agyListCache.list = time.Now(), []ModelOpt{
		{ID: "gemini-4-hydrogen-high", Label: "Gemini 4 Hydrogen (High)"},
		{ID: "gemini-4-hydrogen-xhigh", Label: "Gemini 4 Hydrogen (XHigh)"},
		{ID: "gemini-4-hydrogen-low", Label: "Gemini 4 Hydrogen (Low)"},
		{ID: "gemini-3.8-flash-medium", Label: "Gemini 3.8 Flash (Medium)"},
		{ID: "gemini-3.8-flash-low", Label: "Gemini 3.8 Flash (Low)"},
		{ID: "claude-sonnet-4-6", Label: "Claude Sonnet 4.6 (Thinking)"},
		{ID: "gpt-oss-120b-medium", Label: "GPT-OSS 120B (Medium)"},
	}
	agyListCache.Unlock()
	t.Cleanup(func() { agyListCache.Lock(); agyListCache.list = old; agyListCache.Unlock() })
	home := t.TempDir()
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"model": "Gemini 3.8 Flash (Medium)"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestAgyModels(t *testing.T) {
	s := &Server{home: agyHome(t)}
	info := &ModelInfo{Kind: "agy", Efforts: allEfforts}
	s.agyModels(context.Background(), info)
	want := []ModelOpt{
		{ID: "gemini-4-hydrogen", Label: "Gemini 4 Hydrogen", Efforts: []string{"low", "high", "xhigh"}},
		{ID: "gemini-3.8-flash", Label: "Gemini 3.8 Flash", Efforts: []string{"low", "medium"}},
		{ID: "claude-sonnet-4-6", Label: "Claude Sonnet 4.6 (Thinking)", Efforts: []string{}}, // takes no --effort
		{ID: "gpt-oss-120b", Label: "GPT-OSS 120B", Efforts: []string{"medium"}},
	}
	if !reflect.DeepEqual(info.Models, want) {
		t.Errorf("models = %+v", info.Models)
	}
	if !reflect.DeepEqual(info.Efforts, []string{"low", "medium"}) { // the default model's
		t.Errorf("efforts = %q", info.Efforts)
	}
	if info.Default != "gemini-3.8-flash" || info.DefaultEffort != "medium" {
		t.Errorf("default = %q %q", info.Default, info.DefaultEffort)
	}
}

func TestAgyLaunchModel(t *testing.T) {
	for args, want := range map[string][2]string{
		"--model gemini-3.8-flash-high":               {"gemini-3.8-flash", "high"},
		"--model gemini-3.8-flash --effort low":       {"gemini-3.8-flash", "low"},
		"--model claude-opus-4-6-thinking":            {"claude-opus-4-6-thinking", ""},
		"--dangerously-skip-permissions --effort max": {"", "max"},
	} {
		a, ok := parseLauncher("ag", "agy "+args)
		if !ok || [2]string{a.Model, a.Effort} != want {
			t.Errorf("agy %s: %q %q, want %q", args, a.Model, a.Effort, want)
		}
	}
	// A model id that names its level refuses any other --effort.
	ag, _ := parseLauncher("ag", "agy --model gemini-3.8-flash-high")
	if got := aliasArgs(&ag, []string{"--effort", "low", "--dangerously-skip-permissions"}); !reflect.DeepEqual(got, []string{"--dangerously-skip-permissions"}) {
		t.Errorf("levelled model: %q", got)
	}
	plain, _ := parseLauncher("ag", "agy --model gemini-3.8-flash")
	if got := aliasArgs(&plain, []string{"--effort", "low"}); !reflect.DeepEqual(got, []string{"--effort", "low"}) {
		t.Errorf("model without level: %q", got)
	}
}

func TestAgyAliasRuns(t *testing.T) {
	home := agyHome(t)
	for line, want := range map[string]string{
		"agy":                                "gemini-3.8-flash", // settings.json, by display name
		"agy --model gemini-4-hydrogen-high": "gemini-4-hydrogen",
		`agy --settings '{"model":"GPT-OSS 120B (Medium)"}'`: "gpt-oss-120b",
		`agy --settings '{"mode":"plan"}'`:                   "gemini-3.8-flash", // overrides other keys only
	} {
		a, ok := parseLauncher("ag", line)
		if !ok {
			t.Fatalf("%s: not a launcher", line)
		}
		if got := aliasRuns(home, &a); got != want {
			t.Errorf("%s runs %q, want %q", line, got, want)
		}
	}
}

func TestAgyModelArgsEffort(t *testing.T) {
	agyHome(t)
	for _, c := range []struct {
		model, effort string
		want          []string
	}{
		{"gemini-4-hydrogen", "high", []string{"--model", "gemini-4-hydrogen", "--effort", "high"}},
		{"", "high", []string{"--effort", "high"}},
		{"gemini-3.8-flash-medium", "high", []string{"--model", "gemini-3.8-flash-medium"}}, // the id names its level
		{"claude-sonnet-4-6", "high", []string{"--model", "claude-sonnet-4-6"}},             // no levels
		{"gemini-9-unlisted", "high", []string{"--model", "gemini-9-unlisted", "--effort", "high"}},
	} {
		if got := modelArgs("agy", c.model, c.effort); !reflect.DeepEqual(got, c.want) {
			t.Errorf("modelArgs(agy, %q, %q) = %q, want %q", c.model, c.effort, got, c.want)
		}
	}
	if got := modelArgs("claude", "claude-sonnet-4-6", "high"); !reflect.DeepEqual(got, []string{"--model", "claude-sonnet-4-6", "--effort", "high"}) {
		t.Errorf("claude lost its --effort: %q", got)
	}
}
