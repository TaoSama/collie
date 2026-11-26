package collie

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"mime"
	"net/http"
	"strings"
	"sync"
)

var webBuildOnce struct {
	sync.Once
	v string
}

// shellFiles are what a launch needs before it can draw anything. The
// service worker keeps them, so an installed app starts without the network.
var shellFiles = []string{
	"/", "/icon.svg", "/icon-512.png", "/manifest.webmanifest",
	"/vendor/xterm.css", "/vendor/katex/katex.min.css",
	"/app.css", "/media.css", "/inbox-message.css", "/workbench.css", "/icons.css", "/skins.css",
	"/vendor/xterm.js", "/vendor/katex/katex.min.js",
	"/imgedit.js", "/icons.js", "/threads.js", "/tasks.js", "/machines.js", "/ext.js",
	"/header_status.js", "/wheel.js", "/screencache.js", "/workbench.js", "/markdown.js",
	"/inbox-message.js", "/highlight.js", "/app.js",
}

// webBuild fingerprints the web app's files, the service worker's included.
func (s *Server) webBuild() string {
	webBuildOnce.Do(func() {
		h := sha256.New()
		for _, p := range append([]string{"/sw.js"}, shellFiles...) {
			if p == "/" {
				p = "/index.html"
			}
			b, _ := fs.ReadFile(s.web, p[1:])
			h.Write(b)
		}
		webBuildOnce.v = hex.EncodeToString(h.Sum(nil)[:6])
	})
	return webBuildOnce.v
}

func init() {
	_ = mime.AddExtensionType(".woff2", "font/woff2")
}

func (s *Server) static() http.Handler {
	files := http.FileServerFS(s.web)
	// Embedded files have no modification time, so browsers would guess how
	// long to keep them and run an old app.js after an upgrade. Tag each file
	// by its content and have the browser revalidate every time.
	var etags sync.Map
	etag := func(p string) string {
		if v, ok := etags.Load(p); ok {
			return v.(string)
		}
		b, err := fs.ReadFile(s.web, p)
		if err != nil {
			return ""
		}
		sum := sha256.Sum256(b)
		tag := `"` + hex.EncodeToString(sum[:8]) + `"`
		etags.Store(p, tag)
		return tag
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if p == "sw.js" {
			s.serviceWorker(w, r)
			return
		}
		if tag := etag(p); tag != "" {
			w.Header().Set("ETag", tag)
			w.Header().Set("Cache-Control", "no-cache")
			// Compressed responses carry the tag as weak (W/"…").
			if strings.TrimPrefix(r.Header.Get("If-None-Match"), "W/") == tag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}

// serviceWorker serves web/sw.js stamped with this build and its file list.
// A new build changes the script's bytes, which is how the browser learns to
// fetch the new app shell.
func (s *Server) serviceWorker(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(s.web, "sw.js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	list, err := json.Marshal(shellFiles)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	b = bytes.Replace(b, []byte(`"__BUILD__"`), []byte(`"`+s.webBuild()+`"`), 1)
	b = bytes.Replace(b, []byte(`["__SHELL__"]`), list, 1)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}
