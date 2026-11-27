package collie

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var conversationRe = regexp.MustCompile(`^[0-9a-f-]{16,}$`)

func explicitExternalSession(kind string, argv []string) string {
	if kind == "codex" {
		for len(argv) > 0 {
			name, _, assigned := strings.Cut(argv[0], "=")
			switch name {
			case "resume":
				if !assigned && len(argv) > 1 && conversationRe.MatchString(argv[1]) {
					return argv[1]
				}
				return ""
			case "-m", "--model", "-C", "--cd", "-c", "--config", "--profile":
				if !assigned && len(argv) > 1 {
					argv = argv[1:]
				}
			case "--no-alt-screen":
			default:
				return ""
			}
			argv = argv[1:]
		}
		return ""
	}
	for i, arg := range argv {
		if arg == "--" {
			break
		}
		name, value, assigned := strings.Cut(arg, "=")
		switch {
		case kind == "agy" && (name == "--conversation" || name == "-c"):
		case kind == "claude" && (name == "--resume" || name == "-r" || name == "--session-id"):
		default:
			continue
		}
		if !assigned && i+1 < len(argv) {
			value = argv[i+1]
		}
		if conversationRe.MatchString(value) {
			return value
		}
	}
	return ""
}

// locateExternal ties a transcript to a process registration, an open log, or
// an explicit resume argument. AGY can also match a new conversation in an
// unshared workspace because its CLI doesn't keep the transcript open.
func (s *Server) locateExternal(e *External, p *proc, children ...int) {
	launch := e.launch
	if launch == nil && e.Kind == "agy" {
		launch = readLaunch(p.pid)
	}
	var args []string
	if launch != nil {
		args = launch.args
	}
	id := explicitExternalSession(e.Kind, args)
	switch e.Kind {
	case "claude":
		root := cmp.Or(e.launchEnv("CLAUDE_CONFIG_DIR"), claudeRoot(s.home))
		sessions := filepath.Join(root, "sessions")
		var reg claudeReg
		if b, err := os.ReadFile(filepath.Join(sessions, strconv.Itoa(p.pid)+".json")); err == nil && json.Unmarshal(b, &reg) == nil && reg.SessionID != "" {
			reg.dir = sessions
			id = reg.SessionID
		}
		if e.reg != nil && filepath.Clean(e.reg.dir) != sessions {
			e.reg = nil
		}
		if e.reg == nil && reg.SessionID != "" {
			e.reg = &reg
		}
		if e.reg != nil {
			if reg.SessionID != "" && reg.SessionID != e.reg.SessionID {
				return
			}
			id = e.reg.SessionID
		}
		if conversationRe.MatchString(id) {
			if reg.SessionID != "" || e.reg != nil {
				e.Session = id
			}
			if files, err := filepath.Glob(filepath.Join(root, "projects", "*", id+".jsonl")); err == nil && len(files) == 1 && isFile(files[0]) {
				e.Session, e.path, e.format = id, files[0], "claude"
			}
		}
	case "codex":
		root := cmp.Or(e.launchEnv("CODEX_HOME"), codexRoot(s.home))
		sessions := filepath.Join(root, "sessions")
		dir := cmp.Or(realDir(sessions), sessions)
		match := func(path string) string {
			rel, err := filepath.Rel(dir, path)
			parts := strings.Split(rel, string(filepath.Separator))
			if err != nil || len(parts) != 4 || parts[0] == ".." || !strings.HasPrefix(parts[3], "rollout-") {
				return ""
			}
			id, subagent := codexLogIdentity(path)
			if subagent {
				return "" // the CLI can write its own rollout and its subagents' at once
			}
			return id
		}
		if s, path, ambiguous := matchingWritableTranscript(p.pid, children, match); s != "" || ambiguous {
			if s != "" {
				e.Session, e.path, e.format = s, path, "codex"
			}
			return
		}
		if id != "" {
			files, err := filepath.Glob(filepath.Join(sessions, "*", "*", "*", "rollout-*-"+id+".jsonl"))
			if err == nil && len(files) == 1 {
				if fileID, _ := codexLogIdentity(files[0]); fileID == id {
					e.Session, e.path, e.format = id, files[0], "codex"
				}
			}
		}
	case "agy":
		root := s.agyCLIHome()
		brain := filepath.Join(root, "brain")
		dir := cmp.Or(realDir(brain), brain)
		match := func(path string) string {
			rel, err := filepath.Rel(dir, path)
			session, rest, found := strings.Cut(rel, string(filepath.Separator))
			if err == nil && found && rest == agyTranscriptRelPath && conversationRe.MatchString(session) && isFile(path) {
				return session
			}
			return ""
		}
		if s, path, ambiguous := matchingWritableTranscript(p.pid, children, match); s != "" || ambiguous {
			if s != "" {
				e.Session, e.path, e.format = s, path, "agy"
			}
			return
		}
		if id != "" {
			if f := filepath.Join(brain, id, agyTranscriptRelPath); isFile(f) {
				e.Session, e.path, e.format = id, f, "agy"
			}
			return
		}
		if !e.agySharedWorkspace && e.Cwd != "" && !e.Started.IsZero() {
			started := e.Started.Truncate(time.Second) // ps reports elapsed time in whole seconds
			if path := uniqueAgyTranscript(root, e.Cwd, started, agyWorkspaces(root), nil); path != "" {
				e.agyCandidate = true
				e.Session, e.path, e.format = agyLogID(path), path, "agy"
			}
		}
	}
}

