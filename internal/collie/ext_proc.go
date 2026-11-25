package collie

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

// The process side of an extension, on the computer it runs on (the hub's
// own or a node's): install it, run it, keep it healthy, answer its
// connector.secrets.* from this computer's disk, and pass every other frame
// to and from the hub (extensions/README.md §5, §8). The hub reaches it over
// an "ext" tunnel stream, or a pipe for the hub's own computer.

const (
	extPingEvery    = 30 * time.Second
	extPingWait     = 10 * time.Second
	extPingMisses   = 3
	extInitWait     = 60 * time.Second
	extBackoffFirst = 2 * time.Second
	extBackoffMost  = 5 * time.Minute
	extRecovered    = time.Minute
	extStopGrace    = 5 * time.Second
	// extLease: with no hub attached this long, the program stops, so one
	// moved to another computer never runs twice (two pollers steal messages).
	extLease    = 10 * time.Minute
	extLogLines = 40
)

// Process states, sent to the hub as "$proc.state" events.
const (
	extInstalling = "installing"
	extStarting   = "starting"
	extRunning    = "running"
	extFailed     = "error"
	extStopped    = "stopped"
)

// extStartHeader is the first line on an ext stream.
type extStartHeader struct {
	ID          string         `json:"id"`
	Manifest    *ExtManifest   `json:"manifest"`
	Config      map[string]any `json:"config"`
	HostVersion string         `json:"host_version"`
	// Purge: stop it and delete its folder on this computer, then answer
	// "purged" and close (uninstalling).
	Purge bool `json:"purge,omitempty"`
}

type extProcState struct {
	State string   `json:"state"`
	Error string   `json:"error,omitempty"`
	PID   int      `json:"pid,omitempty"`
	Since string   `json:"since,omitempty"`
	Logs  []string `json:"logs,omitempty"`
}

type extProcs struct {
	mu   sync.Mutex
	byID map[string]*extProc
}

var localExtProcs = &extProcs{byID: map[string]*extProc{}}

// lineWriter writes frames to one side, one at a time.
type lineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lineWriter) frame(f extproto.Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.w.Write(append(b, '\n'))
	return err
}

type extProc struct {
	home, id, key string
	hdr           extStartHeader

	mu       sync.Mutex
	hub      *lineWriter // nil while no hub is attached
	detached time.Time
	state    extProcState
	prog     *lineWriter // the program's stdin, while it runs
	own      map[string]chan extproto.Frame
	next     int
	logs     []string
	quit     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func extKey(h extStartHeader) string {
	b, _ := json.Marshal(struct {
		M *ExtManifest
		C map[string]any
	}{h.Manifest, h.Config})
	return sha256Hex(b)
}

// serveExtStream runs one hub attachment: a header line, then frames both ways.
func (ps *extProcs) serve(conn io.ReadWriteCloser, home string) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 64<<10)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return
	}
	var h extStartHeader
	if err := json.Unmarshal(line, &h); err != nil || h.Manifest == nil || !extIDRe.MatchString(h.ID) || h.ID != h.Manifest.ID ||
		(h.Manifest.Local && h.Manifest.Source != nil) {
		log.Printf("ext: bad start header")
		return
	}
	if h.Purge {
		ps.mu.Lock()
		p := ps.byID[h.ID]
		delete(ps.byID, h.ID)
		ps.mu.Unlock()
		if p != nil {
			p.shutdown()
		}
		answer := "purged"
		if err := os.RemoveAll(extDir(home, h.ID)); err != nil {
			answer = err.Error()
		}
		conn.Write([]byte(answer + "\n"))
		return
	}
	hub := &lineWriter{w: conn}
	p := ps.attach(home, h, hub)
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 0, 64<<10), extproto.MaxFrame)
	for sc.Scan() {
		var f extproto.Frame
		if json.Unmarshal(sc.Bytes(), &f) != nil {
			continue
		}
		if strings.HasPrefix(f.Method, "$proc.") {
			p.control(hub, f)
			continue
		}
		p.toProgram(hub, f)
	}
	p.detach(hub)
}

