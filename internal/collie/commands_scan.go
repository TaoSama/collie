package collie

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Where each CLI loads skills, commands and prompts from, as of Claude Code
// 2.1.306, Codex 0.172 and Antigravity CLI 1.5 (docs/pitfall/slash-commands.md).

// projectDirs: cwd and its parents up to the repository root (a .git file or
// directory), nearest first; just cwd outside a repository.
func projectDirs(cwd string) []string {
	if cwd == "" {
		return nil
	}
	var dirs []string
	for d := cwd; ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return dirs
		}
		if d == filepath.Dir(d) {
			return dirs[:1]
		}
	}
}

func builtinList(bs []builtinCmd) []SlashCmd {
	out := make([]SlashCmd, len(bs))
	for i, b := range bs {
		out[i] = SlashCmd{Name: b.name, Hint: b.hint, Desc: b.desc, Src: "builtin"}
	}
	return out
}

// skillsIn: the skills one level down in dir (dir/<name>/SKILL.md).
func skillsIn(dir, prefix, src, plugin string, hidden func(map[string]string) bool) []SlashCmd {
	var out []SlashCmd
	for _, d := range subdirs(dir) {
		name, desc, fm, ok := skillDir(d)
		if !ok || hidden(fm) {
			continue
		}
		out = append(out, SlashCmd{Name: withPrefix(prefix, name), Desc: desc, Hint: fm["argument-hint"], Src: src, Plugin: plugin})
	}
	return out
}

// commandsIn: Markdown commands under dir, a subdirectory adding "sub:".
func commandsIn(dir, prefix, src, plugin string, hidden func(map[string]string) bool) []SlashCmd {
	var out []SlashCmd
	for _, rel := range mdFiles(dir, true) {
		fm, body, err := readFrontmatter(filepath.Join(dir, rel))
		if err != nil || hidden(fm) {
			continue
		}
		name := strings.ReplaceAll(strings.TrimSuffix(rel, filepath.Ext(rel)), string(filepath.Separator), ":")
		desc := fm["description"]
		if desc == "" {
			desc = body
		}
		out = append(out, SlashCmd{Name: withPrefix(prefix, name), Desc: menuLine(desc, slashDescMax), Hint: fm["argument-hint"], Src: src, Plugin: plugin})
	}
	return out
}

func withPrefix(prefix, name string) string {
	if prefix == "" || strings.HasPrefix(name, prefix+":") {
		return name
	}
	return prefix + ":" + name
}

// manifestName reads the "name" of a plugin manifest; "" if there is none.
func manifestName(paths ...string) string {
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(b, &m) == nil && m.Name != "" {
			return m.Name
		}
	}
	return ""
}

// ---------------------------------------------------------------- Claude Code

// claudeHidden: `user-invocable: false` leaves a skill to the model alone.
func claudeHidden(fm map[string]string) bool {
	v, ok := fm["user-invocable"]
	return ok && !truthy(v)
}

func claudeCommands(home, cwd string) []SlashCmd {
	root := claudeRoot(home)
	projects := projectDirs(cwd)
	enabled := claudeEnabledPlugins(root, projects)
	var out []SlashCmd
	// Skills win over commands of the same name; personal over project.
	// A folder in skills/ with a plugin manifest is a plugin ("name@skills-dir").
	var bundles []string
	for _, d := range subdirs(filepath.Join(root, "skills")) {
		if name := manifestName(filepath.Join(d, ".claude-plugin", "plugin.json")); name != "" {
			if on, set := enabled[name+"@skills-dir"]; !set || on {
				bundles = append(bundles, d)
			}
			continue
		}
		if name, desc, fm, ok := skillDir(d); ok && !claudeHidden(fm) {
			out = append(out, SlashCmd{Name: name, Desc: desc, Hint: fm["argument-hint"], Src: "skill"})
		}
	}
	for _, p := range projects {
		out = append(out, skillsIn(filepath.Join(p, ".claude", "skills"), "", "skill", "", claudeHidden)...)
	}
	out = append(out, commandsIn(filepath.Join(root, "commands"), "", "command", "", claudeHidden)...)
	for _, p := range projects {
		out = append(out, commandsIn(filepath.Join(p, ".claude", "commands"), "", "command", "", claudeHidden)...)
	}
	for _, d := range append(bundles, claudePluginDirs(root, enabled)...) {
		name := manifestName(filepath.Join(d, ".claude-plugin", "plugin.json"))
		if name == "" {
			continue
		}
		out = append(out, skillsIn(filepath.Join(d, "skills"), name, "skill", name, claudeHidden)...)
		out = append(out, commandsIn(filepath.Join(d, "commands"), name, "command", name, claudeHidden)...)
	}
	return append(out, builtinList(claudeBuiltins)...)
}

