package collie

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// remoteNode attaches a node serving f's sessions to a fresh hub Herdr. Its
// herdr is reachable only through the node API's watch, unless live is set.
func remoteNode(t *testing.T, f *fakeHerdr, api http.Handler, live bool) (*Herdr, *Machine) {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	hub := newHerdr("", "hub", t.TempDir())
	hub.local.conn.dial = (&fakeHerdr{ws: map[string]string{}, panes: map[string]*fakePane{}}).dial
	m := &Machine{ID: "mac", Name: "mac", Online: true, snap: &listSnap{},
		conn: &herdrConn{dial: func(context.Context) (net.Conn, error) { return nil, errors.New("no live calls") }},
		client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r = r.Clone(r.Context())
			r.URL.Scheme, r.URL.Host, r.RequestURI = u.Scheme, u.Host, ""
			return http.DefaultTransport.RoundTrip(r)
		})}}
	if live {
		m.conn = &herdrConn{dial: f.dial}
	}
	hub.attach(m)
	t.Cleanup(func() { hub.detach("mac", nil) })
	return hub, m
}

func paneIDs(t *testing.T, h *Herdr, ctx context.Context) string {
	t.Helper()
	raw, err := h.call(ctx, "pane.list", nil)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Panes []struct {
			ID string `json:"pane_id"`
		} `json:"panes"`
	}
	json.Unmarshal(raw, &body)
	var ids []string
	for _, p := range body.Panes {
		ids = append(ids, p.ID)
	}
	return strings.Join(ids, ",")
}

func TestHubShowsListsNodeSends(t *testing.T) {
	f := newFakeHerdr(t)
	f.add("w1", "one", "a1", "claude", "/tmp")
	node := &Server{herdr: newHerdr("", "mac", t.TempDir()), node: true}
	node.herdr.local.conn.dial = f.dial
	hub, m := remoteNode(t, f, node.NodeRoutes(), false)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go hub.watch(ctx, m)

	view := withCachedLists(context.Background())
	deadline := time.Now().Add(3 * time.Second)
	for paneIDs(t, hub, view) != "w1:p1@mac" {
		if time.Now().After(deadline) {
			t.Fatalf("hub never got the node's lists: %q", paneIDs(t, hub, view))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// A change on the node reaches the hub and wakes the state streams.
	woke, stopUpdates := hub.listsUpdates()
	defer stopUpdates()
	for len(woke) > 0 {
		<-woke
	}
	f.mu.Lock()
	f.add("w2", "two", "a2", "codex", "/tmp")
	f.mu.Unlock()
	select {
	case <-woke:
	case <-time.After(3 * time.Second):
		t.Fatal("state streams not woken by the node's change")
	}
	if got := paneIDs(t, hub, view); !strings.Contains(got, "w2:p1@mac") {
		t.Fatalf("panes after change = %q", got)
	}
	// Code that acts on what it finds still asks the node itself.
	if _, err := hub.call(context.Background(), "pane.list", nil); err != nil {
		t.Fatal(err)
	}
}

func TestHubAsksNodesWithoutWatch(t *testing.T) {
	f := newFakeHerdr(t)
	f.add("w1", "one", "a1", "claude", "/tmp")
	old := http.NewServeMux() // a node from before: no /api/watch
	hub, m := remoteNode(t, f, old, true)
	done := make(chan struct{})
	go func() { hub.watch(context.Background(), m); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("watch kept trying an old node")
	}
	if got := paneIDs(t, hub, withCachedLists(context.Background())); got != "w1:p1@mac" {
		t.Fatalf("panes = %q", got)
	}
}

func TestDirectHandler(t *testing.T) {
	s := &Server{}
	tok := s.direct.mint()
	api := http.NewServeMux()
	api.HandleFunc("POST /api/panes/{id}/keys", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"pane": r.PathValue("id")})
	})
	api.HandleFunc("GET /api/dirs", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{}) })
	h := s.directHandler("https://c.example.com", "http://127.0.0.1:17788", api)
	do := func(method, path, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	keys := "/direct/api/panes/w1:p1/keys?access_token="
	if w := do("POST", keys+tok, "https://c.example.com"); w.Code != 200 || !strings.Contains(w.Body.String(), `"w1:p1"`) || w.Header().Get("Access-Control-Allow-Origin") != "https://c.example.com" {
		t.Fatalf("hub page with token: %d %s", w.Code, w.Body)
	}
	if w := do("POST", keys+tok, "http://127.0.0.1:17788"); w.Code != 200 {
		t.Fatalf("local UI page: %d", w.Code)
	}
	if w := do("POST", keys+tok, "https://evil.example"); w.Code != 403 {
		t.Fatalf("other site: %d", w.Code)
	}
	if w := do("POST", keys+"nope", "https://c.example.com"); w.Code != 401 {
		t.Fatalf("bad token: %d", w.Code)
	}
	if w := do("GET", "/direct/api/dirs?access_token="+tok, "https://c.example.com"); w.Code != 404 {
		t.Fatalf("route outside terminals: %d", w.Code)
	}
	if w := do("OPTIONS", keys, "https://c.example.com"); w.Code != 204 || w.Header().Get("Access-Control-Allow-Private-Network") != "true" {
		t.Fatalf("preflight: %d", w.Code)
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := clientIP(r); got != "203.0.113.9" {
		t.Fatalf("behind nginx: %q", got)
	}
	r.Header.Set("X-Forwarded-For", "10.9.9.9, 203.0.113.9") // the client's own claim comes first
	if got := clientIP(r); got != "203.0.113.9" {
		t.Fatalf("behind nginx: %q", got)
	}
	r.RemoteAddr = "198.51.100.7:5000"
	if got := clientIP(r); got != "198.51.100.7" {
		t.Fatalf("direct (a forwarded header is not trusted): %q", got)
	}
}
