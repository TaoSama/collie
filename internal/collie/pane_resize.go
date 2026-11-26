package collie

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	paneResizeIdle      = 2 * time.Minute
	paneResizeMinCols   = 20
	paneResizeMaxCols   = 400
	paneResizeMinRows   = 10
	paneResizeMaxRows   = 200
	paneResizeReadyWait = 3 * time.Second
)

type resizeProc interface {
	resize(cols, rows int) error
	close() error
	done() <-chan struct{}
}

type paneResizeSess struct {
	proc      resizeProc
	origCols  int
	origRows  int
	cols      int
	rows      int
	idleTimer *time.Timer
	onRelease func()
}

// paneResizer manages temporary `herdr terminal session control` controllers so
// narrow mobile browsers in Screen Mode can reflow a pane's PTY to readable
// columns and restore the original geometry when leaving, backgrounding, or
// going idle.
type paneResizer struct {
	mu       sync.Mutex
	sessions map[string]*paneResizeSess
	streams  map[string]int
	gen      map[string]uint64
	origs    map[string]Rect
	idle     time.Duration
	spawn    func(ctx context.Context, socket, home, paneID string, cols, rows int) (resizeProc, error)
	ptySize  func(ctx context.Context, info PaneProcessInfo) (Rect, bool)
}

func (r *paneResizer) origSize(paneID string) (Rect, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.sessions[paneID]; s != nil && s.origCols > 0 && s.origRows > 0 {
		return Rect{Width: s.origCols, Height: s.origRows}, true
	}
	if sz, ok := r.origs[paneID]; ok && sz.Width > 0 && sz.Height > 0 {
		return sz, true
	}
	return Rect{}, false
}

func (r *paneResizer) idleTTL() time.Duration {
	if r.idle > 0 {
		return r.idle
	}
	return paneResizeIdle
}

func (r *paneResizer) size(paneID string) (Rect, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[paneID]
	if s == nil {
		return Rect{}, false
	}
	return Rect{Width: s.cols, Height: s.rows}, true
}

func (r *paneResizer) enterStream(paneID string) {
	r.mu.Lock()
	if r.streams == nil {
		r.streams = map[string]int{}
	}
	r.streams[paneID]++
	r.mu.Unlock()
}

func (r *paneResizer) leaveStream(paneID string) {
	r.mu.Lock()
	if r.streams != nil {
		if n := r.streams[paneID] - 1; n > 0 {
			r.streams[paneID] = n
			r.mu.Unlock()
			return
		}
		delete(r.streams, paneID)
	}
	has := r.sessions[paneID] != nil
	r.mu.Unlock()
	if has {
		r.restore(paneID)
	}
}

func (r *paneResizer) touch(paneID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.sessions[paneID]; s != nil && s.idleTimer != nil {
		s.idleTimer.Reset(r.idleTTL())
	}
}

func clampPaneSize(cols, rows int) (int, int) {
	cols = max(paneResizeMinCols, min(paneResizeMaxCols, cols))
	rows = max(paneResizeMinRows, min(paneResizeMaxRows, rows))
	return cols, rows
}

