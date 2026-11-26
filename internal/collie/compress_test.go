package collie

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/TaoSama/collie/web"
)

func TestStaticGzipAndRevalidate(t *testing.T) {
	h := (&Server{token: "tok", home: t.TempDir(), web: web.FS}).Routes()
	get := func(path string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := get("/app.js", map[string]string{"Accept-Encoding": "gzip, br"})
	if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("app.js = %d, encoding %q; want gzipped 200", rec.Code, rec.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	want, err := fs.ReadFile(web.FS, "app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("gunzipped app.js differs from the embedded file")
	}

	// The browser sends back the weak tag it was given.
	tag := rec.Header().Get("ETag")
	if !strings.HasPrefix(tag, `W/"`) {
		t.Fatalf("ETag = %q, want weak", tag)
	}
	if rec := get("/app.js", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": tag}); rec.Code != http.StatusNotModified {
		t.Fatalf("revalidate = %d, want 304", rec.Code)
	}

	// Without gzip, and for small bodies, the bytes go as they are.
	if rec := get("/app.js", nil); rec.Header().Get("Content-Encoding") != "" || !bytes.Equal(rec.Body.Bytes(), want) {
		t.Fatal("plain request got a compressed or different app.js")
	}
	if rec := get("/api/session", map[string]string{"Accept-Encoding": "gzip"}); rec.Code != 401 || rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("small 401 = %d, encoding %q; want plain", rec.Code, rec.Header().Get("Content-Encoding"))
	}
}

func TestServiceWorkerStamp(t *testing.T) {
	h := (&Server{token: "tok", home: t.TempDir(), web: web.FS}).Routes()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sw.js", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("sw.js = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	if strings.Contains(body, "__BUILD__") || strings.Contains(body, "__SHELL__") {
		t.Fatal("sw.js still has its placeholders")
	}
	m := regexp.MustCompile(`const SHELL = (\[.*\]);`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no SHELL list in sw.js")
	}
	var shell []string
	if err := json.Unmarshal([]byte(m[1]), &shell); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(shell, "/markdown.js") {
		t.Error("Markdown renderer is missing from the offline shell")
	}
	// One missing file fails the whole install, leaving the app uncached.
	for _, p := range shell {
		if p == "/" {
			p = "/index.html"
		}
		if _, err := fs.Stat(web.FS, p[1:]); err != nil {
			t.Errorf("shell file %s: %v", p, err)
		}
	}
	// Each one is linked from the page, or it is not what a launch needs.
	page, err := fs.ReadFile(web.FS, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range shell {
		if p != "/" && !bytes.Contains(page, []byte(`"`+p+`"`)) {
			t.Errorf("shell file %s is not linked from index.html", p)
		}
	}
}
