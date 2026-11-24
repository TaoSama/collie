package collie

import (
	"compress/gzip"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// minGzipSize is the smallest body worth compressing: below it the gzip
// header and the CPU cost outweigh the bytes saved. Only responses that state
// their length (static files) can be told apart; JSON is always compressed.
const minGzipSize = 1024

var gzipPool = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

// compressible lists the text types the web app loads. Event streams are left
// alone so each event reaches the browser as it is sent.
func compressible(ct string) bool {
	t, _, _ := mime.ParseMediaType(ct)
	switch t {
	case "application/json", "application/manifest+json", "image/svg+xml",
		"text/html", "text/css", "text/javascript", "application/javascript", "text/plain":
		return true
	}
	return false
}

// compress gzips text responses. The hub sits a few hundred milliseconds
// away, so a 380 KB app.js or a 130 KB /api/super costs several round trips
// of TCP slow start when sent as is.
func compress(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Upgrades (a node's tunnel) need the connection itself, not a body.
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") || r.Header.Get("Range") != "" || r.Header.Get("Upgrade") != "" {
			h.ServeHTTP(w, r)
			return
		}
		g := &gzipWriter{ResponseWriter: w}
		defer g.close()
		h.ServeHTTP(g, r)
	})
}

type gzipWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	decided bool
}

func (g *gzipWriter) WriteHeader(code int) {
	if !g.decided {
		g.decided = true
		hd := g.Header()
		hd.Add("Vary", "Accept-Encoding")
		n, err := strconv.Atoi(hd.Get("Content-Length"))
		small := err == nil && n < minGzipSize
		if code == http.StatusOK && !small && hd.Get("Content-Encoding") == "" && compressible(hd.Get("Content-Type")) {
			hd.Del("Content-Length")
			hd.Set("Content-Encoding", "gzip")
			// A strong ETag names the plain bytes; the gzipped ones differ.
			if et := hd.Get("ETag"); strings.HasPrefix(et, `"`) {
				hd.Set("ETag", "W/"+et)
			}
			g.gz = gzipPool.Get().(*gzip.Writer)
			g.gz.Reset(g.ResponseWriter)
		}
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.decided {
		g.WriteHeader(http.StatusOK)
	}
	if g.gz != nil {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipWriter) Flush() {
	if !g.decided {
		g.WriteHeader(http.StatusOK)
	}
	if g.gz != nil {
		g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *gzipWriter) close() {
	if g.gz == nil {
		return
	}
	// An error here is the client going away mid-response; the handler is
	// done either way.
	_ = g.gz.Close()
	gzipPool.Put(g.gz)
	g.gz = nil
}
