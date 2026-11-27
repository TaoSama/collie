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
		if t.Source == "chat" || t.Source == inboxDraftSource {
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

func withoutLarkRequests(list []*Trigger) []*Trigger {
	return slices.DeleteFunc(list, func(t *Trigger) bool { return t.Source == larkSource })
}

// A Feishu request or a dedicated draft needs a turn of its own.
func deliveryBatch(list []*Trigger, dedicated bool) []*Trigger {
	for i, t := range list {
		if t.Source == larkSource || (dedicated && t.Source == inboxDraftSource) {
			if i == 0 {
				return list[:1]
			}
			return list[:i]
		}
	}
	return list
}

// deliveryPrompt writes the policy outside the portion that can spill.
func (sp *Super) deliveryPrompt(target string, list []*Trigger, lark larkDelivery, fresh bool, words int) string {
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
		fmt.Fprintf(&b, "\n#%s · ", t.ID)
		if lark.mode != "" {
			b.WriteString("ext:larkx")
		} else {
			fmt.Fprintf(&b, "%s · %s", t.Thread, t.Source)
			if t.From != "" {
				fmt.Fprintf(&b, " · from %s", t.From)
			}
		}
		if t.Action == "candidate" {
			if lark.mode == larkRestricted {
				b.WriteString(" · candidate（仅在已明确允许本会话处理时执行）")
			} else {
				b.WriteString(" · candidate (act only if a rule or memory says so; otherwise stay silent)")
			}
		}
		fmt.Fprintf(&b, " · %s\n", t.At.Format("15:04"))
		text := t.Text
		if lark.mode != "" {
			lark.writePrompt(&b, t, target)
			text = larkPromptData(text)
		}
		text = strings.TrimSpace(text)
		if len([]rune(text)) > 700 { // Inline & Spill
			text = string([]rune(text)[:700]) + fmt.Sprintf(" … [spilled; run `collie show %s`]", t.ID)
		}
		b.WriteString(indent(text, "  "))
		b.WriteString("\n")
	}
	if target == routerName {
		fmt.Fprintf(&b, "\nReply with `collie reply`, usually ≤%d characters.", words)
	}

	return b.String()
}

func (sp *Super) prepareDelivery(target, pane string, list []*Trigger) (larkDelivery, bool, int, bool) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	lark, ok := sp.larkDeliveryLocked(list[0]) // a Feishu batch contains only its own event
	if !ok {
		sp.cancelLarkDeliveryLocked(list[0])
		return larkDelivery{}, false, 0, false
	}
	if !sp.beginDeliveryLocked(target, pane, list) {
		return larkDelivery{}, false, 0, false
	}
	if lark.mode != "" {
		t := list[0]
		log.Printf("larkx guard policy=%s submit trigger=%s thread=%q target=%q actor=%s mode=%s grant=%d",
			larkPolicyVersion, t.ID, t.Thread, target, lark.actor, lark.mode, lark.grant)
	}
	fresh := target == routerName && sp.routerFresh && lark.mode != larkRestricted
	return lark, fresh, sp.st.Settings.ReplyChars, true
}

// deliver submits a pending batch for one session as a single prompt.
func (sp *Super) deliver(ctx context.Context, target string, list []*Trigger) {
	sort.Slice(list, func(i, j int) bool { return list[i].At.Before(list[j].At) })
	list = deliveryBatch(list[:min(len(list), 20)], target != routerName)
	h := sp.s.herdr
	pane := h.paneOfName(ctx, sp.herdrTarget(target))
	if pane == "" || len(list) == 0 {
		return
	}
	lark, fresh, words, ok := sp.prepareDelivery(target, pane, list)
	if !ok {
		return
	}
	err := h.Prompt(ctx, pane, sp.deliveryPrompt(target, list, lark, fresh, words))
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
