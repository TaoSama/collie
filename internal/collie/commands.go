package collie

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// What "/" lists in a session's composer: the agent's built-in commands and
// the skills, commands and prompts it loads on this computer, for the
// directory it runs in. Each CLI finds them in its own places
// (commands_scan.go); its own commands are listed in commands_builtin.go.

// SlashCmd is one entry of the menu.
type SlashCmd struct {
	Name   string `json:"name"`             // without the sigil: "review", "prompts:draft", "build:doc-writer"
	Sigil  string `json:"sigil,omitempty"`  // "$" for a Codex skill; "/" when empty
	Desc   string `json:"desc,omitempty"`   // one line
	Hint   string `json:"hint,omitempty"`   // argument-hint
	Src    string `json:"src"`              // builtin, skill, command, prompt
	Plugin string `json:"plugin,omitempty"` // the plugin it comes with
}

// slashDescMax: a description is one menu row.
const slashDescMax = 160

// slashFresh: how long a scan is used as it is. A later ask still gets it at
// once and has it read again behind (files changed meanwhile show next time).
const slashFresh = 10 * time.Second

// slashKeep: scans not asked for in this long are dropped (cwd comes from
// the page, so the cache would otherwise only grow).
const slashKeep = 10 * time.Minute

type slashScan struct {
	at, used time.Time
	list     []SlashCmd
	done     chan struct{} // closed once list is there
	reading  bool
}

var slashCache = struct {
	sync.Mutex
	m map[string]*slashScan
}{m: map[string]*slashScan{}}

// listCommands is GET /api/commands?kind=&cwd=: the menu for an agent of
// kind running in cwd on this computer (the hub routes ?machine= to its node).
func (s *Server) listCommands(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := kinds[q.Get("kind")]
	if kind == "" {
		writeJSON(w, 400, map[string]any{"error": "unknown agent kind"})
		return
	}
	cwd := q.Get("cwd")
	if cwd != "" {
		cwd = s.expand(cwd)
	}
	writeJSON(w, 200, map[string]any{"kind": kind, "commands": s.slashCommands(kind, cwd)})
}

// slashCommands answers from the last scan when there is one, starting a
// new scan behind if it is old; the first ask for a directory waits for its
// scan (one scan however many ask).
func (s *Server) slashCommands(kind, cwd string) []SlashCmd {
	key := kind + "\x00" + cwd
	now := time.Now()
	slashCache.Lock()
	c := slashCache.m[key]
	first := c == nil
	if first {
		for k, o := range slashCache.m {
			if now.Sub(o.used) > slashKeep && !o.reading {
				delete(slashCache.m, k)
			}
		}
		c = &slashScan{done: make(chan struct{})}
		slashCache.m[key] = c
	}
	c.used = now
	scan := first || (!c.reading && now.Sub(c.at) > slashFresh)
	if scan {
		c.reading = true
	}
	slashCache.Unlock()
	rescan := func() {
		list := s.scanCommands(kind, cwd)
		slashCache.Lock()
		c.list, c.at, c.reading = list, time.Now(), false
		slashCache.Unlock()
	}
	switch {
	case first:
		rescan()
		close(c.done)
	case scan:
		go rescan()
	}
	<-c.done
	slashCache.Lock()
	defer slashCache.Unlock()
	return c.list
}

func (s *Server) scanCommands(kind, cwd string) []SlashCmd {
	var list []SlashCmd
	switch kind {
	case "claude":
		list = claudeCommands(s.home, cwd)
	case "codex":
		list = codexCommands(s.home, cwd)
	case "agy":
		list = agyCommands(s.home, cwd)
	}
	return dedupeCommands(list)
}

// dedupeCommands keeps the first entry of each name (callers list the ones
// that win first) and sorts by name.
func dedupeCommands(list []SlashCmd) []SlashCmd {
	seen := map[string]bool{}
	out := make([]SlashCmd, 0, len(list))
	for _, c := range list {
		k := c.Sigil + c.Name
		if c.Name == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, c)
	}
	slices.SortStableFunc(out, func(a, b SlashCmd) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// skillDir reads a skill: a directory holding SKILL.md. ok is false when
// there is no SKILL.md, or its header hides it from the menu.
func skillDir(dir string) (name, desc string, fm map[string]string, ok bool) {
	fm, body, err := readFrontmatter(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return "", "", nil, false
	}
	name = fm["name"]
	if name == "" {
		name = filepath.Base(dir)
	}
	desc = fm["description"]
	if desc == "" {
		desc = body
	}
	return name, menuLine(desc, slashDescMax), fm, true
}

// subdirs lists the directories in dir (following links), sorted.
func subdirs(dir string) []string {
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range es {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if e.IsDir() || (e.Type()&os.ModeSymlink != 0 && isDir(p)) {
			out = append(out, p)
		}
	}
	return out
}

// mdFiles lists the Markdown files under dir, as paths relative to it;
// deep says whether to go into subdirectories.
func mdFiles(dir string, deep bool) []string {
	var out []string
	var walk func(rel string, depth int)
	walk = func(rel string, depth int) {
		es, err := os.ReadDir(filepath.Join(dir, rel))
		if err != nil {
			return
		}
		for _, e := range es {
			n := e.Name()
			if strings.HasPrefix(n, ".") {
				continue
			}
			r := filepath.Join(rel, n)
			full := filepath.Join(dir, r)
			isD := e.IsDir() || (e.Type()&os.ModeSymlink != 0 && isDir(full))
			switch {
			case isD && deep && depth < 4:
				walk(r, depth+1)
			case !isD && strings.EqualFold(filepath.Ext(n), ".md"):
				out = append(out, r)
			}
		}
	}
	walk("", 0)
	return out
}

// truthy reads a YAML boolean.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "on":
		return true
	}
	return false
}
