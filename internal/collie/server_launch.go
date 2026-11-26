package collie

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

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
