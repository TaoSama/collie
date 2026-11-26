package collie

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type agyBinding struct {
	pid       int
	started   time.Time
	checked   time.Time
	path      string   // claim retained while a competing transcript makes the result ambiguous
	sessionID string   // ID exposed by the latest lookup
	previous  []string // exclude older transcripts after a restart in the same second
}

func (b agyBinding) session() (string, string) {
	if b.sessionID == "" {
		return "", ""
	}
	return b.path, b.sessionID
}

type agyBindings struct {
	sync.Mutex
	byPane map[string]agyBinding
}

func (b *agyBindings) prune(alive map[string]bool) {
	b.Lock()
	defer b.Unlock()
	for pane := range b.byPane {
		if !alive[pane] {
			delete(b.byPane, pane)
		}
	}
}

var agyIndex struct {
	sync.Mutex
	root string
	at   time.Time
	ws   map[string][]string // conversation id → workspace paths
}

func agyWorkspaces(root string) map[string][]string {
	agyIndex.Lock()
	defer agyIndex.Unlock()
	if agyIndex.root == root && time.Since(agyIndex.at) < 3*time.Second && agyIndex.ws != nil {
		return agyIndex.ws
	}
	agyIndex.root, agyIndex.at = root, time.Now()
	m := map[string][]string{}
	path := filepath.Join(root, "conversation_summaries.db")
	if !isFile(path) {
		agyIndex.ws = m
		return m
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		agyIndex.ws = m
		return m
	}
	defer db.Close()
	rows, err := db.Query(`SELECT conversation_id, workspace_uris FROM conversation_summaries WHERE last_modified_time > ?`,
		time.Now().Add(-48*time.Hour).UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		agyIndex.ws = m
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var id, raw string
		if rows.Scan(&id, &raw) != nil {
			continue
		}
		var uris []string
		json.Unmarshal([]byte(raw), &uris)
		for _, u := range uris {
			m[id] = append(m[id], strings.TrimSuffix(strings.TrimPrefix(u, "file://"), "/"))
		}
	}
	agyIndex.ws = m
	return m
}

// agyPaneSession prefers the reported or resumed conversation. Without an ID,
// a pane may bind to one conversation born in its current process generation.
func (s *Server) agyPaneSession(ctx context.Context, pane, cwd, sessKind, value string) (path, id string) {
	if value != "" {
		path, _ = s.findTranscript("agy", sessKind, value, "", "")
		if sessKind == "path" {
			if path != "" && filepath.Base(path) == "transcript.jsonl" &&
				filepath.Base(filepath.Dir(path)) == "logs" &&
				filepath.Base(filepath.Dir(filepath.Dir(path))) == ".system_generated" {
				return path, agyLogID(path)
			}
			return path, ""
		}
		if agyValidLogID(value) {
			return path, value
		}
		return "", ""
	}
	if pane == "" || s.herdr == nil {
		return "", ""
	}
	checked := time.Now()
	pid, args := s.agyForeground(ctx, pane)
	var since time.Time
	if pid != 0 {
		since = agyProcessStart(ctx, pid)
	}
	id = explicitExternalSession("agy", args)
	var root string
	var index map[string][]string
	if !since.IsZero() && id == "" && cwd != "" {
		root = s.agyCLIHome()
		index = agyWorkspaces(root)
	}

	s.agy.Lock()
	defer s.agy.Unlock()
	b := s.agy.byPane[pane]
	if b.checked.After(checked) {
		return b.session() // a later lookup has already observed this pane
	}
	if s.agy.byPane == nil {
		s.agy.byPane = map[string]agyBinding{}
	}
	if !since.IsZero() && (b.pid != pid || !b.started.Equal(since)) {
		if !b.started.Equal(since) {
			b.previous = nil
		}
		if b.path != "" {
			b.previous = append(b.previous, b.path)
		}
		b.pid, b.started, b.path = pid, since, ""
	}
	b.checked = checked
	b.sessionID = id
	if id != "" {
		b.path = s.agyLog(id)
	} else {
		path = s.guessAgy(root, pane, cwd, since, index, b.previous)
		if b.path == "" {
			b.path = path
		}
		// Keep the claim for this generation even if a second candidate
		// makes it temporarily ambiguous.
		if path != "" && path == b.path {
			b.sessionID = agyLogID(path)
		}
	}
	s.agy.byPane[pane] = b
	return b.session()
}

