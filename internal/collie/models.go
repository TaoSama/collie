package collie

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Which models (and thinking efforts) each agent can run here, so the human can
// pick from a list instead of typing launch flags. Each agent is asked in its
// own way: Claude Code's settings (a gateway lists its models and what they
// support), Codex's app-server model/list, `agy models`.

type ModelOpt struct {
	ID      string   `json:"id"`
	Label   string   `json:"label,omitempty"`
	Note    string   `json:"note,omitempty"`
	Efforts []string `json:"efforts"` // nil: the kind's efforts; empty: none
}

type ModelInfo struct {
	Kind          string     `json:"kind"`
	Models        []ModelOpt `json:"models"`
	Efforts       []string   `json:"efforts"`
	Default       string     `json:"default,omitempty"` // what the agent uses when nothing is chosen
	DefaultEffort string     `json:"default_effort,omitempty"`
}

var allEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// modelArgs are the launch flags for a model and effort ("" = the agent's own default).
func modelArgs(kind, model, effort string) []string {
	var a []string
	switch kinds[kind] {
	case "codex":
		if model != "" {
			a = append(a, "-m", model)
		}
		if effort != "" {
			a = append(a, "-c", "model_reasoning_effort="+strconvQuote(effort))
		}
	case "claude", "agy":
		if model != "" {
			a = append(a, "--model", model)
		}
		if effort != "" && (kinds[kind] != "agy" || agyTakesEffort(model)) {
			a = append(a, "--effort", effort)
		}
	}
	return a
}

// agyTakesEffort: whether Antigravity takes --effort with this model. It
// refuses to start when the id already names a level ("…-high", kept in
// settings from before models and levels were picked apart) or the model has
// no levels (Claude's). A model it has not listed is left to it to judge.
func agyTakesEffort(model string) bool {
	if model == "" {
		return true
	}
	if _, level := agySplit(model); level != "" {
		return false
	}
	known := false
	for _, v := range agyKnownVariants() {
		if id, level := agySplit(v.ID); id == model {
			if level != "" {
				return true
			}
			known = true
		}
	}
	return !known
}

// bypassArgs: the launch flag that turns off an agent's permission prompts.
func bypassArgs(kind string) []string {
	switch kinds[kind] {
	case "claude", "agy":
		return []string{"--dangerously-skip-permissions"}
	case "codex":
		return []string{"--dangerously-bypass-approvals-and-sandbox"}
	}
	return nil
}

// withBypass adds bypassArgs to args unless permissions are asked for or the
// flag is already there.
func withBypass(args []string, kind string, ask bool) []string {
	if ask {
		return args
	}
	for _, f := range bypassArgs(kind) {
		if !contains(args, f) {
			args = append(args, f)
		}
	}
	return args
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

var modelCache = struct {
	sync.Mutex
	m map[string]modelMark
}{m: map[string]modelMark{}}

type modelMark struct {
	at   time.Time
	info *ModelInfo
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("kind")
	if q == "agy" { // the web app's short name
		q = "antigravity"
	}
	kind := kinds[q]
	if kind == "" {
		writeJSON(w, 400, map[string]any{"error": "unknown agent kind"})
		return
	}
	modelCache.Lock()
	mk := modelCache.m[kind]
	modelCache.Unlock()
	if mk.info == nil || time.Since(mk.at) > 10*time.Minute || r.URL.Query().Get("refresh") != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		info := &ModelInfo{Kind: kind, Efforts: allEfforts}
		switch kind {
		case "claude":
			s.claudeModels(info)
		case "codex":
			s.codexModels(ctx, info)
		case "agy":
			s.agyModels(ctx, info)
		}
		if info.Models == nil {
			info.Models = []ModelOpt{}
		}
		mk = modelMark{at: time.Now(), info: info}
		modelCache.Lock()
		modelCache.m[kind] = mk
		modelCache.Unlock()
	}
	writeJSON(w, 200, mk.info)
}

// claudeSettings is the part of Claude Code's settings.json that matters here.
type claudeSettings struct {
	Model  string            `json:"model"`
	Effort string            `json:"effortLevel"`
	Env    map[string]string `json:"env"`
}

func (s *Server) readClaudeSettings() claudeSettings { return claudeSettingsAt(claudeRoot(s.home)) }

// claudeEnv is a variable from Claude's settings, else the process environment.
func (c claudeSettings) env(k string) string {
	if v := c.Env[k]; v != "" {
		return v
	}
	return os.Getenv(k)
}

