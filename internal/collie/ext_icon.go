package collie

import (
	"encoding/base64"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
)

// An extension's icon (extensions/README.md §2): an SVG in its package,
// drawn by the page as a mask in one color, so it follows the theme and
// none of its markup reaches the page.
const extIconMax = 16 << 10

var extColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// loadIcon reads the manifest's icon from its package and keeps it as a
// data: URL for the page; a manifest without one keeps its letter.
func (m *ExtManifest) loadIcon(fsys fs.FS) error {
	if m.IconURL != "" {
		return fmt.Errorf("icon_url is set by Collie, not the manifest")
	}
	if m.Color != "" && !extColorRe.MatchString(m.Color) {
		return fmt.Errorf("color %q: want #rrggbb", m.Color)
	}
	if m.Icon == "" {
		return nil
	}
	if !fs.ValidPath(m.Icon) || path.Ext(m.Icon) != ".svg" {
		return fmt.Errorf("icon %q: a .svg path inside the package", m.Icon)
	}
	b, err := fs.ReadFile(fsys, m.Icon)
	if err != nil {
		return fmt.Errorf("icon: %v", err)
	}
	if len(b) > extIconMax || !strings.Contains(string(b[:min(len(b), 512)]), "<svg") {
		return fmt.Errorf("icon %q: not an SVG under %d KB", m.Icon, extIconMax>>10)
	}
	m.IconURL = "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(b)
	return nil
}
