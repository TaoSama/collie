package collie

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A chat's picture in 收件箱: the extension names an https URL on one of
// its permissions.network hosts, the hub fetches it and keeps it a day, and
// the page asks the hub. The browser never reaches the chat app's CDN.

const (
	avatarFresh   = 24 * time.Hour
	avatarMost    = 1 << 20
	avatarTimeout = 10 * time.Second
)

var (
	avatarClient = &http.Client{Timeout: avatarTimeout}
	avatarMu     sync.Mutex // one fetch at a time: a list asks for many at once
)

func avatarDir() string { return filepath.Join(collieDir(), "cache", "avatars") }

// avatarKey names a picture by its URL, so a new picture is a new file (and a new address).
func avatarKey(u string) string {
	h := sha256.Sum256([]byte(u))
	return hex.EncodeToString(h[:])[:16]
}

// hostAllowed: the host is one the manifest says it reaches (*.example.com covers its subdomains).
func (m *ExtManifest) hostAllowed(host string) bool {
	for _, p := range m.Permissions.Network {
		if suffix, ok := strings.CutPrefix(p, "*"); ok && strings.HasSuffix(host, suffix) || host == p {
			return true
		}
	}
	return false
}

// avatarURL checks a picture's address against the extension's hosts.
func avatarURL(m *ExtManifest, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", errors.New("not an https address")
	}
	if !m.hostAllowed(u.Hostname()) {
		return "", fmt.Errorf("%s is not among the extension's hosts", u.Hostname())
	}
	return u.String(), nil
}

// avatarFile is the picture on disk, fetched again once a day; an old copy
// stands in when the fetch fails.
func avatarFile(raw string) (string, error) {
	path := filepath.Join(avatarDir(), avatarKey(raw))
	if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) < avatarFresh {
		return path, nil
	}
	avatarMu.Lock()
	defer avatarMu.Unlock()
	if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) < avatarFresh {
		return path, nil // fetched while this one waited
	}
	err := fetchAvatar(raw, path)
	if err == nil {
		return path, nil
	}
	if _, serr := os.Stat(path); serr == nil {
		log.Printf("inbox avatar: %v; the old copy stays", err)
		return path, nil
	}
	return "", err
}

func fetchAvatar(raw, path string) error {
	resp, err := avatarClient.Get(raw)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/") {
		return fmt.Errorf("not a picture: %s", resp.Header.Get("Content-Type"))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, avatarMost+1))
	if err != nil {
		return err
	}
	if len(b) > avatarMost {
		return errors.New("picture too large")
	}
	if err := os.MkdirAll(avatarDir(), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// inboxAvatar serves a conversation's picture (GET /api/inbox/avatar?id=…).
func (s *Server) inboxAvatar(w http.ResponseWriter, r *http.Request) {
	c, ok := s.ext.store.conv(r.URL.Query().Get("id"))
	m := s.ext.manifest(c.Ext)
	if !ok || c.Avatar == "" || m == nil {
		http.NotFound(w, r)
		return
	}
	raw, err := avatarURL(m, c.Avatar)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	path, err := avatarFile(raw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, path)
}

// avatarPath is where the page asks for a conversation's picture; "" when it has none.
func avatarPath(c InboxConv) string {
	if c.Avatar == "" {
		return ""
	}
	return "/api/inbox/avatar?id=" + url.QueryEscape(c.ID) + "&v=" + avatarKey(c.Avatar)[:8]
}
