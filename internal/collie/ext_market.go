package collie

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Marketplaces (extensions/README.md §9): a GitHub repository, private or
// not, with collie-marketplace.json at its root listing the extensions in it.
// The hub reads the index, installs a package (a directory at a pinned
// commit, or an archive attached to a release, ext_market_pkg.go) into
// COLLIE_HOME/market/pkgs/<id>/<version>/ with every file's sha256, and the
// computer that runs it downloads the package from the hub (with its own
// device token) and checks each file. Only the hub holds the repository token.

const (
	marketIndexFile = "collie-marketplace.json"

	marketWait      = 2 * time.Minute
	marketTarMost   = 256 << 20
	marketSourceHub = "collie-hub:" // a package's source.base: from the hub's cache
)

// ExtMarketRef is where an installed extension came from.
type ExtMarketRef struct {
	Repo    string            `json:"repo"`
	Commit  string            `json:"commit"`
	Path    string            `json:"path,omitempty"`
	Package string            `json:"package,omitempty"` // release:<tag>/<file>
	Files   map[string]string `json:"files"`             // relative path → sha256
}

// ExtMarket is one marketplace you added (kv "ext_markets"; its token is
// in COLLIE_HOME/market/secrets.json).
type ExtMarket struct {
	Repo    string        `json:"repo"` // owner/name
	Ref     string        `json:"ref"`  // branch or tag, "" for the default branch
	Name    string        `json:"name,omitempty"`
	Commit  string        `json:"commit,omitempty"`
	Checked time.Time     `json:"checked,omitempty"`
	Error   string        `json:"error,omitempty"`
	Index   []MarketEntry `json:"index,omitempty"`
}

