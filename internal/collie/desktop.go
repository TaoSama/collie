package collie

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The desktop apps (Claude, Codex, Antigravity) keep their own lists of
// conversations. Collie reads those lists, so a conversation shows whether or
// not it runs right now, the way the app's own sidebar shows it: the running
// ones, and the rest of the last week. Each app is read its own way, read-only:
//
//   - Claude: the desktop app's session index, one JSON per session under
//     ~/Library/Application Support/Claude/claude-code-sessions (title,
//     directory, archived, which transcript it runs).
//   - Codex: the App's thread table in <CODEX_HOME>/state_<n>.sqlite (source
//     "vscode" is the App); a message goes through the App's local message bus,
//     so it runs in the App window that owns the thread.
//   - Antigravity: its conversation index, ~/.gemini/antigravity/conversation_summaries.db.
//
// What an app has archived is left out (archived.go).

// No age window: the apps list their old conversations too, so each app's
// newest ones are kept, however old.
const (
	desktopMax = 20 // per app
	// The Codex App lists every thread it has, so take more of them; the
	// threads its automations write to show on top of those.
	codexMax = 100
)

const (
	appClaude = "Claude 桌面版"
	appCodex  = "Codex 桌面版"
	appAgy    = "Antigravity 桌面版"
	appAgyCLI = "antigravity" // a CLI conversation the app lists too: named as herdr's CLI rows are
)

const (
	surfaceCLI     = "cli"
	surfaceDesktop = "desktop"
)

// desktopRow is one conversation as an app's index lists it.
type desktopRow struct {
	id, kind, app       string
	surface             string // External.Surface
	alias               string // External.Alias, when known from its config dir
	session, title, cwd string
	path, format        string
	created, updated    time.Time
	running             bool // the app says a turn is running (Codex, Antigravity)
	waiting             bool // stopped, with something still to come (an Antigravity timer)
	scheduled           bool // has an active scheduled task / automation
	pinned              bool // pinned in the app's own sidebar
}

var desktopCache struct {
	sync.Mutex
	at   time.Time
	rows []desktopRow
	app  bool // the Codex App is up
}

// desktopRows reads the apps' indexes, at most every 5 seconds.
func (s *Server) desktopRows() ([]desktopRow, bool) {
	desktopCache.Lock()
	defer desktopCache.Unlock()
	if time.Since(desktopCache.at) < 5*time.Second && desktopCache.rows != nil {
		return desktopCache.rows, desktopCache.app
	}
	var rows []desktopRow
	claudeDesk := s.claudeDesktopRows(s.claudeDesktopDirs())
	rows = append(rows, claudeDesk...)
	rows = append(rows, s.claudeCLIRows(claudeDesk)...)
	rows = append(rows, s.codexAppRows()...)
	rows = append(rows, s.agyDesktopRows()...)
	desktopCache.at, desktopCache.rows, desktopCache.app = time.Now(), rows, s.codexAppUp()
	return rows, desktopCache.app
}

func forgetDesktopRows() {
	desktopCache.Lock()
	desktopCache.at = time.Time{}
	desktopCache.Unlock()
}

