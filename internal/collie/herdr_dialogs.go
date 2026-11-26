package collie

import (
	"context"
	"regexp"
	"strings"
	"time"
)

type askCheck struct {
	at   time.Time
	asks bool
}

// idleButAsking corrects herdr's "idle" for a pane whose screen shows a
// question (herdr 0.8 misses some dialogs, like claude's folder trust).
func (h *Herdr) idleButAsking(ctx context.Context, pane string) bool {
	h.mu.RLock()
	c, ok := h.asks[pane]
	h.mu.RUnlock()
	if ok && time.Since(c.at) < 3*time.Second {
		return c.asks
	}
	asks := h.ScreenAsks(ctx, pane)
	h.mu.Lock()
	h.asks[pane] = askCheck{time.Now(), asks}
	h.mu.Unlock()
	return asks
}

// fixStatus rewrites agent_status "idle" to "blocked" where the screen asks.
func (h *Herdr) fixStatus(ctx context.Context, item map[string]any) bool {
	pane, _ := item["pane_id"].(string)
	if st, _ := item["agent_status"].(string); st != "idle" || pane == "" || item["agent"] == nil {
		return false
	}
	if h.idleButAsking(ctx, pane) {
		item["agent_status"] = "blocked"
		return true
	}
	return false
}

// ScreenAsks reports whether a pane's screen shows a question with choices.
func (h *Herdr) ScreenAsks(ctx context.Context, pane string) bool {
	res, err := h.Read(ctx, pane, "detection", "text", 0)
	if err != nil {
		return false
	}
	return screenAsks(res.Text)
}

// ReadAsk reads the screen a dialog would be on. Codex files a question
// asked mid-turn under "Queued follow-up inputs" as "? 1 question ⇧← to
// answer", its options hidden: ⇧← opens it, so it can be read and answered.
func (h *Herdr) ReadAsk(ctx context.Context, pane string) (string, error) {
	res, err := h.Read(ctx, pane, "detection", "text", 0)
	if err != nil || !codexQueuedAsk(res.Text) {
		return res.Text, err
	}
	h.mu.Lock()
	recent := time.Since(h.unfolded[pane]) < unfoldEvery
	if !recent {
		h.unfolded[pane] = time.Now()
	}
	h.mu.Unlock()
	if recent {
		return res.Text, nil // pressed a moment ago: don't press again while it opens
	}
	if err := h.SendKeys(ctx, pane, "shift+left"); err != nil {
		return res.Text, err
	}
	for until := time.Now().Add(unfoldWait); time.Now().Before(until); {
		if err := sleepCtx(ctx, unfoldPoll); err != nil {
			return res.Text, err
		}
		if r, err := h.Read(ctx, pane, "detection", "text", 0); err == nil && !codexQueuedAsk(r.Text) {
			return r.Text, nil
		}
	}
	return res.Text, nil
}

const (
	unfoldEvery = 3 * time.Second
	unfoldWait  = 2 * time.Second
	unfoldPoll  = 150 * time.Millisecond
)

var queuedQuestionRe = regexp.MustCompile(`^\?\s+\d+\s+questions?$`)

// codexQueuedAsk: Codex's folded question ("? 1 question" over "⇧← to
// answer") is on screen.
func codexQueuedAsk(screen string) bool {
	prev := ""
	for _, l := range strings.Split(screen, "\n") {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if t == "⇧← to answer" && queuedQuestionRe.MatchString(prev) {
			return true
		}
		prev = t
	}
	return false
}

func screenAsks(screen string) bool {
	_, ok := visibleApproval(screen)
	return ok
}

// visibleApproval reports a live dialog at the bottom of the screen. An idle prompt
// ("❯ Try …") over a status line is not one, and nor is a list or dialog the
// agent printed in its reply, which has the input box below it:
//   - an empty prompt or the input box's footer under the menu rules it out;
//   - numbered options need a key hint ("Enter to confirm · Esc to cancel")
//     or exactly one selection cursor ("❯ 1. Yes");
//   - unnumbered options need both.
func visibleApproval(screen string) (Approval, bool) {
	a := parseApproval(screen)
	if len(a.Choices) < 2 || a.idleBelow || a.after > maxBelowMenu {
		return a, false
	}
	if a.Choices[0].Text == "" {
		return a, a.Hint != "" && a.cursors == 1
	}
	return a, a.Hint != "" || a.cursors == 1
}

// maxBelowMenu: a live dialog has at most a footer or status line or two
// under its options (agy: "auto · Gemini 4 Carbon · high").
const maxBelowMenu = 4
