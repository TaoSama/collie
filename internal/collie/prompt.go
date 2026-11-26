package collie

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Prompt submits text to an agent, then reads the agent back until it shows
// the message was taken: herdr accepting a prompt only means the keys went
// out, and an agent can drop them (Enter landing before the paste is read,
// a screen that was not taking input yet). The agent's own log says so first
// (see prompt_log.go); its screen settles what the log can't. Text still in
// the input box gets another Enter; text that never arrived is sent again,
// but only when the agent was idle and nothing on its screen or in its state
// moved, so a message that did go in is never sent twice. What stays
// unconfirmed comes back as a "prompt_unconfirmed" error.
func (h *Herdr) Prompt(ctx context.Context, target, text string) error {
	pane := target
	if !strings.Contains(pane, ":") {
		pane = h.paneOfName(ctx, target)
	}
	before, screen, seen := h.lookPrompt(ctx, pane, "")
	probe := promptProbe(text, screen)
	before = before.with(screen, probe)
	mark := h.logMark(ctx, pane)
	err := h.submit(ctx, target, pane, text)
	var he *HerdrError
	if errors.As(err, &he) || !seen {
		return err // refused, so nothing was typed; or nothing to read back
	}
	// A transport error (the node dropped mid-call) may still have typed it:
	// it is only handed back for a retry when nothing of it shows.
	resent := false
	for tries := 1; ; tries++ {
		v, now := h.awaitPrompt(ctx, pane, probe, before, mark)
		more := tries < promptTries && ctx.Err() == nil
		switch {
		case v == promptIn:
			return nil
		case v == promptUnknown && err == nil:
			return nil // as herdr answered: no worse than before reading back
		case more && v == promptTyped:
			err = h.SendKeys(ctx, pane, "enter")
		case v == promptMissing && err != nil:
			return err
		// agent.get already turns "idle" under a dialog into "blocked".
		case more && v == promptMissing && !resent &&
			(now.status == "idle" || now.status == "done" && !h.ScreenAsks(ctx, pane)):
			err, resent = h.paste(ctx, pane, text), true
		default:
			return &HerdrError{Code: "prompt_unconfirmed", Message: v.why()}
		}
	}
}

// promptConfirmWait: how long one read-back waits for the agent to show it.
var promptConfirmWait = 4 * time.Second

// promptLogWait bounds one look at the agent's log, so a slow node still
// leaves time for its screen.
const promptLogWait = time.Second

const (
	promptTries      = 3 // reads back; between them another Enter, or one resend
	promptPoll       = 300 * time.Millisecond
	promptReadLines  = 120
	promptProbeRunes = 16
)

// promptVerdict is what a pane shows of a message sent to it.
type promptVerdict int

const (
	promptIn      promptVerdict = iota // the agent took it
	promptTyped                        // it sits in the input box: Enter didn't take
	promptMissing                      // no sign of it
	promptUnknown                      // can't tell (mid-turn, or the pane unreadable)
)

func (v promptVerdict) why() string {
	switch v {
	case promptTyped:
		return "消息停在输入框里，没有提交"
	case promptMissing:
		return "终端里没看到这条消息"
	}
	return "没能确认终端收到了这条消息"
}

// promptLook is one read of an agent: its state and how often the message's
// first words show on its screen outside the input box.
type promptLook struct {
	status string
	seq    uint64
	seen   int  // probe occurrences above the input box
	typed  bool // the input box holds the probe or a collapsed paste
}

// lookPrompt reads the agent's state and screen, and finds probe on it.
func (h *Herdr) lookPrompt(ctx context.Context, pane, probe string) (promptLook, string, bool) {
	if pane == "" {
		return promptLook{}, "", false
	}
	var info struct {
		Agent Agent `json:"agent"`
	}
	if h.Call(ctx, "agent.get", map[string]any{"target": pane}, &info) != nil {
		return promptLook{}, "", false
	}
	res, err := h.Read(ctx, pane, "recent", "text", promptReadLines)
	if err != nil {
		return promptLook{}, "", false
	}
	l := promptLook{status: info.Agent.AgentStatus, seq: info.Agent.StateSeq}
	return l.with(res.Text, probe), res.Text, true
}

// with is l counting probe on screen.
func (l promptLook) with(screen, probe string) promptLook {
	on := screenPrompt(screen, probe)
	l.seen, l.typed = on.seen, on.typed
	return l
}

// promptMark is where the agent's log ends, read before a send; ok is false
// for an agent without one.
type promptMark struct {
	size int64
	ok   bool
}

func (h *Herdr) logMark(ctx context.Context, pane string) promptMark {
	if h.logged == nil || pane == "" {
		return promptMark{}
	}
	size, _, ok := h.logged(ctx, pane, "", 0)
	return promptMark{size, ok}
}

// inLog looks for probe past mark, then moves mark up to what it read, less
// enough to still find a probe the log was midway through writing.
func (h *Herdr) inLog(ctx context.Context, pane, probe string, mark *promptMark) bool {
	ctx, cancel := context.WithTimeout(ctx, promptLogWait)
	defer cancel()
	size, found, ok := h.logged(ctx, pane, probe, mark.size)
	if ok && !found {
		mark.size = max(mark.size, size-int64(len(jsonInner(probe))))
	}
	return found
}