// desktopSessions turns the apps' rows into sessions, joined with what runs
// now: Claude desktop processes (regs) and terminal agents already listed.
func (s *Server) desktopSessions(regs map[int]*claudeReg, listed map[string]bool, now time.Time) []*External {
	rows, appUp := s.desktopRows()
	bySession := map[string]*claudeReg{}
	for _, r := range regs {
		bySession[r.SessionID] = r
	}
	var out []*External
	for _, d := range rows {
		if listed[d.session] {
			continue
		}
		e := &External{ID: d.id, Kind: d.kind, Cwd: d.cwd, App: d.app, Surface: d.surface, Alias: d.alias, Started: d.created, Session: d.session, Title: d.title, Updated: d.updated, Scheduled: d.scheduled, Pinned: d.pinned, path: d.path, format: d.format, desktop: true}
		if e.path != "" {
			if fi, err := os.Stat(e.path); err == nil && fi.ModTime().After(e.Updated) {
				e.Updated = fi.ModTime()
			}
		}
		switch d.kind {
		case "claude":
			if r := bySession[d.session]; r != nil {
				e.reg, e.PID, e.Live = r, r.PID, true
				s.reachable(e)
			} else if d.surface == surfaceCLI {
				e.CanTakeOver = e.path != "" && e.Cwd != ""
				if e.CanTakeOver {
					e.CanSend, e.SendVia = true, "herdr"
				} else {
					e.ReadOnly = "找不到这个会话的目录或记录，没法继续"
				}
			} else {
				e.ReadOnly = "这个会话现在没在运行：在 Claude 桌面版里打开它，就能从这里发消息"
				e.CanTakeOver = e.path != "" && e.Cwd != ""
			}
		case "codex":
			e.Live = d.running
			if d.running {
				e.Status = "working"
			}
			switch {
			case s.codexDaemonHas(d.session):
				// Loaded in Codex's shared background service (the App or a
				// CLI client connected to it).
				e.CanSend = true
			case d.surface == surfaceCLI:
				if d.running {
					e.ReadOnly = "这个会话正在别处运行，未接入 Codex 共享后台，无法排队发消息"
				} else {
					e.CanTakeOver = e.Session != "" && e.Cwd != ""
					if e.CanTakeOver {
						e.CanSend, e.SendVia = true, "herdr"
					} else {
						e.ReadOnly = "找不到这个会话的目录，没法在 herdr 里继续"
					}
				}
			case appUp:
				e.CanSend, e.SendVia = true, "app"
			case isCodexApp(d.app) && s.codexDaemonSock() != "":
				// Queue through the shared daemon: the App's own app-server
				// picks it up directly if it holds the thread, otherwise we
				// wake the App (or fall back to the daemon).
				e.CanSend, e.SendVia = true, "queue"
				// The CLI shares ~/.codex with the App: `codex resume` picks
				// the thread up in a herdr pane too.
				e.CanTakeOver = !d.running && e.Session != "" && e.Cwd != ""
			case isCodexApp(d.app):
				e.ReadOnly = "这台电脑上没有 Codex 后台：在 Codex 里接着说，或在 herdr 里接管"
				e.CanTakeOver = !d.running && e.Session != "" && e.Cwd != ""
			default:
				e.ReadOnly = "这个对话现在没有在 Codex 里打开：在 Codex App 里点开它，就能从这里发消息"
			}
		case "agy":
			e.Live = d.running || d.waiting
			if d.running {
				e.Status = "working"
			} else if d.waiting {
				e.Status = "waiting"
			}
			// `agy --conversation` resumes the CLI's own conversations, which
			// the App lists too; the App's it can't find. Not while a turn
			// runs: there is no process here to stop first.
			e.CanTakeOver = d.surface == surfaceCLI && e.Session != "" && e.Cwd != "" && !e.Live
			// The App's language server only loads conversations from its own
			// store (~/.gemini/antigravity), not the CLI's (~/.gemini/antigravity-cli).
			switch {
			case d.surface == surfaceCLI:
				if e.Live {
					e.ReadOnly = "这个会话正在别处运行，Antigravity CLI 不支持排队发消息"
				} else if e.CanTakeOver {
					e.CanSend, e.SendVia = true, "herdr"
				} else {
					e.ReadOnly = "这个会话现在没在运行"
				}
			case agyDesktopServer() != nil:
				e.CanSend, e.SendVia = true, "app"
			default:
				e.ReadOnly = "Antigravity 没有打开：打开它之后就能从这里发消息"
			}
		}
		out = append(out, e)
	}
	// Desktop processes the index does not know yet (just created) still show.
	for pid, r := range regs {
		if !strings.HasPrefix(r.Entrypoint, "claude-desktop") || listed[r.SessionID] || rowsHave(rows, r.SessionID) {
			continue
		}
		e := &External{ID: fmt.Sprintf("claude-%d", pid), Kind: "claude", PID: pid, Cwd: r.Cwd, App: appClaude, Surface: surfaceDesktop, Session: r.SessionID, reg: r, Live: true, desktop: true, Started: now}
		if path, format := s.findTranscript("claude", "id", r.SessionID, "", ""); path != "" {
			e.path, e.format = path, format
			if fi, err := os.Stat(path); err == nil {
				e.Updated = fi.ModTime()
			}
			e.Title = firstUserLine(path, format)
		}
		s.reachable(e)
		out = append(out, e)
	}
	return out
}

