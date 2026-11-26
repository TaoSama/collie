package collie

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tarEntry struct {
	name, body string
	mode       int64
	kind       byte
	link       string
}

func makeTgz(t *testing.T, es ...tarEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, e := range es {
		kind := e.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		hd := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: kind, Linkname: e.link}
		if kind == tar.TypeReg {
			hd.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hd); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func rootPick(name string) (string, bool) {
	rel := strings.TrimPrefix(name, "./")
	return rel, rel != "" && rel != "."
}

// Unpacking refuses whatever could reach outside the package or fill the disk.
func TestExtractTarRefuses(t *testing.T) {
	for _, c := range []struct {
		name string
		es   []tarEntry
	}{
		{"parent", []tarEntry{{name: "../evil", body: "x"}}},
		{"parent inside", []tarEntry{{name: "bin/../../evil", body: "x"}}},
		{"absolute", []tarEntry{{name: "/etc/evil", body: "x"}}},
		{"symlink", []tarEntry{{name: "bin/run", kind: tar.TypeSymlink, link: "/bin/sh"}}},
		{"hard link", []tarEntry{{name: "a", body: "x"}, {name: "b", kind: tar.TypeLink, link: "a"}}},
		{"fifo", []tarEntry{{name: "fifo", kind: tar.TypeFifo}}},
		{"too large", []tarEntry{{name: "big", body: strings.Repeat("0", 64<<10)}}},
		{"twice", []tarEntry{{name: "a", body: "x"}, {name: "a", body: "y"}}},
		{"no files", []tarEntry{{name: "bin/", kind: tar.TypeDir}}},
	} {
		dst := t.TempDir()
		err := extractTar(makeTgz(t, c.es...), dst, 32<<10, rootPick)
		if err == nil {
			t.Errorf("%s: unpacked", c.name)
		}
		t.Logf("%s: %v", c.name, err)
		if _, err := os.Stat(filepath.Join(filepath.Dir(dst), "evil")); err == nil {
			t.Fatalf("%s: wrote outside the package", c.name)
		}
	}
	if err := extractTar([]byte("not gzip"), t.TempDir(), 1<<20, rootPick); err == nil {
		t.Fatal("not a gzip")
	}
	dst := t.TempDir()
	ok := makeTgz(t, tarEntry{name: "./", kind: tar.TypeDir}, tarEntry{name: "./bin/run", body: "#!/bin/sh\n", mode: 0o755}, tarEntry{name: "a.json", body: "{}", mode: 0o644})
	if err := extractTar(ok, dst, 1<<20, rootPick); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(dst, "bin", "run")); err != nil || st.Mode()&0o111 == 0 {
		t.Fatalf("an executable stays executable: %v %v", st, err)
	}
}

func TestMarketEntryValid(t *testing.T) {
	sum := strings.Repeat("a", 64)
	for _, c := range []struct {
		e  MarketEntry
		ok bool
	}{
		{MarketEntry{ID: "x", Version: "1.0.0", Path: "exts/x"}, true},
		{MarketEntry{ID: "x", Version: "1.0.0", Package: &MarketPackage{URL: "release:x-v1.0.0/x-1.0.0.tar.gz", SHA256: sum, Size: 10}}, true},
		{MarketEntry{ID: "x", Version: "1.0.0"}, false},
		{MarketEntry{ID: "x", Version: "1.0.0", Path: "exts/x", Package: &MarketPackage{URL: "release:t/f", SHA256: sum, Size: 1}}, false},
		{MarketEntry{ID: "x", Version: "1.0.0", Package: &MarketPackage{URL: "https://example.com/x.tgz", SHA256: sum, Size: 1}}, false},
		{MarketEntry{ID: "x", Version: "1.0.0", Package: &MarketPackage{URL: "release:t/a/b", SHA256: sum, Size: 1}}, false},
		{MarketEntry{ID: "x", Version: "1.0.0", Package: &MarketPackage{URL: "release:../f", SHA256: sum, Size: 1}}, false},
		{MarketEntry{ID: "x", Version: "1.0.0", Package: &MarketPackage{URL: "release:t/f", SHA256: "zz", Size: 1}}, false},
		{MarketEntry{ID: "x", Version: "1.0.0", Package: &MarketPackage{URL: "release:t/f", SHA256: sum, Size: marketTarMost + 1}}, false},
	} {
		if validEntry(c.e) != c.ok {
			t.Errorf("%+v %+v: want %v", c.e, c.e.Package, c.ok)
		}
	}
}

