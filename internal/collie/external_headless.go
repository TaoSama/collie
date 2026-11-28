package collie

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Exited external CLI sessions (Claude Code and Antigravity) can continue in
// place without moving into herdr: Collie runs one headless turn at a time per
// session and lets streamExternal tail the agent's own transcript on disk.
// Running a headless turn against a live interactive session splits or corrupts
// its transcript, so every turn hard-checks liveness / presence locks first.

var headlessBin = findBin

type headlessReq struct {
	kind      string // "claude" | "agy"
	session   string
	cwd       string
	bin       string
	alias     *LauncherAlias
	configDir string
	effort    string
	text      string
}

type headlessErr struct {
	at  time.Time
	msg string
}

type headlessSlot struct {
	running bool
	queue   []headlessReq
	cancel  context.CancelFunc
	subs    map[chan headlessErr]struct{}
}

type headlessManager struct {
	mu    sync.Mutex
	slots map[string]*headlessSlot
}

func (m *headlessManager) slotLocked(session string) *headlessSlot {
	if m.slots == nil {
		m.slots = map[string]*headlessSlot{}
	}
	sl := m.slots[session]
	if sl == nil {
		sl = &headlessSlot{}
		m.slots[session] = sl
	}
	return sl
}

func (m *headlessManager) running(session string) bool {
	if session == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sl := m.slots[session]
	return sl != nil && (sl.running || len(sl.queue) > 0)
}

func (m *headlessManager) subscribeErr(session string) (chan headlessErr, func()) {
	ch := make(chan headlessErr, 4)
	m.mu.Lock()
	sl := m.slotLocked(session)
	if sl.subs == nil {
		sl.subs = map[chan headlessErr]struct{}{}
	}
	sl.subs[ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		if sl := m.slots[session]; sl != nil {
			delete(sl.subs, ch)
			if !sl.running && len(sl.queue) == 0 && len(sl.subs) == 0 {
				delete(m.slots, session)
			}
		}
		m.mu.Unlock()
	}
}

func (m *headlessManager) publishErr(session, msg string) {
	if msg == "" {
		return
	}
	ev := headlessErr{at: time.Now(), msg: msg}
	m.mu.Lock()
	if sl := m.slots[session]; sl != nil {
		for ch := range sl.subs {
			select {
			case ch <- ev:
			default:
			}
		}
	}
	m.mu.Unlock()
}