func (r *paneResizer) set(ctx context.Context, socket, home, paneID string, orig Rect, cols, rows int, onRelease func()) (Rect, error) {
	cols, rows = clampPaneSize(cols, rows)
	if orig.Width <= 0 {
		orig.Width = 120
	}
	if orig.Height <= 0 {
		orig.Height = 40
	}

	r.mu.Lock()
	if r.sessions == nil {
		r.sessions = map[string]*paneResizeSess{}
	}
	if r.gen == nil {
		r.gen = map[string]uint64{}
	}
	if r.origs == nil {
		r.origs = map[string]Rect{}
	}
	r.gen[paneID]++
	myGen := r.gen[paneID]
	cur := r.sessions[paneID]
	if cur != nil {
		orig = Rect{Width: cur.origCols, Height: cur.origRows}
		if cols == cur.origCols && rows == cur.origRows {
			r.mu.Unlock()
			r.restore(paneID)
			return Rect{Width: cols, Height: rows}, nil
		}
		if cur.cols == cols && cur.rows == rows {
			if cur.idleTimer != nil {
				cur.idleTimer.Reset(r.idleTTL())
			}
			r.mu.Unlock()
			return Rect{Width: cols, Height: rows}, nil
		}
		proc := cur.proc
		r.mu.Unlock()
		if err := proc.resize(cols, rows); err == nil {
			r.mu.Lock()
			if r.sessions[paneID] == cur && r.gen[paneID] == myGen {
				cur.cols = cols
				cur.rows = rows
				if cur.idleTimer != nil {
					cur.idleTimer.Reset(r.idleTTL())
				}
			}
			r.mu.Unlock()
			return Rect{Width: cols, Height: rows}, nil
		}
		// Existing controller died; drop it and start fresh below while keeping
		// the original pre-resize PTY dimensions.
		r.dropDead(paneID, cur)
		r.mu.Lock()
		if r.gen[paneID] == myGen {
			r.origs[paneID] = orig
		}
		r.mu.Unlock()
	} else {
		if prevOrig, ok := r.origs[paneID]; ok && prevOrig.Width > 0 && prevOrig.Height > 0 {
			orig = prevOrig
		}
		if cols == orig.Width && rows == orig.Height {
			r.mu.Unlock()
			return Rect{Width: cols, Height: rows}, nil
		}
		r.origs[paneID] = orig
		r.mu.Unlock()
	}

	spawnFn := r.spawn
	if spawnFn == nil {
		spawnFn = spawnHerdrResize
	}
	proc, err := spawnFn(ctx, socket, home, paneID, cols, rows)
	if err != nil {
		r.mu.Lock()
		if r.gen[paneID] == myGen && r.sessions[paneID] == nil {
			delete(r.origs, paneID)
		}
		r.mu.Unlock()
		return Rect{}, err
	}

	r.mu.Lock()
	if r.gen[paneID] != myGen {
		r.mu.Unlock()
		_ = proc.resize(orig.Width, orig.Height)
		_ = proc.close()
		if sz, ok := r.size(paneID); ok {
			return sz, nil
		}
		return orig, nil
	}
	prev := r.sessions[paneID]
	sess := &paneResizeSess{
		proc:      proc,
		origCols:  orig.Width,
		origRows:  orig.Height,
		cols:      cols,
		rows:      rows,
		onRelease: onRelease,
	}
	sess.idleTimer = time.AfterFunc(r.idleTTL(), func() {
		r.restoreIfSame(paneID, sess)
	})
	r.sessions[paneID] = sess
	r.origs[paneID] = orig
	r.mu.Unlock()

	if prev != nil {
		if prev.idleTimer != nil {
			prev.idleTimer.Stop()
		}
		_ = prev.proc.close()
	}

	go func() {
		<-proc.done()
		r.dropDead(paneID, sess)
	}()

	return Rect{Width: cols, Height: rows}, nil
}

func (r *paneResizer) dropDead(paneID string, sess *paneResizeSess) {
	r.mu.Lock()
	removed := r.sessions[paneID] == sess
	if removed {
		delete(r.sessions, paneID)
		delete(r.origs, paneID)
		if sess.idleTimer != nil {
			sess.idleTimer.Stop()
		}
	}
	r.mu.Unlock()
	if removed && sess.onRelease != nil {
		sess.onRelease()
	}
}

func (r *paneResizer) restoreIfSame(paneID string, target *paneResizeSess) {
	r.mu.Lock()
	if r.sessions[paneID] != target {
		r.mu.Unlock()
		return
	}
	delete(r.sessions, paneID)
	delete(r.origs, paneID)
	if r.gen != nil {
		r.gen[paneID]++
	}
	if target.idleTimer != nil {
		target.idleTimer.Stop()
	}
	r.mu.Unlock()
	finishResizeRestore(target)
}

