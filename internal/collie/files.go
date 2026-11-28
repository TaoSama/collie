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
// it wrote, a chart it drew), read-only. Bytes go out as an opaque attachment.
// The HTML mode raises the text limit for the sandboxed file reader.
const (
	fileTextMax     = 1 << 20
	fileHTMLMax     = 5 << 20
	fileImageMax    = 20 << 20
	fileDownloadMax = 100 << 20
)

func isHTMLFileRequest(q url.Values) bool {
	return q.Get("as") == "html" && q.Get("download") != "1" && isHTMLPath(q.Get("path"))
}

func isHTMLPath(p string) bool {
	ext := strings.ToLower(path.Ext(strings.TrimSpace(p)))
	return ext == ".html" || ext == ".htm"
}

// isImagePath: a picture the web app shows (IMG_EXT in app.js).
func isImagePath(p string) bool {
	switch strings.ToLower(path.Ext(strings.TrimSpace(p))) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".bmp", ".ico":
		return true
	}
	return false
}

func setFileAttachmentHeaders(h http.Header, disposition string) {
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", disposition)
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("Cache-Control", "no-store")
}

// Old HTML links opened /api/file as a document. Keep them pointing at the
// reader so an HTML file never runs as the top-level API response.
func (s *Server) routeFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if r.Header.Get("Sec-Fetch-Dest") == "document" && isHTMLFileRequest(q) {
		file := url.Values{"path": {q.Get("path")}}
		if q.Has("machine") {
			file.Set("machine", q.Get("machine"))
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "/#/file?"+file.Encode(), http.StatusFound)
		return
	}
	s.readFile(w, r)
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
			if download && !q.Has("machine") && s.tryRemoteDownloadFile(w, r, rawPath) {
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
	if !download && as == "html" && fi.Size() > fileHTMLMax {
		writeJSON(w, 413, map[string]any{"error": "HTML 文件超过 5 MB，无法预览"})
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
		setFileAttachmentHeaders(h, attachmentDisposition(p))
		h.Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
		h.Set("X-File-Kind", "download")
		h.Set("X-File-Size", strconv.FormatInt(fi.Size(), 10))
		h.Set("X-File-Path", url.PathEscape(s.tilde(p)))
		io.Copy(w, io.LimitReader(f, fileDownloadMax))
		return
	}

	if as == "html" {
		b, err := io.ReadAll(io.LimitReader(f, fileHTMLMax+1))
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": "无法读取 HTML 文件"})
			return
		}
		if len(b) > fileHTMLMax {
			writeJSON(w, 413, map[string]any{"error": "HTML 文件超过 5 MB，无法预览"})
			return
		}
		if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
			writeJSON(w, 400, map[string]any{"error": "这个文件不是有效的 HTML 文本"})
			return
		}
		h := w.Header()
		setFileAttachmentHeaders(h, "attachment")
		h.Set("X-File-Kind", "html")
		h.Set("X-File-Size", strconv.Itoa(len(b)))
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
	setFileAttachmentHeaders(h, "attachment")
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