// awaitPrompt polls the pane for promptConfirmWait, returning as soon as its
// log or its screen shows the message taken.
func (h *Herdr) awaitPrompt(ctx context.Context, pane, probe string, before promptLook, mark promptMark) (promptVerdict, promptLook) {
	v, last := promptUnknown, promptLook{}
	for until := time.Now().Add(promptConfirmWait); ; {
		if sleepCtx(ctx, promptPoll) != nil {
			return v, last
		}
		if mark.ok && h.inLog(ctx, pane, probe, &mark) {
			return promptIn, last
		}
		if now, _, ok := h.lookPrompt(ctx, pane, probe); ok {
			v, last = judgePrompt(before, now), now
			if v == promptIn {
				return v, last
			}
		}
		if time.Now().After(until) {
			return v, last
		}
	}
}

// judgePrompt compares a pane before and after a send. The agent changing
// state (starting a turn, or one so short it is already over: herdr counts
// each change in state_change_seq) or the words appearing once more above
// the input box mean it took the message. Mid-turn the state says nothing
// and a queued message may be drawn anywhere, so missing words there are not
// taken as a lost message.
func judgePrompt(before, now promptLook) promptVerdict {
	switch {
	case now.seq != before.seq, now.status != before.status && now.status != "unknown":
		return promptIn
	case now.typed && !before.typed:
		return promptTyped
	case now.seen > before.seen:
		return promptIn
	case before.status == "working" || now.status == "working":
		return promptUnknown
	}
	return promptMissing
}

// emptyBoxRe is an empty input box: herdr reads it back without its trailing
// space, as a bare "❯". Missing it took the message's own echo above for the
// box, and a message taken for one left in it.
var emptyBoxRe = regexp.MustCompile(`^\s*(?:[│┃|]\s*)?[❯›>]\s*$`)

// screenPrompt finds the input box (the last prompt line) on a screen and
// whether it, or the lines above it, show probe.
func screenPrompt(screen, probe string) promptLook {
	lines := strings.Split(screen, "\n")
	box := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if (promptLineRe.MatchString(lines[i]) || emptyBoxRe.MatchString(lines[i])) && !menuLineRe.MatchString(lines[i]) {
			box = i
			break
		}
	}
	var l promptLook
	above := lines
	if box >= 0 {
		in := lines[box]
		l.typed = (probe != "" && strings.Contains(in, probe)) ||
			strings.Contains(in, "[Pasted text") || strings.Contains(in, "[Pasted Content")
		above = lines[:box]
	}
	if probe != "" {
		for _, s := range above {
			l.seen += strings.Count(s, probe)
		}
	}
	return l
}

// promptProbe is a few words of text as an agent draws them back: the start
// of a line, short enough not to wrap. The first line not already on screen
// is taken, so a header every message has ("[collie] 1 new event") doesn't
// make an old message look like the new one.
func promptProbe(text, screen string) string {
	first := ""
	for _, s := range strings.Split(text, "\n") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if r := []rune(s); len(r) > promptProbeRunes {
			s = string(r[:promptProbeRunes])
		}
		if !strings.Contains(screen, s) {
			return s
		}
		if first == "" {
			first = s
		}
	}
	return first
}

// submit hands text to herdr. herdr 0.8 refuses agent.prompt while it still
// marks the agent launch_pending, which can stick for good; once herdr
// reports the agent idle the terminal is ready, so paste into the pane and
// press enter instead.
func (h *Herdr) submit(ctx context.Context, target, pane, text string) error {
	err := h.Call(ctx, "agent.prompt", map[string]any{"target": target, "text": text}, nil)
	var he *HerdrError
	if err == nil || !errors.As(err, &he) || (he.Code != "agent_not_found" && he.Code != "agent_not_ready") {
		return err
	}
	if pane == "" {
		if pane = h.paneOfName(ctx, target); pane == "" {
			return err
		}
	}
	var info struct {
		Agent struct {
			Status string `json:"agent_status"`
		} `json:"agent"`
	}
	if h.Call(ctx, "agent.get", map[string]any{"target": pane}, &info) != nil {
		return err
	}
	switch info.Agent.Status {
	case "idle", "done":
	case "working", "blocked":
		return &HerdrError{Code: "agent_blocked", Message: "agent is " + info.Agent.Status}
	default:
		return err // still starting: let the caller retry
	}
	// herdr 0.8 also says "idle" while a dialog (folder trust, a permission
	// question) is up; pasting there would answer it. Look first.
	if h.ScreenAsks(ctx, pane) {
		return &HerdrError{Code: "agent_blocked", Message: "agent is showing a question"}
	}
	return h.paste(ctx, pane, text)
}

// paste types text into a pane as one bracketed paste, which keeps
// multi-line text one message, and presses enter.
func (h *Herdr) paste(ctx context.Context, pane, text string) error {
	if err := h.SendText(ctx, pane, "\x1b[200~"+text+"\x1b[201~"); err != nil {
		return err
	}
	if err := sleepCtx(ctx, 300*time.Millisecond); err != nil {
		return err
	}
	return h.SendKeys(ctx, pane, "enter")
}