// MarketEntry is one line of collie-marketplace.json.
type MarketEntry struct {
	ID          string         `json:"id"`
	Path        string         `json:"path,omitempty"`    // a directory of the repository
	Package     *MarketPackage `json:"package,omitempty"` // or a built archive in a release
	Version     string         `json:"version"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
}

type marketIndex struct {
	Name       string        `json:"name"`
	Extensions []MarketEntry `json:"extensions"`
}

func marketDir(home string) string { return filepath.Join(home, "market") }

func validRepo(r string) bool {
	o, n, ok := strings.Cut(r, "/")
	return ok && o != "" && n != "" && !strings.ContainsAny(r, " ?#@:") && strings.Count(r, "/") == 1 && !strings.Contains(r, "..")
}

func (h *extHost) markets() []ExtMarket {
	var out []ExtMarket
	if raw := h.store.kv("ext_markets"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			log.Printf("ext: markets: %v", err)
		}
	}
	return out
}

func (h *extHost) saveMarkets(ms []ExtMarket) {
	b, _ := json.Marshal(ms)
	h.store.setKV("ext_markets", string(b))
}

func (h *extHost) marketTokens() map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(filepath.Join(marketDir(h.home), "secrets.json"))
	if err == nil {
		if err := json.Unmarshal(b, &out); err != nil {
			log.Printf("ext: market secrets: %v", err)
		}
	}
	return out
}

// addMarket adds or changes a marketplace; token "" keeps the one saved.
func (h *extHost) addMarket(ctx context.Context, repo, ref, token string) (ExtMarket, error) {
	if !validRepo(repo) {
		return ExtMarket{}, fmt.Errorf("仓库写成 owner/name")
	}
	h.marketMu.Lock()
	defer h.marketMu.Unlock()
	if token != "" {
		toks := h.marketTokens()
		toks[repo] = token
		if err := writePrivate(filepath.Join(marketDir(h.home), "secrets.json"), toks); err != nil {
			return ExtMarket{}, err
		}
	}
	ms := h.markets()
	i := indexOfMarket(ms, repo)
	if i < 0 {
		ms = append(ms, ExtMarket{Repo: repo})
		i = len(ms) - 1
	}
	ms[i].Ref = ref
	h.readIndex(ctx, &ms[i])
	h.saveMarkets(ms)
	if ms[i].Error != "" {
		return ms[i], fmt.Errorf("%s", ms[i].Error)
	}
	return ms[i], nil
}

func (h *extHost) removeMarket(repo string) {
	h.marketMu.Lock()
	defer h.marketMu.Unlock()
	ms := h.markets()
	if i := indexOfMarket(ms, repo); i >= 0 {
		ms = append(ms[:i], ms[i+1:]...)
	}
	h.saveMarkets(ms)
	toks := h.marketTokens()
	delete(toks, repo)
	if err := writePrivate(filepath.Join(marketDir(h.home), "secrets.json"), toks); err != nil {
		log.Printf("ext: market secrets: %v", err)
	}
}

func indexOfMarket(ms []ExtMarket, repo string) int {
	for i, m := range ms {
		if m.Repo == repo {
			return i
		}
	}
	return -1
}

// refreshMarkets reads every marketplace's index again.
func (h *extHost) refreshMarkets(ctx context.Context) []ExtMarket {
	h.marketMu.Lock()
	defer h.marketMu.Unlock()
	ms := h.markets()
	for i := range ms {
		h.readIndex(ctx, &ms[i])
	}
	h.saveMarkets(ms)
	return ms
}

// readIndex pins the ref to a commit and reads the index there.
func (h *extHost) readIndex(ctx context.Context, m *ExtMarket) {
	m.Checked, m.Error = time.Now(), ""
	sha, err := h.github(ctx, m.Repo, "/commits/"+refOrHead(m.Ref), "application/vnd.github.sha")
	if err != nil {
		m.Error = "读不到仓库：" + err.Error()
		return
	}
	commit := strings.TrimSpace(string(sha))
	raw, err := h.github(ctx, m.Repo, "/contents/"+marketIndexFile+"?ref="+commit, "application/vnd.github.raw")
	if err != nil {
		m.Error = "读不到 " + marketIndexFile + "：" + err.Error()
		return
	}
	var idx marketIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		m.Error = marketIndexFile + " 不是合法的 JSON"
		return
	}
	var ok []MarketEntry
	for _, e := range idx.Extensions {
		if validEntry(e) {
			ok = append(ok, e)
		}
	}
	sort.Slice(ok, func(i, j int) bool { return ok[i].ID < ok[j].ID })
	m.Name, m.Commit, m.Index = idx.Name, commit, ok
}

func refOrHead(ref string) string {
	if ref == "" {
		return "HEAD"
	}
	return ref
}

func cleanRel(p string) bool {
	c := path.Clean(p)
	return p != "" && c == p && !path.IsAbs(p) && c != "." && !strings.HasPrefix(c, "../")
}

// github calls the GitHub API for repo with the marketplace's token, if any.
func (h *extHost) github(ctx context.Context, repo, rest, accept string) ([]byte, error) {
	return h.githubLimit(ctx, repo, rest, accept, marketTarMost)
}

// githubLimit is github for an answer of at most most bytes. A redirect to
// another host (a release asset's storage) does not carry the token.
func (h *extHost) githubLimit(ctx context.Context, repo, rest, accept string, most int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, marketWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", marketAPI+"/repos/"+repo+rest, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	if tok := h.marketTokens()[repo]; tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if resp.StatusCode == 404 || resp.StatusCode == 401 {
			return nil, fmt.Errorf("%s（私有仓库要填 token）", resp.Status)
		}
		return nil, fmt.Errorf("%s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, most+1))
	if err == nil && int64(len(b)) > most {
		err = fmt.Errorf("超过 %d 字节", most)
	}
	return b, err
}

// installFromMarket installs (or updates) one extension of a marketplace at
// the commit its index was read at.
func (h *extHost) installFromMarket(ctx context.Context, repo, id string) (*ExtManifest, error) {
	h.marketMu.Lock()
	ms := h.markets()
	i := indexOfMarket(ms, repo)
	if i < 0 {
		h.marketMu.Unlock()
		return nil, fmt.Errorf("没有这个市场：%s", repo)
	}
	h.readIndex(ctx, &ms[i])
	h.saveMarkets(ms)
	mk := ms[i]
	h.marketMu.Unlock()
	if mk.Error != "" {
		return nil, fmt.Errorf("%s", mk.Error)
	}
	var e *MarketEntry
	for j := range mk.Index {
		if mk.Index[j].ID == id {
			e = &mk.Index[j]
		}
	}
	if e == nil {
		return nil, fmt.Errorf("%s 里没有 %s", repo, id)
	}
	if b := h.manifest(id); b != nil && b.Market == nil {
		return nil, fmt.Errorf("%s 和自带或本地的扩展同名", id)
	}
	base := filepath.Join(marketDir(h.home), "pkgs", id)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(base, ".install-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := h.fetchEntry(ctx, repo, mk.Commit, e, tmp); err != nil {
		return nil, err
	}
	m, err := readStrictManifest(filepath.Join(tmp, "collie-extension.json"))
	if err != nil {
		return nil, err
	}
	if m.ID != id || m.Version != e.Version {
		return nil, fmt.Errorf("清单是 %s %s，索引写的是 %s %s", m.ID, m.Version, id, e.Version)
	}
	files, err := hashFiles(tmp)
	if err != nil {
		return nil, err
	}
	m.Market = &ExtMarketRef{Repo: repo, Commit: mk.Commit, Path: e.Path, Files: files}
	if e.Package != nil {
		m.Market.Package = e.Package.URL
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	dir := filepath.Join(base, m.Version)
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return nil, err
	}
	if err := writePrivate(filepath.Join(dir, ".market.json"), m.Market); err != nil {
		return nil, err
	}
	h.adopt(m)
	return m, nil
}

// adopt puts a newly installed version in the catalog; one that is on
// restarts with it.
func (h *extHost) adopt(m *ExtManifest) {
	h.mu.Lock()
	h.cat[m.ID] = m
	on := h.sets[m.ID].Enabled
	h.mu.Unlock()
	if on {
		h.configure(m.ID, func(x *ExtSetting) { x.Enabled = false })
		h.configure(m.ID, func(x *ExtSetting) { x.Enabled = true })
	}
}

// uninstall stops it, forgets its settings and the hub's copy; with purge
// its data and sign-in on the computer it ran on go too.
func (h *extHost) uninstall(ctx context.Context, id string, purge bool) error {
	m := h.manifest(id)
	if m == nil || m.Market == nil {
		return fmt.Errorf("%s 不是从市场装的", id)
	}
	set := h.setting(id)
	if _, err := h.configure(id, func(x *ExtSetting) { x.Enabled = false }); err != nil {
		return err
	}
	if purge {
		if err := h.purge(ctx, m, set.Machine); err != nil {
			return fmt.Errorf("删那台电脑上的数据：%v", err)
		}
	}
	h.mu.Lock()
	delete(h.cat, id)
	delete(h.sets, id)
	h.saveSetsLocked()
	h.mu.Unlock()
	return os.RemoveAll(filepath.Join(marketDir(h.home), "pkgs", id))
}

// purge asks the computer to delete the extension's folder there.
func (h *extHost) purge(ctx context.Context, m *ExtManifest, machine string) error {
	conn, err := h.dial(ctx, machine)
	if err != nil {
		return err
	}
	defer conn.Close()
	hdr, _ := json.Marshal(extStartHeader{ID: m.ID, Manifest: m, Purge: true})
	if _, err := conn.Write(append(hdr, '\n')); err != nil {
		return err
	}
	line, err := readLineConn(conn)
	if err != nil {
		return err
	}
	if line != "purged" {
		return fmt.Errorf("%s", line)
	}
	return nil
}

func readLineConn(r io.Reader) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for len(b) < 4096 {
		if _, err := r.Read(one); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return string(b), nil
		}
		b = append(b, one[0])
	}
	return "", fmt.Errorf("answer too long")
}

// loadMarketInstalled reads what was installed from marketplaces.
func (h *extHost) loadMarketInstalled() {
	metas, _ := filepath.Glob(filepath.Join(marketDir(h.home), "pkgs", "*", "*", ".market.json"))
	for _, f := range metas {
		dir := filepath.Dir(f)
		m, err := readStrictManifest(filepath.Join(dir, "collie-extension.json"))
		if err != nil {
			log.Printf("ext: market %s: %v", dir, err)
			continue
		}
		var ref ExtMarketRef
		b, err := os.ReadFile(f)
		if err != nil || json.Unmarshal(b, &ref) != nil {
			continue
		}
		m.Market = &ref
		if m.validate() != nil {
			continue
		}
		h.mu.Lock()
		if old := h.cat[m.ID]; old == nil || (old.Market != nil && versionLess(old.Version, m.Version)) {
			h.cat[m.ID] = m
		}
		h.mu.Unlock()
	}
}

func readStrictManifest(f string) (*ExtManifest, error) {
	b, err := os.ReadFile(f)
	if err != nil {
		return nil, fmt.Errorf("包里没有 collie-extension.json")
	}
	m, err := decodeExtManifest(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if m.Local || m.Market != nil || m.Source != nil || m.Runtime.Builtin != "" {
		return nil, fmt.Errorf("市场里的扩展不能写 local、market、source 或 runtime.builtin")
	}
	if err := m.loadIcon(os.DirFS(filepath.Dir(f))); err != nil {
		return nil, err
	}
	return m, nil
}

// hashFiles: every regular file under dir, relative path → sha256.
func hashFiles(dir string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s 不是普通文件", p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = sha256Hex(b)
		return nil
	})
	return out, err
}

// marketAPI is GitHub's API (a test points it elsewhere).
var marketAPI = "https://api.github.com"