// attach finds the running program for h, or starts it; a changed manifest
// or config restarts it.
func (ps *extProcs) attach(home string, h extStartHeader, hub *lineWriter) *extProc {
	key := extKey(h)
	ps.mu.Lock()
	p := ps.byID[h.ID]
	if p != nil && p.key != key {
		delete(ps.byID, h.ID)
		ps.mu.Unlock()
		p.shutdown()
		ps.mu.Lock()
		p = nil
	}
	if p == nil {
		p = &extProc{home: home, id: h.ID, key: key, hdr: h, own: map[string]chan extproto.Frame{},
			quit: make(chan struct{}), done: make(chan struct{})}
		p.state = extProcState{State: extStarting, Since: time.Now().Format(time.RFC3339)}
		ps.byID[h.ID] = p
		go p.run(ps)
		go p.leaseWatch(ps)
	}
	ps.mu.Unlock()
	p.mu.Lock()
	p.hub = hub
	st := p.stateLocked()
	p.mu.Unlock()
	hub.frame(stateFrame(st))
	return p
}

func (p *extProc) detach(hub *lineWriter) {
	p.mu.Lock()
	if p.hub == hub {
		p.hub = nil
		p.detached = time.Now()
	}
	p.mu.Unlock()
}

func (p *extProc) leaseWatch(ps *extProcs) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-t.C:
			p.mu.Lock()
			gone := p.hub == nil && time.Since(p.detached) > extLease
			p.mu.Unlock()
			if gone {
				log.Printf("ext %s: no hub for %s, stopping", p.id, extLease)
				ps.remove(p)
				p.shutdown()
				return
			}
		}
	}
}

func (ps *extProcs) remove(p *extProc) {
	ps.mu.Lock()
	if ps.byID[p.id] == p {
		delete(ps.byID, p.id)
	}
	ps.mu.Unlock()
}

func stateFrame(st extProcState) extproto.Frame {
	b, _ := json.Marshal(st)
	return extproto.Frame{Event: "$proc.state", Params: b}
}

func (p *extProc) stateLocked() extProcState {
	st := p.state
	st.Logs = append([]string(nil), p.logs...)
	return st
}

func (p *extProc) setState(state, errText string, pid int) {
	p.mu.Lock()
	p.state = extProcState{State: state, Error: errText, PID: pid, Since: time.Now().Format(time.RFC3339)}
	st, hub := p.stateLocked(), p.hub
	p.mu.Unlock()
	if hub != nil {
		hub.frame(stateFrame(st))
	}
}

func (p *extProc) control(hub *lineWriter, f extproto.Frame) {
	switch f.Method {
	case "$proc.stop":
		localExtProcs.remove(p)
		p.shutdown()
		hub.frame(extproto.Frame{ID: f.ID, Result: json.RawMessage("{}")})
	case "$proc.state":
		p.mu.Lock()
		st := p.stateLocked()
		p.mu.Unlock()
		b, _ := json.Marshal(st)
		hub.frame(extproto.Frame{ID: f.ID, Result: b})
	default:
		hub.frame(extproto.Frame{ID: f.ID, Error: extproto.Errorf(extproto.ErrUnknownMethod, "%s", f.Method)})
	}
}

// toProgram passes a hub frame on; a request while the program is down is
// answered here.
func (p *extProc) toProgram(hub *lineWriter, f extproto.Frame) {
	p.mu.Lock()
	prog, state := p.prog, p.state.State
	p.mu.Unlock()
	if prog != nil && (state == extRunning || f.Method == "") {
		if prog.frame(f) == nil {
			return
		}
	}
	if f.Method != "" && f.ID != "" {
		hub.frame(extproto.Frame{ID: f.ID, Error: extproto.Errorf(extproto.ErrNotRunning, "the extension is %s", state)})
	}
}

// fromProgram handles one frame the program wrote.
func (p *extProc) fromProgram(prog *lineWriter, f extproto.Frame) {
	if f.Method == "" && strings.HasPrefix(f.ID, "$") {
		p.mu.Lock()
		ch := p.own[f.ID]
		delete(p.own, f.ID)
		p.mu.Unlock()
		if ch != nil {
			ch <- f
		}
		return
	}
	if strings.HasPrefix(f.Method, "connector.secrets.") {
		res, err := p.secrets(f.Method, f.Params)
		out := extproto.Frame{ID: f.ID}
		if err != nil {
			out.Error = extproto.Errorf(extproto.ErrInternal, "secrets: %v", err)
		} else {
			out.Result, _ = json.Marshal(res)
		}
		prog.frame(out)
		return
	}
	p.mu.Lock()
	hub := p.hub
	p.mu.Unlock()
	if hub != nil && hub.frame(f) == nil {
		return
	}
	if f.Method != "" && f.ID != "" {
		prog.frame(extproto.Frame{ID: f.ID, Error: extproto.Errorf(extproto.ErrHubUnavailable, "collie is not reachable right now")})
	}
}

