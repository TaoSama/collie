package collie

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

// jarLocked is module id's jar, read from disk the first time. wb.jarMu held.
func (wb *workbench) jarLocked(id string) (map[string]wbCookie, error) {
	if j, ok := wb.jars[id]; ok {
		return j, nil
	}
	j := map[string]wbCookie{}
	b, err := os.ReadFile(wb.jarPath(id))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		var list []wbCookie
		if err := json.Unmarshal(b, &list); err != nil {
			return nil, fmt.Errorf("%s: %w", wb.jarPath(id), err)
		}
		for _, c := range list {
			j[c.key()] = c
		}
	}
	wb.jars[id] = j
	return j, nil
}

// cookiesFor are the cookies to send for a request to reqPath.
func (wb *workbench) cookiesFor(id, reqPath string, secure bool) ([]wbCookie, error) {
	wb.jarMu.Lock()
	defer wb.jarMu.Unlock()
	j, err := wb.jarLocked(id)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var out []wbCookie
	for _, c := range j {
		if !c.expired(now) && (secure || !c.Secure) && cookiePathMatch(reqPath, c.Path) {
			out = append(out, c)
		}
	}
	// Longer paths first, as browsers send them.
	slices.SortStableFunc(out, func(a, b wbCookie) int { return len(b.Path) - len(a.Path) })
	return out, nil
}

// storeCookies takes the Set-Cookie of a response to reqPath into the jar
// and writes it out when something changed.
func (wb *workbench) storeCookies(id, reqPath string, set []*http.Cookie) error {
	if len(set) == 0 {
		return nil
	}
	wb.jarMu.Lock()
	defer wb.jarMu.Unlock()
	j, err := wb.jarLocked(id)
	if err != nil {
		return err
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
		old, had := j[c.key()]
		if c.expired(now) {
			if had {
				delete(j, c.key())
				changed = true
			}
			continue
		}
		if !had || !sameCookie(old, c) {
			j[c.key()] = c
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return wb.writeJarLocked(id, j)
}

// sameCookie: nothing worth writing changed. A site that renews a Max-Age
// cookie on every response only moves its expiry a little.
func sameCookie(a, b wbCookie) bool {
	d := a.Expires.Sub(b.Expires)
	a.Expires, b.Expires = time.Time{}, time.Time{}
	return a == b && d > -time.Hour && d < time.Hour
}

func (wb *workbench) writeJarLocked(id string, j map[string]wbCookie) error {
	list := make([]wbCookie, 0, len(j))
	now := time.Now()
	for k, c := range j {
		if c.expired(now) {
			delete(j, k)
			continue
		}
		list = append(list, c)
	}
	return writePrivate(wb.jarPath(id), list)
}

// jarCount is how many live cookies module id's jar holds: all the page sees.
func (wb *workbench) jarCount(id string) int {
	wb.jarMu.Lock()
	defer wb.jarMu.Unlock()
	j, err := wb.jarLocked(id)
	if err != nil {
		return 0
	}
	n, now := 0, time.Now()
	for _, c := range j {
		if !c.expired(now) {
			n++
		}
	}
	return n
}

// clearJar forgets module id's login.
func (wb *workbench) clearJar(id string) error {
	wb.jarMu.Lock()
	defer wb.jarMu.Unlock()
	delete(wb.jars, id)
	if err := os.Remove(wb.jarPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// dropJars forgets the logins of modules gone from c or no longer kept by the hub.
func (wb *workbench) dropJars(c wbConfig) error {
	keep := map[string]bool{}
	for _, m := range c.Modules {
		if m.HubLogin {
			keep[m.ID] = true
		}
	}
	files, err := filepath.Glob(filepath.Join(wb.dir, "jar", "*.json"))
	if err != nil {
		return err
	}
	for _, f := range files {
		if id := strings.TrimSuffix(filepath.Base(f), ".json"); !keep[id] {
			if err := wb.clearJar(id); err != nil {
				return err
			}
		}
	}
	return nil
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
