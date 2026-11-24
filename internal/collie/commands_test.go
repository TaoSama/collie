package collie

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// customOnly: the entries that aren't the CLI's own, by sigil (none for "/") and name.
func customOnly(list []SlashCmd) map[string]SlashCmd {
	m := map[string]SlashCmd{}
	for _, c := range list {
		if c.Src != "builtin" {
			m[c.Sigil+c.Name] = c
		}
	}
	return m
}

func keys(m map[string]SlashCmd) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestClaudeCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	repo := filepath.Join(home, "code", "app")
	writeFiles(t, home, map[string]string{
		".claude/skills/go/SKILL.md":                       "---\nname: go\ndescription: |\n  Take an idea\n  to a change.\n---\n",
		".claude/skills/dirname/SKILL.md":                  "---\ndescription: Named by its folder\nargument-hint: \"[pr]\"\n---\n",
		".claude/skills/nobody/SKILL.md":                   "---\nname: nobody\ndescription: x\nuser-invocable: false\n---\n",
		".claude/skills/nodesc/SKILL.md":                   "---\nname: nodesc\n---\n\n# First line of the body\n",
		".claude/skills/notaskill/README.md":               "no SKILL.md here",
		".claude/skills/bundle/.claude-plugin/plugin.json": `{"name": "kit"}`,
		".claude/skills/bundle/skills/doc/SKILL.md":        "---\nname: doc\ndescription: Docs\n---\n",
		".claude/skills/off/.claude-plugin/plugin.json":    `{"name": "off"}`,
		".claude/skills/off/skills/x/SKILL.md":             "---\nname: x\ndescription: X\n---\n",
		".claude/commands/simplify.md":                     "---\ndescription: \"Clean up\"\n---\nbody",
		".claude/commands/git/sync.md":                     "Sync the branch\n",
		".claude/settings.json":                            `{"enabledPlugins": {"off@skills-dir": false, "tools@market": true, "unused@market": false}}`,
		".claude/plugins/installed_plugins.json": `{"version": 2, "plugins": {
			"tools@market": [{"installPath": "` + filepath.Join(home, "cache", "tools") + `"}],
			"unused@market": [{"installPath": "` + filepath.Join(home, "cache", "unused") + `"}]}}`,
		"cache/tools/.claude-plugin/plugin.json":        `{"name": "tools"}`,
		"cache/tools/skills/lint/SKILL.md":              "---\nname: lint\ndescription: Lint it\n---\n",
		"cache/tools/commands/fmt.md":                   "---\ndescription: Format\n---\n",
		"cache/unused/.claude-plugin/plugin.json":       `{"name": "unused"}`,
		"cache/unused/skills/u/SKILL.md":                "---\nname: u\n---\n",
		"code/app/.git/HEAD":                            "ref: refs/heads/main\n",
		"code/app/.claude/skills/deploy/SKILL.md":       "---\nname: deploy\ndescription: Ship it\n---\n",
		"code/app/.claude/commands/test.md":             "---\ndescription: Run tests\n---\n",
		"code/app/web/.claude/skills/web-only/SKILL.md": "---\nname: web-only\n---\n",
		"code/.claude/skills/outside/SKILL.md":          "---\nname: outside\n---\n",
	})
	got := customOnly(dedupeCommands(claudeCommands(home, filepath.Join(repo, "web"))))
	want := []string{"dirname", "git:sync", "go", "kit:doc", "nodesc", "simplify", "tools:fmt", "tools:lint", "web-only", "deploy", "test"}
	slices.Sort(want)
	if k := keys(got); !slices.Equal(k, want) {
		t.Fatalf("names = %v\nwant    %v", k, want)
	}
	for name, desc := range map[string]string{"go": "Take an idea", "nodesc": "First line of the body", "git:sync": "Sync the branch", "simplify": "Clean up"} {
		if got[name].Desc != desc {
			t.Errorf("%s desc = %q, want %q", name, got[name].Desc, desc)
		}
	}
	if c := got["dirname"]; c.Hint != "[pr]" || c.Src != "skill" {
		t.Errorf("dirname = %+v", c)
	}
	if c := got["kit:doc"]; c.Plugin != "kit" {
		t.Errorf("kit:doc plugin = %q", c.Plugin)
	}
	all := claudeCommands(home, "")
	if !slices.ContainsFunc(all, func(c SlashCmd) bool { return c.Name == "compact" && c.Src == "builtin" }) {
		t.Error("built-in /compact missing")
	}
	if slices.ContainsFunc(all, func(c SlashCmd) bool { return c.Name == "deploy" }) {
		t.Error("no cwd should mean no project skills")
	}
}

func TestCodexCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", "")
	writeFiles(t, home, map[string]string{
		".codex/skills/plain/SKILL.md":                                      "---\nname: plain\ndescription: Long description\nmetadata:\n  short-description: Short\n---\n",
		".codex/skills/yaml/SKILL.md":                                       "---\nname: yaml\ndescription: Long\n---\n",
		".codex/skills/yaml/agents/openai.yaml":                             "interface:\n  display_name: \"Y\"\n  short_description: \"From yaml\"\n",
		".codex/skills/.system/imagegen/SKILL.md":                           "---\nname: imagegen\ndescription: Images\n---\n",
		".codex/skills/bundle/.claude-plugin/plugin.json":                   `{"name": "build"}`,
		".codex/skills/bundle/skills/doc-writer/SKILL.md":                   "---\nname: doc-writer\ndescription: Docs\n---\n",
		".codex/skills/deep/a/b/c/SKILL.md":                                 "---\nname: deep\ndescription: Deep\n---\n",
		".codex/prompts/draft.md":                                           "---\ndescription: Old prompt\n---\n",
		".codex/config.toml":                                                "[plugins.\"chrome@bundled\"]\nenabled = true\n\n[plugins.\"pdf@bundled\"]\nenabled = false\n",
		".codex/plugins/cache/bundled/chrome/1.0/.codex-plugin/plugin.json": `{"name": "chrome"}`,
		".codex/plugins/cache/bundled/chrome/1.0/skills/control/SKILL.md":   "---\nname: control\ndescription: Drive Chrome\n---\n",
		".codex/plugins/cache/bundled/pdf/1.0/.codex-plugin/plugin.json":    `{"name": "pdf"}`,
		".codex/plugins/cache/bundled/pdf/1.0/skills/pdf/SKILL.md":          "---\nname: pdf\ndescription: PDF\n---\n",
		".agents/skills/shared/SKILL.md":                                    "---\nname: shared\ndescription: Shared\n---\n",
		"repo/.git/HEAD":                                                    "",
		"repo/.agents/skills/local/SKILL.md":                                "---\nname: local\ndescription: Local\n---\n",
	})
	// A half-installed newer version (no manifest) is passed over.
	if err := os.MkdirAll(filepath.Join(home, ".codex/plugins/cache/bundled/chrome/2.0/.codex-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := customOnly(dedupeCommands(codexCommands(home, filepath.Join(home, "repo"))))
	want := []string{"$build:doc-writer", "$chrome:control", "$deep", "$imagegen", "$local", "$plain", "$shared", "$yaml"}
	if k := keys(got); !slices.Equal(k, want) {
		t.Fatalf("names = %v\nwant    %v", k, want)
	}
	if got["$plain"].Desc != "Short" || got["$yaml"].Desc != "From yaml" {
		t.Errorf("short descriptions: %q %q", got["$plain"].Desc, got["$yaml"].Desc)
	}
	all := codexCommands(home, "")
	if !slices.ContainsFunc(all, func(c SlashCmd) bool { return c.Name == "review" && c.Src == "builtin" && c.Sigil == "" }) {
		t.Error("built-in /review missing")
	}
	if slices.ContainsFunc(all, func(c SlashCmd) bool { return c.Name == "quit" || c.Name == "apps" || c.Name == "debug-m-drop" }) {
		t.Error("commands the popup hides are listed")
	}
}

func TestAgyCommands(t *testing.T) {
	home := t.TempDir()
	writeFiles(t, home, map[string]string{
		".gemini/config/skills/one/SKILL.md":               "---\nname: one\ndescription: One\n---\n",
		".gemini/antigravity-cli/skills/two/SKILL.md":      "---\nname: two\ndescription: Two\n---\n",
		".gemini/skills/hidden/SKILL.md":                   "---\nname: hidden\ndisable-slash-command: true\n---\n",
		".gemini/config/global_workflows/flow.md":          "---\ndescription: A workflow\n---\n",
		".gemini/config/plugins/kit/plugin.json":           `{"name": "kit"}`,
		".gemini/config/plugins/kit/skills/three/SKILL.md": "---\nname: three\n---\nThree",
		"repo/.git/HEAD":                    "",
		"repo/.agents/skills/four/SKILL.md": "---\nname: four\n---\n",
		"repo/.agent/skills/five/SKILL.md":  "---\nname: five\n---\n",
		"repo/.agents/workflows/six.md":     "Six\n",
	})
	got := customOnly(dedupeCommands(agyCommands(home, filepath.Join(home, "repo"))))
	want := []string{"five", "flow", "four", "kit:three", "one", "six", "two"}
	if k := keys(got); !slices.Equal(k, want) {
		t.Fatalf("names = %v\nwant    %v", k, want)
	}
}

func TestListCommandsAPI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	writeFiles(t, home, map[string]string{".claude/skills/go/SKILL.md": "---\nname: go\ndescription: Go\n---\n"})
	s := &Server{home: home}
	get := func(q string) (int, []SlashCmd) {
		w := httptest.NewRecorder()
		s.listCommands(w, httptest.NewRequest("GET", "/api/commands?"+q, nil))
		var d struct {
			Commands []SlashCmd `json:"commands"`
		}
		json.Unmarshal(w.Body.Bytes(), &d)
		return w.Code, d.Commands
	}
	if code, _ := get("kind=nope"); code != 400 {
		t.Errorf("unknown kind: %d", code)
	}
	code, list := get("kind=claude&cwd=~")
	if code != 200 || !slices.ContainsFunc(list, func(c SlashCmd) bool { return c.Name == "go" && c.Src == "skill" }) {
		t.Fatalf("claude: %d %v", code, list)
	}
	// A skill added since is served once the last scan is old: the first ask
	// after that still answers at once, from the old scan.
	writeFiles(t, home, map[string]string{".claude/skills/new/SKILL.md": "---\nname: new\n---\n"})
	if _, list := get("kind=claude&cwd=~"); slices.ContainsFunc(list, func(c SlashCmd) bool { return c.Name == "new" }) {
		t.Error("a fresh scan was not reused")
	}
	slashCache.Lock()
	for _, c := range slashCache.m {
		c.at = c.at.Add(-2 * slashFresh)
	}
	slashCache.Unlock()
	get("kind=claude&cwd=~")
	for i := 0; ; i++ {
		slashCache.Lock()
		busy := false
		for _, c := range slashCache.m {
			busy = busy || c.reading
		}
		slashCache.Unlock()
		if !busy {
			break
		}
		if i > 200 {
			t.Fatal("background scan did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, list := get("kind=claude&cwd=~"); !slices.ContainsFunc(list, func(c SlashCmd) bool { return c.Name == "new" }) {
		t.Error("the new skill never showed")
	}
}
