package collie

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rf(s string, mod time.Time) routerFile { return routerFile{Data: []byte(s), Mod: mod} }

func TestPlanRouterSync(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	t1 := t0.Add(time.Minute)
	h := func(s string) string { return hashOf(rf(s, t0), true) }

	// A computer never synced with: the hub's copy is the truth, even over a
	// newer leftover there, and what only that computer has goes.
	push, pull, base := planRouterSync(
		map[string]routerFile{"ROUTER.md": rf("doc", t0), "memory/a.md": rf("hub", t0)},
		map[string]routerFile{"memory/a.md": rf("stale", t1), "memory/tasks/x.md": rf("x", t0)},
		nil)
	if string(push.Put["ROUTER.md"].Data) != "doc" || string(push.Put["memory/a.md"].Data) != "hub" || len(push.Del) != 1 || push.Del[0] != "memory/tasks/x.md" {
		t.Fatalf("push: %+v", push)
	}
	if len(pull.Put) != 0 || len(pull.Del) != 0 {
		t.Fatalf("pull: %+v", pull)
	}
	if base["memory/a.md"] != h("hub") || len(base) != 2 {
		t.Fatalf("base: %v", base)
	}

	// Too big to mirror on either side: neither copy is touched or forgotten.
	push, pull, base = planRouterSync(
		map[string]routerFile{"big.log": rf("small", t0)},
		map[string]routerFile{"big.log": {Mod: t1, Big: true}},
		map[string]string{"big.log": h("small")})
	if len(push.Put)+len(push.Del)+len(pull.Put)+len(pull.Del) != 0 || base["big.log"] != h("small") {
		t.Fatalf("big file: push %+v pull %+v base %v", push, pull, base)
	}

	// Collie moved a task to done/ on its computer: the hub follows, and its
	// stale copy must not come back.
	prev := map[string]string{"memory/tasks/x.md": h("x"), "memory/messages.md": h("m1")}
	push, pull, base = planRouterSync(
		map[string]routerFile{"memory/tasks/x.md": rf("x", t0), "memory/messages.md": rf("m2", t1)},
		map[string]routerFile{"memory/tasks/done/x.md": rf("x", t1), "memory/messages.md": rf("m1", t0)},
		prev)
	if len(pull.Del) != 1 || pull.Del[0] != "memory/tasks/x.md" || string(pull.Put["memory/tasks/done/x.md"].Data) != "x" {
		t.Fatalf("pull: %+v", pull)
	}
	if string(push.Put["memory/messages.md"].Data) != "m2" || len(push.Del) != 0 || len(push.Put) != 1 {
		t.Fatalf("the hub's own change should go across: %+v", push)
	}
	if _, ok := base["memory/tasks/x.md"]; ok || base["memory/messages.md"] != h("m2") {
		t.Fatalf("base: %v", base)
	}

	// Changed on both sides since the last sync: the newer one wins.
	push, pull, _ = planRouterSync(
		map[string]routerFile{"n.md": rf("hub", t0)},
		map[string]routerFile{"n.md": rf("node", t1)},
		map[string]string{"n.md": h("orig")})
	if string(pull.Put["n.md"].Data) != "node" || len(push.Put) != 0 {
		t.Fatalf("newer should win: push %+v pull %+v", push, pull)
	}
}

