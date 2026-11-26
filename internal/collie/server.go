package collie

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type Server struct {
	seen  seenPanes // panes you have opened here since they finished
	herdr *Herdr
	token string
	home  string
	web   fs.FS
	sup   *Super
	ext   *extHost // hub only: extensions and their conversations
	auths *Auth    // hub only: paired devices and computers
	node  bool     // serving the node API inside a tunnel (no auth, no super)

	port       int      // hub only: listening port, for addresses
	publicURLs []string // hub only: addresses given with -url, shown first

	agy agyBindings // Antigravity transcripts inferred for the local panes

	aliases aliasBook    // hub only: launcher aliases each computer has
	sysmon  *sysMonitor  // this computer's load and tokens; nil in tests
	wakes   paneWake     // streams to nudge when input reaches a pane
	direct  directTokens // node only: tokens for a browser on this computer
}

// Agent kinds Collie offers in its launcher, mapped to herdr's --kind values.
// resumeArgs open an agent on an earlier conversation of its own.
func resumeArgs(agent, session string) []string {
	if session == "" {
		return nil
	}
	switch agent {
	case "claude":
		return []string{"--resume", session}
	case "codex":
		return []string{"resume", session}
	case "agy":
		return []string{"--conversation", session}
	}
	return nil
}

var kinds = map[string]string{
	"claude":      "claude",
	"codex":       "codex",
	"antigravity": "agy",
	"agy":         "agy",
}

// maxNameLen bounds an agent's name (nameRe).
const maxNameLen = 32

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", s.static())
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)

	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.auth(h)) }
	api("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
			setTokenCookie(w, r, c.Value)
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	api("GET /api/events", s.streamState)
	s.machineRoutes(mux, func(h http.HandlerFunc) http.Handler { return s.auth(s.routed(h)) })
	s.superRoutes(mux)
	s.extRoutes(mux)
	s.wbRoutes(mux)
	s.pushRoutes(mux)
	s.hubRoutes(mux)
	return securityHeaders(compress(cors(mux)))
}

// machineRoutes are the endpoints about one machine's panes. The hub serves
// them for its own machine and forwards the rest to the right node; a node
// serves them for itself inside the tunnel.
func (s *Server) machineRoutes(mux *http.ServeMux, wrap func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /api/state", wrap(s.getState))
	mux.Handle("GET /api/ext-local", wrap(s.localExts))
	mux.Handle("GET /api/dirs", wrap(s.listDirs))
	mux.Handle("GET /api/file", wrap(s.readFile))
	mux.Handle("POST /api/agents", wrap(s.launchAgent))
	mux.Handle("POST /api/agents/{target}/prompt", wrap(s.promptAgent))
	mux.Handle("GET /api/panes/{id}/stream", wrap(s.streamPane))
	mux.Handle("GET /api/panes/{id}/chat", wrap(s.streamChat))
	mux.Handle("GET /api/panes/{id}/earlier", wrap(s.paneEarlier))
	mux.Handle("GET /api/panes/{id}/last", wrap(s.lastMessage))
	mux.Handle("GET /api/panes/{id}/ask", wrap(s.paneAsk))
	mux.Handle("POST /api/panes/{id}/text", wrap(s.sendText))
	mux.Handle("POST /api/panes/{id}/keys", wrap(s.sendKeys))
	mux.Handle("POST /api/panes/{id}/model", wrap(s.switchModel))
	mux.Handle("POST /api/panes/{id}/close", wrap(s.closePane))
	mux.Handle("POST /api/workspaces/{id}/close", wrap(s.closeWorkspace))
	mux.Handle("POST /api/uploads", wrap(s.upload))
	mux.Handle("GET /api/titles", wrap(s.getTitles))
	mux.Handle("GET /api/models", wrap(s.listModels))
	mux.Handle("GET /api/commands", wrap(s.listCommands))
	mux.Handle("GET /api/aliases", wrap(s.getAliases))
	mux.Handle("GET /api/sysmon", wrap(s.getSysmon))
	mux.Handle("POST /api/trust", wrap(s.trustFolder))
	mux.Handle("GET /api/router/files", wrap(s.getRouterFiles))
	mux.Handle("POST /api/router/files", wrap(s.putRouterFiles))
	mux.Handle("GET /api/context", wrap(s.getContextUsage))
	mux.Handle("POST /api/update", wrap(s.selfUpdate))
	mux.Handle("GET /api/transcript", wrap(s.getTranscript))
	mux.Handle("POST /api/seen", wrap(s.seenPane))
	mux.Handle("GET /api/external", wrap(s.listExternal))
	mux.Handle("GET /api/external/{id}/chat", wrap(s.streamExternal))
	mux.Handle("GET /api/external/{id}/earlier", wrap(s.externalEarlier))
	mux.Handle("POST /api/external/{id}/takeover", wrap(s.takeOverExternal))
	mux.Handle("POST /api/external/{id}/send", wrap(s.sendExternal))
	mux.Handle("POST /api/codex/interrupt", wrap(s.interruptCodex))
	mux.Handle("POST /api/external/{id}/reveal", wrap(s.revealExternal))
	mux.Handle("POST /api/sessions/{id}/rename", wrap(s.renameSession))
	mux.Handle("POST /api/sessions/{id}/archive", wrap(s.archiveSession))
}

