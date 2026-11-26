package collie

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// An extension's icon (extensions/README.md §2): its service's own logo, a
// PNG or SVG in its package. The hub keeps the bytes and serves them at
// icon_url, so the extension list the page polls stays small; the page
// shows it as an <img>, which runs none of an SVG's markup.
const extIconMax = 256 << 10

var extIconTypes = map[string]string{".png": "image/png", ".svg": "image/svg+xml"}

// loadIcon reads the manifest's icon from its package; a manifest without
// one keeps its letter.
func (m *ExtManifest) loadIcon(fsys fs.FS) error {
	if m.IconURL != "" {
		return fmt.Errorf("icon_url is set by Collie, not the manifest")
	}
	if m.Icon == "" {
		return nil
	}
	typ := extIconTypes[path.Ext(m.Icon)]
	if !fs.ValidPath(m.Icon) || typ == "" {
		return fmt.Errorf("icon %q: a .png or .svg path inside the package", m.Icon)
	}
	b, err := fs.ReadFile(fsys, m.Icon)
	if err != nil {
		return fmt.Errorf("icon: %v", err)
	}
	ok := bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n"))
	if typ == "image/svg+xml" {
		ok = strings.Contains(string(b[:min(len(b), 512)]), "<svg")
	}
	if !ok || len(b) > extIconMax {
		return fmt.Errorf("icon %q: not a %s under %d KB", m.Icon, typ, extIconMax>>10)
	}
	sum := sha256.Sum256(b)
	m.icon, m.iconType = b, typ
	m.IconURL = "/api/ext/" + url.PathEscape(m.ID) + "/icon?v=" + hex.EncodeToString(sum[:6])
	return nil
}

// extIcon serves an icon; its URL changes with its content, so it may be
// kept for good.
func (s *Server) extIcon(w http.ResponseWriter, r *http.Request) {
	m := s.ext.manifest(r.PathValue("id"))
	if m == nil || m.icon == nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", m.iconType)
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	if _, err := w.Write(m.icon); err != nil {
		log.Printf("ext %s: icon: %v", m.ID, err)
	}
}