// call asks the program itself (initialize, ping), with "$" ids.
func (p *extProc) call(ctx context.Context, prog *lineWriter, method string, params, out any) error {
	raw, _ := json.Marshal(params)
	p.mu.Lock()
	p.next++
	id := "$" + strconv.Itoa(p.next)
	ch := make(chan extproto.Frame, 1)
	p.own[id] = ch
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.own, id); p.mu.Unlock() }()
	if err := prog.frame(extproto.Frame{ID: id, Method: method, Params: raw}); err != nil {
		return err
	}
	select {
	case f := <-ch:
		if f.Error != nil {
			return f.Error
		}
		if out != nil {
			return json.Unmarshal(f.Result, out)
		}
		return nil
	case <-ctx.Done():
		return extproto.Errorf(extproto.ErrTimeout, "%s", method)
	}
}

func (p *extProc) shutdown() {
	p.stopOnce.Do(func() { close(p.quit) })
	<-p.done
}

// run keeps the program going until shutdown: install, start, handshake,
// ping; after an exit start it again with backoff.
func (p *extProc) run(ps *extProcs) {
	defer close(p.done)
	backoff := extBackoffFirst
	for {
		started := time.Now()
		err := p.once()
		select {
		case <-p.quit:
			p.setState(extStopped, "", 0)
			return
		default:
		}
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		log.Printf("ext %s: exited: %s", p.id, msg)
		if time.Since(started) >= extRecovered {
			backoff = extBackoffFirst
		}
		p.setState(extFailed, msg, 0)
		select {
		case <-p.quit:
			p.setState(extStopped, "", 0)
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > extBackoffMost {
			backoff = extBackoffMost
		}
	}
}

func (p *extProc) once() error {
	m := p.hdr.Manifest
	p.setState(extInstalling, "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-p.quit:
			cancel()
		case <-ctx.Done():
		}
	}()
	if err := ensureInstalled(ctx, p.home, m); err != nil {
		return fmt.Errorf("install: %v", err)
	}
	dirs, miss := findRequires(m)
	if len(miss) > 0 {
		return fmt.Errorf("this computer has no %s", strings.Join(miss, ", "))
	}
	bin, args, dir, err := extCommand(p.home, m)
	if err != nil {
		return err
	}
	data := filepath.Join(extDir(p.home, m.ID), "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		return err
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = extEnv(m, dir, data, dirs)
	cmd.SysProcAttr = extSysProcAttr()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	p.setState(extStarting, "", 0)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %v", err)
	}
	prog := &lineWriter{w: stdin}
	p.mu.Lock()
	p.prog = prog
	p.mu.Unlock()
	exited := make(chan error, 1)
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); p.readStderr(stderr) }()
	go func() {
		defer readers.Done()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64<<10), extproto.MaxFrame)
		for sc.Scan() {
			var f extproto.Frame
			if json.Unmarshal(sc.Bytes(), &f) == nil {
				p.fromProgram(prog, f)
			}
		}
	}()
	go func() { readers.Wait(); exited <- cmd.Wait() }()
	defer func() {
		p.mu.Lock()
		p.prog = nil
		p.mu.Unlock()
	}()

	ictx, icancel := context.WithTimeout(ctx, extInitWait)
	var init extproto.InitializeResult
	err = p.call(ictx, prog, extproto.MInitialize, extproto.InitializeParams{
		Protocol:  extproto.Protocol,
		Host:      extproto.HostInfo{Name: "collie", Version: p.hdr.HostVersion},
		Extension: extproto.ExtensionInfo{ID: m.ID, Version: m.Version, Dir: dir, DataDir: data},
		Config:    p.hdr.Config,
	}, &init)
	icancel()
	if err != nil {
		stopProcess(cmd, stdin, exited)
		return fmt.Errorf("initialize: %v", err)
	}
	p.setState(extRunning, "", cmd.Process.Pid)
	misses := 0
	tick := time.NewTicker(extPingEvery)
	defer tick.Stop()
	for {
		select {
		case err := <-exited:
			if err == nil {
				return errors.New("it quit")
			}
			return err
		case <-p.quit:
			stopProcess(cmd, stdin, exited)
			return nil
		case <-tick.C:
			pctx, pcancel := context.WithTimeout(ctx, extPingWait)
			err := p.call(pctx, prog, extproto.MPing, struct{}{}, nil)
			pcancel()
			if err == nil {
				misses = 0
				continue
			}
			if misses++; misses >= extPingMisses {
				stopProcess(cmd, stdin, exited)
				return fmt.Errorf("no answer to %d pings", misses)
			}
		}
	}
}