func (m *headlessManager) stop(session string) bool {
	m.mu.Lock()
	sl := m.slots[session]
	if sl == nil || (!sl.running && len(sl.queue) == 0) {
		m.mu.Unlock()
		return false
	}
	sl.queue = nil
	cancel := sl.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

// agyPresenceLocked reports whether an interactive agy CLI currently holds the
// per-conversation presence lock without creating a lock file.
func (s *Server) agyPresenceLocked(id string) bool {
	if !conversationRe.MatchString(id) {
		return false
	}
	path := filepath.Join(s.agyCLIHome(), "presence", id+".lock")
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// claudeConfigDirForPath extracts <configDir> from <configDir>/projects/<proj>/<id>.jsonl.
func claudeConfigDirForPath(path string) string {
	if path == "" {
		return ""
	}
	proj := filepath.Dir(path)
	projects := filepath.Dir(proj)
	if filepath.Base(projects) != "projects" {
		return ""
	}
	return filepath.Dir(projects)
}

// claudeSessionLive reports whether any live Claude process holds sessionID.
func (s *Server) claudeSessionLive(sessionID, configDir string) bool {
	if sessionID == "" {
		return false
	}
	for _, r := range s.claudeRegsIn(s.claudeConfigDirs(configDir)...) {
		if r.SessionID == sessionID {
			return true
		}
	}
	return false
}

func (s *Server) checkHeadlessSafe(kind, session, configDir string) error {
	switch kind {
	case "claude":
		if s.claudeSessionLive(session, configDir) {
			return errors.New("这个 Claude 会话正在别处运行，不能并发无头发送")
		}
	case "agy":
		if s.agyPresenceLocked(session) {
			return errors.New("这个 Antigravity 会话正在终端里打开，不能并发无头发送")
		}
	default:
		return errors.New("这种 agent 不支持无头续发")
	}
	return nil
}

func (s *Server) sendHeadless(ctx context.Context, e *External, text, effort string) (int, error) {
	if e.Session == "" || !conversationRe.MatchString(e.Session) {
		return 409, errors.New("找不到这个会话的 ID，没法续发")
	}
	cwd := cmp.Or(e.startDir, e.Cwd)
	if cwd == "" {
		return 409, errors.New("找不到这个会话的目录，没法续发")
	}
	if e.Kind == "agy" && effort != "" && e.Model != "" && !agyTakesEffort(e.Model) {
		return 400, errors.New("当前模型不支持调整思考强度")
	}
	configDir := ""
	if e.Kind == "claude" {
		configDir = claudeConfigDirForPath(e.path)
	}
	if !s.headless.running(e.Session) {
		if err := s.checkHeadlessSafe(e.Kind, e.Session, configDir); err != nil {
			return 409, err
		}
	}
	var alias *LauncherAlias
	if e.Kind == "claude" && e.Alias != "" {
		a, err := s.aliasOn(ctx, "", e.Alias, "claude")
		if err != nil {
			return 400, err
		}
		alias = a
	}
	bin := ""
	if alias == nil || !aliasNeedsShell(alias) {
		bin = headlessBin(e.Kind, s.home)
		if bin == "" {
			return 502, fmt.Errorf("这台电脑上找不到 %s 命令", e.Kind)
		}
	}
	req := headlessReq{
		kind: e.Kind, session: e.Session, cwd: cwd,
		bin: bin, alias: alias, configDir: configDir, effort: effort, text: text,
	}
	s.headless.mu.Lock()
	sl := s.headless.slotLocked(e.Session)
	sl.queue = append(sl.queue, req)
	start := !sl.running
	if start {
		sl.running = true
	}
	s.headless.mu.Unlock()
	forgetExternal()
	if start {
		go s.runHeadlessQueue(e.Session)
	}
	return 0, nil
}

func (s *Server) runHeadlessQueue(session string) {
	for {
		s.headless.mu.Lock()
		sl := s.headless.slots[session]
		if sl == nil || len(sl.queue) == 0 {
			if sl != nil {
				sl.running = false
				sl.cancel = nil
				if len(sl.subs) == 0 {
					delete(s.headless.slots, session)
				}
			}
			s.headless.mu.Unlock()
			forgetExternal()
			forgetDesktopRows()
			return
		}
		req := sl.queue[0]
		sl.queue = sl.queue[1:]
		runCtx, cancel := context.WithCancel(context.Background())
		sl.cancel = cancel
		s.headless.mu.Unlock()

		err := s.runHeadlessTurn(runCtx, req)
		cancelled := runCtx.Err() != nil
		cancel()
		if err != nil && !cancelled {
			log.Printf("headless %s %s: %v", req.kind, req.session, err)
			s.headless.publishErr(session, err.Error())
		}
		forgetExternal()
		forgetDesktopRows()
	}
}

func (s *Server) runHeadlessTurn(ctx context.Context, req headlessReq) error {
	if err := s.checkHeadlessSafe(req.kind, req.session, req.configDir); err != nil {
		return err
	}
	cmd, err := s.headlessCommand(ctx, req)
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr cappedBuf
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	exited, killed := make(chan struct{}), make(chan struct{})
	// A descendant can keep stdout open after the CLI exits. Escalate a
	// stop even when the stream reader cannot yet reach Wait.
	stopKill := context.AfterFunc(ctx, func() {
		defer close(killed)
		select {
		case <-exited:
		case <-time.After(cmd.WaitDelay):
		}
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			log.Printf("headless %s %s: stop process group: %v", req.kind, req.session, err)
		}
	})
	streamErr := readHeadlessStream(req.kind, stdout)
	waitErr := cmd.Wait()
	close(exited)
	if ctx.Err() != nil || !stopKill() {
		<-killed
		return ctx.Err()
	}
	if streamErr != "" {
		return errors.New(streamErr)
	}
	if waitErr != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return errors.New(clip(oneLine(msg), 240))
		}
		return waitErr
	}
	return nil
}

