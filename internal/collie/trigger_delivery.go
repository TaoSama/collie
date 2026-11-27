package collie

import (
	"context"
	"fmt"
	"log"
	"maps"
	"strings"
	"time"
)

// The terminal cannot take part in the SQLite transaction. Record the pane
// first so a restart in between can look for the event before acting on it.
func (sp *Super) beginDeliveryLocked(target, pane string, list []*Trigger) bool {
	var worker *Worker
	if w := sp.st.Workers[target]; w != nil && w.Conv != "" {
		if w.DraftTrigger != "" {
			// An unconfirmed attempt can still produce a reply. Keep its
			// ownership until the turn finishes or an idle worker takes new work.
			previous, err := sp.deliveryTriggerLocked(w.DraftTrigger)
			if err != nil {
				log.Printf("worker %s: read draft trigger %s: %v", w.Name, w.DraftTrigger, err)
				return false
			}
			if previous == nil || previous.Status != "failed" || (w.Status != "idle" && w.Status != "done") {
				return false
			}
		}
		draft := ""
		if list[0].Source == inboxDraftSource {
			if w.Status != "idle" && w.Status != "done" {
				return false
			}
			draft = list[0].ID
		}
		if w.DraftTrigger != draft {
			next := *w
			next.DraftTrigger = draft
			worker = &next
		}
	}
	attempt := deliveryAttempt{Pane: pane, At: time.Now()}
	return sp.updateDeliveryStateLocked(list, nil, worker, func(t *Trigger) {
		if t.Deliveries == nil {
			t.Deliveries = map[string]deliveryAttempt{}
		}
		t.Deliveries[target] = attempt
	})
}

func (sp *Super) finishDeliveryLocked(target string, list []*Trigger, retry bool, failure string) {
	if len(list) == 0 {
		return
	}
	var notice *Entry
	if failure != "" {
		ids := make([]string, len(list))
		for i, t := range list {
			ids[i] = "#" + t.ID
		}
		who := target
		if target == routerName {
			who = "Collie"
		}
		notice = &Entry{ID: sp.st.nextID("e"), Role: "event", Thread: list[0].Thread, At: time.Now(), Ref: target, Tone: "error",
			Text: fmt.Sprintf("没能把消息送进 %s 的终端（%s）：%s", who, strings.Join(ids, " "), failure)}
	}
	var worker *Worker
	if w := sp.st.Workers[target]; retry && w != nil && w.DraftTrigger == list[0].ID {
		next := *w
		next.DraftTrigger = ""
		worker = &next
	}
	if !sp.updateDeliveryStateLocked(list, notice, worker, func(t *Trigger) {
		delete(t.Deliveries, target)
		if retry {
			return
		}
		t.Targets = remove(t.Targets, target)
		if len(t.Targets) == 0 {
			t.Status = "delivered"
			if failure != "" {
				t.Status = "failed"
			}
		}
	}) {
		return
	}
	if notice != nil {
		sp.appendEntryLocked(notice)
	} else {
		sp.changed()
	}
}

func (sp *Super) deliveryTriggerLocked(id string) (*Trigger, error) {
	t := sp.st.trigger(id)
	if t == nil && sp.db != nil {
		return triggerByID(sp.db, id)
	}
	return t, nil
}

func (sp *Super) updateDeliveryLocked(list []*Trigger, notice *Entry, update func(*Trigger)) bool {
	return sp.updateDeliveryStateLocked(list, notice, nil, update)
}

func (sp *Super) updateDeliveryStateLocked(list []*Trigger, notice *Entry, worker *Worker, update func(*Trigger)) bool {
	updated := make([]Trigger, len(list))
	for i, t := range list {
		updated[i] = *t
		updated[i].Deliveries = maps.Clone(t.Deliveries)
		update(&updated[i])
	}
	if err := sp.saveDeliveryStateLocked(updated, notice, worker); err != nil {
		log.Printf("save delivery for %s: %v", list[0].ID, err)
		return false
	}
	for i, t := range list {
		t.Deliveries = updated[i].Deliveries
		t.Targets, t.Status = updated[i].Targets, updated[i].Status
	}
	if worker != nil {
		sp.st.Workers[worker.Name].DraftTrigger = worker.DraftTrigger
	}
	return true
}

func (sp *Super) saveDeliveryLocked(list []Trigger, notice *Entry) error {
	return sp.saveDeliveryStateLocked(list, notice, nil)
}

func (sp *Super) saveDeliveryStateLocked(list []Trigger, notice *Entry, worker *Worker) error {
	tx, err := sp.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := range list {
		if err := writeTrigger(tx, &list[i]); err != nil {
			return err
		}
	}
	if notice != nil {
		if err := writeStateSeq(tx, sp.st.Seq); err != nil {
			return err
		}
		if err := writeEntry(tx, notice); err != nil {
			return err
		}
	}
	if worker != nil {
		if err := writeWorker(tx, worker); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// inLog: the tail of pane's agent log holds probe. A trigger's id is never
// sent twice, so no mark from before the send is needed.
func (sp *Super) inLog(ctx context.Context, pane, probe string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, found, _ := sp.s.promptLogged(ctx, pane, probe, -promptLogTail)
	return found
}

func (sp *Super) recoverDeliveries(ctx context.Context, away map[string]bool, live map[string]liveAgent) {
	type destination struct{ target, pane string }
	pending := map[destination][]*Trigger{}
	sp.mu.Lock()
	for _, t := range sp.st.Triggers {
		for target, attempt := range t.Deliveries {
			if !away[machineOf(attempt.Pane)] {
				dest := destination{target, attempt.Pane}
				pending[dest] = append(pending[dest], t)
			}
		}
	}
	sp.mu.Unlock()

	for dest, triggers := range pending {
		if ctx.Err() != nil {
			return
		}
		readCtx, cancel := context.WithTimeout(ctx, time.Second)
		res, err := sp.s.herdr.Read(readCtx, dest.pane, "recent_unwrapped", "text", promptReadLines)
		cancel()
		if ctx.Err() != nil {
			return
		}
		var confirmed, uncertain []*Trigger
		for _, t := range triggers {
			if err != nil && live[dest.target].Pane == dest.pane && time.Since(t.Deliveries[dest.target].At) < awayGrace {
				continue // the pane is still present, so try reading it again next tick
			}
			probe := "#" + t.ID + " · "
			if (err == nil && screenPrompt(res.Text, probe).seen > 0) || sp.inLog(ctx, dest.pane, probe) {
				confirmed = append(confirmed, t)
			} else {
				uncertain = append(uncertain, t)
			}
		}
		// A missing line could have scrolled off. Keep the uncertainty visible
		// in the conversation and leave that target out of automatic delivery.
		sp.mu.Lock()
		sp.finishDeliveryLocked(dest.target, confirmed, false, "")
		sp.finishDeliveryLocked(dest.target, uncertain, false, "Hub 重启或写入中断后无法确认终端是否收到，请核对后再决定是否重发")
		sp.mu.Unlock()
	}
}
