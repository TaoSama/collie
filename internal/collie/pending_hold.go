package collie

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// What Collie asked the human (--opt, --check) stays pending until they
// answer it, however much is said after: one that falls out of the entries
// kept in memory is held by ID (State.Held, saved with the rest). Those
// still waiting after a day come back once a day, as one reminder.

const (
	maxHeld = 200
	// remindAge: how long something waits before the daily reminder lists it.
	remindAge = 24 * time.Hour
	// remindHour: the reminder goes out once a day, from this hour on (the
	// hub's local time, like timers).
	remindHour = 10
	// remindList: summaries the reminder spells out; the rest are counted.
	remindList = 5
	// remindLookEvery: how often to look again for what has waited a day.
	remindLookEvery = 10 * time.Minute
)

// askEntry: a message of Collie's in the main chat that asks the human something.
func askEntry(e *Entry) bool {
	return e != nil && e.ID != "" && e.Role == "collie" && (e.Thread == "" || e.Thread == mainThread) &&
		(len(cleanOptions(e.Options)) > 0 || e.Check)
}

// askExpired: a --opt question nobody answered for a week is dropped; a
// --check waits until it is seen.
func askExpired(e *Entry, now time.Time) bool {
	return !e.Check && !now.IsZero() && !e.At.IsZero() && now.Sub(e.At) > pendingMaxAge
}

// waitsLocked: the message at Entries[i] asks and is still unsettled.
func (sp *Super) waitsLocked(i int, now time.Time) bool {
	e := sp.st.Entries[i]
	return askEntry(e) && !sp.st.Dismissed[e.ID] && !askExpired(e, now) && sp.replyLocked(i) == nil
}

// heldWaitsLocked is waitsLocked for a held message: what answers it now is
// the human quoting it (an option tapped quotes it too) or dismissing it.
// quoted is quotesLocked().
func (sp *Super) heldWaitsLocked(e *Entry, now time.Time, quoted map[string]*Entry) bool {
	return !sp.st.Dismissed[e.ID] && !askExpired(e, now) && quoted[e.ID] == nil
}

// quotesLocked maps each entry the human quoted in memory to their message.
func (sp *Super) quotesLocked() map[string]*Entry {
	out := map[string]*Entry{}
	for _, e := range sp.st.Entries {
		if e.Role == "user" && e.Re != nil && e.Re.Entry != "" && (e.Thread == "" || e.Thread == mainThread) {
			if _, seen := out[e.Re.Entry]; !seen {
				out[e.Re.Entry] = e
			}
		}
	}
	return out
}

// heldEntryLocked reads a held message, from SQLite the first time (nil,
// remembered, if it is gone or no longer asks).
func (sp *Super) heldEntryLocked(id string) *Entry {
	if e, ok := sp.heldCache[id]; ok {
		return e
	}
	e, _ := sp.entryLocked(id)
	if !askEntry(e) {
		e = nil
	}
	if sp.heldCache == nil {
		sp.heldCache = map[string]*Entry{}
	}
	sp.heldCache[id] = e
	return e
}

func (sp *Super) heldLocked(id string) bool {
	return slices.Contains(sp.st.Held, id)
}

// holdPendingLocked runs before the entries are trimmed: the messages about
// to go that still wait are held, and held ones answered since are let go.
func (sp *Super) holdPendingLocked(now time.Time) {
	var keep []string
	seen, quoted := map[string]bool{}, sp.quotesLocked()
	for _, id := range sp.st.Held {
		if e := sp.heldEntryLocked(id); e != nil && !seen[id] && sp.heldWaitsLocked(e, now, quoted) {
			keep, seen[id] = append(keep, id), true
		} else {
			delete(sp.heldCache, id)
		}
	}
	for i := 0; i < len(sp.st.Entries)-keepEntries; i++ {
		if e := sp.st.Entries[i]; sp.waitsLocked(i, now) && !seen[e.ID] {
			keep, seen[e.ID] = append(keep, e.ID), true
		}
	}
	if n := len(keep); n > maxHeld {
		for _, id := range keep[:n-maxHeld] {
			delete(sp.heldCache, id)
		}
		keep = keep[n-maxHeld:]
	}
	if !slices.Equal(keep, sp.st.Held) {
		sp.st.Held = keep
		sp.dirty = true
	}
}

// remindPendingLocked tells the human, once a day, what has waited on them
// for more than a day: one event that lists them and links to the oldest.
func (sp *Super) remindPendingLocked(now time.Time) {
	y, m, d := now.Date()
	if now.Hour() < remindHour || now.Before(sp.remindLookAt) || !sp.st.RemindedAt.Before(time.Date(y, m, d, remindHour, 0, 0, 0, now.Location())) {
		return
	}
	sp.remindLookAt = now.Add(remindLookEvery) // nothing old enough yet: not every tick
	var old []PendingItem
	for _, p := range sp.pendingLocked(now) {
		if now.Sub(p.At) >= remindAge {
			old = append(old, p)
		}
	}
	if len(old) == 0 {
		return
	}
	sp.st.RemindedAt = now
	var sums []string
	for _, p := range old[:min(len(old), remindList)] {
		sums = append(sums, clip(p.Summary, 40))
	}
	text := fmt.Sprintf("有 %d 件事等你超过一天：%s", len(old), strings.Join(sums, "；"))
	if len(old) > remindList {
		text += fmt.Sprintf(" 等 %d 件", len(old))
	}
	sp.appendEntryLocked(&Entry{ID: sp.st.nextID("e"), Role: "event", Thread: mainThread, Text: text, At: now, Ref: old[0].ID, Tone: toneRemind})
}

// toneRemind marks the daily reminder; Ref is the oldest thing it lists.
const toneRemind = "remind"
