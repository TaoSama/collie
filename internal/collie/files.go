package collie

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
)

// readFile lets the web app show or download a file an agent mentions (a report
// it wrote, a chart it drew), read-only. Bytes go out as an opaque download so
// a browser never runs them unless ?as=html is requested for an .html/.htm
// file, which renders inside a strict opaque-origin CSP sandbox.
const (
	fileTextMax     = 1 << 20
	fileHTMLMax     = 5 << 20
	fileImageMax    = 20 << 20
	fileDownloadMax = 100 << 20
	htmlSandboxCSP  = "sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads"
)

func isHTMLPath(p string) bool {
	ext := strings.ToLower(path.Ext(strings.TrimSpace(p)))
	return ext == ".html" || ext == ".htm"
}

func setHTMLSandboxHeaders(h http.Header) {
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Del("Content-Disposition")
	h.Set("Content-Security-Policy", htmlSandboxCSP)
	h.Set("Cache-Control", "no-store")
}

func attachmentDisposition(p string) string {
	name := path.Base(strings.ReplaceAll(strings.TrimSpace(p), "\\", "/"))
	if name == "" || name == "." || name == "/" {
		name = "download"
	}
	var ascii strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 0x20 && c <= 0x7e && c != '"' && c != '\\' && c != ';' {
			ascii.WriteByte(c)
		} else {
			ascii.WriteByte('_')
		}
	}
	fallback := strings.TrimSpace(ascii.String())
	if fallback == "" {
		fallback = "download"
	}
	return `attachment; filename="` + fallback + `"; filename*=UTF-8''` + url.PathEscape(name)
}

func (s *Server) readFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rawPath := q.Get("path")
	as := q.Get("as")
	download := q.Get("download") == "1"
	p := s.expand(rawPath)
	if !download && as == "html" && (!isHTMLPath(rawPath) || !isHTMLPath(p)) {
		writeJSON(w, 400, map[string]any{"error": "仅支持直接打开 .html 或 .htm 文件"})
		return
	}
	fi, err := os.Stat(p)
	switch {
	case os.IsNotExist(err):
		if !s.node && s.herdr != nil {
			if download && s.tryRemoteDownloadFile(w, r, rawPath) {
				return
			}
			if !download && as == "html" && s.tryRemoteHTMLFile(w, r, rawPath) {
				return
			}
		}
		writeJSON(w, 404, map[string]any{"error": "找不到这个文件"})
		return
	case err != nil:
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	case fi.IsDir():
		writeJSON(w, 400, map[string]any{"error": "这是一个文件夹"})
		return
	}
	if download && fi.Size() > fileDownloadMax {
		writeJSON(w, 413, map[string]any{"error": "文件超过 100 MB，无法通过网页直接下载"})
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	defer f.Close()

	if download {
		h := w.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", attachmentDisposition(p))
		h.Set("Content-Security-Policy", "sandbox")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
		h.Set("X-File-Kind", "download")
		h.Set("X-File-Size", strconv.FormatInt(fi.Size(), 10))
		h.Set("X-File-Path", url.PathEscape(s.tilde(p)))
		io.Copy(w, io.LimitReader(f, fileDownloadMax))
		return
	}

	if as == "html" {
		b, _ := io.ReadAll(io.LimitReader(f, fileHTMLMax))
		if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
			writeJSON(w, 400, map[string]any{"error": "这个文件不是有效的 HTML 文本"})
			return
		}
		h := w.Header()
		setHTMLSandboxHeaders(h)
		h.Set("X-File-Kind", "html")
		h.Set("X-File-Size", strconv.FormatInt(fi.Size(), 10))
		h.Set("X-File-Path", url.PathEscape(s.tilde(p)))
		w.Write(b)
		return
	}

	isImage := as == "image" || as == "thumb"
	limit := int64(fileTextMax)
	if isImage {
		limit = fileImageMax
	}
	b, _ := io.ReadAll(io.LimitReader(f, limit))
	kind := "text"
	if isImage {
		kind = "image"
	} else if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
		kind, b = "binary", nil
	}
	if kind == "image" && fi.Size() > limit {
		kind, b = "binary", nil
	}
	h := w.Header()
	if kind == "image" && as == "thumb" {
		if t, ok := makeThumb(b); ok {
			b = t
			h.Set("X-File-Thumb", "1")
		}
	}
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", "attachment")
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("Cache-Control", "no-store")
	h.Set("X-File-Kind", kind)
	h.Set("X-File-Size", strconv.FormatInt(fi.Size(), 10))
	h.Set("X-File-Path", url.PathEscape(s.tilde(p)))
	w.Write(b)
}

// tryRemoteDownloadFile looks across connected online nodes when a direct
// ?download=1 link hits the hub without a machine parameter and is not on hub.
func (s *Server) tryRemoteDownloadFile(w http.ResponseWriter, r *http.Request, rawPath string) bool {
	for _, m := range s.herdr.Machines() {
		if m.Local || !m.Online {
			continue
		}
		client, err := s.herdr.Client(m.ID)
		if err != nil {
			continue
		}
		u := "http://node/api/file?download=1&path=" + url.QueryEscape(rawPath)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			continue
		}
		defer resp.Body.Close()
		h := w.Header()
		for _, k := range []string{"Content-Type", "Content-Disposition", "Content-Security-Policy", "Cache-Control", "Content-Length", "X-File-Kind", "X-File-Size", "X-File-Path"} {
			if v := resp.Header.Get(k); v != "" {
				h.Set(k, v)
			}
		}
		if resp.StatusCode == http.StatusOK {
			h.Set("Content-Type", "application/octet-stream")
			if h.Get("Content-Disposition") == "" {
				h.Set("Content-Disposition", attachmentDisposition(rawPath))
			}
			h.Set("Content-Security-Policy", "sandbox")
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, io.LimitReader(resp.Body, fileDownloadMax+4096))
		return true
	}
	return false
}

// tryRemoteHTMLFile looks across connected online nodes when a direct ?as=html
// link hits the hub without a machine parameter and the file is not on the hub.
func (s *Server) tryRemoteHTMLFile(w http.ResponseWriter, r *http.Request, rawPath string) bool {
	for _, m := range s.herdr.Machines() {
		if m.Local || !m.Online {
			continue
		}
		client, err := s.herdr.Client(m.ID)
		if err != nil {
			continue
		}
		u := "http://node/api/file?as=html&path=" + url.QueryEscape(rawPath)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			continue
		}
		defer resp.Body.Close()
		h := w.Header()
		for _, k := range []string{"Content-Type", "Content-Security-Policy", "Cache-Control", "X-File-Kind", "X-File-Size", "X-File-Path"} {
			if v := resp.Header.Get(k); v != "" {
				h.Set(k, v)
			}
		}
		if resp.StatusCode == http.StatusOK {
			setHTMLSandboxHeaders(h)
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, io.LimitReader(resp.Body, fileHTMLMax+4096))
		return true
	}
	return false
}
