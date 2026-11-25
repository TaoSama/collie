package collie

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Antigravity names a model with its thinking level ("gemini-3.8-flash-high",
// "Gemini 3.8 Flash (High)"), and takes the same pick as --model
// gemini-3.8-flash --effort high; a model whose id names no level (Claude's)
// takes no --effort. So its list is shown as models and their levels, as
// Codex's is. settings.json keeps the default by its display name.

var (
	agyModelRe = regexp.MustCompile(`^(\S+)\t(.+)$`)
	agyLevelRe = regexp.MustCompile(`^(.+)-(` + strings.Join(effortOrder, "|") + `)$`)
	agyNameRe  = regexp.MustCompile(`^(.+) \((?i:` + strings.Join(effortOrder, "|") + `)\)$`)
)

// agySplit: the model and level an Antigravity model id names ("" when it
// names none).
func agySplit(id string) (model, effort string) {
	if m := agyLevelRe.FindStringSubmatch(id); m != nil {
		return m[1], m[2]
	}
	return id, ""
}

// agyRoot is Antigravity CLI's own directory.
func agyRoot(home string) string {
	if d := filepath.Join(home, ".gemini", "antigravity-cli"); isDir(d) {
		return d
	}
	return filepath.Join(home, ".gemini", "antigravity")
}

var agyListCache = struct {
	sync.Mutex
	at   time.Time
	list []ModelOpt // nil: the last ask failed
}{}

// agyVariants: `agy models`, each id with its level, as the CLI lists them.
// A failed ask is kept for a minute too, so callers don't queue up behind
// one slow run after another.
func agyVariants(ctx context.Context, home string) []ModelOpt {
	agyListCache.Lock()
	defer agyListCache.Unlock()
	keep := 10 * time.Minute
	if agyListCache.list == nil {
		keep = time.Minute
	}
	if !agyListCache.at.IsZero() && time.Since(agyListCache.at) < keep {
		return agyListCache.list
	}
	var list []ModelOpt
	if bin := findBin("agy", home); bin != "" {
		if out, err := exec.CommandContext(ctx, bin, "models").Output(); err == nil {
			for _, l := range strings.Split(string(out), "\n") {
				if m := agyModelRe.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
					list = append(list, ModelOpt{ID: m[1], Label: m[2]})
				}
			}
		}
	}
	agyListCache.at, agyListCache.list = time.Now(), list
	return list
}

// agyKnownVariants: the list as last asked, without asking.
func agyKnownVariants() []ModelOpt {
	agyListCache.Lock()
	defer agyListCache.Unlock()
	return agyListCache.list
}

// agySettingsModel: the model a settings.json (or --settings, inline JSON or
// a file) picks, as an id: settings name it by display name, found in list.
func agySettingsModel(home, settings string, list []ModelOpt) string {
	b := []byte(settings)
	if !strings.HasPrefix(strings.TrimSpace(settings), "{") {
		var err error
		if b, err = os.ReadFile(expandIn(home, settings)); err != nil {
			return ""
		}
	}
	var c struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(b, &c) != nil || c.Model == "" {
		return ""
	}
	for _, v := range list {
		if v.Label == c.Model || v.ID == c.Model {
			return v.ID
		}
	}
	return c.Model
}

// agyDefault: the model new sessions start with, by id when list knows it.
func agyDefault(home string, list []ModelOpt) string {
	return agySettingsModel(home, filepath.Join(agyRoot(home), "settings.json"), list)
}

func (s *Server) agyModels(ctx context.Context, info *ModelInfo) {
	variants := agyVariants(ctx, s.home)
	info.Default, info.DefaultEffort = agySplit(agyDefault(s.home, variants))
	byID := map[string]int{}
	for _, v := range variants {
		id, effort := agySplit(v.ID)
		i, ok := byID[id]
		if !ok {
			label := v.Label
			if effort != "" {
				if m := agyNameRe.FindStringSubmatch(label); m != nil {
					label = m[1]
				}
			}
			i, byID[id] = len(info.Models), len(info.Models)
			info.Models = append(info.Models, ModelOpt{ID: id, Label: label, Efforts: []string{}}) // empty: takes no --effort
		}
		if effort != "" && !slices.Contains(info.Models[i].Efforts, effort) {
			info.Models[i].Efforts = append(info.Models[i].Efforts, effort)
		}
	}
	for i := range info.Models {
		slices.SortFunc(info.Models[i].Efforts, func(a, b string) int {
			return cmp.Compare(slices.Index(effortOrder, a), slices.Index(effortOrder, b))
		})
	}
	// Every model here names its own levels, so the agent's are what "the
	// default model" offers: those of the one settings.json picks.
	if i, ok := byID[info.Default]; ok {
		info.Efforts = info.Models[i].Efforts
	} else if len(info.Models) > 0 {
		info.Efforts = unionEfforts(info.Models)
	}
}