func rowsHave(rows []desktopRow, session string) bool {
	for _, d := range rows {
		if d.session == session {
			return true
		}
	}
	return false
}

// firstUserLine is a conversation's first message, as a title. It reads from
// the top only until that message (a session's log can be tens of MB, and the
// session list asks for every running one every few seconds), and remembers it.
var firstUserCache = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

const titleScanBytes = 8 << 20

func firstUserLine(path, format string) string {
	if compressedCodex(path, format) {
		return codexSummary(path).title
	}
	firstUserCache.Lock()
	t, ok := firstUserCache.m[path]
	firstUserCache.Unlock()
	if ok {
		return t
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	tr := newTranscript(path, format)
	r := bufio.NewReaderSize(f, 1<<20)
	read := 0
	for read < titleScanBytes && t == "" {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break
		}
		read += len(line)
		tr.line(line)
		for _, m := range tr.msgs {
			if m.Kind == "user" && strings.TrimSpace(m.Text) != "" && !strings.HasPrefix(m.Text, "[collie]") {
				t = messageTitle(m.Text)
				break
			}
		}
		tr.msgs = tr.msgs[:0]
	}
	if t != "" {
		firstUserCache.Lock()
		if len(firstUserCache.m) > 2000 {
			firstUserCache.m = map[string]string{}
		}
		firstUserCache.m[path] = t
		firstUserCache.Unlock()
	}
	return t
}

// ---------------------------------------------------------------- Claude

// claudeDesktopDirs are the desktop apps' data directories (the build on a
// third-party provider keeps its own). Only macOS's layout is known.
func (s *Server) claudeDesktopDirs() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	base := filepath.Join(s.home, "Library", "Application Support")
	return []string{filepath.Join(base, "Claude"), filepath.Join(base, "Claude-3p")}
}

type claudeDesktopJSON struct {
	SessionID       string `json:"sessionId"`
	CliSessionID    string `json:"cliSessionId"`
	Cwd             string `json:"cwd"`
	Title           string `json:"title"`
	Archived        bool   `json:"isArchived"`
	ScheduledTaskID string `json:"scheduledTaskId"`
	Created         int64  `json:"createdAt"`
	Activity        int64  `json:"lastActivityAt"`
}

// Index files are read again only when they change.
var claudeDesktopFiles = struct {
	sync.Mutex
	m map[string]struct {
		mod time.Time
		v   claudeDesktopJSON
	}
}{m: map[string]struct {
	mod time.Time
	v   claudeDesktopJSON
}{}}

func (s *Server) claudeDesktopRows(dirs []string) []desktopRow {
	var rows []desktopRow
	claudeDesktopFiles.Lock()
	defer claudeDesktopFiles.Unlock()
	for _, dir := range dirs {
		files, _ := filepath.Glob(filepath.Join(dir, "claude-code-sessions", "*", "*", "local_*.json"))
		for _, f := range files {
			fi, err := os.Stat(f)
			if err != nil {
				continue
			}
			c, ok := claudeDesktopFiles.m[f]
			if !ok || !c.mod.Equal(fi.ModTime()) {
				c.mod = fi.ModTime()
				c.v = claudeDesktopJSON{}
				if b, err := os.ReadFile(f); err == nil {
					json.Unmarshal(b, &c.v)
				}
				claudeDesktopFiles.m[f] = c
			}
			v := c.v
			last := time.UnixMilli(v.Activity)
			if v.Activity == 0 {
				last = fi.ModTime()
			}
			if v.CliSessionID == "" || v.Archived {
				continue
			}
			local := strings.TrimPrefix(v.SessionID, "local_")
			if local == "" {
				local = v.CliSessionID
			}
			d := desktopRow{id: "cd-" + local, kind: "claude", app: appClaude, surface: surfaceDesktop, session: v.CliSessionID, title: v.Title, cwd: v.Cwd, created: time.UnixMilli(v.Created), updated: last, scheduled: v.ScheduledTaskID != ""}
			rows = append(rows, d)
		}
	}
	rows = newestRows(rows)
	for i := range rows {
		d := &rows[i]
		d.path, d.format = s.findTranscript(d.kind, "id", d.session, "", "")
	}
	return rows
}