// NodeRoutes is what a node serves to its hub through the tunnel.
func (s *Server) NodeRoutes() http.Handler {
	mux := http.NewServeMux()
	s.machineRoutes(mux, func(h http.HandlerFunc) http.Handler { return h })
	mux.HandleFunc("POST /api/direct/token", s.mintDirect)
	mux.HandleFunc("GET /api/watch", s.watchLists)
	return mux
}

// ---------------------------------------------------------------- auth

const cookieName = "collie_token"

// authed lets in the master token and paired phones/browsers everywhere, and
// paired computers (their node and the CLI their agents run) only where the
// CLI needs to go.
func (s *Server) authed(r *http.Request) bool {
	master, d := s.identity(r)
	if master {
		return true
	}
	if d == nil {
		return false
	}
	return d.Kind == "browser" || strings.HasPrefix(r.URL.Path, "/api/super/")
}

func (s *Server) auth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			writeJSON(w, 401, map[string]any{"error": "unauthorized"})
			return
		}
		if !s.node {
			// Which web app the hub serves: an open page that sees it change offers to reload.
			w.Header().Set("X-Collie-Web", s.webBuild())
		}
		h.ServeHTTP(w, r)
	})
}

var webBuildOnce struct {
	sync.Once
	v string
}

// shellFiles are what a launch needs before it can draw anything. The
// service worker keeps them, so an installed app starts without the network.
var shellFiles = []string{
	"/", "/app.js", "/imgedit.js", "/threads.js", "/ext.js", "/workbench.js", "/app.css", "/skins.css", "/icon.svg", "/manifest.webmanifest",
	"/vendor/xterm.css", "/vendor/xterm.js", "/vendor/katex/katex.min.css", "/vendor/katex/katex.min.js",
}

// webBuild fingerprints the web app's files, the service worker's included.
func (s *Server) webBuild() string {
	webBuildOnce.Do(func() {
		h := sha256.New()
		for _, p := range append([]string{"/sw.js"}, shellFiles...) {
			if p == "/" {
				p = "/index.html"
			}
			b, _ := fs.ReadFile(s.web, p[1:])
			h.Write(b)
		}
		webBuildOnce.v = hex.EncodeToString(h.Sum(nil)[:6])
	})
	return webBuildOnce.v
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct{ Token string }
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	a, b := sha256.Sum256([]byte(strings.TrimSpace(body.Token))), sha256.Sum256([]byte(s.token))
	if subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
		time.Sleep(400 * time.Millisecond)
		writeJSON(w, 401, map[string]any{"error": "invalid token"})
		return
	}
	setTokenCookie(w, r, s.token)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(0, 0), Secure: r.TLS != nil,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- state

