package collie

import (
	"fmt"
	"net/http"
	"time"
)

// switchPause: an agent takes a moment to finish one slash command before
// the next one submitted reads as a command again.
const switchPause = 1500 * time.Millisecond

// switchCommands: what to submit to a running agent to go from one model
// and effort to another for the rest of the session. Codex takes both in one
// /model (0.164 on). Claude Code and Antigravity have none: their /model and
// /effort also save the pick as the default for every new session, so they
// aren't switched from here.
func switchCommands(kind string, from, to [2]string) []string {
	var c []string
	switch kinds[kind] {
	case "codex":
		if to != from && to[0] != "" {
			cmd := "/model " + to[0]
			if to[1] != "" {
				cmd += " --reasoning=" + to[1]
			}
			c = append(c, cmd)
		}
	}
	return c
}

// switchModel submits the commands as prompts (herdr's, which wait for the
// paste to settle and refuse a busy agent) and records the run, so the
// session shows the model it now uses.
func (s *Server) switchModel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind   string    `json:"kind"`
		Model  string    `json:"model"`
		Effort string    `json:"effort"`
		From   [2]string `json:"from"` // what the session used before
	}
	if !decode(w, r, &body) {
		return
	}
	for _, v := range [...]string{body.Model, body.Effort, body.From[0], body.From[1]} {
		if !modelNameRe.MatchString(v) {
			writeJSON(w, 400, map[string]any{"error": "bad model or effort"})
			return
		}
	}
	to := [2]string{body.Model, body.Effort}
	cmds := switchCommands(body.Kind, body.From, to)
	if len(cmds) == 0 {
		writeJSON(w, 400, map[string]any{"error": "nothing to change"})
		return
	}
	id, ctx := r.PathValue("id"), r.Context()
	for i, c := range cmds {
		if i > 0 {
			if err := sleepCtx(ctx, switchPause); err != nil {
				fail(w, err)
				return
			}
		}
		if err := s.herdr.Prompt(ctx, id, c); err != nil {
			fail(w, fmt.Errorf("%s: %w", c, err))
			return
		}
	}
	s.herdr.RememberRun(id, to[0], to[1])
	writeJSON(w, 200, map[string]any{"ok": true, "sent": cmds})
}
