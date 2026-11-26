package collie

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Each agent names its own session: claude puts the topic in the terminal
// title, codex keeps thread names in session_index.jsonl, antigravity keeps
// titles in conversation_summaries.db. Those beat names like "agy-1".
// The files live on the agent's machine, so every machine reads its own and
// the hub collects them from the nodes.

var genericTitles = map[string]bool{"": true, "claude": true, "claude code": true, "codex": true, "agy": true, "antigravity": true, "bash": true, "zsh": true}

// titleRunes caps a title by characters, not bytes, so a Chinese one isn't cut at a third.
const titleRunes = 60

// spinnerGlyph is what an agent puts before the topic in its terminal title to
// show it's busy or idle: braille spinner frames and claude's "✳"-style stars.
// A title's own leading emoji or bracket ("🟢 [下载] …") is part of the name.
func spinnerGlyph(r rune) bool {
	return unicode.IsSpace(r) || r >= 0x2800 && r <= 0x28FF || r >= 0x2722 && r <= 0x273D || r == '·' || r == '*'
}

// cleanTitle drops the spinner/status glyphs claude puts in front of the topic.
func cleanTitle(t string) string {
	t = strings.TrimSpace(strings.TrimLeftFunc(t, spinnerGlyph))
	if genericTitles[strings.ToLower(t)] {
		return ""
	}
	if r := []rune(t); len(r) > titleRunes {
		t = strings.TrimSpace(string(r[:titleRunes])) + " …"
	}
	return t
}

var codexNames struct {
	sync.Mutex
	mod    time.Time
	m      map[string]string
	byName map[string]string
}

func (s *Server) codexThreadName(id string) string {
	s.loadCodexNames()
	codexNames.Lock()
	defer codexNames.Unlock()
	return codexNames.m[id]
}

func (s *Server) codexIDByName(name string) string {
	s.loadCodexNames()
	codexNames.Lock()
	defer codexNames.Unlock()
	return codexNames.byName[name]
}

func (s *Server) loadCodexNames() {
	path := filepath.Join(envOr("CODEX_HOME", filepath.Join(s.home, ".codex")), "session_index.jsonl")
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	codexNames.Lock()
	defer codexNames.Unlock()
	if !fi.ModTime().Equal(codexNames.mod) {
		m, byName := map[string]string{}, map[string]string{}
		if f, err := os.Open(path); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			for sc.Scan() {
				var e struct {
					ID   string `json:"id"`
					Name string `json:"thread_name"`
				}
				if json.Unmarshal(sc.Bytes(), &e) == nil && e.ID != "" {
					m[e.ID] = e.Name // later lines are renames
					byName[e.Name] = e.ID
				}
			}
			f.Close()
		}
		codexNames.mod, codexNames.m, codexNames.byName = fi.ModTime(), m, byName
	}
}

var agyTitles struct {
	sync.Mutex
	at time.Time
	m  map[string]string
}

func (s *Server) agyTitle(id string) string {
	agyTitles.Lock()
	defer agyTitles.Unlock()
	if time.Since(agyTitles.at) > 5*time.Second {
		agyTitles.at = time.Now()
		m := map[string]string{}
		path := filepath.Join(s.agyCLIHome(), "conversation_summaries.db")
		if isFile(path) {
			if db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(2000)"); err == nil {
				if rows, err := db.Query(`SELECT conversation_id, title FROM conversation_summaries WHERE last_modified_time > ?`,
					time.Now().Add(-7*24*time.Hour).UTC().Format("2006-01-02 15:04:05")); err == nil {
					for rows.Next() {
						var cid, t string
						if rows.Scan(&cid, &t) == nil {
							m[cid] = t
						}
					}
					rows.Close()
				}
				db.Close()
			}
		}
		agyTitles.m = m
	}
	return agyTitles.m[id]
}

// paneSession is what a machine knows about one of its agents' sessions.
type paneSession struct {
	Title   string     `json:"title,omitempty"`
	Session string     `json:"session,omitempty"` // id to resume it by
	Run     *[2]string `json:"run,omitempty"`     // model, effort it was started with
	// Ran: the model and effort its latest reply used, from its own log. It
	// beats Run: the agent may have been started elsewhere, or switched since.
	Ran *[2]string `json:"ran,omitempty"`
	// Context: how full its context window is, from the same log.
	Context *ctxUsage `json:"context,omitempty"`
	// HumanAt: when the human last wrote in it, else when it started (humanAt).
	HumanAt time.Time `json:"human_at,omitzero"`
	// Archived: its agent's own archive has it (archived.go).
	Archived bool `json:"archived,omitempty"`
}