type AgentView struct {
	PaneID         string     `json:"pane_id"`
	WorkspaceID    string     `json:"workspace_id"`
	WorkspaceLabel string     `json:"workspace_label"`
	Name           string     `json:"name"`
	Kind           string     `json:"kind"`
	Status         string     `json:"status"`
	Cwd            string     `json:"cwd"`
	Title          string     `json:"title"`
	Topic          string     `json:"topic,omitempty"`   // the session's own title
	Session        string     `json:"session,omitempty"` // id to resume it by
	IsAgent        bool       `json:"is_agent"`
	IsRouter       bool       `json:"is_router"`
	Machine        string     `json:"machine"`
	Run            *[2]string `json:"run,omitempty"` // model, effort: its latest reply's, else what Collie started it with
	Context        *ctxUsage  `json:"context,omitempty"`
	HumanAt        time.Time  `json:"human_at,omitzero"`  // the human's last message in it, else its start
	Archived       bool       `json:"archived,omitempty"` // its agent's own archive has it (archived.go)
}

type PaneState struct {
	OK       bool        `json:"ok"`
	Error    string      `json:"error,omitempty"`
	Agents   []AgentView `json:"agents"`
	Shells   []AgentView `json:"shells"`
	Machines []Machine   `json:"machines"`
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (s *Server) state(ctx context.Context) PaneState {
	ctx, cancel := context.WithTimeout(withCachedLists(ctx), 5*time.Second)
	defer cancel()
	machines := s.herdr.Machines()
	ws, err := s.herdr.Workspaces(ctx)
	if err != nil {
		return PaneState{Error: err.Error(), Agents: []AgentView{}, Shells: []AgentView{}, Machines: machines}
	}
	panes, err := s.herdr.Panes(ctx)
	if err != nil {
		return PaneState{Error: err.Error(), Agents: []AgentView{}, Shells: []AgentView{}, Machines: machines}
	}
	agents, _ := s.herdr.Agents(ctx)

	labels := map[string]string{}
	order := map[string]int{}
	for _, w := range ws {
		labels[w.ID] = w.Label
		order[w.ID] = w.Number
	}
	names := map[string]string{}
	var routerPane string
	router := routerName
	if s.sup != nil {
		router = s.sup.routerAgent()
	}
	for _, a := range agents {
		names[a.PaneID] = deref(a.Name)
		if names[a.PaneID] == router {
			routerPane = a.PaneID
		}
	}

	st := PaneState{OK: true, Agents: []AgentView{}, Shells: []AgentView{}, Machines: machines}
	topics := map[string]map[string]paneSession{"": s.titlesFor("", ctx)}
	for _, m := range machines {
		if !m.Local && m.Online {
			topics[m.ID] = s.titlesFor(m.ID, ctx)
		}
	}
	for _, p := range panes {
		cwd := deref(p.ForegroundCwd)
		if cwd == "" {
			cwd = deref(p.Cwd)
		}
		v := AgentView{
			PaneID: p.ID, WorkspaceID: p.WorkspaceID, WorkspaceLabel: labels[p.WorkspaceID],
			Kind: deref(p.Agent), Status: p.AgentStatus, Cwd: cwd, Title: deref(p.Title),
			Name: names[p.ID], IsAgent: p.Agent != nil, IsRouter: p.ID == routerPane && p.Agent != nil,
		}
		var local string
		local, v.Machine = split(p.ID)
		if v.IsAgent {
			ps := topics[v.Machine][local]
			v.Topic, v.Session, v.Context, v.HumanAt, v.Archived = ps.Title, ps.Session, ps.Context, ps.HumanAt, ps.Archived
			if ps.Ran != nil {
				v.Run = ps.Ran // what it last replied with
			} else if r, ok := s.herdr.Run(p.ID); ok {
				v.Run = r // started from here
			} else {
				v.Run = ps.Run // started by that machine's own node
			}
		}
		if v.Name == "" {
			v.Name = v.WorkspaceLabel
		}
		if v.IsAgent {
			v.Status = s.seen.status(p.ID, v.Status)
		}
		if v.IsAgent && s.waiting(p.ID, v.Status) {
			v.Status = "waiting"
		}
		if v.IsAgent {
			st.Agents = append(st.Agents, v)
		} else {
			v.Status = "shell"
			st.Shells = append(st.Shells, v)
		}
	}
	less := func(list []AgentView) func(i, j int) bool {
		return func(i, j int) bool {
			if list[i].Machine != list[j].Machine {
				return list[i].Machine < list[j].Machine
			}
			if order[list[i].WorkspaceID] != order[list[j].WorkspaceID] {
				return order[list[i].WorkspaceID] < order[list[j].WorkspaceID]
			}
			return list[i].PaneID < list[j].PaneID
		}
	}
	alive := map[string]bool{}
	for _, p := range panes { // shells too: an agent may not show as one yet
		alive[p.ID] = true
	}
	online := map[string]bool{"": true}
	for _, m := range machines {
		online[m.ID] = m.Local || m.Online
	}
	s.herdr.pruneRuns(alive, func(m string) bool { return online[m] })
	sort.SliceStable(st.Agents, less(st.Agents))
	sort.SliceStable(st.Shells, less(st.Shells))
	return st
}

func (s *Server) getState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.state(r.Context()))
}