func (r *paneResizer) restore(paneID string) bool {
	r.mu.Lock()
	if r.gen == nil {
		r.gen = map[string]uint64{}
	}
	r.gen[paneID]++
	delete(r.origs, paneID)
	sess := r.sessions[paneID]
	if sess == nil {
		r.mu.Unlock()
		return false
	}
	delete(r.sessions, paneID)
	if sess.idleTimer != nil {
		sess.idleTimer.Stop()
	}
	r.mu.Unlock()
	finishResizeRestore(sess)
	return true
}

func finishResizeRestore(sess *paneResizeSess) {
	if sess.origCols > 0 && sess.origRows > 0 {
		_ = sess.proc.resize(sess.origCols, sess.origRows)
	}
	_ = sess.proc.close()
	if sess.onRelease != nil {
		sess.onRelease()
	}
}

// herdrResizeProc wraps a `herdr terminal session control` child process. Its
// stdin is an OS pipe, so if collie exits or is killed with SIGKILL, herdr sees
// EOF, sends Detach to the server, and exits without leaving an orphan process.
type herdrResizeProc struct {
	cmd    *exec.Cmd
	mu     sync.Mutex
	stdin  io.WriteCloser
	closed bool
	doneCh chan struct{}
}

func spawnHerdrResize(ctx context.Context, socket, home, paneID string, cols, rows int) (resizeProc, error) {
	bin := findBin("herdr", home)
	if bin == "" {
		return nil, errors.New("herdr binary not found")
	}
	cmd := exec.Command(bin, "terminal", "session", "control", paneID, "--takeover",
		"--cols", strconv.Itoa(cols), "--rows", strconv.Itoa(rows))
	if socket != "" {
		cmd.Env = append(os.Environ(), "HERDR_SOCKET_PATH="+socket)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, err
	}

	p := &herdrResizeProc{cmd: cmd, stdin: stdin, doneCh: make(chan struct{})}
	ready := make(chan error, 1)
	go func() {
		defer close(p.doneCh)
		br := bufio.NewReaderSize(stdout, 64*1024)
		first := true
		for {
			line, err := br.ReadBytes('\n')
			if first && len(line) > 0 {
				first = false
				var ev struct {
					Type   string `json:"type"`
					Reason string `json:"reason"`
				}
				if json.Unmarshal(line, &ev) == nil && ev.Type == "terminal.closed" {
					msg := strings.TrimSpace(ev.Reason)
					if msg == "" {
						msg = "terminal session closed"
					}
					ready <- errors.New(msg)
				} else {
					ready <- nil
				}
			}
			if err != nil {
				break
			}
		}
		waitErr := cmd.Wait()
		if first {
			msg := strings.TrimSpace(stderr.String())
			if msg != "" {
				ready <- errors.New(msg)
			} else if waitErr != nil {
				ready <- waitErr
			} else {
				ready <- errors.New("herdr terminal session ended before ready")
			}
		}
	}()

	waitCtx, cancel := context.WithTimeout(ctx, paneResizeReadyWait)
	defer cancel()
	select {
	case err := <-ready:
		if err != nil {
			_ = p.close()
			return nil, err
		}
		return p, nil
	case <-waitCtx.Done():
		_ = p.close()
		return nil, fmt.Errorf("herdr terminal resize timed out: %w", waitCtx.Err())
	}
}

func (p *herdrResizeProc) resize(cols, rows int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("resize session closed")
	}
	line := fmt.Sprintf("{\"type\":\"terminal.resize\",\"cols\":%d,\"rows\":%d}\n", cols, rows)
	_, err := io.WriteString(p.stdin, line)
	return err
}

func (p *herdrResizeProc) close() error {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		_, _ = io.WriteString(p.stdin, "{\"type\":\"terminal.release\"}\n")
		_ = p.stdin.Close()
	}
	p.mu.Unlock()

	select {
	case <-p.doneCh:
	case <-time.After(1500 * time.Millisecond):
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		<-p.doneCh
	}
	return nil
}

