package collie

import (
	"encoding/json"
	"sort"
	"time"
)

// Use the original daily batch for linked questions as well. A task only
// changes the eligibility of its own reminder. Its message is still usable.
func (sp *Super) reminderItemsLocked(now time.Time) (old []PendingItem, includesRecent bool) {
	pending := sp.pendingLocked(now)
	if len(sp.st.TaskFollowups) == 0 {
		for _, p := range pending {
			if now.Sub(p.At) >= remindAge {
				old = append(old, p)
			}
		}
		return old, false
	}

	linked := map[string]*TaskFollowup{}
	for _, task := range sp.st.TaskFollowups {
		for _, id := range task.PastDecisions {
			linked[id] = nil
		}
		if task.Decision == "" {
			continue
		}
		linked[task.Decision] = task
		if task.Waiting && task.ClosedAt.IsZero() && !task.NextAt.After(now) {
			if e := sp.taskDecisionLocked(task.Decision, now); e != nil && e.Ask != nil {
				pending = append(pending, PendingItem{ID: e.ID, At: e.At, Summary: entryTask(e) + " 等你确认"})
			}
		}
	}
	for _, p := range pending {
		aged := now.Sub(p.At) >= remindAge
		task, bound := linked[p.ID]
		if bound && (task == nil || !task.ClosedAt.IsZero() || !task.Waiting || task.NextAt.After(now)) {
			continue
		}
		if aged || task != nil && task.Manual {
			old = append(old, p)
			includesRecent = includesRecent || !aged
		}
	}
	sort.SliceStable(old, func(i, j int) bool { return old[i].At.Before(old[j].At) })
	return old, includesRecent
}

// The entry and the day's receipt must survive a restart together before
// the reminder is visible to the browser or the push queue.
func (sp *Super) commitReminderLocked(entry *Entry) error {
	stamp, err := json.Marshal(entry.At)
	if err != nil {
		return err
	}
	tx, err := sp.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := writeStateSeq(tx, sp.st.Seq); err != nil {
		return err
	}
	if err := writeEntry(tx, entry); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO kv(k,v) VALUES('reminded_at',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, string(stamp)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	sp.st.RemindedAt = entry.At
	sp.appendEntryLocked(entry)
	return nil
}
