package collie

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TaoSama/collie/extensions"
	"github.com/TaoSama/collie/extensions/wechat"
	"github.com/TaoSama/collie/internal/extproto"
)

// An extension's manifest, collie-extension.json (extensions/README.md §2).
type ExtManifest struct {
	ManifestVersion int    `json:"manifest_version"`
	ID              string `json:"id"`
	Name            string `json:"name"`
	Label           string `json:"label,omitempty"` // short, where space is tight: 飞书
	Version         string `json:"version"`
	Description     string `json:"description"`
	Warning         string `json:"warning,omitempty"`
	Homepage        string `json:"homepage,omitempty"`
	Icon            string `json:"icon,omitempty"` // a PNG or SVG in the package (ext_icon.go)
	Engines         struct {
		CollieProtocol int `json:"collie_protocol"`
	} `json:"engines"`
	Runtime struct {
		Command json.RawMessage `json:"command,omitempty"` // "path" or {"GOOS-GOARCH": "path"}
		Builtin string          `json:"builtin,omitempty"`
		Args    []string        `json:"args,omitempty"`
		// Feature: a part of Collie itself that 接入 turns on and off; no
		// program runs (built-in catalog only, its id the feature's name).
		Feature string `json:"feature,omitempty"`
	} `json:"runtime"`
	Source *struct {
		Base  string            `json:"base"`  // pinned to a commit
		Files map[string]string `json:"files"` // relative path → sha256
	} `json:"source,omitempty"`
	Requires    []string `json:"requires,omitempty"`
	Contributes struct {
		Connector struct {
			ID           string   `json:"id"`
			Login        string   `json:"login,omitempty"`
			Capabilities []string `json:"capabilities"`
			SendAction   string   `json:"send_action,omitempty"`
			// Actions agents call with `collie ext do` (§3.6).
			Actions []ExtAction `json:"actions,omitempty"`
		} `json:"connector"`
		Channel struct {
			Format  string   `json:"format"`
			MaxText int      `json:"max_text,omitempty"`
			Kinds   []string `json:"kinds,omitempty"`
			// NoFooter: answers go without Collie's footer (WeChat: the bot
			// is plainly not you, and the footer only takes room).
			NoFooter bool `json:"no_footer,omitempty"`
		} `json:"channel"`
		// UI is reserved for views an extension draws in Collie (not used yet).
		UI []json.RawMessage `json:"ui,omitempty"`
		// Workbench: modules the 工作台 shows, as declared (its code reads them).
		Workbench []json.RawMessage `json:"workbench,omitempty"`
	} `json:"contributes"`
	Configuration *ExtConfig `json:"configuration,omitempty"`
	Permissions   struct {
		Network []string `json:"network,omitempty"`
	} `json:"permissions"`
	Defaults struct {
		Machine       string `json:"machine,omitempty"`       // hub | router | ask
		Handler       string `json:"handler,omitempty"`       // collie | agent
		Conversations string `json:"conversations,omitempty"` // off | owner
	} `json:"defaults"`

	// Local: found in COLLIE_HOME/ext/<id>/pkg/<version>/ on LocalOn
	// ("" the hub), not compiled in; Collie doesn't check what it contains.
	Local   bool   `json:"local,omitempty"`
	LocalOn string `json:"local_on,omitempty"`
	// Market: installed from a marketplace repository (ext_market.go).
	Market *ExtMarketRef `json:"market,omitempty"`
	// IconURL: where the hub serves its icon (ext_icon.go).
	IconURL  string `json:"icon_url,omitempty"`
	icon     []byte
	iconType string
}

// declarative: nothing to run, only declarations (a 工作台 module): on is
// enough, no program starts.
func (m *ExtManifest) declarative() bool {
	return len(m.Runtime.Command) == 0 && m.Runtime.Builtin == "" && m.Contributes.Connector.ID == ""
}

