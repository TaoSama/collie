package collie

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The login a module's site gives is kept on the hub (COLLIE_HOME/wb/jar/<id>.json,
// 0600) for modules whose 「由 hub 保管登录」 is on: the person signs in once
// through the proxy (workbench_proxy.go) and every device uses it. Only the
// module's own site is reached through its proxy, so a jar holds the cookies
// of one host, keyed by name and path; Domain is not needed.

type wbCookie struct {
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Path     string    `json:"path"`
	Expires  time.Time `json:"expires,omitzero"` // zero: a session cookie, kept until the site drops it
	Secure   bool      `json:"secure,omitempty"`
	HTTPOnly bool      `json:"http_only,omitempty"`
}

func (c wbCookie) key() string { return c.Name + "\x00" + c.Path }

func (c wbCookie) expired(now time.Time) bool { return !c.Expires.IsZero() && !c.Expires.After(now) }

func (wb *workbench) jarPath(id string) string { return filepath.Join(wb.dir, "jar", id+".json") }

type wbJar struct {
	Host    string
	Cookies map[string]wbCookie
	ctx     context.Context
	cancel  context.CancelFunc
}

func (wb *workbench) jarLocked(id string) (*wbJar, error) {
	if j := wb.jars[id]; j != nil {
		return j, nil
	}
	var list []wbCookie
	b, err := os.ReadFile(wb.jarPath(id))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &list); err != nil {
			return nil, fmt.Errorf("%s: %w", wb.jarPath(id), err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &wbJar{
		Host:    randomToken()[:wbProxyPrefixLen] + "-" + id,
		Cookies: make(map[string]wbCookie, len(list)), ctx: ctx, cancel: cancel,
	}
	for _, c := range list {
		j.Cookies[c.key()] = c
	}
	wb.jars[id] = j
	return j, nil
}

func (wb *workbench) proxyModule(id, host string) (WBModule, *wbJar, error) {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	m, ok := wb.moduleLocked(id)
	if !ok || m.Kind != wbKindWeb || !m.HubLogin {
		return WBModule{}, nil, nil
	}
	if host == "" {
		j, err := wb.jarLocked(id)
		return m, j, err
	}
	j := wb.jars[id]
	if j == nil || j.Host != host {
		return m, nil, nil
	}
	return m, j, nil
}

// cookiesFor are the cookies to send for a request to reqPath.
func (wb *workbench) cookiesFor(j *wbJar, reqPath string, secure bool) []wbCookie {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	now := time.Now()
	var out []wbCookie
	for _, c := range j.Cookies {
		if !c.expired(now) && (secure || !c.Secure) && cookiePathMatch(reqPath, c.Path) {
			out = append(out, c)
		}
	}
	// Longer paths first, as browsers send them.
	slices.SortStableFunc(out, func(a, b wbCookie) int { return len(b.Path) - len(a.Path) })
	return out
}

// storeCookies takes the Set-Cookie of a response to reqPath into the jar
// and writes it out when something changed.
func (wb *workbench) storeCookies(id string, j *wbJar, reqPath string, set []*http.Cookie) error {
	if len(set) == 0 {
		return nil
	}
	wb.mu.Lock()
	defer wb.mu.Unlock()
	if wb.jars[id] != j {
		return nil
	}
	now, changed := time.Now(), false
	for _, sc := range set {
		c := wbCookie{Name: sc.Name, Value: sc.Value, Path: sc.Path, Secure: sc.Secure, HTTPOnly: sc.HttpOnly}
		if c.Path == "" || !strings.HasPrefix(c.Path, "/") {
			c.Path = defaultCookiePath(reqPath)
		}
		switch {
		case sc.MaxAge < 0:
			c.Expires = now
		case sc.MaxAge > 0:
			c.Expires = now.Add(time.Duration(sc.MaxAge) * time.Second)
		case !sc.Expires.IsZero():
			c.Expires = sc.Expires
		}
		old, had := j.Cookies[c.key()]
		if c.expired(now) {
			if had {
				delete(j.Cookies, c.key())
				changed = true
			}
			continue
		}
		if !had || !sameCookie(old, c) {
			j.Cookies[c.key()] = c
			changed = true
		}
	}
	if !changed {
		return nil
	}
	list := make([]wbCookie, 0, len(j.Cookies))
	for k, c := range j.Cookies {
		if c.expired(now) {
			delete(j.Cookies, k)
			continue
		}
		list = append(list, c)
	}
	return writePrivate(wb.jarPath(id), list)
}

// sameCookie: nothing worth writing changed. A site that renews a Max-Age
// cookie on every response only moves its expiry a little.
func sameCookie(a, b wbCookie) bool {
	d := a.Expires.Sub(b.Expires)
	a.Expires, b.Expires = time.Time{}, time.Time{}
	return a == b && d > -time.Hour && d < time.Hour
}

// jarCount is how many live cookies module id's jar holds: all the page sees.
func (wb *workbench) jarCount(id string) int {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	if m, ok := wb.moduleLocked(id); !ok || !m.HubLogin {
		return 0
	}
	j, err := wb.jarLocked(id)
	if err != nil {
		return 0
	}
	n, now := 0, time.Now()
	for _, c := range j.Cookies {
		if !c.expired(now) {
			n++
		}
	}
	return n
}

func (wb *workbench) clearJar(id string) error {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	return wb.clearJarLocked(id)
}

func (wb *workbench) clearJarLocked(id string) error {
	if err := os.Remove(wb.jarPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if j := wb.jars[id]; j != nil {
		j.cancel()
		delete(wb.jars, id)
	}
	return nil
}

func (wb *workbench) dropJarsLocked(c wbConfig) error {
	origins := map[string]string{}
	for _, m := range c.Modules {
		if m.HubLogin {
			origins[m.ID] = wbOrigin(m.URL)
		}
	}
	keep := map[string]bool{}
	for _, m := range wb.cfg.Modules {
		if !m.HubLogin {
			continue
		}
		if origin, ok := origins[m.ID]; ok && origin == wbOrigin(m.URL) {
			keep[m.ID] = true
			continue
		}
		if err := wb.clearJarLocked(m.ID); err != nil {
			return err
		}
	}
	files, err := os.ReadDir(filepath.Join(wb.dir, "jar"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, f := range files {
		if id, ok := strings.CutSuffix(f.Name(), ".json"); ok && !f.IsDir() && !keep[id] {
			if err := wb.clearJarLocked(id); err != nil {
				return err
			}
		}
	}
	return nil
}

func wbOrigin(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return u.Scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// cookiePathMatch is RFC 6265 5.1.4.
func cookiePathMatch(reqPath, cookiePath string) bool {
	if reqPath == "" {
		reqPath = "/"
	}
	if reqPath == cookiePath {
		return true
	}
	return strings.HasPrefix(reqPath, cookiePath) && (strings.HasSuffix(cookiePath, "/") || reqPath[len(cookiePath)] == '/')
}

// defaultCookiePath is RFC 6265 5.1.4's default-path.
func defaultCookiePath(reqPath string) string {
	if !strings.HasPrefix(reqPath, "/") || strings.Count(reqPath, "/") == 1 {
		return "/"
	}
	return path.Dir(reqPath)
}