// Takeover rechecks the live process without using the workspace heuristic.
func (s *Server) sameExternalAgy(ctx context.Context, e *External) bool {
	procs, err := listProcs()
	if err != nil {
		return false
	}
	p := procs[e.PID]
	if p == nil || externalKinds[p.name] != "agy" {
		return false
	}
	var children []int
	for _, child := range procs {
		if child.ppid == p.pid && externalKinds[child.name] == "agy" {
			children = append(children, child.pid)
		}
	}
	current := &External{Kind: "agy"}
	s.locateExternal(current, p, children...)
	return current.Session == e.Session && !e.agyStarted.IsZero() && agyProcessStart(ctx, p.pid).Equal(e.agyStarted)
}

func realDir(path string) string {
	dir, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	return dir
}

func codexLogIdentity(path string) (id string, subagent bool) {
	if !isFile(path) {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 1<<20)
	if !sc.Scan() {
		return "", false
	}
	var m struct {
		Type    string `json:"type"`
		Payload struct {
			ID     string `json:"id"`
			Source any    `json:"source"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(sc.Bytes(), &m); err != nil || m.Type != "session_meta" || !conversationRe.MatchString(m.Payload.ID) {
		return "", false
	}
	// The source is a string for top-level CLI sessions and an object for
	// subagents. Older rollouts may omit it.
	source, _ := m.Payload.Source.(map[string]any)
	_, subagent = source["subagent"]
	return m.Payload.ID, subagent
}

func matchingWritableTranscript(pid int, children []int, match func(string) string) (session, path string, ambiguous bool) {
	for _, p := range append([]int{pid}, children...) {
		for _, f := range writableProcessLogs(p) {
			if id := match(f); id != "" {
				if path != "" && f != path {
					return "", "", true
				}
				session, path = id, f
			}
		}
	}
	return session, path, false
}

func writableProcessLogs(pid int) []string {
	var files []string
	switch runtime.GOOS {
	case "linux":
		root := filepath.Join("/proc", strconv.Itoa(pid))
		dir := filepath.Join(root, "fd")
		fds, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(dir, fd.Name()))
			if err != nil || !strings.HasSuffix(target, ".jsonl") {
				continue
			}
			info, err := os.ReadFile(filepath.Join(root, "fdinfo", fd.Name()))
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(info), "\n") {
				if flags, ok := strings.CutPrefix(line, "flags:"); ok {
					mode, err := strconv.ParseUint(strings.TrimSpace(flags), 8, 64)
					if err == nil && mode&syscall.O_ACCMODE != syscall.O_RDONLY {
						files = append(files, target)
					}
					break
				}
			}
		}
	case "darwin":
		return lsofWritableProcessLogs(pid)
	}
	return files
}

func lsofWritableProcessLogs(pid int) []string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-p", strconv.Itoa(pid), "-Ffan").Output()
	if err != nil {
		return nil
	}
	var files []string
	writable := false
	for _, l := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(l, "f"):
			writable = false
		case l == "aw" || l == "au":
			writable = true
		case writable && strings.HasPrefix(l, "n") && strings.HasSuffix(l, ".jsonl"):
			files = append(files, l[1:])
		}
	}
	return files
}
