package collie

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
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

	agy     agyBindings // Antigravity transcripts inferred for the local panes
	dialogs dialogSubmissions

	aliases aliasBook    // hub only: launcher aliases each computer has
	sysmon  *sysMonitor  // this computer's load and tokens; nil in tests
	wakes   paneWake     // streams to nudge when input reaches a pane
	screens screenMetaCache
	direct  directTokens // node only: tokens for a browser on this computer

	wbDomain string  // hub only: modules kept by the hub open at <proxy host>.<wbDomain> (-wb-domain)
	wbGate   *wbGate // hub only: the way into those modules
}

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
	h := securityHeaders(compress(cors(mux)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A module's own host is its site, through the hub: none of Collie's routes.
		if host, ok := s.wbProxyHost(r); ok && s.wbGate != nil {
			s.serveWBProxy(w, r, host)
			return
		}
		h.ServeHTTP(w, r)
	})
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
	mux.Handle("GET /api/panes/{id}/logged", wrap(s.paneLogged))
	mux.Handle("GET /api/panes/{id}/ask", wrap(s.paneAsk))
	mux.Handle(paneSubmitRoute, wrap(s.submitPane))
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
	mux.Handle("POST /api/sessions/{id}/unarchive", wrap(s.unarchiveSession))
}

// NodeRoutes is what a node serves to its hub through the tunnel.
func (s *Server) NodeRoutes() http.Handler {
	mux := http.NewServeMux()
	s.machineRoutes(mux, func(h http.HandlerFunc) http.Handler { return h })
	mux.HandleFunc("POST /api/direct/token", s.mintDirect)
	mux.HandleFunc("POST /api/panes/{id}/decide", s.paneDecide)
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
	if !cookieAllowed(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(0, 0), Secure: r.TLS != nil,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- helpers

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

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

func (s *sseWriter) replayProgress(p transcriptProgress) {
	data, err := json.Marshal(p)
	if err == nil {
		err = s.send("replay-progress", data)
	}
	if err != nil {
		log.Printf("collie: send replay progress: %v", err)
	}
}

func (s *sseWriter) sendChangedJSON(event string, value any, last *[]byte) (bool, error) {
	data, err := json.Marshal(value)
	if err != nil {
		// An encoding failure is an empty frame after previously valid data.
		data = nil
	}
	if bytes.Equal(data, *last) {
		return false, nil
	}
	if err := s.send(event, data); err != nil {
		return false, err
	}
	*last = data
	return true, nil
}

func (s *sseWriter) comment() { fmt.Fprint(s.w, ": ping\n\n"); s.f.Flush() }
