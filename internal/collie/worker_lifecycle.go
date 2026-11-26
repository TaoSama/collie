package collie

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

// paneTail is the last non-blank lines on a pane's screen ("" if unreadable).
func (sp *Super) paneTail(ctx context.Context, pane string) string {
	res, err := sp.s.herdr.Read(ctx, pane, "recent_unwrapped", "text", 40)
	if err != nil {
		return ""
	}
	var keep []string
	for _, l := range strings.Split(res.Text, "\n") {
		if l = strings.TrimRight(l, " "); strings.TrimSpace(l) != "" {
			keep = append(keep, l)
		}
	}
	if len(keep) > 12 {
		keep = keep[len(keep)-12:]
	}
	return strings.Join(keep, "\n")
}

// closeWorkspace closes a herdr workspace, logging a failure.
func (sp *Super) closeWorkspace(ctx context.Context, ws string) {
	if err := sp.s.herdr.Call(ctx, "workspace.close", map[string]any{"workspace_id": ws}, nil); err != nil {
		log.Printf("close workspace %s: %v", ws, err)
	}
}

// workerSettled turns a worker reaching done/blocked into a router trigger,
// carrying a short summary from the worker's transcript or screen.
// workerDead tells the router that a worker's agent isn't running, with what
// its screen says (usually the error it died with).
func (sp *Super) workerDead(ctx context.Context, w *Worker) {
	tail := sp.paneTail(ctx, w.PaneID)
	// Its screen is in the messages below; the shell it left goes.
	if w.WorkspaceID != "" {
		sp.closeWorkspace(ctx, w.WorkspaceID)
	}
	why := deathReason(tail)
	log.Printf("worker %s: no agent in its pane %s; marked failed: %s", w.Name, w.PaneID, why)
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.ingestLocked("worker", w.Thread, w.Name, fmt.Sprintf(
		"worker %s failed: %s isn't running in its pane (it never started, or it quit), so its task was not delivered. Last lines on its screen:\n%s\nFix the cause or tell the human; spawn a new worker to retry.",
		w.Name, w.Kind, tail), "", "", w.Name)
	// The Router retries it or tells the human; the spawn line shows it ended.
}

// deathReason picks the line that says why: "Error: …" over a stack frame
// like "throw new Error(".
func deathReason(screen string) string {
	var loose string
	for _, l := range strings.Split(screen, "\n") {
		t := strings.TrimSpace(l)
		low := strings.ToLower(t)
		if strings.Contains(low, "error:") || strings.HasPrefix(low, "fatal") || strings.Contains(low, "panic:") ||
			strings.Contains(low, "command not found") || strings.Contains(low, "no such file") ||
			strings.HasPrefix(low, "no conversation found") {
			return t
		}
		if loose == "" && strings.Contains(low, "error") {
			loose = t
		}
	}
	return loose
}

