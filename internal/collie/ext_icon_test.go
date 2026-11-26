package collie

import (
	"bytes"
	"net/http/httptest"
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
		if !strings.HasPrefix(m.IconURL, "/api/ext/"+m.ID+"/icon?v=") || len(m.icon) == 0 {
			t.Errorf("%s: built-in extension has no icon (%q)", m.ID, m.IconURL)
		}
	}
}

func TestExtLoadIcon(t *testing.T) {
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><rect width="24" height="24"/></svg>`)
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	pkg := fstest.MapFS{
		"icon.svg": {Data: svg}, "icon.png": {Data: png}, "fake.png": {Data: svg}, "logo.gif": {Data: []byte("GIF89a")},
		"big.png": {Data: append(append([]byte{}, png...), make([]byte, extIconMax)...)},
	}
	cases := []struct {
		name string
		m    ExtManifest
		ok   bool
	}{
		{"none", ExtManifest{}, true},
		{"svg", ExtManifest{ID: "x", Icon: "icon.svg"}, true},
		{"png", ExtManifest{ID: "x", Icon: "icon.png"}, true},
		{"set by manifest", ExtManifest{IconURL: "https://example.com/x.svg"}, false},
		{"escapes", ExtManifest{Icon: "../icon.svg"}, false},
		{"other type", ExtManifest{Icon: "logo.gif"}, false},
		{"not what it says", ExtManifest{Icon: "fake.png"}, false},
		{"missing", ExtManifest{Icon: "gone.svg"}, false},
		{"too big", ExtManifest{Icon: "big.png"}, false},
	}
	for _, c := range cases {
		err := c.m.loadIcon(pkg)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok %v", c.name, err, c.ok)
		}
		if c.ok && c.m.Icon != "" && (c.m.IconURL == "" || c.m.icon == nil) {
			t.Errorf("%s: icon not kept", c.name)
		}
	}
}

func TestExtIconServed(t *testing.T) {
	cat, err := extCatalog()
	if err != nil {
		t.Fatal(err)
	}
	h := &extHost{cat: map[string]*ExtManifest{}}
	for _, m := range cat {
		h.cat[m.ID] = m
	}
	srv := &Server{ext: h}
	for _, m := range cat {
		r := httptest.NewRequest("GET", m.IconURL, nil)
		r.SetPathValue("id", m.ID)
		w := httptest.NewRecorder()
		srv.extIcon(w, r)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), m.icon) || w.Header().Get("Content-Type") != m.iconType ||
			!strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
			t.Errorf("%s: %d %q %q", m.ID, w.Code, w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"))
		}
	}
	r := httptest.NewRequest("GET", "/api/ext/nope/icon", nil)
	r.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	srv.extIcon(w, r)
	if w.Code != 404 {
		t.Errorf("unknown extension: %d, want 404", w.Code)
	}
}