func newestRows(rows []desktopRow) []desktopRow {
	sort.Slice(rows, func(i, j int) bool { return rows[i].updated.After(rows[j].updated) })
	if len(rows) > desktopMax {
		rows = rows[:desktopMax]
	}
	return rows
}

type claudeCLIMeta struct {
	mod     time.Time
	size    int64
	cwd     string
	title   string
	created time.Time
	collie  bool
}

var claudeCLIFiles = struct {
	sync.Mutex
	m map[string]claudeCLIMeta
}{m: map[string]claudeCLIMeta{}}

// claudeCLIRows lists recent Claude Code CLI sessions outside herdr, skipping
// those already listed by Claude Desktop and Collie's own workers.
func (s *Server) claudeCLIRows(desktop []desktopRow) []desktopRow {
	seen := map[string]bool{}
	for _, d := range desktop {
		seen[d.session] = true
	}
	type rootAlias struct{ dir, alias string }
	roots := []rootAlias{{dir: filepath.Clean(claudeRoot(s.home))}}
	seenDir := map[string]bool{roots[0].dir: true}
	if snap, ok := s.aliases.get(""); ok {
		for _, a := range snap.rep.Aliases {
			if a.Kind != "claude" {
				continue
			}
			if raw := a.rawEnv["CLAUDE_CONFIG_DIR"]; raw != "" && !shellOnly(raw) {
				dir := filepath.Clean(expandTilde(raw, s.home))
				if !seenDir[dir] {
					seenDir[dir] = true
					roots = append(roots, rootAlias{dir: dir, alias: a.Name})
				}
			}
		}
	}
	type cand struct {
		path, session, alias string
		mod                  time.Time
		size                 int64
	}
	var cands []cand
	for _, r := range roots {
		files, _ := filepath.Glob(filepath.Join(r.dir, "projects", "*", "*.jsonl"))
		for _, f := range files {
			id := strings.TrimSuffix(filepath.Base(f), ".jsonl")
			if !conversationRe.MatchString(id) || seen[id] {
				continue
			}
			fi, err := os.Stat(f)
			if err != nil || fi.IsDir() || fi.Size() == 0 {
				continue
			}
			cands = append(cands, cand{path: f, session: id, alias: r.alias, mod: fi.ModTime(), size: fi.Size()})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	var rows []desktopRow
	for _, c := range cands {
		if len(rows) == desktopMax {
			break
		}
		if seen[c.session] {
			continue
		}
		meta := readClaudeCLIMeta(c.path, c.mod, c.size)
		if meta.collie || meta.cwd == "" || meta.title == "" {
			continue
		}
		seen[c.session] = true
		created := meta.created
		if created.IsZero() {
			created = c.mod
		}
		rows = append(rows, desktopRow{
			id: "cc-" + c.session, kind: "claude", app: "claude", surface: surfaceCLI, alias: c.alias,
			session: c.session, title: meta.title, cwd: meta.cwd, path: c.path, format: "claude",
			created: created, updated: c.mod,
		})
	}
	return rows
}

func readClaudeCLIMeta(path string, mod time.Time, size int64) claudeCLIMeta {
	claudeCLIFiles.Lock()
	if m, ok := claudeCLIFiles.m[path]; ok && m.mod.Equal(mod) && m.size == size {
		claudeCLIFiles.Unlock()
		return m
	}
	claudeCLIFiles.Unlock()
	m := claudeCLIMeta{mod: mod, size: size}
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	tr := newTranscript(path, "claude")
	r := bufio.NewReaderSize(f, 1<<20)
	read := 0
	for read < titleScanBytes && (m.cwd == "" || (m.title == "" && !m.collie)) {
		line, err := r.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			break
		}
		read += len(line)
		if m.cwd == "" || m.created.IsZero() {
			var h struct {
				Cwd       string `json:"cwd"`
				Timestamp string `json:"timestamp"`
			}
			if json.Unmarshal(line, &h) == nil {
				if m.cwd == "" && h.Cwd != "" {
					m.cwd = h.Cwd
				}
				if m.created.IsZero() && h.Timestamp != "" {
					if t, err := time.Parse(time.RFC3339Nano, h.Timestamp); err == nil {
						m.created = t
					}
				}
			}
		}
		if m.title == "" && !m.collie {
			tr.line(line)
			for _, msg := range tr.msgs {
				if msg.Kind == "user" && strings.TrimSpace(msg.Text) != "" {
					if collieSession(msg.Text) {
						m.collie = true
					} else {
						m.title = messageTitle(msg.Text)
					}
					break
				}
			}
			tr.msgs = tr.msgs[:0]
		}
		if err != nil {
			break
		}
	}
	claudeCLIFiles.Lock()
	if len(claudeCLIFiles.m) > 2000 {
		claudeCLIFiles.m = map[string]claudeCLIMeta{}
	}
	claudeCLIFiles.m[path] = m
	claudeCLIFiles.Unlock()
	return m
}

// ---------------------------------------------------------------- Codex App

// codexStateDB is the newest of the App's state databases (its number goes up with its schema).
func (s *Server) codexStateDB() string {
	paths, _ := filepath.Glob(filepath.Join(s.codexHome(), "state_*.sqlite"))
	best, bestN := "", -1
	for _, p := range paths {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "state_"), ".sqlite"))
		if err == nil && n > bestN {
			best, bestN = p, n
		}
	}
	return best
}

