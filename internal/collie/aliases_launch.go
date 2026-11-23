package collie

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// herdr's agent.start types the agent's own command (claude, codex, agy) at
// the pane's shell prompt, so an alias with another name never runs. To start
// through one, Collie types the alias itself, waits for herdr to see the agent
// it launches, then gives it its name as agent.start would.

const (
	shellPromptWait = 10 * time.Second
	aliasStartWait  = 60 * time.Second
	aliasPoll       = 500 * time.Millisecond
)

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// startThroughAlias runs a's command line plus args in paneID's shell. Like
// agent.start, an "agent_not_ready" error means the agent is up but sitting
// on a dialog.
func (s *Server) startThroughAlias(ctx context.Context, paneID, name, herdrKind string, a *LauncherAlias, args []string) error {
	if !aliasNameRe.MatchString(a.Name) {
		return fmt.Errorf("bad alias name %q", a.Name)
	}
	// Typed before the prompt, the line would sit in the terminal while rc
	// files run, and some of them read or flush input.
	for until := time.Now().Add(shellPromptWait); time.Now().Before(until); {
		if rr, err := s.herdr.Read(ctx, paneID, "recent", "text", 5); err == nil && strings.TrimSpace(rr.Text) != "" {
			break
		}
		if err := sleepCtx(ctx, aliasPoll); err != nil {
			return err
		}
	}
	line := shellLine(append([]string{a.Name}, aliasArgs(a, args)...))
	if a.Via != "" {
		line = shellLine([]string{a.Via, "-ic", line})
	}
	if err := s.herdr.Call(ctx, "pane.send_text", map[string]any{"pane_id": paneID, "text": line}, nil); err != nil {
		return err
	}
	if err := s.herdr.Call(ctx, "pane.send_keys", map[string]any{"pane_id": paneID, "keys": []string{"enter"}}, nil); err != nil {
		return err
	}
	named := false
	for until := time.Now().Add(aliasStartWait); ; {
		var p Pane
		err := s.herdr.Call(ctx, "pane.get", map[string]any{"pane_id": paneID}, &struct {
			Pane *Pane `json:"pane"`
		}{&p})
		if err == nil && deref(p.Agent) == herdrKind {
			if !named {
				if err := s.herdr.Call(ctx, "agent.rename", map[string]any{"target": paneID, "name": name}, nil); err != nil {
					return fmt.Errorf("agent 起来了，但没能命名为 %s：%w", name, err)
				}
				named = true
			}
			switch p.AgentStatus {
			case "idle", "done":
				return nil
			case "blocked":
				return &HerdrError{Code: "agent_not_ready", Message: "agent is waiting on a dialog"}
			}
		}
		if time.Now().After(until) {
			if named {
				return &HerdrError{Code: "agent_not_ready", Message: "agent did not become ready"}
			}
			return fmt.Errorf("alias %s 没有启动 %s（到终端里看看它停在哪）", a.Name, herdrKind)
		}
		if err := sleepCtx(ctx, aliasPoll); err != nil {
			return err
		}
	}
}
