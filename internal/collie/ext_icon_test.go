package collie

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestExtCatalogIcons(t *testing.T) {
	cat, err := extCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range cat {
		if !strings.HasPrefix(m.IconURL, "data:image/svg+xml;base64,") {
			t.Errorf("%s: built-in extension has no icon", m.ID)
		}
	}
}

func TestExtLoadIcon(t *testing.T) {
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><rect width="24" height="24"/></svg>`)
	pkg := fstest.MapFS{"icon.svg": {Data: svg}, "logo.png": {Data: []byte("\x89PNG")}, "big.svg": {Data: []byte("<svg>" + strings.Repeat(" ", extIconMax))}}
	cases := []struct {
		name string
		m    ExtManifest
		ok   bool
	}{
		{"none", ExtManifest{}, true},
		{"svg", ExtManifest{Icon: "icon.svg", Color: "#07C160"}, true},
		{"set by manifest", ExtManifest{IconURL: "https://example.com/x.svg"}, false},
		{"bad color", ExtManifest{Icon: "icon.svg", Color: "red"}, false},
		{"escapes", ExtManifest{Icon: "../icon.svg"}, false},
		{"not svg", ExtManifest{Icon: "logo.png"}, false},
		{"missing", ExtManifest{Icon: "gone.svg"}, false},
		{"too big", ExtManifest{Icon: "big.svg"}, false},
	}
	for _, c := range cases {
		err := c.m.loadIcon(pkg)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok %v", c.name, err, c.ok)
		}
		if c.ok && c.m.Icon != "" && c.m.IconURL == "" {
			t.Errorf("%s: no icon_url", c.name)
		}
	}
}
