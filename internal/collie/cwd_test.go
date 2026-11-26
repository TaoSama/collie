package collie

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestRehome(t *testing.T) {
	for _, c := range []struct{ p, home, want string }{
		{"/Users/luwentao/Workspaces/collie", "/home/luwentao", "/home/luwentao/Workspaces/collie"},
		{"/home/luwentao/Workspaces/collie", "/Users/luwentao", "/Users/luwentao/Workspaces/collie"},
		{"/data00/home/luwentao/x", "/Users/me", "/Users/me/x"},
		{"/root/x", "/home/me", "/home/me/x"},
		{"/Users/luwentao", "/home/luwentao", "/home/luwentao"},
		{"/home/me/x", "/home/me", ""}, // already this home
		{"/opt/x", "/home/me", ""},     // not under a home
		{"/Users", "/home/me", ""},
	} {
		if got := rehome(c.p, c.home); got != c.want {
			t.Errorf("rehome(%q, %q) = %q, want %q", c.p, c.home, got, c.want)
		}
	}
}

func TestResolveDir(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "Workspaces", "collie")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"~/Workspaces/collie", "Workspaces/collie", repo,
		"/Users/collie-nobody/Workspaces/collie", "/home/collie-nobody/Workspaces/collie"} {
		if got, err := resolveDir(home, p); err != nil || got != repo {
			t.Errorf("resolveDir(%q) = %q, %v; want %s", p, got, err, repo)
		}
	}
	if got, err := resolveDir(home, "~"); err != nil || got != home {
		t.Errorf("resolveDir(~) = %q, %v", got, err)
	}
	// A path that is there stays, even under another home.
	if real, err := os.UserHomeDir(); err == nil && homePrefixRe.MatchString(real) && isDir(real) {
		if got, err := resolveDir(home, real); err != nil || got != real {
			t.Errorf("resolveDir(%q) = %q, %v; want it unchanged", real, got, err)
		}
	}
	// Missing either way: say both places, start nowhere.
	_, err := resolveDir(home, "/Users/collie-nobody/nope")
	if err == nil || !strings.Contains(err.Error(), "/Users/collie-nobody/nope") || !strings.Contains(err.Error(), filepath.Join(home, "nope")) {
		t.Errorf("missing dir: %v", err)
	}
	if _, err := resolveDir(home, "/opt/collie-nobody"); err == nil {
		t.Error("missing dir outside any home: no error")
	}
}

func TestResolveDirOnNode(t *testing.T) {
	nodeHome := t.TempDir()
	repo := filepath.Join(nodeHome, "Workspaces", "collie")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	nodeHTTP := httptest.NewServer((&Server{home: nodeHome, node: true, web: fstest.MapFS{}}).NodeRoutes())
	defer nodeHTTP.Close()
	nodeURL, _ := url.Parse(nodeHTTP.URL)

	h := newHerdr(filepath.Join(t.TempDir(), "missing.sock"), "hub", "/root")
	box := &Machine{ID: "box", Name: "devbox", Home: nodeHome, Online: true, client: &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			cp := req.Clone(req.Context())
			cp.URL.Scheme, cp.URL.Host, cp.RequestURI = nodeURL.Scheme, nodeURL.Host, ""
			return http.DefaultTransport.RoundTrip(cp)
		}),
	}}
	h.attach(box)
	// A computer whose home no pattern knows; the hub knows it.
	h.attach(&Machine{ID: "odd", Name: "odd", Home: "/srv/people/me", Online: true})
	s := &Server{herdr: h, home: "/root"}

	for _, p := range []string{"~/Workspaces/collie", repo, "/Users/collie-nobody/Workspaces/collie", "/srv/people/me/Workspaces/collie"} {
		if got, err := s.resolveDirOn(box, p); err != nil || got != repo {
			t.Errorf("resolveDirOn(%q) = %q, %v; want %s", p, got, err, repo)
		}
	}
	_, err := s.resolveDirOn(box, "/Users/collie-nobody/nope")
	if err == nil || !strings.Contains(err.Error(), "directory not found") || !strings.Contains(err.Error(), "devbox") {
		t.Errorf("missing dir on devbox: %v", err)
	}
}