// claudeEnabledPlugins merges enabledPlugins from the user's settings and the
// project's (shared, then local, each overriding the one before).
func claudeEnabledPlugins(root string, projects []string) map[string]bool {
	on := map[string]bool{}
	files := []string{filepath.Join(root, "settings.json")}
	if len(projects) > 0 {
		top := projects[len(projects)-1]
		files = append(files, filepath.Join(top, ".claude", "settings.json"), filepath.Join(top, ".claude", "settings.local.json"))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s struct {
			EnabledPlugins map[string]bool `json:"enabledPlugins"`
		}
		if json.Unmarshal(b, &s) != nil {
			continue
		}
		for k, v := range s.EnabledPlugins {
			on[k] = v
		}
	}
	return on
}

// claudePluginDirs: where each enabled installed plugin is, from
// plugins/installed_plugins.json; a plugin from a directory marketplace whose
// copy isn't cached is read where the marketplace is.
func claudePluginDirs(root string, enabled map[string]bool) []string {
	b, err := os.ReadFile(filepath.Join(root, "plugins", "installed_plugins.json"))
	if err != nil {
		return nil
	}
	var inst struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if json.Unmarshal(b, &inst) != nil {
		return nil
	}
	var markets map[string]struct {
		InstallLocation string `json:"installLocation"`
	}
	if b, err := os.ReadFile(filepath.Join(root, "plugins", "known_marketplaces.json")); err == nil {
		json.Unmarshal(b, &markets)
	}
	keys := make([]string, 0, len(inst.Plugins))
	for k := range inst.Plugins {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var dirs []string
	for _, k := range keys {
		if !enabled[k] {
			continue
		}
		found := false
		for _, e := range inst.Plugins[k] {
			if e.InstallPath != "" && isDir(e.InstallPath) {
				dirs, found = append(dirs, e.InstallPath), true
				break
			}
		}
		_, market, _ := strings.Cut(k, "@")
		if loc := markets[market].InstallLocation; !found && loc != "" && isDir(loc) {
			dirs = append(dirs, loc)
		}
	}
	return dirs
}

// ---------------------------------------------------------------- Codex

// codexSkillDepth and codexSkillDirs bound how far Codex looks under a skills root.
const (
	codexSkillDepth = 6
	codexSkillDirs  = 2000
)

var codexManifests = []string{".codex-plugin", ".claude-plugin", ".cursor-plugin"}

// codexCommands: Codex's own commands, then its skills, which are invoked
// with "$name". (0.172 no longer reads ~/.codex/prompts.)
func codexCommands(home, cwd string) []SlashCmd {
	root := codexRoot(home)
	var roots []string
	for _, p := range projectDirs(cwd) {
		roots = append(roots, filepath.Join(p, ".agents", "skills"), filepath.Join(p, ".codex", "skills"))
	}
	roots = append(roots, filepath.Join(root, "skills"), filepath.Join(home, ".agents", "skills"),
		filepath.Join(root, "skills", ".system"), "/etc/codex/skills")
	out := builtinList(codexBuiltins)
	for _, r := range roots {
		out = append(out, codexSkills(r, "")...)
	}
	for _, p := range codexPlugins(root) {
		out = append(out, codexSkills(p.dir, p.name)...)
	}
	return out
}

// codexSkills walks a skills root for SKILL.md files; a skill under a plugin
// manifest is named "<plugin>:<skill>"; plugin names the ones under root.
func codexSkills(root, plugin string) []SlashCmd {
	var out []SlashCmd
	seen := 0
	var walk func(dir, plugin string, depth int)
	walk = func(dir, plugin string, depth int) {
		if seen++; seen > codexSkillDirs {
			return
		}
		for _, m := range codexManifests {
			if n := manifestName(filepath.Join(dir, m, "plugin.json")); n != "" {
				plugin = n
				break
			}
		}
		if name, desc, fm, ok := skillDir(dir); ok {
			if d := codexShortDesc(dir, fm); d != "" {
				desc = d
			}
			out = append(out, SlashCmd{Name: withPrefix(plugin, name), Sigil: "$", Desc: desc, Src: "skill", Plugin: plugin})
		}
		if depth >= codexSkillDepth {
			return
		}
		for _, d := range subdirs(dir) {
			walk(d, plugin, depth+1)
		}
	}
	if isDir(root) {
		walk(root, plugin, 0)
	}
	return out
}

// codexShortDesc: the menu shows a skill's short description when it has one.
func codexShortDesc(dir string, fm map[string]string) string {
	if b, err := os.ReadFile(filepath.Join(dir, "agents", "openai.yaml")); err == nil {
		y, _ := parseFrontmatter("---\n" + string(b) + "\n---\n")
		if d := y["interface.short_description"]; d != "" {
			return menuLine(d, slashDescMax)
		}
	}
	return menuLine(fm["metadata.short-description"], slashDescMax)
}

type codexPlugin struct{ dir, name string }

// codexPlugins: the cached copy (newest whole version) of each plugin that
// config.toml enables with [plugins."name@market"] enabled = true.
func codexPlugins(root string) []codexPlugin {
	f, err := os.Open(filepath.Join(root, "config.toml"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []codexPlugin
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(l, "[") {
			section = ""
			if k, ok := strings.CutPrefix(l, `[plugins."`); ok {
				section, _, _ = strings.Cut(k, `"`)
			}
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if section == "" || !ok || strings.TrimSpace(k) != "enabled" || strings.TrimSpace(v) != "true" {
			continue
		}
		name, market, _ := strings.Cut(section, "@")
		if d := newestPlugin(filepath.Join(root, "plugins", "cache", market, name)); d != "" {
			out = append(out, codexPlugin{d, name})
		}
	}
	return out
}

// newestPlugin: the most recently changed version in dir that has its
// manifest (an install cut short leaves an empty one).
func newestPlugin(dir string) string {
	best, bestAt := "", int64(0)
	for _, d := range subdirs(dir) {
		fi, err := os.Stat(filepath.Join(d, ".codex-plugin", "plugin.json"))
		if err == nil && fi.ModTime().UnixNano() > bestAt {
			best, bestAt = d, fi.ModTime().UnixNano()
		}
	}
	return best
}

// ---------------------------------------------------------------- Antigravity CLI

// agyHidden: `disable-slash-command: true` keeps a skill out of the menu.
func agyHidden(fm map[string]string) bool { return truthy(fm["disable-slash-command"]) }

func agyCommands(home, cwd string) []SlashCmd {
	gem := filepath.Join(home, ".gemini")
	projects := projectDirs(cwd)
	var out []SlashCmd
	for _, p := range projects {
		out = append(out, skillsIn(filepath.Join(p, ".agents", "skills"), "", "skill", "", agyHidden)...)
		out = append(out, skillsIn(filepath.Join(p, ".agent", "skills"), "", "skill", "", agyHidden)...)
	}
	for _, d := range []string{filepath.Join(gem, "config", "skills"), filepath.Join(gem, "antigravity-cli", "skills"), filepath.Join(gem, "skills")} {
		out = append(out, skillsIn(d, "", "skill", "", agyHidden)...)
	}
	// Workflows, which skills replace: /<file name>.
	for _, p := range projects {
		out = append(out, flatCommands(filepath.Join(p, ".agents", "workflows"), agyHidden)...)
	}
	for _, d := range []string{filepath.Join(gem, "config", "global_workflows"), filepath.Join(gem, "config", "workflows")} {
		out = append(out, flatCommands(d, agyHidden)...)
	}
	for _, d := range subdirs(filepath.Join(gem, "config", "plugins")) {
		name := manifestName(filepath.Join(d, "plugin.json"), filepath.Join(d, ".antigravity-plugin", "plugin.json"))
		if name == "" {
			name = filepath.Base(d)
		}
		out = append(out, skillsIn(filepath.Join(d, "skills"), name, "skill", name, agyHidden)...)
	}
	return append(out, builtinList(agyBuiltins)...)
}

// flatCommands: the Markdown files right in dir, each a command by its name.
func flatCommands(dir string, hidden func(map[string]string) bool) []SlashCmd {
	var out []SlashCmd
	for _, c := range commandsIn(dir, "", "command", "", hidden) {
		if !strings.Contains(c.Name, ":") {
			out = append(out, c)
		}
	}
	return out
}
