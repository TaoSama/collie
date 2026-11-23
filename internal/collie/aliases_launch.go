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
	promptLines     = 5
)

// waitPrompt waits, for shellPromptWait at most, until the pane's shell
// shows its prompt: some output that has stopped changing. Typed while rc
// files still run ("Last login…", plugin managers), a line can be read or
// flushed by them.
func (s *Server) waitPrompt(ctx context.Context, paneID string) error {
	last := ""
	for until := time.Now().Add(shellPromptWait); time.Now().Before(until); {
		if rr, err := s.herdr.Read(ctx, paneID, "recent", "text", promptLines); err == nil {
			text := strings.TrimSpace(rr.Text)
			if text != "" && text == last {
				return nil
			}
			last = text
		}
		if err := sleepCtx(ctx, aliasPoll); err != nil {
			return err
		}
	}
	return nil // a slow rc file: the terminal keeps what we type until the shell reads it
}

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
	if err := s.waitPrompt(ctx, paneID); err != nil {
		return err
	}
	line, err := shellLine(append([]string{a.Name}, aliasArgs(a, args)...))
	if err == nil && a.Via != "" {
		line, err = shellLine([]string{a.Via, "-ic", line})
	}
	if err != nil {
		return err
	}
	if err := s.herdr.Type(ctx, paneID, line); err != nil {
		return err
	}
	named := false
	for until := time.Now().Add(aliasStartWait); ; {
		p, err := s.herdr.Pane(ctx, paneID)
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