// ExtAction is one action an extension offers agents.
type ExtAction struct {
	Name        string                   `json:"name"`
	Description string                   `json:"description"`
	Params      map[string]ExtConfigProp `json:"params,omitempty"`
	Required    []string                 `json:"required,omitempty"`
	Readonly    bool                     `json:"readonly,omitempty"`  // false: it changes something; can be turned off
	Untrusted   bool                     `json:"untrusted,omitempty"` // its result is someone else's content: material, not instructions
	Slow        bool                     `json:"slow,omitempty"`
	TimeoutS    int                      `json:"timeout_s,omitempty"` // default 30, at most 600
}

const (
	actionWaitDefault = 30
	actionWaitMost    = 600
	machineAsk        = "ask"    // defaults.machine: you pick the computer, there is no default
	machineRouter     = "router" // defaults.machine: where Collie itself runs
)

func (m *ExtManifest) action(name string) *ExtAction {
	for i := range m.Contributes.Connector.Actions {
		if m.Contributes.Connector.Actions[i].Name == name {
			return &m.Contributes.Connector.Actions[i]
		}
	}
	return nil
}

func (a *ExtAction) wait() time.Duration {
	s := a.TimeoutS
	if s <= 0 {
		s = actionWaitDefault
	}
	return time.Duration(min(s, actionWaitMost)) * time.Second
}

// checkArgs: only the declared params, of their types and choices, and the
// required ones present.
func (a *ExtAction) checkArgs(args map[string]any) error {
	for k, v := range args {
		p, ok := a.Params[k]
		if !ok {
			return fmt.Errorf("%s has no param %q", a.Name, k)
		}
		if s, isStr := v.(string); isStr && p.Type != "string" {
			// --arg k=v from the command line: typed by the param.
			if p.Type == "boolean" && (s == "true" || s == "false") {
				v = s == "true"
			} else if n, err := strconv.ParseInt(s, 10, 64); p.Type == "integer" && err == nil {
				v = float64(n)
			}
			args[k] = v
		}
		if !validConfigValue(p, v) {
			return fmt.Errorf("param %q must be a %s%s", k, p.Type, choices(p))
		}
	}
	for _, k := range a.Required {
		if _, ok := args[k]; !ok {
			return fmt.Errorf("param %q is required", k)
		}
	}
	return nil
}

func choices(p ExtConfigProp) string {
	if len(p.Enum) == 0 {
		return ""
	}
	return " (" + strings.Join(p.Enum, " | ") + ")"
}

// ExtConfig is the configuration form (§6).
type ExtConfig struct {
	Type       string                   `json:"type"`
	Properties map[string]ExtConfigProp `json:"properties"`
}

func (c *ExtConfig) propertyOK(k string) (ExtConfigProp, bool) {
	if c == nil {
		return ExtConfigProp{}, false
	}
	p, ok := c.Properties[k]
	return p, ok
}

