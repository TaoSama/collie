package collie

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The node side of running Collie on this computer (router_remote.go).

func nodeRouterDir() string { return filepath.Join(collieDir(), "router") }

// getRouterFiles is the node side of syncRouter: the folder as it is here.
func (s *Server) getRouterFiles(w http.ResponseWriter, r *http.Request) {
	if !s.node {
		http.NotFound(w, r)
		return
	}
	dir := nodeRouterDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	files, err := readRouterFiles(dir)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, routerFiles{Dir: dir, Files: files})
}

// putRouterFiles applies the hub's side of a sync here.
func (s *Server) putRouterFiles(w http.ResponseWriter, r *http.Request) {
	if !s.node {
		http.NotFound(w, r)
		return
	}
	var c routerChanges
	if !decode(w, r, &c) {
		return
	}
	if err := applyRouterChanges(nodeRouterDir(), c); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// getContextUsage: how full a session's context window is, from its log on
// this computer (the hub asks for Collie's when Collie runs here).
func (s *Server) getContextUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind, session := q.Get("kind"), q.Get("session")
	if strings.ContainsAny(session, "/\\*?[") {
		writeJSON(w, 400, map[string]any{"error": "bad session"})
		return
	}
	var window int
	fmt.Sscan(q.Get("window"), &window)
	writeJSON(w, 200, s.contextFor(kind, session, q.Get("pane"), s.expand(q.Get("cwd")), window))
}

func (s *Server) contextFor(kind, session, pane, cwd string, window int) ctxUsage {
	if path, format := s.findTranscript(kind, "id", session, pane, cwd); path != "" {
		return contextUsage(path, format, window)
	}
	return ctxUsage{}
}
