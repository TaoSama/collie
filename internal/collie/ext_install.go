package collie

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/TaoSama/collie/extensions"
)

// Installing a packaged extension (extensions/README.md §9): download each
// file of the package from its pinned address, check its sha256, then move
// the whole package into place. One file off and nothing is left behind.

const (
	extFileMost     = 96 << 20 // a program built for one platform fits
	extDownloadWait = 5 * time.Minute
)

func extDir(home, id string) string { return filepath.Join(home, "ext", id) }

type extInstalled struct {
	Version string `json:"version"`
	Source  string `json:"source"` // base and files, hashed: a changed pin reinstalls
	At      string `json:"at"`
}

// extPackageDir is where m's package lives once installed ("" for builtins).
func extPackageDir(home string, m *ExtManifest) string {
	if m.Source == nil && !m.Local && m.Market == nil {
		return ""
	}
	return filepath.Join(extDir(home, m.ID), "pkg", m.Version)
}

// pinned is where m's files come from and their sha256s: its source, or
// the hub's copy of a marketplace package.
func pinned(m *ExtManifest) (base string, files map[string]string) {
	if m.Market != nil {
		return marketSourceHub + m.ID + "/" + m.Version + "/", m.Market.Files
	}
	return m.Source.Base, m.Source.Files
}

// bundledFiles is what Collie itself carries of a built-in package
// (extensions/<id>/pkg/): its own code, next to the pinned files.
func bundledFiles(m *ExtManifest) (map[string][]byte, error) {
	if m.Market != nil || m.Local {
		return nil, nil
	}
	root := m.ID + "/pkg"
	if _, err := fs.Stat(extensions.Bundled, root); errors.Is(err, fs.ErrNotExist) {
		return nil, nil // nothing bundled
	} else if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	err := fs.WalkDir(extensions.Bundled, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(extensions.Bundled, p)
		out[strings.TrimPrefix(p, root+"/")] = b
		return err
	})
	return out, err
}

func sourceKey(m *ExtManifest, bundled map[string][]byte) string {
	base, files := pinned(m)
	names := make([]string, 0, len(files))
	for p := range files {
		names = append(names, p)
	}
	sort.Strings(names)
	h := sha256.New()
	io.WriteString(h, base+"\n")
	for _, p := range names {
		io.WriteString(h, files[p]+"  "+p+"\n")
	}
	names = names[:0]
	for p := range bundled {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		io.WriteString(h, sha256Hex(bundled[p])+"  bundled/"+p+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ensureInstalled installs m on this computer unless that version is there.
func ensureInstalled(ctx context.Context, home string, m *ExtManifest) error {
	if m.Source == nil && m.Market == nil {
		return nil
	}
	bundled, err := bundledFiles(m)
	if err != nil {
		return fmt.Errorf("its bundled files: %v", err)
	}
	dir, key := extPackageDir(home, m), sourceKey(m, bundled)
	var have extInstalled
	if b, err := os.ReadFile(filepath.Join(extDir(home, m.ID), "install.json")); err == nil && json.Unmarshal(b, &have) == nil &&
		have.Version == m.Version && have.Source == key {
		if _, err := os.Stat(dir); err == nil {
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, extDownloadWait)
	defer cancel()
	if err := os.MkdirAll(filepath.Join(extDir(home, m.ID), "pkg"), 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Join(extDir(home, m.ID), "pkg"), ".install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	base, files := pinned(m)
	for rel, want := range files {
		if err := fetchPinned(ctx, home, base+rel, want, filepath.Join(tmp, filepath.FromSlash(rel))); err != nil {
			return fmt.Errorf("%s: %v", rel, err)
		}
	}
	for rel, b := range bundled {
		if _, dup := files[rel]; dup {
			return fmt.Errorf("%s is both pinned and bundled", rel)
		}
		if err := writePkgFile(filepath.Join(tmp, filepath.FromSlash(rel)), b); err != nil {
			return err
		}
	}
	if cmd, err := m.command(runtime.GOOS, runtime.GOARCH); err == nil {
		if err := os.Chmod(filepath.Join(tmp, filepath.FromSlash(cmd)), 0o755); err != nil {
			return fmt.Errorf("the package has no %s", cmd)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return err
	}
	b, _ := json.Marshal(extInstalled{Version: m.Version, Source: key, At: time.Now().Format(time.RFC3339)})
	return os.WriteFile(filepath.Join(extDir(home, m.ID), "install.json"), b, 0o600)
}

// fetchPinned downloads url into dst when its sha256 is want. A
// collie-hub: url is a marketplace package: read from the hub's copy when
// this is the hub, otherwise downloaded from the hub with this node's token.
func fetchPinned(ctx context.Context, home, url, want, dst string) error {
	if rest, ok := strings.CutPrefix(url, marketSourceHub); ok {
		local := filepath.Join(marketPackagesDir(home), filepath.FromSlash(rest))
		if b, err := os.ReadFile(local); err == nil {
			return writePinned(b, want, dst)
		}
		cfg, ok := loadNodeConfig()
		if !ok {
			return fmt.Errorf("not the hub, and not joined to one")
		}
		url = strings.TrimRight(cfg.Hub, "/") + "/api/super/ext-pkg/" + rest
		return fetchURL(ctx, url, "Bearer "+cfg.Token, want, dst)
	}
	return fetchURL(ctx, url, "", want, dst)
}

func fetchURL(ctx context.Context, url, auth, want, dst string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, extFileMost+1))
	if err != nil {
		return fmt.Errorf("download: %v", err)
	}
	if len(b) > extFileMost {
		return fmt.Errorf("larger than %d MB", extFileMost>>20)
	}
	return writePinned(b, want, dst)
}

func writePinned(b []byte, want, dst string) error {
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != strings.ToLower(want) {
		return fmt.Errorf("does not match its pinned sha256")
	}
	return writePkgFile(dst, b)
}

func writePkgFile(dst string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// findRequires looks for the commands m needs, where an agent's would be
// looked for too (a service's PATH is short): the folders they are in, and
// the ones not found.
func findRequires(m *ExtManifest) (dirs, missing []string) {
	home, _ := os.UserHomeDir()
	for _, c := range m.Requires {
		if p := findBin(c, home); p != "" {
			dirs = append(dirs, filepath.Dir(p))
		} else {
			missing = append(missing, c)
		}
	}
	return dirs, missing
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