// ExtConfigProp is one field of the configuration form (§6).
type ExtConfigProp struct {
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Default     any      `json:"default,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Labels      []string `json:"x-labels,omitempty"`
	// Secret: kept apart from the config, never shown again (§6).
	Secret bool `json:"x-secret,omitempty"`
	// Hide: a 收件箱 rule Collie applies while this switch (a boolean) is
	// on: "muted" hides muted chats, "name:<text>" chats whose name has it.
	// The program doesn't read it, so changing it restarts nothing.
	Hide string `json:"x-hide,omitempty"`
}

// programConfig is the config the program gets: without Collie's own switches.
func (m *ExtManifest) programConfig(cfg map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range cfg {
		if m.Configuration == nil || m.Configuration.Properties[k].Hide == "" {
			out[k] = v
		}
	}
	return out
}

// hides: the 收件箱 rules switched on in cfg.
func (m *ExtManifest) hides(cfg map[string]any) []string {
	var out []string
	if m.Configuration != nil {
		for k, p := range m.Configuration.Properties {
			if on, _ := cfg[k].(bool); on && p.Hide != "" {
				out = append(out, p.Hide)
			}
		}
	}
	return out
}

// Connector capabilities (extensions/README.md §2).
const (
	capStatus   = "status"
	capLogin    = "login"
	capArchive  = "archive"
	capReceive  = "receive"
	capActions  = "actions"
	capSend     = "send"
	capInterest = "interest"
	capWorking  = "working"
	chatSelf    = "self"  // a chat kind: your chat with yourself
	chatGroup   = "group" // a chat kind: a group
)

// featureWorkbench: the 工作台, an extension turned on in 接入.
const featureWorkbench = "workbench"

var extFeatures = map[string]bool{featureWorkbench: true}

var extIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// builtinExts are the extensions compiled into collie: `collie ext-<name>`.
// A manifest naming a builtin that isn't here is left out of the catalog.
var builtinExts = map[string]func(args []string) int{"wechat": wechat.Main}

func (m *ExtManifest) has(capability string) bool {
	return slices.Contains(m.Contributes.Connector.Capabilities, capability)
}

func (m *ExtManifest) validate() error {
	if m.Configuration != nil {
		for k, p := range m.Configuration.Properties {
			name, isName := strings.CutPrefix(p.Hide, hideNamePrefix)
			if p.Hide != "" && (p.Type != "boolean" || !(p.Hide == hideMuted || isName && name != "")) {
				return fmt.Errorf("config %s: x-hide is a boolean's, %q or %q<text>", k, hideMuted, hideNamePrefix)
			}
		}
	}
	switch {
	case m.ManifestVersion != 1:
		return fmt.Errorf("manifest_version must be 1")
	case !extIDRe.MatchString(m.ID):
		return fmt.Errorf("bad id %q", m.ID)
	case m.Name == "" || m.Version == "":
		return fmt.Errorf("name and version are required")
	case m.Runtime.Feature != "":
		if !extFeatures[m.Runtime.Feature] || m.ID != m.Runtime.Feature || m.Local || m.Market != nil || len(m.Runtime.Command) > 0 || m.Runtime.Builtin != "" {
			return fmt.Errorf("feature %q: only Collie's own (%v), built in, under the same id", m.Runtime.Feature, extFeatures)
		}
		return nil
	case m.declarative():
		if len(m.Contributes.Workbench) == 0 {
			return fmt.Errorf("an extension without a program must declare contributes.workbench")
		}
		return nil
	case m.Engines.CollieProtocol != extproto.Protocol:
		return fmt.Errorf("speaks protocol %d, collie speaks %d", m.Engines.CollieProtocol, extproto.Protocol)
	case m.Contributes.Connector.ID == "":
		return fmt.Errorf("contributes.connector.id is required")
	case !m.has(capStatus):
		return fmt.Errorf("the connector must have the status capability")
	case !m.has(capArchive) && !m.has(capReceive) && len(m.Contributes.Connector.Actions) == 0:
		return fmt.Errorf("the connector needs archive or receive, or actions of its own")
	case (m.has(capArchive) || m.has(capReceive)) && !m.has(capActions) && !m.has(capSend):
		return fmt.Errorf("a connector that receives needs actions or send to answer")
	case (m.has(capArchive) || m.has(capReceive)) && !m.has(capSend) && m.Contributes.Connector.SendAction == "":
		return fmt.Errorf("a connector that answers through actions needs send_action")
	case (m.Runtime.Builtin == "") == (len(m.Runtime.Command) == 0):
		return fmt.Errorf("runtime needs exactly one of command and builtin")
	case len(m.Runtime.Command) > 0 && m.Source == nil && !m.Local && m.Market == nil:
		return fmt.Errorf("a packaged extension needs source")
	}
	if m.Source != nil {
		if !strings.HasPrefix(m.Source.Base, "https://") || !strings.HasSuffix(m.Source.Base, "/") || len(m.Source.Files) == 0 {
			return fmt.Errorf("source needs an https base ending in / and its files")
		}
		for p, sum := range m.Source.Files {
			if c := path.Clean(p); c != p || path.IsAbs(p) || strings.HasPrefix(c, "../") || len(sum) != 64 {
				return fmt.Errorf("source file %q needs a clean relative path and a sha256", p)
			}
		}
	}
	if len(m.Runtime.Command) > 0 {
		if _, err := m.command(runtime.GOOS, runtime.GOARCH); err != nil && !strings.Contains(err.Error(), "no program for") {
			return err
		}
	}
	for _, a := range m.Contributes.Connector.Actions {
		if !extIDRe.MatchString(strings.ReplaceAll(a.Name, "_", "-")) || a.Description == "" {
			return fmt.Errorf("action %q needs a lower-case name and a description", a.Name)
		}
		for k, p := range a.Params {
			if p.Type != "string" && p.Type != "integer" && p.Type != "boolean" {
				return fmt.Errorf("action %s param %s: type must be string, integer or boolean", a.Name, k)
			}
		}
		if a.TimeoutS < 0 || a.TimeoutS > actionWaitMost {
			return fmt.Errorf("action %s: timeout_s must be 1–%d", a.Name, actionWaitMost)
		}
	}
	if len(m.Contributes.Connector.Actions) > 0 && !m.has(capActions) {
		return fmt.Errorf("actions need the actions capability")
	}
	return nil
}

// command is the program's path inside the package for a platform.
func (m *ExtManifest) command(goos, goarch string) (string, error) {
	var p string
	if json.Unmarshal(m.Runtime.Command, &p) != nil {
		var per map[string]string
		if err := json.Unmarshal(m.Runtime.Command, &per); err != nil {
			return "", fmt.Errorf("runtime.command must be a path or a map of platforms")
		}
		if p = per[goos+"-"+goarch]; p == "" {
			return "", fmt.Errorf("no program for %s-%s", goos, goarch)
		}
	}
	clean := path.Clean(p)
	if p == "" || path.IsAbs(p) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("runtime.command %q must stay inside the package", p)
	}
	return clean, nil
}

// configWithDefaults fills in every field the form has.
func (m *ExtManifest) configWithDefaults(set map[string]any) map[string]any {
	out := map[string]any{}
	if m.Configuration != nil {
		for k, p := range m.Configuration.Properties {
			if p.Secret {
				continue
			}
			if v, ok := set[k]; ok && validConfigValue(p, v) {
				out[k] = v
			} else if p.Default != nil {
				out[k] = p.Default
			}
		}
	}
	return out
}

func validConfigValue(p ExtConfigProp, v any) bool {
	switch p.Type {
	case "string":
		s, ok := v.(string)
		return ok && (len(p.Enum) == 0 || slices.Contains(p.Enum, s))
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	}
	return false
}

func decodeExtManifest(r io.Reader) (*ExtManifest, error) {
	m := &ExtManifest{}
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields() // a misspelled field must not pass unnoticed
	if err := dec.Decode(m); err != nil {
		return nil, err
	}
	return m, nil
}

// extCatalog reads the manifests compiled in (extensions/*/collie-extension.json).
func extCatalog() ([]*ExtManifest, error) {
	return readExtCatalog(extensions.Manifests)
}

func readExtCatalog(fsys fs.FS) ([]*ExtManifest, error) {
	files, err := fs.Glob(fsys, "*/collie-extension.json")
	if err != nil {
		return nil, err
	}
	var out []*ExtManifest
	for _, f := range files {
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		m, err := decodeExtManifest(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %v", f, err)
		}
		if err := m.validate(); err != nil {
			return nil, fmt.Errorf("%s: %v", f, err)
		}
		if dir := path.Dir(f); dir != m.ID {
			return nil, fmt.Errorf("%s: id %q is not its directory", f, m.ID)
		}
		if m.Local {
			return nil, fmt.Errorf("%s: local is set by Collie, not the manifest", f)
		}
		pkg, err := fs.Sub(fsys, path.Dir(f))
		if err != nil {
			return nil, err
		}
		if err := m.loadIcon(pkg); err != nil {
			return nil, fmt.Errorf("%s: %v", f, err)
		}
		if m.Runtime.Builtin != "" && builtinExts[m.Runtime.Builtin] == nil {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