func (s *Server) codexAppRows() []desktopRow {
	path := s.codexStateDB()
	if path == "" {
		return nil
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil
	}
	defer db.Close()
	automation := map[string]bool{}
	for _, id := range s.codexAutomationThreads() {
		automation[id] = true
	}
	sectionCol := "''"
	var hasSection int
	if db.QueryRow(`SELECT count(*) FROM pragma_table_info('threads') WHERE name = 'thread_section_id'`).Scan(&hasSection) == nil && hasSection > 0 {
		sectionCol = "COALESCE(thread_section_id, '')"
	}
	// No LIMIT: Collie's own workers are told apart in Go below, and they
	// mustn't take the places of the App's threads; nor may the newer
	// threads push out an automation's or a pinned thread.
	q := `SELECT id, COALESCE(NULLIF(name,''), NULLIF(title,''), first_user_message), COALESCE(first_user_message, ''), cwd, rollout_path,
		COALESCE(created_at_ms, created_at*1000), COALESCE(updated_at_ms, updated_at*1000), COALESCE(originator, ''), ` + sectionCol + `, COALESCE(source, '')
		FROM threads WHERE archived = 0 AND source IN ('vscode', 'cli') ORDER BY updated_at DESC`
	res, err := db.Query(q)
	if err != nil {
		return nil
	}
	defer res.Close()
	var rows []desktopRow
	kept := 0
	for res.Next() {
		var id, title, first, cwd, rollout, originator, sectionID, source string
		var created, updated int64
		if res.Scan(&id, &title, &first, &cwd, &rollout, &created, &updated, &originator, &sectionID, &source) != nil {
			continue
		}
		// A Collie worker's thread, named by Codex after its task: listed
		// as Collie's, not as the App's.
		if collieSession(first) || strings.TrimSpace(title) == "" {
			continue
		}
		scheduled := automation[id]
		pinned := sectionID == codexPinnedSectionID
		if !scheduled && !pinned {
			if kept == codexMax {
				continue
			}
			kept++
		}
		app, surface := codexClient(originator, cwd), codexSurface(originator)
		if source == "cli" {
			app, surface = "codex", surfaceCLI
		}
		d := desktopRow{id: "cx-" + id, kind: "codex", app: app, surface: surface, session: id, title: messageTitle(title), cwd: cwd, created: time.UnixMilli(created), updated: time.UnixMilli(updated), scheduled: scheduled, pinned: pinned}
		if p := codexRollout(rollout); p != "" {
			d.path, d.format = p, "codex"
			d.running = codexTurnRunning(p)
		}
		rows = append(rows, d)
	}
	return rows
}

// codexAutomationThreads: the threads the App's active automations write to,
// <CODEX_HOME>/automations/<id>/automation.toml's target_thread_id. However far
// down the list its thread is, it belongs there.
func (s *Server) codexAutomationThreads() []string {
	paths, _ := filepath.Glob(filepath.Join(s.codexHome(), "automations", "*", "automation.toml"))
	var ids []string
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var status, thread string
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "[") {
				break // the top-level keys end at the first table
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			switch strings.TrimSpace(k) {
			case "status":
				status = tomlString(v)
			case "target_thread_id":
				thread = tomlString(v)
			}
		}
		if status == "ACTIVE" && thread != "" {
			ids = append(ids, thread)
		}
	}
	return ids
}

