package collie

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The workbench (工作台) is a page of modules the human pins: web pages
// (embedded when the site allows it, else a link card) and shortcuts to
// Collie's own pages; views from extensions come later. Collie holds no data
// of its own here. A module can be shared with agents: "read" lets Collie and
// the workers it started read the module's feed (`collie wb read`); "keep"
// also snapshots it into the router folder, where the dream distills it into
// memory/notes/wb-<id>.md.

const (
	wbKV = "workbench"

	wbKindWeb  = "web"
	wbKindLink = "link"

	wbShareOff  = "off"
	wbShareRead = "read"
	wbShareKeep = "keep"

	wbEmbedFrame = "iframe"
	wbEmbedTab   = "tab"

	wbMaxModules = 100
	wbMaxGroups  = 30
	wbMaxTitle   = 40
	wbMaxToken   = 4096
)

var wbIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// wbLinks are the Collie pages a shortcut module can open, as view:tab.
var wbLinks = map[string]bool{
	"auto:inbox": true, "auto:events": true, "auto:timers": true, "auto:memory": true, "auto:hooks": true,
	"devices:machines": true, "devices:connect": true,
}

type WBGroup struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type WBModule struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Icon  string `json:"icon,omitempty"` // one emoji or a short mark; "" = the default
	Group string `json:"group"`
	Kind  string `json:"kind"` // web | link
	// web: the page, how it opens, and where agents read its data.
	URL   string `json:"url,omitempty"` // link: a key of wbLinks
	Embed string `json:"embed,omitempty"`
	Feed  string `json:"feed,omitempty"`
	// Share: off | read | keep. Sensitive modules are never read for a
	// conversation with someone outside (an ext:* thread).
	Share     string `json:"share"`
	Sensitive bool   `json:"sensitive,omitempty"`
	Every     int    `json:"every,omitempty"` // hours between snapshots, 0 = daily
	// HubLogin: the page opens through the hub, which keeps its site's login
	// for every device (workbench_proxy.go).
	HubLogin bool `json:"hub_login,omitempty"`
}

type wbConfig struct {
	Groups  []WBGroup  `json:"groups"`
	Modules []WBModule `json:"modules"`
}

// workbench keeps the layout (kv "workbench" in herd.db), the feed tokens
// (COLLIE_HOME/wb/secrets.json, never sent back out) and the snapshots
// (router/wb/<id>/<date>.md).
type workbench struct {
	db      *sql.DB
	dir     string // COLLIE_HOME/wb
	router  string // the router folder: snapshots under wb/, notes under memory/notes/
	client  *http.Client
	saveMu  sync.Mutex // one layout save at a time, database and memory together
	mu      sync.Mutex // the fields below, and secrets.json
	cfg     wbConfig
	checked map[string]wbCheck // module → its last snapshot attempt

	jars map[string]*wbJar // module → its current proxy host and login, guarded by mu

	snapping  atomic.Bool
	lastCheck atomic.Int64 // unix seconds of the last due-check
}

func newWorkbench(db *sql.DB, home, routerDir string) *workbench {
	wb := &workbench{
		db: db, dir: filepath.Join(home, "wb"), router: routerDir,
		client:  &http.Client{Timeout: wbFetchTimeout},
		checked: map[string]wbCheck{},
		jars:    map[string]*wbJar{},
	}
	var raw string
	err := db.QueryRow(`SELECT v FROM kv WHERE k=?`, wbKV).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		wb.cfg = defaultWBConfig()
	case err != nil:
		log.Printf("workbench: load: %v", err)
		wb.cfg = defaultWBConfig()
	default:
		if err := json.Unmarshal([]byte(raw), &wb.cfg); err != nil {
			log.Printf("workbench: parse: %v", err)
			wb.cfg = defaultWBConfig()
		}
	}
	return wb
}

// defaultWBConfig is a new workbench: shortcuts to Collie's own pages, so it
// is useful before anything is pinned.
func defaultWBConfig() wbConfig {
	link := func(id, title, icon, target string) WBModule {
		return WBModule{ID: id, Title: title, Icon: icon, Group: "collie", Kind: wbKindLink, URL: target, Share: wbShareOff}
	}
	return wbConfig{
		Groups: []WBGroup{{ID: "fav", Title: "收藏"}, {ID: "collie", Title: "Collie"}},
		Modules: []WBModule{
			link("inbox", "收件箱", "", "auto:inbox"),
			link("timers", "定时", "", "auto:timers"),
			link("memory", "记忆", "", "auto:memory"),
			link("machines", "电脑", "", "devices:machines"),
		},
	}
}