// streamState pushes the herdr session state and the super-agent view over one
// SSE connection. Browsers allow ~6 HTTP/1.1 connections per origin, so every
// stream a tab opens counts.
func (s *Server) streamState(w http.ResponseWriter, r *http.Request) {
	sse, ok := newSSE(w)
	if !ok {
		return
	}
	var ch chan struct{}
	if s.sup != nil {
		c, done := s.sup.subscribe()
		defer done()
		ch = c
	}
	var lastState, lastSuper string
	listsChanged, stopLists := s.herdr.listsUpdates()
	defer stopLists()
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	beat := time.NewTicker(15 * time.Second)
	defer beat.Stop()
	for {
		// Collie's chat first: it is quick, and a message just sent should
		// show before the slower look across every computer's sessions.
		if s.sup != nil {
			b, _ := json.Marshal(s.superState(r.Context()))
			if string(b) != lastSuper {
				lastSuper = string(b)
				if sse.send("super", b) != nil {
					return
				}
			}
		}
		b, _ := json.Marshal(s.state(r.Context()))
		if string(b) != lastState {
			lastState = string(b)
			if sse.send("state", b) != nil {
				return
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			sse.comment()
		case <-ch:
		case <-listsChanged:
		case <-tick.C:
		}
	}
}

// streamPane mirrors one pane over SSE.
//
//	mode=screen: recent output with ANSI styling, for a scrollable terminal render,
//	             the cursor marked in it (markScreenCursor).
//	mode=log:    recent output with soft wraps joined, for reading on narrow screens.
//
// screenLines is how far back screen mode can scroll: herdr's pane.read
// returns at most 1000 lines however many you ask for.
const screenLines = 1000

func (s *Server) streamPane(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	mode := r.URL.Query().Get("mode")
	sse, ok := newSSE(w)
	if !ok {
		return
	}
	rates := pollRates{normal: 200 * time.Millisecond, quick: 50 * time.Millisecond, burst: 10 * time.Millisecond}
	if mode == "log" {
		rates = pollRates{normal: 700 * time.Millisecond, quick: 100 * time.Millisecond}
	}
	var woke time.Time
	wake, unsubscribe := s.wakes.subscribe(id)
	defer unsubscribe()
	var lastText string
	var lastSize Rect
	var sizeAt time.Time
	beat := time.Now()
	for {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		var (
			res ReadResult
			err error
		)
		if mode == "log" {
			res, err = s.herdr.Read(ctx, id, "recent_unwrapped", "text", 600)
		} else {
			if time.Since(sizeAt) > 2*time.Second {
				if sz, e := s.herdr.PaneSize(ctx, id); e == nil {
					lastSize = sz
				}
				sizeAt = time.Now()
			}
			res, err = s.herdr.Read(ctx, id, "recent", "ansi", screenLines)
		}
		cancel()
		if err != nil {
			if notFound(err) {
				b, _ := json.Marshal(map[string]any{"error": err.Error()})
				sse.send("gone", b)
				return
			}
		} else {
			if res.Text != lastText {
				lastText = res.Text
				text, cursor := res.Text, false
				if mode != "log" {
					text, cursor = markScreenCursor(text, lastSize.Height)
					text = trimScreenBlankTail(text)
				}
				b, _ := json.Marshal(map[string]any{
					"text": text, "revision": res.Revision, "truncated": res.Truncated,
					"cols": lastSize.Width, "rows": lastSize.Height, "cursor": cursor,
				})
				if sse.send("frame", b) != nil {
					return
				}
				beat = time.Now()
			}
		}
		if time.Since(beat) > 15*time.Second {
			sse.comment()
			beat = time.Now()
		}
		if !pollAfter(r.Context().Done(), wake, rates, &woke) {
			return
		}
	}
}

// trimScreenBlankTail drops unpainted blank rows at the bottom of an ANSI
// snapshot while keeping internal blank rows and background-styled bars.
func trimScreenBlankTail(s string) string {
	for len(s) > 0 {
		i := strings.LastIndexByte(s, '\n')
		line := s[i+1:]
		if !isUnpaintedBlankRow(line) {
			break
		}
		if i < 0 {
			return ""
		}
		if i > 0 && s[i-1] == '\r' {
			i--
		}
		s = s[:i]
	}
	return s
}

func isUnpaintedBlankRow(line string) bool {
	rest := strings.ReplaceAll(line, "\x1b[0m", "")
	rest = strings.ReplaceAll(rest, "\x1b[m", "")
	for i := 0; i < len(rest); i++ {
		if rest[i] != ' ' && rest[i] != '\r' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- actions

func (s *Server) sendText(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text  string `json:"text"`
		Enter bool   `json:"enter"`
	}
	if !decode(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	ctx := r.Context()
	if body.Text != "" {
		if err := s.herdr.Call(ctx, "pane.send_text", map[string]any{"pane_id": id, "text": body.Text}, nil); err != nil {
			fail(w, err)
			return
		}
	}
	if body.Enter {
		if err := s.herdr.Call(ctx, "pane.send_keys", map[string]any{"pane_id": id, "keys": []string{"enter"}}, nil); err != nil {
			fail(w, err)
			return
		}
	}
	s.wakes.wake(id)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) sendKeys(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Keys []string `json:"keys"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.Keys) == 0 || len(body.Keys) > 64 {
		writeJSON(w, 400, map[string]any{"error": "keys required"})
		return
	}
	if err := s.herdr.Call(r.Context(), "pane.send_keys", map[string]any{"pane_id": r.PathValue("id"), "keys": body.Keys}, nil); err != nil {
		fail(w, err)
		return
	}
	s.wakes.wake(r.PathValue("id"))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) promptAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeJSON(w, 400, map[string]any{"error": "empty prompt"})
		return
	}
	if err := s.herdr.Prompt(r.Context(), r.PathValue("target"), body.Text); err != nil {
		fail(w, err)
		return
	}
	s.wakes.wake(r.PathValue("target"))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// launchAgent opens a fresh herdr workspace in cwd and starts an agent in its
// shell pane, optionally handing it a first task.
func (s *Server) launchAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind   string   `json:"kind"`
		Name   string   `json:"name"`
		Cwd    string   `json:"cwd"`
		Prompt string   `json:"prompt"`
		Label  string   `json:"label"`
		Args   []string `json:"args"`
		Model  string   `json:"model"`  // "" = the agent's own default
		Effort string   `json:"effort"` // likewise
		Alias  string   `json:"alias"`  // a launcher alias in this computer's shell to start through
	}
	if !decode(w, r, &body) {
		return
	}
	if !modelNameRe.MatchString(body.Model) || !modelNameRe.MatchString(body.Effort) {
		writeJSON(w, 400, map[string]any{"error": "invalid model or effort"})
		return
	}
	if body.Kind == "shell" {
		s.openShell(w, r, body.Cwd, body.Label)
		return
	}
	kind, ok := kinds[body.Kind]
	if !ok {
		writeJSON(w, 400, map[string]any{"error": "unsupported agent kind"})
		return
	}
	if body.Name == "" {
		body.Name = s.freeName(r.Context(), body.Kind)
	}
	if !nameRe.MatchString(body.Name) || (body.Name == routerName && !s.node) {
		writeJSON(w, 400, map[string]any{"error": "name must be lowercase letters, digits, - or _ (max 32), starting with a letter; 'collie' is reserved"})
		return
	}
	cwd, err := resolveDir(s.home, body.Cwd)
	cwd, fallback := orHome(s.home, cwd, err)
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	label := body.Label
	if label == "" {
		label = body.Name
	}
	var alias *LauncherAlias
	if body.Alias != "" {
		a, err := s.aliasOn(ctx, "", body.Alias, kind)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		alias = a // it runs its own model: aliasArgs drops --model
	}
	args := append(modelArgs(body.Kind, body.Model, body.Effort), body.Args...)
	paneID, wsID, err := s.startAgentAs(ctx, "", kind, body.Name, cwd, label, alias, args)
	if paneID != "" {
		s.herdr.RememberRun(paneID, aliasModel(alias, body.Model), body.Effort)
	}
	if err != nil {
		var he *HerdrError
		if errors.As(err, &he) && he.Code == "agent_not_ready" {
			writeJSON(w, 202, withCwd(map[string]any{"pane_id": paneID, "workspace_id": wsID, "warning": "agent 启动时停在了一个对话框上，请在页面里处理"}, cwd, fallback))
			return
		}
		writeJSON(w, 502, map[string]any{"error": err.Error(), "pane_id": paneID, "workspace_id": wsID})
		return
	}
	if p := strings.TrimSpace(body.Prompt); p != "" {
		// A fresh agent takes a few seconds before it takes input.
		var err error
		for wait := time.Now().Add(20 * time.Second); ; {
			err = s.herdr.Prompt(ctx, paneID, p)
			var he *HerdrError
			if err == nil || !errors.As(err, &he) || (he.Code != "agent_not_ready" && he.Code != "agent_not_found") || time.Now().After(wait) || ctx.Err() != nil {
				break
			}
			time.Sleep(1500 * time.Millisecond)
		}
		if err != nil {
			writeJSON(w, 202, withCwd(map[string]any{"pane_id": paneID, "workspace_id": wsID, "warning": "已启动，但第一个任务发送失败：" + err.Error()}, cwd, fallback))
			return
		}
	}
	writeJSON(w, 200, withCwd(map[string]any{"pane_id": paneID, "workspace_id": wsID}, cwd, fallback))
}

// openShell opens a plain terminal: a herdr workspace in cwd, no agent.
func (s *Server) openShell(w http.ResponseWriter, r *http.Request, dir, label string) {
	cwd, err := resolveDir(s.home, dir)
	cwd, fallback := orHome(s.home, cwd, err)
	if label == "" {
		label = filepath.Base(cwd)
		if cwd == s.home {
			label = "~"
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	wsID, paneID, err := s.herdr.CreateWorkspace(ctx, "", cwd, label)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, withCwd(map[string]any{"pane_id": paneID, "workspace_id": wsID}, cwd, fallback))
}

// startAgent creates a workspace in cwd and starts a herdr agent in its pane.
// An "agent_not_ready" error means the agent is up but sitting on a dialog.
func (s *Server) startAgent(ctx context.Context, machine, herdrKind, name, cwd, label string, args []string) (paneID, wsID string, err error) {
	return s.startAgentAs(ctx, machine, herdrKind, name, cwd, label, nil, args)
}

// startAgentAs is startAgent through a launcher alias in that computer's
// shell (nil: the agent's own command).
func (s *Server) startAgentAs(ctx context.Context, machine, herdrKind, name, cwd, label string, alias *LauncherAlias, args []string) (paneID, wsID string, err error) {
	if err := s.agentMissing(machine, herdrKind); err != nil {
		return "", "", err
	}
	trustArgs := args
	if alias != nil {
		trustArgs = append(append([]string{}, alias.launchArgs()...), args...)
	}
	s.trustForStart(ctx, machine, herdrKind, cwd, trustArgs)
	wsID, paneID, err = s.herdr.CreateWorkspace(ctx, machine, cwd, label)
	if err != nil {
		return "", "", err
	}
	if alias != nil {
		err = s.startThroughAlias(ctx, paneID, name, herdrKind, alias, args)
	} else {
		err = s.herdrStart(ctx, paneID, name, herdrKind, args)
	}
	s.noteStarted(paneID, name, err)
	return paneID, wsID, err
}

// noteStarted records an agent that came up in paneID as name (err nil or
// "agent_not_ready": up, on a dialog).
func (s *Server) noteStarted(paneID, name string, err error) {
	var he *HerdrError
	if err == nil || (errors.As(err, &he) && he.Code == "agent_not_ready") {
		s.herdr.Remember(paneID, name)
	}
}

// herdrStart has herdr start the agent in paneID's shell.
func (s *Server) herdrStart(ctx context.Context, paneID, name, herdrKind string, args []string) (err error) {
	// A brand-new shell needs a moment to reach its prompt; retry until herdr
	// accepts the pane or we run out of patience.
	params := map[string]any{"name": name, "kind": herdrKind, "pane_id": paneID, "timeout_ms": 60000}
	if len(args) > 0 {
		params["args"] = args
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = s.herdr.Call(ctx, "agent.start", params, nil)
		var he *HerdrError
		if err == nil || !errors.As(err, &he) || he.Code == "agent_not_ready" || time.Now().After(deadline) ||
			strings.Contains(he.Code, "name") {
			break
		}
		time.Sleep(600 * time.Millisecond)
	}
	return err
}

// closePane closes one terminal; herdr closes its workspace with its last pane.
func (s *Server) closePane(w http.ResponseWriter, r *http.Request) {
	if err := s.herdr.Call(r.Context(), "pane.close", map[string]any{"pane_id": r.PathValue("id")}, nil); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) closeWorkspace(w http.ResponseWriter, r *http.Request) {
	if err := s.herdr.Call(r.Context(), "workspace.close", map[string]any{"workspace_id": r.PathValue("id")}, nil); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// listDirs powers the directory picker in the launcher.
func (s *Server) listDirs(w http.ResponseWriter, r *http.Request) {
	dir, err := resolveDir(s.home, r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	out := []string{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.IsDir() || (e.Type()&os.ModeSymlink != 0 && isDir(filepath.Join(dir, e.Name()))) {
			out = append(out, e.Name())
		}
		if len(out) >= 300 {
			break
		}
	}
	writeJSON(w, 200, map[string]any{"path": dir, "display": s.tilde(dir), "parent": filepath.Dir(dir), "dirs": out})
}

// ---------------------------------------------------------------- helpers

func (s *Server) expand(p string) string { return expandIn(s.home, p) }

// expandIn resolves "~", "~/x" and relative paths against a home directory.
// It uses "/" so it works for paths on another (Unix) machine too.
func expandIn(home, p string) string {
	p = strings.TrimSpace(p)
	switch {
	case p == "" || p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		return path.Join(home, p[2:])
	case !strings.HasPrefix(p, "/"):
		return path.Join(home, p)
	}
	return path.Clean(p)
}

func (s *Server) tilde(p string) string { return tildeIn(s.home, p) }

// tildeIn writes a path under home as "~" or "~/x"; others stay as they are.
func tildeIn(home, p string) string {
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}

func isDir(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }

func init() {
	_ = mime.AddExtensionType(".woff2", "font/woff2")
}

func (s *Server) static() http.Handler {
	files := http.FileServerFS(s.web)
	// Embedded files have no modification time, so browsers would guess how
	// long to keep them and run an old app.js after an upgrade. Tag each file
	// by its content and have the browser revalidate every time.
	var etags sync.Map
	etag := func(p string) string {
		if v, ok := etags.Load(p); ok {
			return v.(string)
		}
		b, err := fs.ReadFile(s.web, p)
		if err != nil {
			return ""
		}
		sum := sha256.Sum256(b)
		tag := `"` + hex.EncodeToString(sum[:8]) + `"`
		etags.Store(p, tag)
		return tag
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if p == "sw.js" {
			s.serviceWorker(w, r)
			return
		}
		if tag := etag(p); tag != "" {
			w.Header().Set("ETag", tag)
			w.Header().Set("Cache-Control", "no-cache")
			// Compressed responses carry the tag as weak (W/"…").
			if strings.TrimPrefix(r.Header.Get("If-None-Match"), "W/") == tag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}

// serviceWorker serves web/sw.js stamped with this build and its file list.
// A new build changes the script's bytes, which is how the browser learns to
// fetch the new app shell.
func (s *Server) serviceWorker(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(s.web, "sw.js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	list, err := json.Marshal(shellFiles)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	b = bytes.Replace(b, []byte(`"__BUILD__"`), []byte(`"`+s.webBuild()+`"`), 1)
	b = bytes.Replace(b, []byte(`["__SHELL__"]`), list, 1)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}

// crossSite reports a request made by a page from another origin: another
// hub's web app showing this hub's sessions.
func crossSite(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" || o == "null" {
		return false
	}
	u, err := url.Parse(o)
	return err != nil || u.Host != r.Host
}

// cors lets another hub's page (the browser shows several hubs in one
// sidebar) call this hub's API. Such calls carry the token themselves. No
// Access-Control-Allow-Credentials: a page asking to send our cookie fails
// the preflight, so allowing any origin hands nothing to a page that doesn't
// already hold a token.
func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if crossSite(r) && strings.HasPrefix(r.URL.Path, "/api/") {
			hd := w.Header()
			hd.Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
			hd.Add("Vary", "Origin")
			hd.Set("Access-Control-Expose-Headers", "X-Collie-Web, X-File-Kind, X-File-Size, X-File-Path, X-File-Thumb, Content-Disposition")
			if r.Method == http.MethodOptions {
				hd.Set("Access-Control-Allow-Methods", "GET, POST")
				hd.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				hd.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		h.ServeHTTP(w, r)
	})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid JSON body"})
		return false
	}
	return true
}

// notFound reports herdr saying the target does not exist. For a pane read
// that means the pane is closed; any other failure (a timeout, a node that is
// reconnecting) is on the way there and worth another try.
func notFound(err error) bool {
	var he *HerdrError
	return errors.As(err, &he) && strings.Contains(he.Code, "not_found")
}

func fail(w http.ResponseWriter, err error) {
	code := 502
	var he *HerdrError
	if errors.As(err, &he) {
		code = 409
		if strings.Contains(he.Code, "not_found") {
			code = 404
		}
	}
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func newSSE(w http.ResponseWriter) (*sseWriter, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	f.Flush()
	return &sseWriter{w, f}, true
}

func (s *sseWriter) send(event string, data []byte) error {
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	s.f.Flush()
	return nil
}

func (s *sseWriter) comment() { fmt.Fprint(s.w, ": ping\n\n"); s.f.Flush() }