func (p *herdrResizeProc) done() <-chan struct{} { return p.doneCh }

func (s *Server) herdrSocket() string {
	if s.herdr != nil && s.herdr.local != nil {
		return s.herdr.local.socket
	}
	return ""
}

func (s *Server) paneDisplaySize(ctx context.Context, id string) (Rect, error) {
	if sz, ok := s.resizer.size(id); ok {
		return sz, nil
	}
	return s.herdr.PaneSize(ctx, id)
}

func ttyPath(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || s == "?" || s == "??" || s == "-" {
		return ""
	}
	if strings.HasPrefix(s, "/") {
		return s
	}
	return "/dev/" + s
}

func readTTYSize(path string) (Rect, bool) {
	if path == "" {
		return Rect{}, false
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return Rect{}, false
	}
	defer unix.Close(fd)
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 || ws.Row == 0 {
		return Rect{}, false
	}
	return Rect{Width: int(ws.Col), Height: int(ws.Row)}, true
}

func readPIDTTYSize(ctx context.Context, pid int) (Rect, bool) {
	if pid <= 0 {
		return Rect{}, false
	}
	if runtime.GOOS == "linux" {
		for _, n := range []string{"0", "1", "2"} {
			target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, n))
			if err == nil && strings.HasPrefix(target, "/dev/") {
				if sz, ok := readTTYSize(target); ok {
					return sz, true
				}
			}
		}
	}
	pctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	out, err := exec.CommandContext(pctx, "ps", "-o", "tty=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return Rect{}, false
	}
	return readTTYSize(ttyPath(string(out)))
}

func panePTYSizeFromInfo(ctx context.Context, info PaneProcessInfo) (Rect, bool) {
	if sz, ok := readTTYSize(ttyPath(info.TTY)); ok {
		return sz, true
	}
	seen := map[int]bool{}
	tryPID := func(pid int) (Rect, bool) {
		if pid <= 0 || seen[pid] {
			return Rect{}, false
		}
		seen[pid] = true
		return readPIDTTYSize(ctx, pid)
	}
	if sz, ok := tryPID(info.ShellPID); ok {
		return sz, true
	}
	for _, p := range info.ForegroundProcesses {
		if sz, ok := tryPID(p.PID); ok {
			return sz, true
		}
	}
	return Rect{}, false
}

// paneRestoreSize returns the pane's actual PTY dimensions before resizing.
// herdr's pane.layout reports outer pane rectangles (e.g. 120x40 when the real
// PTY is 119x40, or 60x40 when a split pane's PTY is 57x38), so reading the
// PTY winsize directly avoids restoring to the wrong geometry.
func (s *Server) paneRestoreSize(ctx context.Context, id string) (Rect, error) {
	if orig, ok := s.resizer.origSize(id); ok {
		return orig, nil
	}
	if info, err := s.herdr.PaneProcessInfo(ctx, id); err == nil {
		readPTY := s.resizer.ptySize
		if readPTY == nil {
			readPTY = panePTYSizeFromInfo
		}
		if sz, ok := readPTY(ctx, info); ok {
			return sz, nil
		}
	}
	return s.herdr.PaneSize(ctx, id)
}

func (s *Server) resizePane(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cols    int  `json:"cols"`
		Rows    int  `json:"rows"`
		Restore bool `json:"restore"`
	}
	if !decode(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	if body.Restore || body.Cols <= 0 || body.Rows <= 0 {
		s.resizer.restore(id)
		writeJSON(w, 200, map[string]any{"ok": true, "restored": true})
		return
	}
	orig, err := s.paneRestoreSize(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	got, err := s.resizer.set(r.Context(), s.herdrSocket(), s.home, id, orig, body.Cols, body.Rows, func() {
		s.wakes.wake(id)
	})
	if err != nil {
		fail(w, err)
		return
	}
	s.wakes.wake(id)
	writeJSON(w, 200, map[string]any{"ok": true, "cols": got.Width, "rows": got.Height})
}