func (wb *workbench) config() wbConfig {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	c := wbConfig{Groups: append([]WBGroup{}, wb.cfg.Groups...), Modules: append([]WBModule{}, wb.cfg.Modules...)}
	return c
}

func (wb *workbench) module(id string) (WBModule, bool) {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	return wb.moduleLocked(id)
}

func (wb *workbench) moduleLocked(id string) (WBModule, bool) {
	for _, m := range wb.cfg.Modules {
		if m.ID == id {
			return m, true
		}
	}
	return WBModule{}, false
}

// save validates and stores a whole layout. Modules that are gone lose their
// token, and their login when the hub no longer keeps it; purge also deletes their snapshots and notes.
func (wb *workbench) save(c wbConfig, purge []string) error {
	wb.saveMu.Lock()
	defer wb.saveMu.Unlock()
	if err := c.normalize(); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := wb.saveLayout(c, b); err != nil {
		return err
	}
	if err := wb.dropTokens(c); err != nil {
		return err
	}
	for _, id := range purge {
		if err := wb.purge(id); err != nil {
			return err
		}
	}
	return wb.writeIndex()
}

func (wb *workbench) saveLayout(c wbConfig, b []byte) error {
	if _, err := wb.db.Exec(`INSERT INTO kv(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, wbKV, string(b)); err != nil {
		return err
	}
	wb.mu.Lock()
	defer wb.mu.Unlock()
	if err := wb.dropJarsLocked(c); err != nil {
		return err
	}
	wb.cfg = c
	for id := range wb.checked {
		if !slices.ContainsFunc(c.Modules, func(m WBModule) bool { return m.ID == id && m.Share == wbShareKeep }) {
			delete(wb.checked, id)
		}
	}
	return nil
}

// normalize checks a layout from the page and fills in defaults.
func (c *wbConfig) normalize() error {
	if len(c.Groups) > wbMaxGroups || len(c.Modules) > wbMaxModules {
		return fmt.Errorf("最多 %d 个分组、%d 个模块", wbMaxGroups, wbMaxModules)
	}
	groups := map[string]bool{}
	for i := range c.Groups {
		g := &c.Groups[i]
		g.Title = strings.TrimSpace(g.Title)
		if !wbIDRe.MatchString(g.ID) || groups[g.ID] {
			return fmt.Errorf("分组 id %q 无效或重复", g.ID)
		}
		if g.Title == "" || len([]rune(g.Title)) > wbMaxTitle {
			return fmt.Errorf("分组名不能为空，最多 %d 个字", wbMaxTitle)
		}
		groups[g.ID] = true
	}
	ids := map[string]bool{}
	for i := range c.Modules {
		m := &c.Modules[i]
		m.Title, m.Icon = strings.TrimSpace(m.Title), strings.TrimSpace(m.Icon)
		if !wbIDRe.MatchString(m.ID) || ids[m.ID] {
			return fmt.Errorf("模块 id %q 无效或重复", m.ID)
		}
		ids[m.ID] = true
		if m.Title == "" || len([]rune(m.Title)) > wbMaxTitle {
			return fmt.Errorf("模块名不能为空，最多 %d 个字", wbMaxTitle)
		}
		if len([]rune(m.Icon)) > 4 {
			return fmt.Errorf("「%s」的图标最多 4 个字符", m.Title)
		}
		if !groups[m.Group] {
			return fmt.Errorf("「%s」的分组 %q 不存在", m.Title, m.Group)
		}
		switch m.Kind {
		case wbKindWeb:
			if !httpURL(m.URL) {
				return fmt.Errorf("「%s」的网址要以 http:// 或 https:// 开头", m.Title)
			}
			if m.Feed != "" && !httpURL(m.Feed) {
				return fmt.Errorf("「%s」的数据地址要以 http:// 或 https:// 开头", m.Title)
			}
			if m.Embed != wbEmbedFrame {
				m.Embed = wbEmbedTab
			}
		case wbKindLink:
			if !wbLinks[m.URL] {
				return fmt.Errorf("「%s」指向的页面 %q 不存在", m.Title, m.URL)
			}
			m.Embed, m.Feed, m.HubLogin = "", "", false
		default:
			return fmt.Errorf("「%s」的类型 %q 不支持", m.Title, m.Kind)
		}
		switch m.Share {
		case "":
			m.Share = wbShareOff
		case wbShareOff, wbShareRead, wbShareKeep:
		default:
			return fmt.Errorf("「%s」的共享档 %q 无效", m.Title, m.Share)
		}
		if m.Share != wbShareOff && m.Feed == "" {
			return fmt.Errorf("「%s」没有数据地址，不能给 Agent", m.Title)
		}
		if m.Every < 0 || m.Every > 24*7 {
			return fmt.Errorf("「%s」的快照间隔最多 168 小时（不填是 24）", m.Title)
		}
	}
	return nil
}

func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

// ---------------------------------------------------------------- secrets

func (wb *workbench) secretsPath() string { return filepath.Join(wb.dir, "secrets.json") }

func (wb *workbench) secrets() (map[string]string, error) {
	m := map[string]string{}
	b, err := os.ReadFile(wb.secretsPath())
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", wb.secretsPath(), err)
	}
	return m, nil
}

func (wb *workbench) writeSecrets(m map[string]string) error {
	return writePrivate(wb.secretsPath(), m)
}

// dropTokens forgets the tokens of modules no longer in c.
func (wb *workbench) dropTokens(c wbConfig) error {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	secrets, err := wb.secrets()
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, m := range c.Modules {
		keep[m.ID] = true
	}
	dropped := false
	for id := range secrets {
		if !keep[id] {
			delete(secrets, id)
			dropped = true
		}
	}
	if !dropped {
		return nil
	}
	return wb.writeSecrets(secrets)
}

func (wb *workbench) setToken(id, token string) error {
	if len(token) > wbMaxToken || strings.ContainsAny(token, "\r\n") {
		return errors.New("token 太长或含换行")
	}
	wb.mu.Lock()
	defer wb.mu.Unlock()
	if !slices.ContainsFunc(wb.cfg.Modules, func(m WBModule) bool { return m.ID == id }) {
		return fmt.Errorf("没有模块 %q", id)
	}
	m, err := wb.secrets()
	if err != nil {
		return err
	}
	if token == "" {
		delete(m, id)
	} else {
		m[id] = token
	}
	return wb.writeSecrets(m)
}

func (wb *workbench) token(id string) (string, error) {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	m, err := wb.secrets()
	return m[id], err
}

// ---------------------------------------------------------------- access

// routerExternalWindow: how far before the router's turn began an event it
// is handling may have arrived (it queued while the router was busy).
const routerExternalWindow = 30 * time.Minute

// wbCaller says who runs `collie wb` from a herdr pane: Collie itself or a
// worker it started, and whether that agent is serving an outside
// conversation. Any other agent is refused. pane "" is the human (or a
// script) holding the token.
func (s *Server) wbCaller(r *http.Request, pane string) (who string, external bool, err error) {
	if pane == "" {
		return wbHuman, false, nil
	}
	if _, d := s.identity(r); d != nil && d.Kind == "machine" {
		pane = join(pane, d.ID)
	}
	sp := s.sup
	if live, ok := sp.liveAgents(withCachedLists(r.Context())); ok {
		if a, ok := live[routerName]; ok && a.Pane == pane {
			sp.mu.Lock()
			defer sp.mu.Unlock()
			return routerName, sp.routerOnExternalLocked(), nil
		}
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	name := sp.workerByPaneLocked(pane)
	if name == "" {
		return "", false, errors.New("只有 Collie 和它派出的 worker 能读工作台")
	}
	w := sp.st.Workers[name]
	if strings.HasPrefix(w.Thread, extThreadPrefix) {
		return name, true, nil
	}
	for id, th := range sp.st.Threads {
		if strings.HasPrefix(id, extThreadPrefix) && slices.Contains(th.Subscribers, name) {
			return name, true, nil
		}
	}
	return name, false, nil
}

// routerOnExternalLocked: is the router's current turn about an outside
// conversation? Unsure counts as yes: it only withholds sensitive modules.
func (sp *Super) routerOnExternalLocked() bool {
	if sp.routerAskedAt.IsZero() {
		return false
	}
	since := sp.routerAskedAt.Add(-routerExternalWindow)
	for i := len(sp.st.Triggers) - 1; i >= 0; i-- {
		t := sp.st.Triggers[i]
		if t.At.Before(since) {
			break
		}
		if t.Status == "delivered" && strings.HasPrefix(t.Thread, extThreadPrefix) {
			return true
		}
	}
	return false
}

// wbHuman is the caller outside herdr: the human, or a script with the token.
const wbHuman = "human"

// wbAllowed: may this caller read module m?
func wbAllowed(m WBModule, human, external bool) error {
	if !human && m.Share == wbShareOff {
		return fmt.Errorf("「%s」没有共享给 Agent（在工作台里打开「给 Agent」）", m.Title)
	}
	if m.Sensitive && external {
		return fmt.Errorf("「%s」是敏感模块，不能在和外部的会话里读取", m.Title)
	}
	return nil
}