func agyProcessStart(ctx context.Context, pid int) time.Time {
	const startFormat = "Mon Jan _2 15:04:05 2006"
	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}
	}
	started, err := time.Parse(startFormat, strings.TrimSpace(string(out)))
	if err != nil {
		return time.Time{}
	}
	return started
}

func (s *Server) agyForeground(ctx context.Context, pane string) (pid int, args []string) {
	var info struct {
		ProcessInfo struct {
			Foreground []struct {
				PID  int      `json:"pid"`
				Name string   `json:"name"`
				Argv []string `json:"argv"`
			} `json:"foreground_processes"`
		} `json:"process_info"`
	}
	if err := s.herdr.Call(ctx, "pane.process_info", map[string]any{"pane_id": pane}, &info); err != nil {
		return 0, nil
	}
	for _, p := range info.ProcessInfo.Foreground {
		isAgy := externalKinds[strings.ToLower(p.Name)] == "agy"
		cmd := 0
		if len(p.Argv) > 0 {
			cmd = progIndex(p.Argv)
			isAgy = isAgy || externalKinds[progName(p.Argv)] == "agy"
		}
		if !isAgy || p.PID <= 0 {
			continue
		}
		if pid != 0 {
			return 0, nil
		}
		pid = p.PID
		if len(p.Argv) > cmd {
			args = p.Argv[cmd+1:]
		}
	}
	return pid, args
}

// guessAgy runs with s.agy held. Multiple candidates leave the pane unbound.
func (s *Server) guessAgy(root, pane, cwd string, since time.Time, index map[string][]string, previous []string) string {
	claimed := map[string]bool{}
	for _, p := range previous {
		claimed[p] = true
	}
	for owner, binding := range s.agy.byPane {
		if owner != pane {
			claimed[binding.path] = true
		}
	}
	return uniqueAgyTranscript(root, cwd, since, index, claimed)
}

// uniqueAgyTranscript is shared by pane and external session discovery.
// The conversation must belong to this workspace and be new for the process.
func uniqueAgyTranscript(root, cwd string, since time.Time, index map[string][]string, claimed map[string]bool) string {
	if cwd == "" || since.IsZero() {
		return ""
	}
	cwd = strings.TrimSuffix(filepath.Clean(cwd), "/")
	var candidate string
	for id, workspaces := range index {
		if !slices.Contains(workspaces, cwd) || !agyValidLogID(id) {
			continue
		}
		dir := filepath.Join(root, "brain", id)
		f := filepath.Join(dir, ".system_generated", "logs", "transcript.jsonl")
		if claimed[f] || !isFile(f) {
			continue
		}
		di, err := os.Stat(dir)
		if err != nil {
			continue
		}
		born := di.ModTime()
		if bt := birthTime(di); !bt.IsZero() {
			born = bt
		}
		if born.Before(since) {
			continue
		}
		if candidate != "" {
			return ""
		}
		candidate = f
	}
	return candidate
}

func agyLogID(path string) string {
	return filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path))))
}

func agyValidLogID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\*?[`)
}

func (s *Server) agyLog(id string) string {
	if !agyValidLogID(id) {
		return ""
	}
	for _, root := range []string{s.agyCLIHome(), filepath.Join(s.home, ".gemini", "antigravity")} {
		if p := filepath.Join(root, "brain", id, ".system_generated", "logs", "transcript.jsonl"); isFile(p) {
			return p
		}
	}
	return ""
}

// agyCLIHome is where the agy CLI keeps its conversations. The desktop app
// keeps its own in ~/.gemini/antigravity (desktop.go); the CLI, in a
// terminal or a herdr pane, uses antigravity-cli (older ones used the app's).
func (s *Server) agyCLIHome() string { return agyRoot(s.home) }
