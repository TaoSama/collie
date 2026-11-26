package collie

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"time"
)

// targetMachine is the computer a delivery target runs on.
func (sp *Super) targetMachine(target string) string {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if target == routerName {
		return sp.routerMachineLocked()
	}
	if w := sp.st.Workers[target]; w != nil {
		return w.Machine
	}
	return ""
}

// fromHuman keeps what the human said in the chat.
func fromHuman(list []*Trigger) []*Trigger {
	var out []*Trigger
	for _, t := range list {
		if t.Source == "chat" {
			out = append(out, t)
		}
	}
	return out
}

func (sp *Super) rerouteToRouter(target string, list []*Trigger) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	w := sp.st.Workers[target]
	if w != nil && !w.Ended && (w.Status == "starting" || time.Since(w.CreatedAt) < 2*time.Minute) {
		return // still launching
	}
	for _, t := range list {
		t.Targets = replace(t.Targets, target, routerName)
	}
	sp.changed()
}

func replace(list []string, from, to string) []string {
	out := []string{}
	for _, x := range list {
		if x == from {
			x = to
		}
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// deliver submits all pending triggers for one session as a single prompt.
func (sp *Super) deliver(ctx context.Context, target string, list []*Trigger) {
	sort.Slice(list, func(i, j int) bool { return list[i].At.Before(list[j].At) })
	if len(list) > 20 {
		list = list[:20]
	}
	sp.mu.Lock()
	fresh := target == routerName && sp.routerFresh
	words := sp.st.Settings.ReplyChars
	sp.mu.Unlock()

	var b strings.Builder
	if fresh {
		b.WriteString(sp.sessionStart())
		b.WriteString("\n")
	}
	if len(list) == 1 {
		b.WriteString("[collie] 1 new event\n")
	} else {
		fmt.Fprintf(&b, "[collie] %d new events (batched)\n", len(list))
	}
	for _, t := range list {
		fmt.Fprintf(&b, "\n#%s · %s · %s", t.ID, t.Thread, t.Source)
		if t.From != "" {
			fmt.Fprintf(&b, " · from %s", t.From)
		}
		if t.Action == "candidate" {
			b.WriteString(" · candidate (act only if a rule or memory says so; otherwise stay silent)")
		}
		fmt.Fprintf(&b, " · %s\n", t.At.Format("15:04"))
		text := strings.TrimSpace(t.Text)
		if len([]rune(text)) > 700 { // Inline & Spill
			text = string([]rune(text)[:700]) + fmt.Sprintf(" … [spilled; run `collie show %s`]", t.ID)
		}
		b.WriteString(indent(text, "  "))
		b.WriteString("\n")
	}
	if target == routerName {
		fmt.Fprintf(&b, "\nReply with `collie reply`, usually ≤%d characters.", words)
	}

	h := sp.s.herdr
	pane := h.paneOfName(ctx, sp.herdrTarget(target))
	if pane == "" || !sp.beginDelivery(target, pane, list) {
		return
	}
	err := h.Prompt(ctx, pane, b.String())
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if err != nil {
		var he *HerdrError
		switch {
		case errors.As(err, &he) && he.Code == "prompt_unconfirmed":
			sp.finishDeliveryLocked(target, list, false, he.Message)
		case ctx.Err() != nil && he == nil:
			// Shutdown or a deadline can interrupt the read-back after submission.
			// Keep the journal intact so the next tick can check the terminal.
			log.Printf("deliver to %s: %v", target, err)
		default:
			if he == nil || (he.Code != "agent_blocked" && he.Code != "agent_not_ready") {
				log.Printf("deliver to %s: %v", target, err)
			}
			sp.finishDeliveryLocked(target, list, true, "")
		}
		return
	}
	sp.finishDeliveryLocked(target, list, false, "")
	if w := sp.st.Workers[target]; w != nil {
		w.clearResume() // new words for it: they carry it on
	}
	if fresh {
		sp.routerFresh = false
	}
	if target == routerName && sp.routerAskedAt.IsZero() {
		sp.routerAskedAt = time.Now() // a message mid-turn keeps the turn's start
	}
}

func remove(list []string, v string) []string {
	out := list[:0:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func indent(s, p string) string {
	return p + strings.ReplaceAll(s, "\n", "\n"+p)
}
