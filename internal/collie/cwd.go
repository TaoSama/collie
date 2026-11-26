package collie

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"time"
)

// homePrefixRe matches the home directory a path starts with on the usual
// macOS and Linux layouts (ByteDance dev machines keep homes under /data00).
var homePrefixRe = regexp.MustCompile(`^(/Users/[^/]+|/data00/home/[^/]+|/home/[^/]+|/root)(/|$)`)

// rehome moves an absolute path under another computer's home to the same
// place under home: a shell on one computer expands "~" before the path
// reaches another. "" when p is not under a home other than this one.
func rehome(p, home string) string {
	if tildeIn(home, p) != p {
		return ""
	}
	m := homePrefixRe.FindStringSubmatch(p)
	if m == nil {
		return ""
	}
	return path.Join(home, p[len(m[1]):])
}

// resolveDir expands p against home and checks it is a directory here. A
// missing one under another computer's home is looked for under this home.
func resolveDir(home, p string) (string, error) {
	dir := expandIn(home, p)
	if isDir(dir) {
		return dir, nil
	}
	if alt := rehome(dir, home); alt != "" {
		if isDir(alt) {
			return alt, nil
		}
		return "", fmt.Errorf("directory not found: %s (nor %s)", dir, alt)
	}
	return "", fmt.Errorf("directory not found: %s", dir)
}

// resolveDirOn is resolveDir on computer m: another computer's node checks it.
// A path under a home the hub knows for another computer gets a second try
// as the same path under m's home, for homes resolveDir can't tell.
func (s *Server) resolveDirOn(m *Machine, p string) (string, error) {
	if m.Local {
		return resolveDir(s.home, p)
	}
	dir, err := s.remoteDir(m, p)
	if err != nil {
		for _, o := range s.herdr.Machines() {
			if t := tildeIn(o.Home, p); o.Home != "" && o.Home != m.Home && t != p {
				if d, e := s.remoteDir(m, t); e == nil {
					dir, err = d, nil
				}
				break
			}
		}
	}
	if err != nil {
		return "", err
	}
	if want := expandIn(m.Home, p); dir != want {
		log.Printf("%s: %s is not there, using %s", m.Name, want, dir)
	}
	return dir, nil
}

// remoteDir asks m's node, through its directory listing, where p is.
func (s *Server) remoteDir(m *Machine, p string) (string, error) {
	client, err := s.herdr.Client(m.ID)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://node/api/dirs?path="+url.QueryEscape(p), nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("checking the directory on %s: %w", m.Name, err)
	}
	defer resp.Body.Close()
	var r struct {
		Path  string `json:"path"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", fmt.Errorf("checking the directory on %s: %s: %w", m.Name, resp.Status, err)
	}
	if resp.StatusCode != 200 || r.Path == "" {
		return "", fmt.Errorf("%s on %s", r.Error, m.Name)
	}
	return r.Path, nil
}
