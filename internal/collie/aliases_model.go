package collie

import (
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// The model an alias actually runs, for showing it: what it pins, else what
// the configuration it points the agent at says, with Claude's family
// aliases ("sonnet") followed to what the alias remaps them to.

// aliasRuns is that model, or "" when only the agent's own default is known.
func aliasRuns(home string, a *LauncherAlias) string {
	env := func(k string) string {
		if v := a.rawEnv[k]; v != "" && !strings.HasPrefix(v, "$") {
			return expandIn(home, v)
		}
		return ""
	}
	model := a.Model
	switch a.Kind {
	case "claude":
		cfg := claudeSettingsAt(cmp.Or(env("CLAUDE_CONFIG_DIR"), claudeRoot(home)))
		model = cmp.Or(model, cfg.Model)
		base, oneM := strings.CutSuffix(model, "[1m]")
		if bareAlias.MatchString(base) {
			k := "ANTHROPIC_DEFAULT_" + strings.ToUpper(base) + "_MODEL"
			if to := cmp.Or(a.rawEnv[k], cfg.Env[k]); to != "" && !strings.HasPrefix(to, "$") {
				model = to
				if oneM && !strings.HasSuffix(to, "[1m]") {
					model += "[1m]"
				}
			}
		}
	case "codex":
		if model == "" {
			model = codexConfigModel(filepath.Join(cmp.Or(env("CODEX_HOME"), codexRoot(home)), "config.toml"), codexProfile(a.launchArgs()))
		}
	}
	return model
}

// claudeSettingsAt reads settings.json in a Claude Code config directory.
func claudeSettingsAt(root string) claudeSettings {
	var c claudeSettings
	if b, err := os.ReadFile(filepath.Join(root, "settings.json")); err == nil {
		json.Unmarshal(b, &c) // a broken file says nothing
	}
	return c
}

// codexProfile: the profile a codex command line picks (-p / --profile).
func codexProfile(args []string) string {
	for i, w := range args {
		if f, v, ok := strings.Cut(w, "="); ok && f == "--profile" {
			return v
		}
		if (w == "-p" || w == "--profile") && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// codexConfigModel: the model in Codex's config.toml, the profile's when it
// sets one, else the top-level one.
func codexConfigModel(file, profile string) string {
	b, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	top, prof, section := "", "", ""
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[") {
			section = strings.Trim(l, "[] ")
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok || strings.TrimSpace(k) != codexModelKey {
			continue
		}
		switch section {
		case "":
			top = tomlString(v)
		case "profiles." + profile:
			prof = tomlString(v)
		}
	}
	if profile != "" && prof != "" {
		return prof
	}
	return top
}