// A marketplace whose package is an archive in a release: the hub downloads
// it with the token, checks size and sha256, unpacks it and runs it.
func TestExtMarketRelease(t *testing.T) {
	_, h, dir := newExtTestHub(t)
	self, _ := os.Executable()
	pkg := makeTgz(t,
		tarEntry{name: "collie-extension.json", mode: 0o644, body: `{"manifest_version":1,"id":"tool","name":"工具","version":"2.0.0","description":"d",
	  "engines":{"collie_protocol":1},"runtime":{"command":"bin/run"},
	  "contributes":{"connector":{"id":"tool","capabilities":["status","actions"],"actions":[{"name":"echo","description":"e","readonly":true,"params":{"text":{"type":"string"}}}]},"channel":{"format":"markdown"}},
	  "configuration":{"type":"object","properties":{"mode":{"type":"string","default":"actions"}}}}`},
		tarEntry{name: "README.md", mode: 0o644, body: "# tool"},
		tarEntry{name: "bin/run", mode: 0o755, body: "#!/bin/sh\nexec " + self + " ext-fake \"$@\"\n"})
	sum, size := sha256Hex(pkg), len(pkg)

	var storageAuth string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageAuth = r.Header.Get("Authorization")
		w.Write(pkg)
	}))
	defer storage.Close()
	// another host name, so the redirect must not carry the token
	storageURL := strings.Replace(storage.URL, "127.0.0.1", "localhost", 1)
	index := func(sum string, size int) string {
		return fmt.Sprintf(`{"name":"私有扩展","extensions":[{"id":"tool","version":"2.0.0","description":"一个工具",
		  "package":{"url":"release:tool-v2.0.0/tool-2.0.0.tar.gz","sha256":"%s","size":%d}}]}`, sum, size)
	}
	idx := index(sum, size)
	var auth string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/commits/HEAD"):
			fmt.Fprint(w, "abc123")
		case strings.HasSuffix(r.URL.Path, "/contents/collie-marketplace.json"):
			fmt.Fprint(w, idx)
		case strings.HasSuffix(r.URL.Path, "/releases/tags/tool-v2.0.0"):
			fmt.Fprintf(w, `{"tag_name":"tool-v2.0.0","assets":[{"id":7,"name":"tool-2.0.0.tar.gz","size":%d}]}`, size)
		case strings.HasSuffix(r.URL.Path, "/releases/assets/7"):
			auth = r.Header.Get("Authorization")
			if r.Header.Get("Accept") != "application/octet-stream" {
				t.Errorf("asset accept: %q", r.Header.Get("Accept"))
			}
			http.Redirect(w, r, storageURL+"/blob", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	old := marketAPI
	marketAPI = gh.URL
	defer func() { marketAPI = old }()

	if _, err := h.addMarket(context.Background(), "owner/repo", "", "tok-1"); err != nil {
		t.Fatal(err)
	}
	// the index says another sha256: nothing is installed
	idx = index(strings.Repeat("0", 64), size)
	if _, err := h.installFromMarket(context.Background(), "owner/repo", "tool"); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("a wrong sha256 installs: %v", err)
	}
	idx = index(sum, size+1)
	if _, err := h.installFromMarket(context.Background(), "owner/repo", "tool"); err == nil {
		t.Fatal("a wrong size installs")
	}
	if _, err := os.Stat(filepath.Join(dir, "market", "pkgs", "tool", "2.0.0")); !os.IsNotExist(err) {
		t.Fatal("a failed install leaves nothing behind")
	}

	idx = index(sum, size)
	m, err := h.installFromMarket(context.Background(), "owner/repo", "tool")
	if err != nil || m.Market == nil || m.Market.Package != "release:tool-v2.0.0/tool-2.0.0.tar.gz" || len(m.Market.Files) != 3 {
		t.Fatalf("install: %+v %v", m, err)
	}
	if auth != "Bearer tok-1" || storageAuth != "" {
		t.Fatalf("the token goes to the API only: api %q, storage %q", auth, storageAuth)
	}
	startExt(t, h, "tool")
	if out, err := h.do(context.Background(), "tool", "echo", map[string]any{"text": "hi"}); err != nil || !strings.HasPrefix(out.Text, "hi") {
		t.Fatalf("installed tool runs: %+v %v", out, err)
	}
}
