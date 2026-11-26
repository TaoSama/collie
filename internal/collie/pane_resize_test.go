package collie

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeResizeProc struct {
	mu        sync.Mutex
	resizes   [][2]int
	resizeErr error
	onResize  func(cols, rows int)
	closed    bool
	doneCh    chan struct{}
}

func newFakeResizeProc(cols, rows int) *fakeResizeProc {
	return &fakeResizeProc{
		resizes: [][2]int{{cols, rows}},
		doneCh:  make(chan struct{}),
	}
}

func (f *fakeResizeProc) resize(cols, rows int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resizeErr != nil {
		return f.resizeErr
	}
	f.resizes = append(f.resizes, [2]int{cols, rows})
	if f.onResize != nil {
		f.onResize(cols, rows)
	}
	return nil
}

func (f *fakeResizeProc) close() error {
	f.mu.Lock()
	if !f.closed {
		f.closed = true
		close(f.doneCh)
	}
	f.mu.Unlock()
	return nil
}

func (f *fakeResizeProc) done() <-chan struct{} { return f.doneCh }

func (f *fakeResizeProc) history() [][2]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([][2]int, len(f.resizes))
	copy(cp, f.resizes)
	return cp
}

func TestPaneResizerLifecycle(t *testing.T) {
	var spawned []*fakeResizeProc
	var releases int
	var mu sync.Mutex
	r := &paneResizer{
		idle: 80 * time.Millisecond,
		spawn: func(ctx context.Context, socket, home, paneID string, cols, rows int) (resizeProc, error) {
			p := newFakeResizeProc(cols, rows)
			mu.Lock()
			spawned = append(spawned, p)
			mu.Unlock()
			return p, nil
		},
	}

	orig := Rect{Width: 120, Height: 40}
	got, err := r.set(context.Background(), "", "", "w1:p1", orig, 48, 30, func() {
		mu.Lock()
		releases++
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("set 48x30: %v", err)
	}
	if got != (Rect{Width: 48, Height: 30}) {
		t.Fatalf("got = %+v, want 48x30", got)
	}
	if sz, ok := r.size("w1:p1"); !ok || sz != got {
		t.Fatalf("size = %+v, %v; want 48x30, true", sz, ok)
	}

	// Dynamic update reuses the same controller without respawning.
	got, err = r.set(context.Background(), "", "", "w1:p1", orig, 82, 32, nil)
	if err != nil {
		t.Fatalf("set 82x32: %v", err)
	}
	if got != (Rect{Width: 82, Height: 32}) {
		t.Fatalf("got = %+v, want 82x32", got)
	}
	mu.Lock()
	nSpawn := len(spawned)
	proc := spawned[0]
	mu.Unlock()
	if nSpawn != 1 {
		t.Fatalf("spawned = %d, want 1", nSpawn)
	}

	// Explicit restore sends original 120x40 before closing.
	if !r.restore("w1:p1") {
		t.Fatalf("restore returned false")
	}
	if _, ok := r.size("w1:p1"); ok {
		t.Fatalf("size still set after restore")
	}
	hist := proc.history()
	want := [][2]int{{48, 30}, {82, 32}, {120, 40}}
	if len(hist) != len(want) {
		t.Fatalf("resize history = %v, want %v", hist, want)
	}
	for i := range want {
		if hist[i] != want[i] {
			t.Fatalf("resize history[%d] = %v, want %v (full: %v)", i, hist[i], want[i], hist)
		}
	}

	// Idle timer automatically restores and releases.
	if _, err := r.set(context.Background(), "", "", "w1:p1", orig, 50, 28, nil); err != nil {
		t.Fatalf("set for idle: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := r.size("w1:p1"); !ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := r.size("w1:p1"); ok {
		t.Fatalf("idle timeout did not restore resize")
	}

	// Leaving the last screen stream automatically restores and releases.
	r.enterStream("w1:p1")
	r.enterStream("w1:p1")
	if _, err := r.set(context.Background(), "", "", "w1:p1", orig, 52, 29, nil); err != nil {
		t.Fatalf("set for stream: %v", err)
	}
	r.leaveStream("w1:p1")
	if _, ok := r.size("w1:p1"); !ok {
		t.Fatalf("released while one screen stream still active")
	}
	r.leaveStream("w1:p1")
	if _, ok := r.size("w1:p1"); ok {
		t.Fatalf("still active after last screen stream closed")
	}
}

func TestResizePaneEndpointAndDirectRoute(t *testing.T) {
	if !directRoute("POST", "/api/panes/w1:p1/resize", nil) {
		t.Fatalf("directRoute must allow POST /api/panes/{id}/resize")
	}
	if directRoute("GET", "/api/panes/w1:p1/resize", nil) {
		t.Fatalf("directRoute must reject GET /api/panes/{id}/resize")
	}

	var proc *fakeResizeProc
	h := &Herdr{
		local: &Machine{
			ID: "", Name: "test", Local: true, Online: true,
			conn: &herdrConn{dial: func(ctx context.Context) (net.Conn, error) {
				c1, c2 := net.Pipe()
				go func() {
					defer c2.Close()
					sc := bufio.NewScanner(c2)
					for sc.Scan() {
						var req struct {
							ID     string         `json:"id"`
							Method string         `json:"method"`
							Params map[string]any `json:"params"`
						}
						if json.Unmarshal(sc.Bytes(), &req) != nil {
							return
						}
						var resp map[string]any
						switch req.Method {
						case "pane.layout":
							resp = map[string]any{
								"id": req.ID,
								"result": map[string]any{
									"layout": map[string]any{
										"panes": []map[string]any{
											{"pane_id": "w1:p1", "rect": map[string]any{"width": 120, "height": 40}},
										},
									},
								},
							}
						case "pane.get":
							resp = map[string]any{
								"id": req.ID,
								"result": map[string]any{
									"pane": map[string]any{"pane_id": "w1:p1", "agent": "claude"},
								},
							}
						case "pane.read":
							resp = map[string]any{
								"id": req.ID,
								"result": map[string]any{
									"read": map[string]any{"text": "hello", "revision": 1},
								},
							}
						default:
							resp = map[string]any{
								"id":    req.ID,
								"error": map[string]any{"code": "unsupported", "message": "not mocked"},
							}
						}
						b, _ := json.Marshal(resp)
						c2.Write(append(b, '\n'))
					}
				}()
				return c1, nil
			}},
		},
		remote: map[string]*Machine{},
	}
	s := &Server{herdr: h, token: "tok"}
	s.resizer.spawn = func(ctx context.Context, socket, home, paneID string, cols, rows int) (resizeProc, error) {
		proc = newFakeResizeProc(cols, rows)
		return proc, nil
	}

	req := httptest.NewRequest("POST", "/api/panes/w1:p1/resize", bytes.NewBufferString(`{"cols":49,"rows":30}`))
	req.SetPathValue("id", "w1:p1")
	w := httptest.NewRecorder()
	s.resizePane(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("resize status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"cols":49`) || !strings.Contains(w.Body.String(), `"rows":30`) {
		t.Fatalf("unexpected resize response: %s", w.Body.String())
	}
	if sz, err := s.paneDisplaySize(context.Background(), "w1:p1"); err != nil || sz != (Rect{Width: 49, Height: 30}) {
		t.Fatalf("paneDisplaySize = %+v, %v; want 49x30", sz, err)
	}

	req2 := httptest.NewRequest("POST", "/api/panes/w1:p1/resize", bytes.NewBufferString(`{"restore":true}`))
	req2.SetPathValue("id", "w1:p1")
	w2 := httptest.NewRecorder()
	s.resizePane(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("restore status = %d, body = %s", w2.Code, w2.Body.String())
	}
	if sz, err := s.paneDisplaySize(context.Background(), "w1:p1"); err != nil || sz != (Rect{Width: 120, Height: 40}) {
		t.Fatalf("paneDisplaySize after restore = %+v, %v; want 120x40", sz, err)
	}
	if proc == nil || len(proc.history()) != 2 || proc.history()[1] != [2]int{120, 40} {
		t.Fatalf("proc did not restore original 120x40: %v", proc.history())
	}
}

func mockResizeHerdr(layout Rect, procInfo *PaneProcessInfo) *Herdr {
	return &Herdr{
		local: &Machine{
			ID: "", Name: "test", Local: true, Online: true,
			conn: &herdrConn{dial: func(ctx context.Context) (net.Conn, error) {
				c1, c2 := net.Pipe()
				go func() {
					defer c2.Close()
					sc := bufio.NewScanner(c2)
					for sc.Scan() {
						var req struct {
							ID     string         `json:"id"`
							Method string         `json:"method"`
							Params map[string]any `json:"params"`
						}
						if json.Unmarshal(sc.Bytes(), &req) != nil {
							return
						}
						var resp map[string]any
						switch req.Method {
						case "pane.layout":
							resp = map[string]any{
								"id": req.ID,
								"result": map[string]any{
									"layout": map[string]any{
										"panes": []map[string]any{
											{"pane_id": "w1:p1", "rect": map[string]any{"width": layout.Width, "height": layout.Height}},
										},
									},
								},
							}
						case "pane.process_info":
							if procInfo != nil {
								resp = map[string]any{
									"id":     req.ID,
									"result": map[string]any{"process_info": procInfo},
								}
							} else {
								resp = map[string]any{
									"id":    req.ID,
									"error": map[string]any{"code": "not_found", "message": "no process info"},
								}
							}
						default:
							resp = map[string]any{
								"id":    req.ID,
								"error": map[string]any{"code": "unsupported", "message": "not mocked"},
							}
						}
						b, _ := json.Marshal(resp)
						c2.Write(append(b, '\n'))
					}
				}()
				return c1, nil
			}},
		},
		remote: map[string]*Machine{},
	}
}

func TestResizePaneRestoresActualPTYSize(t *testing.T) {
	// Simulate herdr where pane.layout reports 120x40 while the actual PTY is 119x40.
	h := mockResizeHerdr(Rect{Width: 120, Height: 40}, &PaneProcessInfo{
		PaneID:   "w1:p1",
		ShellPID: 4242,
	})
	s := &Server{herdr: h, token: "tok"}

	var mu sync.Mutex
	livePTY := Rect{Width: 119, Height: 40}
	var spawned []*fakeResizeProc
	s.resizer.ptySize = func(ctx context.Context, info PaneProcessInfo) (Rect, bool) {
		if info.ShellPID != 4242 {
			return Rect{}, false
		}
		mu.Lock()
		defer mu.Unlock()
		return livePTY, true
	}
	s.resizer.spawn = func(ctx context.Context, socket, home, paneID string, cols, rows int) (resizeProc, error) {
		mu.Lock()
		livePTY = Rect{Width: cols, Height: rows}
		p := newFakeResizeProc(cols, rows)
		p.onResize = func(c, r int) {
			mu.Lock()
			livePTY = Rect{Width: c, Height: r}
			mu.Unlock()
		}
		spawned = append(spawned, p)
		mu.Unlock()
		return p, nil
	}

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/panes/w1:p1/resize", bytes.NewBufferString(body))
		req.SetPathValue("id", "w1:p1")
		w := httptest.NewRecorder()
		s.resizePane(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("resize %s status = %d, body = %s", body, w.Code, w.Body.String())
		}
		return w
	}

	// First visit: 119x40 -> 49x30 -> 82x32 -> leave restores 119x40 (not layout's 120x40).
	post(`{"cols":49,"rows":30}`)
	post(`{"cols":82,"rows":32}`)
	post(`{"restore":true}`)

	mu.Lock()
	gotPTY := livePTY
	firstHist := spawned[0].history()
	mu.Unlock()
	if gotPTY != (Rect{Width: 119, Height: 40}) {
		t.Fatalf("livePTY after first restore = %+v, want 119x40 (history=%v)", gotPTY, firstHist)
	}
	wantHist := [][2]int{{49, 30}, {82, 32}, {119, 40}}
	if len(firstHist) != len(wantHist) {
		t.Fatalf("first resize history = %v, want %v", firstHist, wantHist)
	}
	for i := range wantHist {
		if firstHist[i] != wantHist[i] {
			t.Fatalf("first resize history[%d] = %v, want %v (full: %v)", i, firstHist[i], wantHist[i], firstHist)
		}
	}

	// Second visit: dead controller respawn still keeps original 119x40 instead of live 49x30.
	post(`{"cols":49,"rows":30}`)
	mu.Lock()
	secondProc := spawned[1]
	secondProc.mu.Lock()
	secondProc.resizeErr = errors.New("controller exited")
	secondProc.mu.Unlock()
	mu.Unlock()

	post(`{"cols":60,"rows":28}`)
	post(`{"restore":true}`)

	mu.Lock()
	gotPTY = livePTY
	nSpawn := len(spawned)
	thirdHist := spawned[2].history()
	mu.Unlock()
	if nSpawn != 3 {
		t.Fatalf("spawned = %d, want 3", nSpawn)
	}
	if gotPTY != (Rect{Width: 119, Height: 40}) {
		t.Fatalf("livePTY after respawn restore = %+v, want 119x40 (history=%v)", gotPTY, thirdHist)
	}
}

func TestTTYPath(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"   ":         "",
		"?":           "",
		"??":          "",
		"-":           "",
		"pts/4":       "/dev/pts/4",
		"ttys005\n":   "/dev/ttys005",
		"/dev/pts/12": "/dev/pts/12",
	}
	for in, want := range cases {
		if got := ttyPath(in); got != want {
			t.Errorf("ttyPath(%q) = %q, want %q", in, got, want)
		}
	}
}