// localSessions maps this machine's agent panes (local ids) to their sessions.
func (s *Server) localSessions(ctx context.Context) map[string]paneSession {
	out := map[string]paneSession{}
	agents, err := s.herdr.Agents(ctx)
	if err != nil {
		return out
	}
	if panes, err := s.herdr.Panes(ctx); err == nil {
		alive := map[string]bool{}
		for _, p := range panes {
			alive[p.ID] = true
		}
		s.herdr.pruneRuns(alive, func(m string) bool { return m == "" })
		s.agy.prune(alive)
	}
	archived := s.archivedIDs()
	for _, a := range agents {
		local, machine := split(a.PaneID)
		if machine != "" {
			continue
		}
		sess := ""
		if a.Session != nil && !strings.ContainsAny(a.Session.Value, "/") {
			sess = a.Session.Value
		}
		kind := deref(a.Agent)
		var t string
		switch kind {
		case "codex":
			t = s.codexThreadName(sess)
		case "agy":
			sessKind, value := "", ""
			if a.Session != nil {
				sessKind, value = a.Session.Kind, a.Session.Value
			}
			_, sess = s.agyPaneSession(ctx, local, deref(a.Cwd), sessKind, value)
			t = s.agyTitle(sess)
		}
		if t = cleanTitle(t); t == "" {
			tt := a.TerminalTitle
			if kind == "codex" {
				// codex titles its terminal "<thread name> | <user>"
				if i := strings.LastIndex(tt, " | "); i > 0 {
					tt = tt[:i]
				}
				if sess == "" {
					sess = s.codexIDByName(strings.TrimSpace(tt))
				}
			}
			t = cleanTitle(tt)
		}
		run, _ := s.herdr.Run(local)
		ps := paneSession{Title: t, Session: sess, Run: run, Archived: archived[sess]}
		if sess != "" && (kind == "claude" || kind == "codex" || kind == "agy") {
			ps.HumanAt = humanAt(s.sessionLog(kind, sess), kind)
			if u := s.sessionUsage(kind, sess); u != nil {
				if u.Model != "" {
					ps.Ran = &[2]string{u.Model, u.Effort}
				}
				if u.Used > 0 {
					ps.Context = &ctxUsage{Used: u.Used, Window: u.Window}
				}
			}
		}
		if t != "" || sess != "" || run != nil || ps.Context != nil || !ps.HumanAt.IsZero() {
			out[local] = ps
		}
	}
	return out
}

// sessionLogs remembers where each session's log is, so the titles refresh
// doesn't search for it again every few seconds.
var sessionLogs sync.Map

// sessionLog is where a session's log is ("" when it can't be found).
func (s *Server) sessionLog(kind, session string) string {
	key := kind + "\x00" + session
	known, _ := sessionLogs.Load(key)
	path, _ := known.(string)
	if !isFile(path) {
		if path, _ = s.findTranscript(kind, "", session, "", ""); path == "" {
			return ""
		}
		sessionLogs.Store(key, path)
	}
	return path
}

// sessionUsage is how full a Claude, Codex or Antigravity session's context
// is, and for Claude and Antigravity the model and effort its latest reply used.
func (s *Server) sessionUsage(kind, session string) *ctxUsage {
	path := s.sessionLog(kind, session)
	if path == "" {
		return nil
	}
	u := contextUsage(path, kind, 0)
	if kind == "claude" {
		u.Window = s.claudeWindowFor(u.Model)
	}
	if u.Used == 0 && u.Model == "" {
		return nil
	}
	return &u
}

// getTitles: GET /api/titles?archived=<ids>; the hub names the ended
// sessions it remembers here, and hears which of them are archived.
func (s *Server) getTitles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"sessions": s.titlesFor("", r.Context()), "archived": s.archivedAmong(splitIDs(r.URL.Query().Get("archived")))})
}

// titlesFor returns the cached titles of one machine ("" = this one) and
// refreshes them in the background, so building the state never waits.
type titleCache struct {
	at      time.Time
	m       map[string]paneSession
	loading bool
	// archived: those of its ended sessions it says are archived (nil: it didn't say).
	archived map[string]bool
}

var titles = struct {
	sync.Mutex
	by map[string]*titleCache
}{by: map[string]*titleCache{}}

func (s *Server) titlesFor(machine string, ctx context.Context) map[string]paneSession {
	titles.Lock()
	c := titles.by[machine]
	if c == nil {
		c = &titleCache{}
		titles.by[machine] = c
	}
	stale := time.Since(c.at) > 4*time.Second && !c.loading
	if stale {
		c.loading = true
	}
	m := c.m
	titles.Unlock()
	if !stale {
		return m
	}
	load := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		var got map[string]paneSession
		var archived map[string]bool
		if machine == "" {
			got = s.localSessions(ctx)
		} else if client, err := s.herdr.Client(machine); err == nil {
			u := "http://node/api/titles?archived=" + url.QueryEscape(strings.Join(s.endedOn(machine), ","))
			req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
			if resp, err := client.Do(req); err == nil {
				var body struct {
					Sessions map[string]paneSession `json:"sessions"`
					Archived *[]string              `json:"archived"`
				}
				if json.NewDecoder(resp.Body).Decode(&body) == nil && body.Sessions != nil {
					got = body.Sessions
					if body.Archived != nil {
						archived = map[string]bool{}
						for _, id := range *body.Archived {
							archived[id] = true
						}
					}
				}
				resp.Body.Close()
			}
		}
		titles.Lock()
		c.loading, c.at = false, time.Now()
		if got != nil {
			c.m, c.archived = got, archived
		}
		titles.Unlock()
	}
	if m == nil {
		load() // first time: worth the wait
		titles.Lock()
		m = c.m
		titles.Unlock()
		return m
	}
	go load()
	return m
}

// freeName picks an unused handle like "claude-2" for an agent the human
// didn't name; the UI shows the session's title instead.
func (s *Server) freeName(ctx context.Context, kind string) string {
	base := kind
	if base == "antigravity" {
		base = "agy"
	}
	taken := map[string]bool{}
	if agents, err := s.herdr.Agents(ctx); err == nil {
		for _, a := range agents {
			taken[deref(a.Name)] = true
		}
	}
	for i := 1; ; i++ {
		if n := base + "-" + strconv.Itoa(i); !taken[n] {
			return n
		}
	}
}
