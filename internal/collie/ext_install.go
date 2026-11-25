package collie

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Installing a packaged extension (extensions/README.md §9): download each
// file of the package from its pinned address, check its sha256, then move
// the whole package into place. One file off and nothing is left behind.

const (
	extFileMost     = 16 << 20
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
	if m.Source == nil && !m.Local {
		return ""
	}
	return filepath.Join(extDir(home, m.ID), "pkg", m.Version)
}

func sourceKey(m *ExtManifest) string {
	names := make([]string, 0, len(m.Source.Files))
	for p := range m.Source.Files {
		names = append(names, p)
	}
	sort.Strings(names)
	h := sha256.New()
	io.WriteString(h, m.Source.Base+"\n")
	for _, p := range names {
		io.WriteString(h, m.Source.Files[p]+"  "+p+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ensureInstalled installs m on this computer unless that version is there.
func ensureInstalled(ctx context.Context, home string, m *ExtManifest) error {
	if m.Source == nil {
		return nil
	}
	dir, key := extPackageDir(home, m), sourceKey(m)
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
	for rel, want := range m.Source.Files {
		if err := fetchPinned(ctx, m.Source.Base+rel, want, filepath.Join(tmp, filepath.FromSlash(rel))); err != nil {
			return fmt.Errorf("%s: %v", rel, err)
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

// fetchPinned downloads url into dst when its sha256 is want.
func fetchPinned(ctx context.Context, url, want, dst string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
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
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != strings.ToLower(want) {
		return fmt.Errorf("does not match its pinned sha256")
	}
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