func TestRouterPathStaysInside(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"", "../x", "/etc/passwd", "a/../../b", "a//b", "./a"} {
		if _, err := routerPath(dir, bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	if p, err := routerPath(dir, "memory/notes/a.md"); err != nil || p != filepath.Join(dir, "memory", "notes", "a.md") {
		t.Fatalf("good path: %q %v", p, err)
	}
}

// remoteRig: a hub Super whose Collie runs on "mac", a node served over HTTP.
func remoteRig(t *testing.T) (sp *Super, hubDir, nodeDir string) {
	t.Helper()
	nodeHome := t.TempDir()
	t.Setenv("COLLIE_HOME", nodeHome)
	node := &Server{node: true}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/router/files", node.getRouterFiles)
	mux.HandleFunc("POST /api/router/files", node.putRouterFiles)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	target, _ := url.Parse(ts.URL)
	client := &http.Client{Transport: rewriteHost{target.Host}}
	h := &Herdr{local: &Machine{Local: true}, remote: map[string]*Machine{
		"mac": {ID: "mac", Name: "mac", Online: true, Bin: "/Users/me/.local/bin/collie", client: client},
	}}
	hub := t.TempDir()
	db, err := openDB(filepath.Join(hub, "herd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st := &State{Settings: Settings{RouterMachine: "mac"}}
	sp = &Super{s: &Server{herdr: h}, db: db, dir: hub, st: st}
	sp.setRouterOn("mac")
	return sp, filepath.Join(hub, "router"), filepath.Join(nodeHome, "router")
}

type rewriteHost struct{ host string }

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Host = r.host
	return http.DefaultTransport.RoundTrip(req)
}

func TestSyncRouterBothWays(t *testing.T) {
	sp, hubDir, nodeDir := remoteRig(t)
	ctx := context.Background()
	write := func(dir, rel, body string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(dir, rel string) string {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return "<missing>"
		}
		return string(b)
	}

	if dir, bin := sp.routerTarget("mac"); dir != "" || bin != "/Users/me/.local/bin/collie" {
		t.Fatalf("before the first sync: %q %q", dir, bin)
	}
	write(hubDir, "ROUTER.md", "doc")
	write(hubDir, "memory/tasks/x.md", "task")
	if err := sp.syncRouter(ctx); err != nil {
		t.Fatal(err)
	}
	if read(nodeDir, "ROUTER.md") != "doc" || read(nodeDir, "memory/tasks/x.md") != "task" {
		t.Fatal("the hub's folder should reach the node")
	}
	if got := sp.routerCwd("mac"); got != nodeDir {
		t.Fatalf("Collie should work in the node's folder: %q", got)
	}

	// Collie, on its computer, finishes the task and writes a note.
	os.MkdirAll(filepath.Join(nodeDir, "memory/tasks/done"), 0o700)
	if err := os.Rename(filepath.Join(nodeDir, "memory/tasks/x.md"), filepath.Join(nodeDir, "memory/tasks/done/x.md")); err != nil {
		t.Fatal(err)
	}
	write(nodeDir, "memory/notes/n.md", "note")
	if err := sp.syncRouter(ctx); err != nil {
		t.Fatal(err)
	}
	if read(hubDir, "memory/tasks/x.md") != "<missing>" || read(hubDir, "memory/tasks/done/x.md") != "task" || read(hubDir, "memory/notes/n.md") != "note" {
		t.Fatal("the node's changes should come back to the hub")
	}
	if err := sp.syncRouter(ctx); err != nil {
		t.Fatal(err)
	}
	if read(nodeDir, "memory/tasks/x.md") != "<missing>" {
		t.Fatal("a moved file must not come back")
	}

	// A hub restart forgets the in-memory state, not the base: a file Collie
	// removes on its computer afterwards is removed here too, not sent back.
	sp.rsync = routerSync{}
	os.Remove(filepath.Join(nodeDir, "memory/notes/n.md"))
	if err := sp.syncRouter(ctx); err != nil {
		t.Fatal(err)
	}
	if read(hubDir, "memory/notes/n.md") != "<missing>" || read(nodeDir, "memory/notes/n.md") != "<missing>" {
		t.Fatal("after a restart the removal should still win")
	}
}

func TestWriteRouterFilesUsesThatComputersPaths(t *testing.T) {
	sp, hubDir, nodeDir := remoteRig(t)
	sp.st.Settings.ReplyChars = 200
	sp.st.Settings.WorkerKind = "claude"
	if err := sp.writeRouterFiles(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(hubDir, "ROUTER.md")); err == nil {
		t.Fatal("before the folder there is known, ROUTER.md should wait")
	}
	if err := sp.syncRouter(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sp.writeRouterFiles(); err != nil {
		t.Fatal(err)
	}
	doc, _ := os.ReadFile(filepath.Join(hubDir, "ROUTER.md"))
	perms, _ := os.ReadFile(filepath.Join(hubDir, ".claude", "settings.json"))
	if !strings.Contains(string(doc), "/Users/me/.local/bin/collie") {
		t.Fatal("ROUTER.md should name the node's collie")
	}
	if !strings.Contains(string(perms), nodeDir) || strings.Contains(string(perms), hubDir) {
		t.Fatalf("permissions should be for the node's folder: %s", perms)
	}
}

func TestDialHubThroughProxy(t *testing.T) {
	// The "hub": answers one line.
	hub, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	go func() {
		c, err := hub.Accept()
		if err == nil {
			io.WriteString(c, "hello\n")
			c.Close()
		}
	}()
	// The proxy: CONNECT only, to that address, the way real ones answer.
	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Close()
	asked := make(chan string, 1)
	go func() {
		c, err := pl.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		req, err := http.ReadRequest(bufio.NewReader(c))
		if err != nil {
			return
		}
		asked <- req.Method + " " + req.Host
		up, err := net.Dial("tcp", req.Host)
		if err != nil {
			io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
			return
		}
		io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		io.Copy(c, up)
	}()
	pu := &url.URL{Scheme: "http", Host: pl.Addr().String()}
	old := hubProxy
	hubProxy = func(*http.Request) (*url.URL, error) { return pu, nil }
	defer func() { hubProxy = old }()

	addr := hub.Addr().String()
	conn, err := dialHub(context.Background(), &net.Dialer{Timeout: 5 * time.Second}, &url.URL{Scheme: "http", Host: addr}, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadString('\n')
	if got := <-asked; err != nil || line != "hello\n" || got != "CONNECT "+addr {
		t.Fatalf("got %q %v via %q", line, err, got)
	}

	hubProxy = func(*http.Request) (*url.URL, error) { return nil, nil } // NO_PROXY: straight there
	if _, err := dialHub(context.Background(), &net.Dialer{Timeout: 5 * time.Second}, &url.URL{Scheme: "http", Host: "127.0.0.1:1"}, "127.0.0.1:1"); err == nil || strings.Contains(err.Error(), "proxy") {
		t.Fatalf("without a proxy it should dial directly: %v", err)
	}
}