// codexTurnRunning: the rollout's last turn has started and not ended. A turn
// silent for 20 minutes is taken as over (the App may have died mid-turn).
// A long turn's start can sit megabytes back, so read backwards a chunk at a
// time until a start or an end turns up.
const codexRunningAge = 20 * time.Minute

func codexTurnRunning(path string) bool {
	if compressedCodex(path, "codex") {
		fi, err := os.Stat(path)
		return err == nil && time.Since(fi.ModTime()) <= codexRunningAge && codexSummary(path).running
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || time.Since(fi.ModTime()) > codexRunningAge {
		return false
	}
	return lastTurnOpen(f, fi.Size())
}

func lastTurnOpen(r io.ReaderAt, size int64) bool {
	const chunk, overlap = 512 << 10, 64
	for end := size; end > 0; end -= chunk - overlap {
		off := max(end-chunk, 0)
		b := make([]byte, end-off)
		if _, err := r.ReadAt(b, off); err != nil && err != io.EOF {
			return false
		}
		started, ended := codexTurnMarkers(b)
		if started >= 0 || ended >= 0 {
			return started > ended
		}
		if off == 0 {
			break
		}
	}
	return false
}

func codexTurnMarkers(b []byte) (started, ended int) {
	started = bytes.LastIndex(b, []byte(`"type":"task_started"`))
	ended = max(bytes.LastIndex(b, []byte(`"type":"task_complete"`)), bytes.LastIndex(b, []byte(`"type":"turn_aborted"`)))
	return started, ended
}

// The Codex App's local message bus: its windows ask each other to run a turn
// in a thread one of them owns; Collie joins as one more client. Frames are a
// 4-byte little-endian length and a JSON object; each request carries the
// version the App expects for its method.
func (s *Server) codexAppSock() string { return filepath.Join(s.codexHome(), "ipc", "ipc.sock") }

func (s *Server) codexAppUp() bool {
	c, err := net.DialTimeout("unix", s.codexAppSock(), 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

var codexAppVersions = map[string]int{"thread-owner-discovery": 1, "thread-follower-start-turn": 2, "thread-follower-steer-turn": 1}

type codexAppFrame struct {
	Type            string          `json:"type"`
	RequestID       string          `json:"requestId,omitempty"`
	Method          string          `json:"method,omitempty"`
	ResultType      string          `json:"resultType,omitempty"`
	Error           string          `json:"error,omitempty"`
	HandledByClient string          `json:"handledByClientId,omitempty"`
	Params          any             `json:"params,omitempty"`
	Version         int             `json:"version,omitempty"`
	TargetClientID  string          `json:"targetClientId,omitempty"`
	Response        any             `json:"response,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
}

type codexApp struct{ conn net.Conn }

func (c *codexApp) write(f codexAppFrame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	buf := make([]byte, 4+len(b))
	binary.LittleEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = c.conn.Write(buf)
	return err
}

func (c *codexApp) read() (codexAppFrame, error) {
	var f codexAppFrame
	var head [4]byte
	if _, err := io.ReadFull(c.conn, head[:]); err != nil {
		return f, err
	}
	n := binary.LittleEndian.Uint32(head[:])
	if n == 0 || n > 64<<20 {
		return f, fmt.Errorf("Codex App 消息格式不对（%d 字节）", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c.conn, b); err != nil {
		return f, err
	}
	return f, json.Unmarshal(b, &f)
}

// request sends one request and waits for its answer. When the bus asks
// whether Collie handles someone else's request, it says no at once.
func (c *codexApp) request(method string, params any, target string) (codexAppFrame, error) {
	id := uuid4()
	if err := c.write(codexAppFrame{Type: "request", RequestID: id, Method: method, Params: params, Version: codexAppVersions[method], TargetClientID: target}); err != nil {
		return codexAppFrame{}, err
	}
	for {
		f, err := c.read()
		if err != nil {
			return f, err
		}
		switch f.Type {
		case "client-discovery-request":
			if err := c.write(codexAppFrame{Type: "client-discovery-response", RequestID: f.RequestID, Response: map[string]any{"canHandle": false}}); err != nil {
				return f, err
			}
		case "response":
			if f.RequestID != id {
				continue
			}
			if f.ResultType != "success" {
				return f, fmt.Errorf("Codex App %s: %s", method, f.Error)
			}
			return f, nil
		}
	}
}

var errCodexNotLoaded = errors.New("Codex App 没能打开这个对话：在电脑上的 Codex App 里点开它一次再发")

func (c *codexApp) owner(thread string) (string, error) {
	f, err := c.request("thread-owner-discovery", map[string]any{"hostId": "local", "conversationId": thread}, "")
	if err == nil && f.HandledByClient == "" {
		err = errCodexNotLoaded
	}
	return f.HandledByClient, err
}

// codexAppSend runs a turn in the App window that owns the thread. A thread
// the App has not loaded is opened in it first (the App comes forward).
func (s *Server) codexAppSend(ctx context.Context, thread, text string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", s.codexAppSock())
	if err != nil {
		return errors.New("连不上电脑上的 Codex App：它可能没有打开")
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	c := &codexApp{conn: conn}
	if _, err := c.request("initialize", map[string]any{"clientType": "collie"}, ""); err != nil {
		return err
	}
	owner, err := c.owner(thread)
	if err != nil && exec.Command("open", "codex://threads/"+thread).Run() == nil {
		for deadline := time.Now().Add(30 * time.Second); err != nil && time.Now().Before(deadline) && ctx.Err() == nil; {
			time.Sleep(time.Second)
			owner, err = c.owner(thread)
		}
	}
	if err != nil {
		return errCodexNotLoaded
	}
	turn := map[string]any{
		"conversationId": thread,
		"turnStart": map[string]any{
			"request": map[string]any{"threadId": thread, "clientUserMessageId": uuid4(), "input": []map[string]any{{"type": "text", "text": text, "text_elements": []any{}}}},
			"context": map[string]any{},
		},
	}
	_, err = c.request("thread-follower-start-turn", turn, owner)
	forgetDesktopRows()
	return err
}

// ---------------------------------------------------------------- Antigravity

func (s *Server) agyDesktopRows() []desktopRow {
	root := filepath.Join(s.home, ".gemini", "antigravity")
	path := filepath.Join(root, "conversation_summaries.db")
	// The App's index lists the CLI's conversations too. Without the App
	// (Linux), the CLI's own index lists them, all the CLI's.
	cliOnly := false
	if !isFile(path) {
		path, cliOnly = filepath.Join(s.agyCLIHome(), "conversation_summaries.db"), true
		if !isFile(path) {
			return nil
		}
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil
	}
	defer db.Close()
	// not_fully_idle: the turn is over but the agent will be back on its own
	// (it set a timer, or a command runs in the background).
	// source names the client that started a conversation: the app lists the
	// CLI's conversations too. Older indexes may lack the column.
	// No LIMIT: archived conversations (whichever index says so) are left
	// out below, and mustn't take the places of the others.
	// A conversation the app failed to create stays in the index with no
	// steps and a zero last_modified_time (0001-01-01). Older indexes may
	// lack step_count.
	hasColumn := func(name string) bool {
		var n int
		return db.QueryRow(`SELECT count(*) FROM pragma_table_info('conversation_summaries') WHERE name = ?`, name).Scan(&n) == nil && n > 0
	}
	source, steps := "''", ""
	if hasColumn("source") {
		source = "source"
	}
	if hasColumn("step_count") {
		steps = " AND step_count > 0"
	}
	res, err := db.Query(`SELECT conversation_id, title, preview, workspace_uris, status, not_fully_idle, last_modified_time, last_user_input_time, ` + source + `
		FROM conversation_summaries WHERE nesting_depth = 0 AND killed = 0 AND last_modified_time > '0001-12-31'` + steps + `
		ORDER BY last_modified_time DESC`)
	if err != nil {
		return nil
	}
	defer res.Close()
	archived := s.archivedIDs()
	var agySched map[string]bool
	if !cliOnly {
		agySched = s.agyScheduledIDs()
	}
	var rows []desktopRow
	for res.Next() {
		var id, title, preview, uris, status, src string
		var notIdle bool
		var mod, input any
		if res.Scan(&id, &title, &preview, &uris, &status, &notIdle, &mod, &input, &src) != nil || archived[id] {
			continue
		}
		if len(rows) == desktopMax {
			break
		}
		if title == "" {
			title = preview
		}
		var ws []string
		json.Unmarshal([]byte(uris), &ws)
		cwd := ""
		if len(ws) > 0 {
			cwd = strings.TrimSuffix(strings.TrimPrefix(ws[0], "file://"), "/")
		}
		app, surface := appAgy, agySurface(src)
		if cliOnly {
			surface = surfaceCLI // the CLI leaves source empty in its own index
		}
		if surface == surfaceCLI {
			app = appAgyCLI
		}
		d := desktopRow{id: "ag-" + id, kind: "agy", app: app, surface: surface, session: id, title: messageTitle(title), cwd: cwd,
			created: sqlTime(input), updated: sqlTime(mod), running: status == "CASCADE_RUN_STATUS_RUNNING"}
		if surface != surfaceCLI {
			d.scheduled = agySched[id]
			d.pinned = agyPinned(root, id)
		}
		d.waiting = !d.running && notIdle
		// A conversation the CLI started keeps its log in the CLI's home.
		for _, r := range slices.Compact([]string{root, s.agyCLIHome()}) {
			if f := filepath.Join(r, "brain", id, agyTranscriptRelPath); isFile(f) {
				d.path, d.format = f, "agy"
				break
			}
		}
		// The CLI's own index can keep a row whose conversation is gone;
		// nothing would open or resume it.
		if cliOnly && d.path == "" {
			continue
		}
		rows = append(rows, d)
	}
	return rows
}

// sqlTime reads a datetime column, which the driver may hand back as a time or as text.
func sqlTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		for _, layout := range []string{"2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05"} {
			if p, err := time.Parse(layout, t); err == nil {
				return p
			}
		}
	}
	return time.Time{}
}

// leadingTag is a block of context a tool put before the message itself
// (<persona-context>…</persona-context> and the like).
var leadingTag = regexp.MustCompile(`(?s)^\s*<([A-Za-z][\w:-]*)[^>]*>.*?</([A-Za-z][\w:-]*)>`)

// messageTitle is a conversation's title as one short line, without the context
// blocks some tools put before the message.
var imageMark = regexp.MustCompile(`\[image(:[^\]\s]*)?\]\s*`)

func messageTitle(t string) string {
	t = imageMark.ReplaceAllString(t, "") // a picture says nothing as a title
	for i := 0; i < 4; i++ {
		m := leadingTag.FindStringSubmatchIndex(t)
		if m == nil || t[m[2]:m[3]] != t[m[4]:m[5]] {
			break
		}
		t = t[m[1]:]
	}
	return clip(oneLine(t), 80)
}

// codexDaemonHas: Codex's shared background service has the thread loaded.
func (s *Server) codexDaemonHas(thread string) bool {
	ids, err := s.codexLoaded()
	return err == nil && ids[thread]
}

const appCodexRemote = "Codex App（远程）"

func isCodexApp(app string) bool { return app == appCodex || app == appCodexRemote }

// codexClient names what started a "vscode" thread: the Codex App, or another
// program driving Codex's app-server.
func codexClient(originator, cwd string) string {
	// The Mac's shared Codex service stamps every thread it runs with the
	// program that started the service, so "persona" says nothing about who
	// asked: these are the App's.
	if originator == "persona" {
		originator = ""
	}
	switch originator {
	case "", "Codex Desktop", "codex_work_desktop":
		if runtime.GOOS != "darwin" {
			return appCodexRemote // the App on another computer opened it here over SSH
		}
		return appCodex
	case "codex-tui", "codex_cli_rs":
		return "codex" // the CLI: named as herdr's CLI rows are
	}
	return originator + "（Codex）"
}

// codexSurface is the client a thread's originator names: the App (as
// codexClient counts it) or the command line; `codex exec` and other
// programs on the app-server say neither.
func codexSurface(originator string) string {
	switch {
	case isCodexApp(codexClient(originator, "")):
		return surfaceDesktop
	case originator == "codex-tui" || originator == "codex_cli_rs":
		return surfaceCLI
	}
	return ""
}

// agySurface is the client an Antigravity conversation's source names. Only
// the CLI's is known for sure; the app's own may come under several names.
func agySurface(source string) string {
	if source == "CORTEX_TRAJECTORY_SOURCE_CLI" {
		return surfaceCLI
	}
	return ""
}