func (s *Server) headlessCommand(ctx context.Context, req headlessReq) (*exec.Cmd, error) {
	env := cleanHeadlessEnv(os.Environ())
	var cmd *exec.Cmd
	switch req.kind {
	case "claude":
		baseArgs := []string{"-p", "--resume", req.session, "--output-format", "stream-json", "--verbose"}
		extraEffort := []string{}
		if req.effort != "" {
			extraEffort = []string{"--effort", req.effort}
		}
		if req.alias != nil && aliasNeedsShell(req.alias) {
			sh := req.alias.Via
			if sh == "" {
				_, sh, _ = aliasShell(s.home, loginShell())
			}
			all := append(append([]string{req.alias.Name}, aliasArgs(req.alias, baseArgs)...), extraEffort...)
			line, err := shellLine(all)
			if err != nil {
				return nil, err
			}
			cmd = exec.CommandContext(ctx, sh, "-ic", line+` -- "$1"`, "_", req.text)
		} else if req.alias != nil {
			var args []string
			for _, w := range req.alias.launchArgs() {
				args = append(args, expandTilde(w, s.home))
			}
			args = append(args, aliasArgs(req.alias, baseArgs)...)
			args = append(args, extraEffort...)
			args = append(args, "--", req.text)
			cmd = exec.CommandContext(ctx, req.bin, args...)
			for k, v := range req.alias.rawEnv {
				env = setEnvKV(env, k, expandTilde(v, s.home))
			}
		} else {
			args := append(append(baseArgs, extraEffort...), "--", req.text)
			cmd = exec.CommandContext(ctx, req.bin, args...)
			if req.configDir != "" && filepath.Clean(req.configDir) != filepath.Clean(claudeRoot(s.home)) {
				env = setEnvKV(env, "CLAUDE_CONFIG_DIR", req.configDir)
			}
		}
	case "agy":
		args := []string{"--conversation", req.session, "--output-format", "stream-json"}
		if req.effort != "" {
			args = append(args, "--effort", req.effort)
		}
		args = append(args, "-p="+req.text)
		cmd = exec.CommandContext(ctx, req.bin, args...)
	default:
		return nil, errors.New("这种 agent 不支持无头续发")
	}
	cmd.Dir = req.cwd
	cmd.Env = env
	cmd.SysProcAttr = extSysProcAttr()
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd, nil
}

func aliasNeedsShell(a *LauncherAlias) bool {
	if slices.ContainsFunc(a.launchArgs(), shellOnly) {
		return true
	}
	for _, v := range a.rawEnv {
		if shellOnly(v) {
			return true
		}
	}
	return false
}

func cleanHeadlessEnv(in []string) []string {
	out := make([]string, 0, len(in))
	for _, kv := range in {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CLAUDECODE" || strings.HasPrefix(k, "HERDR_") || strings.HasPrefix(k, "CLAUDE_CODE_") || strings.HasPrefix(k, "ANTIGRAVITY_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func setEnvKV(env []string, key, val string) []string {
	prefix := key + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + val
			return env
		}
	}
	return append(env, prefix+val)
}

func readHeadlessStream(kind string, r io.Reader) string {
	br := bufio.NewReaderSize(r, 64<<10)
	errMsg := ""
	for {
		line, err := br.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 0 && line[0] == '{' {
			if msg := parseHeadlessStreamErr(kind, line); msg != "" {
				errMsg = msg
			}
		}
		if err != nil {
			break
		}
	}
	return errMsg
}

func parseHeadlessStreamErr(kind string, line []byte) string {
	var ev struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
		Status  string `json:"status"`
		Error   any    `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return ""
	}
	switch kind {
	case "claude":
		if ev.Type == "result" && ev.IsError {
			return clip(oneLine(cmp.Or(strings.TrimSpace(ev.Result), ev.Subtype, "Claude 执行失败")), 240)
		}
	case "agy":
		if ev.Type == "result" && ev.Status != "" && !strings.EqualFold(ev.Status, "SUCCESS") {
			if s, ok := ev.Error.(string); ok && strings.TrimSpace(s) != "" {
				return clip(oneLine(s), 240)
			}
			return clip(oneLine(cmp.Or(strings.TrimSpace(ev.Message), "Antigravity 执行失败（"+ev.Status+"）")), 240)
		}
		if ev.Type == "error" && strings.TrimSpace(ev.Message) != "" {
			return clip(oneLine(ev.Message), 240)
		}
	}
	return ""
}

type cappedBuf struct {
	b bytes.Buffer
}

func (c *cappedBuf) Write(p []byte) (int, error) {
	const limit = 16 << 10
	if rem := limit - c.b.Len(); rem > 0 {
		if len(p) > rem {
			c.b.Write(p[:rem])
		} else {
			c.b.Write(p)
		}
	}
	return len(p), nil
}

func (c *cappedBuf) String() string { return c.b.String() }