func (s *Server) claudeModels(info *ModelInfo) {
	cfg := s.readClaudeSettings()
	info.Default, info.DefaultEffort = cfg.Model, cfg.Effort
	seen := map[string]bool{}
	add := func(o ModelOpt) {
		if o.ID != "" && !seen[o.ID] {
			seen[o.ID] = true
			info.Models = append(info.Models, o)
		}
	}
	// Aliases first, showing what each points to when a gateway remaps them.
	for _, a := range []struct{ id, env string }{{"opus", "ANTHROPIC_DEFAULT_OPUS_MODEL"}, {"sonnet", "ANTHROPIC_DEFAULT_SONNET_MODEL"}, {"haiku", "ANTHROPIC_DEFAULT_HAIKU_MODEL"}} {
		add(ModelOpt{ID: a.id, Label: aliasLabel(a.id), Note: cfg.env(a.env)})
	}
	// A gateway declares its models and what they support:
	// "model=effort,xhigh_effort,max_effort;other=effort".
	var gw []ModelOpt
	for _, part := range strings.Split(cfg.env("CLAUDE_CODE_MODEL_CAPABILITIES"), ";") {
		id, caps, _ := strings.Cut(strings.TrimSpace(part), "=")
		if id == "" {
			continue
		}
		o := ModelOpt{ID: id}
		for _, c := range strings.Split(caps, ",") {
			switch strings.TrimSpace(c) {
			case "effort":
				o.Efforts = append(o.Efforts, "low", "medium", "high")
			case "xhigh_effort":
				o.Efforts = append(o.Efforts, "xhigh")
			case "max_effort":
				o.Efforts = append(o.Efforts, "max")
			}
		}
		gw = append(gw, o)
	}
	sort.SliceStable(gw, func(i, j int) bool { return gw[i].ID < gw[j].ID })
	for _, o := range gw {
		add(o)
	}
	add(ModelOpt{ID: cfg.Model, Label: aliasLabel(cfg.Model)})
}

var bareAlias = regexp.MustCompile(`^[a-z]+$`)

// aliasLabel names a bare model alias ("fable") the way the built-in ones are
// named; a full model id is shown as it is.
func aliasLabel(id string) string {
	if !bareAlias.MatchString(id) {
		return ""
	}
	return strings.ToUpper(id[:1]) + id[1:]
}

func (s *Server) codexModels(ctx context.Context, info *ModelInfo) {
	home := s.codexHome()
	// The configured default (config.toml's top-level model / model_reasoning_effort).
	if b, err := os.ReadFile(filepath.Join(home, "config.toml")); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			l = strings.TrimSpace(l)
			if strings.HasPrefix(l, "[") {
				break
			}
			k, v, ok := strings.Cut(l, "=")
			if !ok {
				continue
			}
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			switch strings.TrimSpace(k) {
			case "model":
				info.Default = v
			case "model_reasoning_effort":
				info.DefaultEffort = v
			}
		}
	}
	bin := findBin("codex", s.home)
	if bin != "" {
		for _, m := range codexModelList(ctx, bin) {
			info.Models = append(info.Models, m)
		}
	}
	if info.Default != "" {
		found := false
		for _, m := range info.Models {
			found = found || m.ID == info.Default
		}
		if !found {
			info.Models = append([]ModelOpt{{ID: info.Default, Note: "当前默认"}}, info.Models...)
		}
	}
	if e := unionEfforts(info.Models); e != nil {
		info.Efforts = e
	}
}

// effortOrder: every thinking level, lowest first.
var effortOrder = []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// unionEfforts: the levels any of the models offers, in order (nil: none).
func unionEfforts(models []ModelOpt) []string {
	var out []string
	for _, e := range effortOrder {
		for _, m := range models {
			if slices.Contains(m.Efforts, e) {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// codexModelList asks a short-lived `codex app-server` for its models.
func codexModelList(ctx context.Context, bin string) []ModelOpt {
	raw, err := codexOnce(ctx, bin, "model/list", map[string]any{})
	if err != nil {
		return nil
	}
	var res struct {
		Data []struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Display string `json:"displayName"`
			Desc    string `json:"description"`
			Hidden  bool   `json:"hidden"`
			Efforts []struct {
				Effort string `json:"reasoningEffort"`
			} `json:"supportedReasoningEfforts"`
		} `json:"data"`
	}
	json.Unmarshal(raw, &res)
	var list []ModelOpt
	for _, d := range res.Data {
		if d.Hidden {
			continue
		}
		id := d.Model
		if id == "" {
			id = d.ID
		}
		o := ModelOpt{ID: id, Label: d.Display, Note: d.Desc}
		for _, e := range d.Efforts {
			o.Efforts = append(o.Efforts, e.Effort)
		}
		list = append(list, o)
	}
	return list
}

// findBin locates an agent's program: PATH, the usual install places, then
// the user's login shell (which knows nvm and friends).
var binCache sync.Map

func findBin(name, home string) string {
	if v, ok := binCache.Load(name); ok {
		return v.(string)
	}
	found := ""
	if p, err := exec.LookPath(name); err == nil {
		found = p
	}
	for _, d := range []string{".local/bin", ".npm-global/bin", ".bun/bin", "bin"} {
		if found == "" && isFile(filepath.Join(home, d, name)) {
			found = filepath.Join(home, d, name)
		}
	}
	for _, d := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		if found == "" && isFile(filepath.Join(d, name)) {
			found = filepath.Join(d, name)
		}
	}
	if found == "" {
		found = binInShell(context.Background(), home, name)
	}
	if found != "" {
		binCache.Store(name, found)
	}
	return found
}
