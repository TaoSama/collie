package collie

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Local extensions (extensions/README.md §9): a package someone put in
// COLLIE_HOME/ext/<id>/pkg/<version>/ on some computer, with its
// collie-extension.json and program. Not compiled in and not checked: the
// 接入 page says so. It runs on the computer it was found on.

const extLocalEvery = time.Minute

// scanLocalExts reads the local packages on this computer (the newest
// version of each id).
func scanLocalExts(home string) []*ExtManifest {
	files, err := filepath.Glob(filepath.Join(home, "ext", "*", "pkg", "*", "collie-extension.json"))
	if err != nil {
		return nil
	}
	best := map[string]*ExtManifest{}
	for _, f := range files {
		id := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(f))))
		if _, err := os.Stat(filepath.Join(home, "ext", id, "install.json")); err == nil {
			continue // Collie installed it (built in or from a marketplace): not a local one
		}
		m, err := readLocalManifest(f)
		if err != nil {
			log.Printf("ext: local %s: %v", f, err)
			continue
		}
		if old := best[m.ID]; old == nil || versionLess(old.Version, m.Version) {
			best[m.ID] = m
		}
	}
	out := make([]*ExtManifest, 0, len(best))
	for _, m := range best {
		out = append(out, m)
	}
	return out
}

func readLocalManifest(f string) (*ExtManifest, error) {
	b, err := os.ReadFile(f)
	if err != nil {
		return nil, err
	}
	m, err := decodeExtManifest(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	verDir := filepath.Dir(f)
	if m.ID != filepath.Base(filepath.Dir(filepath.Dir(verDir))) || m.Version != filepath.Base(verDir) {
		return nil, fmt.Errorf("must sit in ext/<id>/pkg/<version>/ of its own id and version")
	}
	if m.Runtime.Builtin != "" || m.Source != nil {
		return nil, fmt.Errorf("a local extension runs its own command, without builtin or source")
	}
	m.Local, m.LocalOn = true, ""
	return m, m.validate()
}

// versionLess compares MAJOR.MINOR.PATCH numerically.
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		var x, y int
		fmt.Sscan(pa[i], &x)
		fmt.Sscan(pb[i], &y)
		if x != y {
			return x < y
		}
	}
	return len(pa) < len(pb)
}

// localExts answers a node's own local packages (GET /api/ext-local).
func (s *Server) localExts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"extensions": scanLocalExts(collieDir())})
}

// refreshLocal gathers the local packages of the hub and every computer
// online. One that a computer no longer has drops out, unless it is on:
// then it stays until you turn it off.
func (h *extHost) refreshLocal(ctx context.Context) {
	found := map[string]*ExtManifest{}
	for _, m := range scanLocalExts(h.home) {
		found[m.ID] = m
	}
	for _, mc := range h.s.herdr.Machines() {
		if mc.Local || !mc.Online {
			continue
		}
		list, err := h.remoteLocal(ctx, mc.ID)
		if err != nil {
			continue // an older node, or it didn't answer: next time
		}
		for _, m := range list {
			if found[m.ID] == nil {
				m.Local, m.LocalOn = true, mc.ID
				found[m.ID] = m
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, m := range h.cat {
		if m.Local && found[id] == nil && !h.sets[id].Enabled {
			delete(h.cat, id)
		}
	}
	for id, m := range found {
		if old := h.cat[id]; old == nil || old.Local {
			h.cat[id] = m
		}
	}
}

func (h *extHost) remoteLocal(ctx context.Context, machine string) ([]*ExtManifest, error) {
	c, err := h.s.herdr.Client(machine)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://node/api/ext-local", nil)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	var out struct {
		Extensions []*ExtManifest `json:"extensions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	var ok []*ExtManifest
	for _, m := range out.Extensions {
		m.Local = true // the manifest came through a node: still checked here
		if m.validate() == nil {
			ok = append(ok, m)
		}
	}
	return ok, nil
}