func (sp *Super) workerSettled(ctx context.Context, w *Worker, blocked bool) {
	// The human chose to bypass permissions in settings, so a worker's
	// "trust this folder?" (the folder Collie started it in) isn't theirs to
	// click. Anything else still goes to them.
	sp.mu.Lock()
	ask := sp.st.Settings.AskPermissions
	sp.mu.Unlock()
	if blocked && !ask && sp.answerTrust(ctx, w.PaneID, w.Cwd) {
		sp.mu.Lock()
		sp.noteAnsweredLocked(w)
		sp.mu.Unlock()
		log.Printf("worker %s: trusted its folder %s", w.Name, w.Cwd)
		return
	}
	summary := sp.lastAssistant(ctx, w)
	var dialog *Approval
	if blocked {
		if screen, err := sp.s.herdr.ReadAsk(ctx, w.PaneID); err == nil {
			if a, ok := visibleApproval(screen); ok {
				dialog = &a
			}
		}
	}
	if w.Conv != "" && !blocked && sp.s.ext != nil {
		// An extension conversation's agent: its turn's last message is the
		// answer, sent back to that chat, not a report for the Router.
		// Only its own words go out: never the screen, which may be the
		// task it was given.
		text, kind := sp.lastReply(ctx, w, 0, false), extproto.KindAnswer
		if msg := sp.workerAgentError(ctx, w, text); msg != "" {
			text = "（agent 出错，没能完成：" + msg + "）"
		} else if text == "" {
			text, kind = "（没取到 agent 的回答：找不到它的会话记录。请在 Collie 里看 "+w.Name+"。）", extproto.KindProgress
		}
		if _, err := sp.s.ext.sendOut(w.Thread, text, kind, sp.workerRunInfo(w), ""); err != nil {
			log.Printf("worker %s: send the answer back: %v", w.Name, err)
		}
		return
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if blocked {
		sp.ingestLocked("worker", w.Thread, w.Name,
			fmt.Sprintf("worker %s is BLOCKED on a dialog in its terminal (the human also sees it as a card in chat). Screen:\n%s%s\nIf the answer follows from what the human asked, or is routine, answer it yourself: `collie answer %s <choice>`. Leave it to the human only when it is theirs to decide (see ROUTER.md).", w.Name, summary, choicesText(dialog), w.Name), "", "", w.Name)
		sp.addAskLocked(w, dialog)
		return
	}
	if msg := sp.workerAgentError(ctx, w, summary); msg != "" {
		// Its agent stopped it (out of quota, logged out, the API down):
		// the turn ended, the work did not.
		sp.workerStoppedLocked(w, msg, summary, time.Now())
		return
	}
	sp.ingestLocked("worker", w.Thread, w.Name,
		fmt.Sprintf("worker %s finished a turn.%s Last message:\n%s", w.Name, resumedNote(w.Resumed), summary), "", "", w.Name)
	w.clearResume()
	sp.addEntryLocked("event", w.Thread, w.Name+" 完成了一轮", w.Name, "done")
}

// workerAgentError reads the bottom of a worker's terminal after a turn:
// what its agent printed when it gave up, or "". A line that is also in its
// last message is the work talking about such things, not the agent.
func (sp *Super) workerAgentError(ctx context.Context, w *Worker, summary string) string {
	if w.PaneID == "" {
		return ""
	}
	rr, err := sp.s.herdr.Read(ctx, w.PaneID, "recent", "text", 40)
	if err != nil {
		return ""
	}
	return agentStopLine(rr.Text, summary)
}

func agentStopLine(screen, summary string) string {
	var tail []string
	for _, l := range strings.Split(screen, "\n") {
		if strings.Contains(l, workerResumeMark) {
			tail = nil // what is above was handled when Collie said to carry on
		} else if t := strings.TrimSpace(l); t != "" {
			tail = append(tail, t)
		}
	}
	if len(tail) > 12 {
		tail = tail[len(tail)-12:]
	}
	for _, l := range tail {
		if workerStopRe.MatchString(l) && !strings.Contains(summary, strings.Trim(l, "⎿⏺●•✗✘⚠■│ ")) {
			return clip(strings.Trim(l, "⎿⏺●•✗✘⚠■│ "), 200)
		}
	}
	return ""
}

func (sp *Super) lastAssistant(ctx context.Context, w *Worker) string {
	return sp.lastReply(ctx, w, 1200, true)
}

// lastReply is w's last message, clipped to most runes (0: all of it). With
// screen, the bottom of its screen stands in when its transcript can't be
// found; without, that is "".
func (sp *Super) lastReply(ctx context.Context, w *Worker, most int, screen bool) string {
	cut := func(s string) string {
		if most > 0 {
			return clip(s, most)
		}
		return s
	}
	if pane, machine := split(w.PaneID); machine != "" {
		// Its transcript lives on that computer: ask its node.
		if c, err := sp.s.herdr.Client(machine); err == nil {
			u := "http://node/api/panes/" + url.PathEscape(pane) + "/last"
			if !screen {
				u += "?screen=0"
			}
			req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
			if resp, err := c.Do(req); err == nil {
				var out struct {
					Text string `json:"text"`
				}
				json.NewDecoder(resp.Body).Decode(&out)
				resp.Body.Close()
				if out.Text != "" {
					return cut(out.Text)
				}
			}
		}
	} else if w.PaneID != "" {
		var info struct {
			Agent agentDetail `json:"agent"`
		}
		if sp.s.herdr.Call(ctx, "agent.get", map[string]any{"target": w.PaneID}, &info) == nil && info.Agent.replyTranscriptKnown(screen) {
			if tr := sp.s.transcriptFor(w.Name, w.PaneID, info.Agent, sp.transcripts); tr != nil {
				tr.update()
				msgs := tr.view()
				for i := len(msgs) - 1; i >= 0; i-- {
					if msgs[i].Kind == "assistant" {
						return cut(msgs[i].Text)
					}
				}
			}
		}
		if !screen {
			return ""
		}
		if res, err := sp.s.herdr.Read(ctx, w.PaneID, "detection", "text", 0); err == nil {
			return tailLines(res.Text, 14)
		}
	}
	if !screen {
		return ""
	}
	return "(no output captured)"
}