// stopProcess: close stdin, then SIGTERM, then SIGKILL the process group.
func stopProcess(cmd *exec.Cmd, stdin io.Closer, exited chan error) {
	stdin.Close()
	select {
	case <-exited:
		return
	case <-time.After(extStopGrace):
	}
	syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-exited:
		return
	case <-time.After(extStopGrace):
	}
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	<-exited
}

func (p *extProc) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := clip(sc.Text(), 4096)
		log.Printf("ext %s: %s", p.id, line)
		p.mu.Lock()
		p.logs = append(p.logs, line)
		if n := len(p.logs); n > extLogLines {
			p.logs = p.logs[n-extLogLines:]
		}
		p.mu.Unlock()
	}
}

// extCommand is how to start m on this computer.
func extCommand(home string, m *ExtManifest) (bin string, args []string, dir string, err error) {
	if m.Runtime.Builtin != "" {
		exe, err := os.Executable()
		if err != nil {
			return "", nil, "", err
		}
		dir = filepath.Join(extDir(home, m.ID), "data")
		return exe, append([]string{"ext-" + m.Runtime.Builtin}, m.Runtime.Args...), dir, nil
	}
	rel, err := m.command(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", nil, "", err
	}
	dir = extPackageDir(home, m)
	return filepath.Join(dir, filepath.FromSlash(rel)), m.Runtime.Args, dir, nil
}

// extEnvKeep is what an extension inherits (§7): never Collie's token or herdr.
var extEnvKeep = []string{"PATH", "HOME", "USER", "LANG", "LC_ALL", "TZ", "TMPDIR",
	"http_proxy", "https_proxy", "no_proxy", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR"}

func extEnv(m *ExtManifest, dir, data string, pathDirs []string) []string {
	var env []string
	keep := extEnvKeep
	// For development: COLLIE_EXT_PASS_ENV names more variables to pass on
	// (COLLIE_LARKX_FAKE=1 runs larkx without an account).
	keep = append(keep, strings.Fields(strings.ReplaceAll(os.Getenv("COLLIE_EXT_PASS_ENV"), ",", " "))...)
	for _, k := range keep {
		v, ok := os.LookupEnv(k)
		if k == "PATH" && len(pathDirs) > 0 {
			v, ok = strings.Join(append(pathDirs, v), string(os.PathListSeparator)), true
		}
		if ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env, "COLLIE_EXTENSION_ID="+m.ID, "COLLIE_EXTENSION_DIR="+dir, "COLLIE_EXTENSION_DATA="+data)
}

// secrets answers connector.secrets.* from secrets.json on this computer.
func (p *extProc) secrets(method string, raw json.RawMessage) (any, error) {
	var in extproto.Secrets
	json.Unmarshal(raw, &in)
	name := in.Contribution
	if name == "" {
		name = p.hdr.Manifest.Contributes.Connector.ID
	}
	file := filepath.Join(extDir(p.home, p.id), "secrets.json")
	all := map[string]string{}
	if b, err := os.ReadFile(file); err == nil {
		if err := json.Unmarshal(b, &all); err != nil {
			return nil, fmt.Errorf("secrets.json is damaged")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	switch method {
	case extproto.MSecretsLoad:
		return extproto.Secrets{Data: all[name]}, nil
	case extproto.MSecretsSave:
		all[name] = in.Data
	case extproto.MSecretsDelete:
		delete(all, name)
	default:
		return nil, extproto.Errorf(extproto.ErrUnknownMethod, "%s", method)
	}
	return struct{}{}, writePrivate(file, all)
}

// writePrivate writes v as JSON, 0600, replacing the file in one step.
func writePrivate(file string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// localExtConn is the hub's own computer: the process side on a pipe.
func localExtConn(home string) net.Conn {
	a, b := net.Pipe()
	go localExtProcs.serve(a, home)
	return b
}
