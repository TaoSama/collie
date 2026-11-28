package collie

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/TaoSama/collie/web"
)

func TestReadFileAsHTMLLimitsAndNodeForwarding(t *testing.T) {
	hubHome := t.TempDir()
	nodeHome := t.TempDir()

	htmlContent := "<!doctype html><html><body><h1>Interactive Chart</h1><script>console.log(1)</script></body></html>"
	if err := os.WriteFile(filepath.Join(hubHome, "report.html"), []byte(htmlContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hubHome, "legacy.HTM"), []byte("<p>htm ok</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hubHome, "notes.txt"), []byte("<script>alert(1)</script>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hubHome, "vector.svg"), []byte("<svg onload='alert(1)'/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hubHome, "binary.html"), []byte{'<', 'h', '1', '>', 0, 'x'}, 0o600); err != nil {
		t.Fatal(err)
	}

	// 2 MiB HTML (> fileTextMax 1 MiB, <= fileHTMLMax 5 MiB) must NOT be truncated.
	twoMiB := bytes.Repeat([]byte("a"), 2<<20)
	if err := os.WriteFile(filepath.Join(hubHome, "large2m.html"), twoMiB, 0o600); err != nil {
		t.Fatal(err)
	}
	// 6 MiB HTML (> fileHTMLMax 5 MiB) must be rejected in preview mode.
	sixMiB := bytes.Repeat([]byte("b"), 6<<20)
	if err := os.WriteFile(filepath.Join(hubHome, "huge6m.html"), sixMiB, 0o600); err != nil {
		t.Fatal(err)
	}

	// Remote node file (only exists on nodeHome, NOT on hubHome).
	nodeHTML := "<!doctype html><title>Mac Visualization</title><div id='mac'>ok</div>"
	if err := os.WriteFile(filepath.Join(nodeHome, "mac-only.html"), []byte(nodeHTML), 0o600); err != nil {
		t.Fatal(err)
	}

	nodeSrv := &Server{home: nodeHome, node: true, web: fstest.MapFS{}}
	nodeRoutes := nodeSrv.NodeRoutes()
	nodeHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") == "~/old-node.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "sandbox allow-scripts")
			w.Header().Set("X-File-Kind", "html")
			_, err := w.Write([]byte(nodeHTML))
			if err != nil {
				t.Error(err)
			}
			return
		}
		nodeRoutes.ServeHTTP(w, r)
	}))
	defer nodeHTTP.Close()
	nodeURL, _ := url.Parse(nodeHTTP.URL)

	herdr := newHerdr(filepath.Join(hubHome, "missing.sock"), "hub", hubHome)
	herdr.attach(&Machine{
		ID:     "mac",
		Name:   "mac",
		Home:   nodeHome,
		Online: true,
		client: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				cp := req.Clone(req.Context())
				cp.URL.Scheme = nodeURL.Scheme
				cp.URL.Host = nodeURL.Host
				cp.RequestURI = ""
				return http.DefaultTransport.RoundTrip(cp)
			}),
		},
	})

	hubSrv := &Server{
		herdr: herdr,
		token: "secret-tok",
		home:  hubHome,
		web:   web.FS,
	}
	handler := hubSrv.Routes()

	get := func(target string, extraHeaders map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if _, hasAuth := extraHeaders["Authorization"]; !hasAuth {
			req.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-tok"})
		}
		for k, v := range extraHeaders {
			if v != "" {
				req.Header.Set(k, v)
			}
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// 1. Default mode (no as=html) stays an opaque attachment download.
	rec := get("/api/file?path=~/report.html", nil)
	if rec.Code != 200 {
		t.Fatalf("default mode status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("default Content-Type = %q, want application/octet-stream", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "attachment" {
		t.Fatalf("default Content-Disposition = %q, want attachment", cd)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp != "sandbox" {
		t.Fatalf("default CSP = %q, want sandbox", csp)
	}

	assertAttachment := func(rec *httptest.ResponseRecorder) {
		t.Helper()
		for name, want := range map[string]string{
			"Content-Type": "application/octet-stream", "Content-Disposition": "attachment",
			"Content-Security-Policy": "sandbox", "Cache-Control": "no-store", "X-File-Kind": "html",
		} {
			if got := rec.Header().Get(name); got != want {
				t.Fatalf("%s = %q, want %q", name, got, want)
			}
		}
	}
	for _, p := range []string{"~/report.html", "~/legacy.HTM"} {
		rec = get("/api/file?path="+url.QueryEscape(p)+"&as=html", nil)
		if rec.Code != 200 {
			t.Fatalf("as=html %s status = %d: %s", p, rec.Code, rec.Body.String())
		}
		assertAttachment(rec)
	}

	// 3. Non-HTML suffixes and binary disguised as .html are rejected on ?as=html.
	for _, bad := range []string{"~/notes.txt", "~/vector.svg", "~/binary.html"} {
		rec = get("/api/file?path="+url.QueryEscape(bad)+"&as=html", nil)
		if rec.Code != 400 {
			t.Fatalf("bad as=html %s status = %d, want 400", bad, rec.Code)
		}
		if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("bad as=html %s returned text/html Content-Type", bad)
		}
	}

	// Reports must be read whole, within the preview limit.
	rec = get("/api/file?path=~/large2m.html&as=html", nil)
	if rec.Code != 200 || rec.Body.Len() != 2<<20 {
		t.Fatalf("2 MiB as=html status=%d len=%d, want 200 len=%d", rec.Code, rec.Body.Len(), 2<<20)
	}
	rec = get("/api/file?path=~/huge6m.html&as=html", nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized report status = %d, want 413", rec.Code)
	}

	remote := "/api/file?path=~/mac-only.html&as=html&machine=mac"
	rec = get(remote, nil)
	if rec.Code != 200 || rec.Body.String() != nodeHTML {
		t.Fatalf("explicit remote HTML: %d %q", rec.Code, rec.Body.String())
	}
	assertAttachment(rec)
	rec = get("/api/file?path=~/old-node.html&as=html&machine=mac", nil)
	if rec.Code != 200 || rec.Body.String() != nodeHTML {
		t.Fatalf("legacy node HTML: %d %q", rec.Code, rec.Body.String())
	}
	assertAttachment(rec)
	for _, suffix := range []string{"as=html", "as=html&machine=", "download=1&machine="} {
		rec = get("/api/file?path=~/mac-only.html&"+suffix, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("local-only %s unexpectedly found a remote file: %d", suffix, rec.Code)
		}
	}
	rec = get(remote, map[string]string{"Sec-Fetch-Dest": "document"})
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/#/file?machine=mac&path=~%2Fmac-only.html" {
		t.Fatalf("legacy HTML link = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	offline := "/api/file?path=~/offline.html&as=html&machine=offline"
	rec = get(offline, map[string]string{"Sec-Fetch-Dest": "document"})
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/#/file?machine=offline&path=~%2Foffline.html" {
		t.Fatalf("offline HTML link = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	rec = get("/html-viewer.html", nil)
	if rec.Code != 200 || rec.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatalf("viewer status = %d headers = %v", rec.Code, rec.Header())
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"frame-ancestors 'self'", "frame-src 'none'", "connect-src 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("viewer CSP %q is missing %q", csp, directive)
		}
	}
	metaCSP := strings.TrimSuffix(csp, "; frame-ancestors 'self'")
	if !strings.Contains(rec.Body.String(), `http-equiv="Content-Security-Policy" content="`+metaCSP+`"`) {
		t.Error("static viewer meta CSP must match the HTTP policy for the report")
	}
	if !strings.Contains(rec.Body.String(), `sandbox="allow-scripts"`) || strings.Contains(rec.Body.String(), "allow-same-origin") {
		t.Error("viewer must allow report scripts only in an opaque frame")
	}

	if got := get("/", nil).Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("main page X-Frame-Options = %q", got)
	}

	// 6. Sandboxed opaque origin ("Origin: null") gets neither auth nor CORS grant on /api/*.
	unauthReq := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	unauthReq.Header.Set("Origin", "null")
	unauthRec := httptest.NewRecorder()
	handler.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != 401 {
		t.Fatalf("Origin: null unauthenticated /api/version = %d, want 401", unauthRec.Code)
	}
	if acao := unauthRec.Header().Get("Access-Control-Allow-Origin"); acao != "" {
		t.Fatalf("Origin: null must not receive Access-Control-Allow-Origin, got %q", acao)
	}
}

func TestReadFileDownloadFullUntruncatedAndCrossMachine(t *testing.T) {
	hubHome := t.TempDir()
	nodeHome := t.TempDir()

	// 2.4 MiB Markdown report (> fileTextMax 1 MiB): preview truncates, download=1 returns full bytes.
	bigMD := append([]byte("# Full Report\n\n"), bytes.Repeat([]byte("row-data-0123456789abcdef\n"), 100_000)...)
	if len(bigMD) <= fileTextMax {
		t.Fatalf("expected test fixture > %d bytes, got %d", fileTextMax, len(bigMD))
	}
	if err := os.WriteFile(filepath.Join(hubHome, "SUMMARY.md"), bigMD, 0o600); err != nil {
		t.Fatal(err)
	}

	// UTF-8 filename & binary file containing NUL bytes.
	utf8Name := "季度复盘报告.md"
	utf8Body := []byte("# 季度复盘\n完整原文内容\n")
	if err := os.WriteFile(filepath.Join(hubHome, utf8Name), utf8Body, 0o600); err != nil {
		t.Fatal(err)
	}
	binBody := []byte{0x89, 'P', 'K', 0x00, 0x01, 0x02, 0xff, 'E', 'N', 'D'}
	if err := os.WriteFile(filepath.Join(hubHome, "archive.bin"), binBody, 0o600); err != nil {
		t.Fatal(err)
	}

	// Remote node >1 MiB file & HTML file downloaded as raw attachment.
	nodeBig := append([]byte("package main\n"), bytes.Repeat([]byte("// remote line padding 0123456789\n"), 40_000)...)
	if err := os.WriteFile(filepath.Join(nodeHome, "remote_gen.go"), nodeBig, 0o600); err != nil {
		t.Fatal(err)
	}
	nodeHTML := []byte("<!doctype html><script>alert(1)</script>")
	if err := os.WriteFile(filepath.Join(nodeHome, "chart.html"), nodeHTML, 0o600); err != nil {
		t.Fatal(err)
	}

	nodeSrv := &Server{home: nodeHome, node: true, web: fstest.MapFS{}}
	nodeHTTP := httptest.NewServer(nodeSrv.NodeRoutes())
	defer nodeHTTP.Close()
	nodeURL, _ := url.Parse(nodeHTTP.URL)

	herdr := newHerdr(filepath.Join(hubHome, "missing.sock"), "hub", hubHome)
	herdr.attach(&Machine{
		ID:     "mac",
		Name:   "mac",
		Home:   nodeHome,
		Online: true,
		client: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				cp := req.Clone(req.Context())
				cp.URL.Scheme = nodeURL.Scheme
				cp.URL.Host = nodeURL.Host
				cp.RequestURI = ""
				return http.DefaultTransport.RoundTrip(cp)
			}),
		},
	})

	hubSrv := &Server{
		herdr: herdr,
		token: "secret-tok",
		home:  hubHome,
		web:   fstest.MapFS{"index.html": {Data: []byte("ok")}, "app.js": {Data: []byte("")}, "app.css": {Data: []byte("")}},
	}
	handler := hubSrv.Routes()

	get := func(target string, extraHeaders map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if _, hasAuth := extraHeaders["Authorization"]; !hasAuth {
			req.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-tok"})
		}
		for k, v := range extraHeaders {
			if v != "" {
				req.Header.Set(k, v)
			}
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// 1. Preview truncates >1 MiB text, while download=1 streams the complete file.
	prev := get("/api/file?path=~/SUMMARY.md", nil)
	if prev.Code != 200 || prev.Body.Len() != fileTextMax {
		t.Fatalf("preview SUMMARY.md status=%d len=%d, want 200 len=%d", prev.Code, prev.Body.Len(), fileTextMax)
	}
	dl := get("/api/file?path=~/SUMMARY.md&download=1", nil)
	if dl.Code != 200 {
		t.Fatalf("download SUMMARY.md status=%d: %s", dl.Code, dl.Body.String())
	}
	if !bytes.Equal(dl.Body.Bytes(), bigMD) {
		t.Fatalf("download SUMMARY.md len=%d, want exact %d bytes", dl.Body.Len(), len(bigMD))
	}
	if cd := dl.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="SUMMARY.md"`) {
		t.Fatalf("Content-Disposition = %q, want SUMMARY.md", cd)
	}
	if csp := dl.Header().Get("Content-Security-Policy"); csp != "sandbox" {
		t.Fatalf("CSP = %q, want sandbox", csp)
	}

	// 2. UTF-8 filename & binary file download in full.
	dlUTF8 := get("/api/file?path="+url.QueryEscape("~/"+utf8Name)+"&download=1", nil)
	if dlUTF8.Code != 200 || !bytes.Equal(dlUTF8.Body.Bytes(), utf8Body) {
		t.Fatalf("UTF-8 download failed: code=%d len=%d", dlUTF8.Code, dlUTF8.Body.Len())
	}
	if cd := dlUTF8.Header().Get("Content-Disposition"); !strings.Contains(cd, "filename*=UTF-8''"+url.PathEscape(utf8Name)) {
		t.Fatalf("UTF-8 Content-Disposition = %q", cd)
	}

	prevBin := get("/api/file?path=~/archive.bin", nil)
	if prevBin.Header().Get("X-File-Kind") != "binary" || prevBin.Body.Len() != 0 {
		t.Fatalf("preview archive.bin kind=%q len=%d, want binary len=0", prevBin.Header().Get("X-File-Kind"), prevBin.Body.Len())
	}
	dlBin := get("/api/file?path=~/archive.bin&download=1", nil)
	if dlBin.Code != 200 || !bytes.Equal(dlBin.Body.Bytes(), binBody) {
		t.Fatalf("download archive.bin code=%d body=%v, want full binary", dlBin.Code, dlBin.Body.Bytes())
	}

	// 3. Cross-machine >1 MiB download (explicit machine=mac and implicit fallback) + HTML attachment safety.
	for _, q := range []string{
		"/api/file?path=~/remote_gen.go&machine=mac&download=1",
		"/api/file?path=~/remote_gen.go&download=1",
	} {
		rec := get(q, nil)
		if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), nodeBig) {
			t.Fatalf("remote download %s code=%d len=%d, want %d", q, rec.Code, rec.Body.Len(), len(nodeBig))
		}
		if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="remote_gen.go"`) {
			t.Fatalf("remote %s Content-Disposition = %q", q, cd)
		}
	}
	dlHTML := get("/api/file?path=~/chart.html&machine=mac&download=1&as=html", nil)
	if dlHTML.Code != 200 || dlHTML.Header().Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(dlHTML.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("download=1 on .html must stay attachment, got ct=%q cd=%q", dlHTML.Header().Get("Content-Type"), dlHTML.Header().Get("Content-Disposition"))
	}

	// 4. Multi-Hub cross-origin Bearer request exposes Content-Disposition and streams full bytes.
	corsDL := get("/api/file?path=~/SUMMARY.md&download=1", map[string]string{
		"Authorization": "Bearer secret-tok",
		"Origin":        "http://other-hub.local:7788",
	})
	if corsDL.Code != 200 || corsDL.Body.Len() != len(bigMD) {
		t.Fatalf("multi-hub Bearer download code=%d len=%d", corsDL.Code, corsDL.Body.Len())
	}
	if exp := corsDL.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(exp, "Content-Disposition") {
		t.Fatalf("Access-Control-Expose-Headers = %q, want Content-Disposition", exp)
	}
}

func TestAttachmentDispositionSanitizesQuotesAndCRLF(t *testing.T) {
	got := attachmentDisposition("/tmp/bad\"name\r\n;x.txt")
	if strings.ContainsAny(got, "\r\n") || strings.Count(got, `"`) != 2 {
		t.Fatalf("unsafe Content-Disposition = %q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
