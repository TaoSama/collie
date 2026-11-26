package collie

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
)

// quoteLine is a Quote as one line: "你：首页加一个暗色模式…".
func (q *Quote) line() string {
	t := oneLine(q.Text)
	return q.Who + "：" + clip(t, 60)
}

// quoteLocked finds what a reply's --re points at: an event Collie was given
// (#t942), or an entry. Unknown ids give nil: the reply goes out anyway.
func (sp *Super) quoteLocked(id string) *Quote {
	id = strings.TrimPrefix(strings.TrimSpace(id), "#")
	if id == "" {
		return nil
	}
	entry := func(eid string) *Quote {
		for i := len(sp.st.Entries) - 1; i >= 0; i-- {
			if e := sp.st.Entries[i]; e.ID == eid && e.Role == "user" {
				return &Quote{Entry: e.ID, Who: "你", Text: humanText(e.Text)}
			}
		}
		return nil
	}
	if q := entry(id); q != nil {
		return q
	}
	t := sp.st.trigger(id)
	if t == nil {
		return nil
	}
	switch t.Source {
	case "chat":
		if q := entry(t.Ref); q != nil {
			return q
		}
		return &Quote{Who: "你", Text: humanText(t.Text)}
	case "timer":
		return &Quote{Who: "定时", Text: t.Text}
	}
	name := t.Ref
	if name == "" {
		name = t.From
	}
	if w := sp.st.Workers[name]; w != nil {
		return &Quote{Worker: w.Name, Who: w.Name, Text: w.Task}
	}
	return nil
}

// sayQuoteLocked is a chat message the human answers: one of Collie's, or
// one of their own when a run of replies last ended on what they said.
func (sp *Super) sayQuoteLocked(id string) *Quote {
	if id == "" {
		return nil
	}
	e, _ := sp.entryLocked(id)
	switch {
	case e == nil:
		return nil
	case e.Role == "collie":
		return &Quote{Entry: e.ID, Who: "Collie", Text: clip(e.Text, 600)}
	case e.Role == "user":
		return &Quote{Entry: e.ID, Who: "你", Text: clip(humanText(e.Text), 600)}
	}
	return nil
}

// pendingMaxAge bounds unanswered quick-option prompts so forgotten topics from
// past weeks never pile up as stale badges.
const pendingMaxAge = 7 * 24 * time.Hour

// PendingItem is one Collie message in chat:main (--opt or --check) that still
// awaits the human's answer, acknowledgement or dismissal.
type PendingItem struct {
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Summary string    `json:"summary"`
	Options []string  `json:"options,omitempty"`
	Check   bool      `json:"check,omitempty"`
	Task    string    `json:"task,omitempty"` // the worker it is about, or Collie
}

// PinnedItem is one message in chat:main manually pinned by the human. Unlike
// PendingItem, pins never expire on a 7-day timer and stay until unpinned.
type PinnedItem struct {
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Summary string    `json:"summary"`
	Role    string    `json:"role"`
}

func pinnableEntry(e *Entry) bool {
	return e != nil && e.ID != "" && (e.Thread == "" || e.Thread == mainThread) && (e.Role == "collie" || e.Role == "user")
}

func (sp *Super) entryForPinLocked(id string) *Entry {
	for i := len(sp.st.Entries) - 1; i >= 0; i-- {
		if e := sp.st.Entries[i]; e.ID == id {
			if pinnableEntry(e) {
				return e
			}
			return nil
		}
	}
	if e, ok := sp.pinnedCache[id]; ok {
		return e
	}
	if sp.db != nil {
		var raw string
		var e Entry
		if sp.db.QueryRow(`SELECT data FROM entries WHERE id = ?`, id).Scan(&raw) == nil &&
			json.Unmarshal([]byte(raw), &e) == nil && pinnableEntry(&e) {
			if sp.pinnedCache == nil {
				sp.pinnedCache = map[string]*Entry{}
			}
			sp.pinnedCache[id] = &e
			return &e
		}
	}
	return nil
}

// Pin idempotently pins or unpins one chat:main message and persists it.
func (sp *Super) Pin(id string, pinned bool) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("id is required")
	}
	sp.mu.Lock()
	idx := slices.Index(sp.st.Pinned, id)
	changed := false
	if pinned {
		if idx < 0 {
			if sp.entryForPinLocked(id) == nil {
				sp.mu.Unlock()
				return errors.New("找不到这条可钉住的消息")
			}
			sp.st.Pinned = append(sp.st.Pinned, id)
			if n := len(sp.st.Pinned); n > maxPinned {
				for _, drop := range sp.st.Pinned[:n-maxPinned] {
					delete(sp.pinnedCache, drop)
				}
				sp.st.Pinned = append([]string(nil), sp.st.Pinned[n-maxPinned:]...)
			}
			changed = true
		}
	} else if idx >= 0 {
		sp.st.Pinned = slices.Delete(sp.st.Pinned, idx, idx+1)
		delete(sp.pinnedCache, id)
		changed = true
	}
	if changed {
		sp.changed()
	}
	hasDB := sp.db != nil
	sp.mu.Unlock()
	if changed && hasDB {
		sp.flush()
	}
	return nil
}

func (sp *Super) pinnedLocked() []PinnedItem {
	if len(sp.st.Pinned) == 0 {
		return []PinnedItem{}
	}
	out := make([]PinnedItem, 0, len(sp.st.Pinned))
	for _, id := range sp.st.Pinned {
		e := sp.entryForPinLocked(id)
		if e == nil {
			continue
		}
		sum := pendingSummary(e.Text, e.Options)
		if e.Role == "user" {
			sum = pendingSummary(humanText(e.Text), nil)
		}
		out = append(out, PinnedItem{
			ID:      e.ID,
			At:      e.At,
			Summary: sum,
			Role:    e.Role,
		})
	}
	return out
}

// Dismiss marks one pending message as acknowledged/ignored by the human and persists it.
func (sp *Super) Dismiss(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	sp.mu.Lock()
	changed := sp.dismissLocked(id)
	hasDB := sp.db != nil
	sp.mu.Unlock()
	if changed && hasDB {
		sp.flush()
	}
}

func (sp *Super) dismissLocked(id string) bool {
	if sp.st.Dismissed == nil {
		sp.st.Dismissed = map[string]bool{}
	}
	if sp.st.Dismissed[id] {
		return false
	}
	sp.st.Dismissed[id] = true
	sp.changed()
	return true
}

// pendingLocked computes unanswered --opt and --check Collie messages in chat:main.
// An item resolves when:
//  1. the human tapped「已看」or × (/api/super/dismiss),
//  2. the human tapped one of its options or quoted it (re.entry == e.id),
//  3. for --opt only: the human sent a plain unquoted message right after it
//     with no other role=="collie" reply in between (system/worker events do
//     not interrupt). A --check waits for 1 or 2: a quick message after it is
//     often about something else.
//
// Ones older than the entries in memory are held (pending_hold.go).
func (sp *Super) pendingLocked(now time.Time) []PendingItem {
	var out []PendingItem
	seen := map[string]bool{}
	for i, e := range sp.st.Entries {
		if sp.waitsLocked(i, now) {
			out = append(out, pendingItem(e))
			seen[e.ID] = true
		}
	}
	var held []PendingItem
	quoted := map[string]*Entry(nil)
	if len(sp.st.Held) > 0 {
		quoted = sp.quotesLocked()
	}
	for _, id := range sp.st.Held {
		if e := sp.heldEntryLocked(id); e != nil && !seen[id] && sp.heldWaitsLocked(e, now, quoted) {
			held = append(held, pendingItem(e))
		}
	}
	return append(held, out...)
}

func pendingItem(e *Entry) PendingItem {
	opts := cleanOptions(e.Options)
	return PendingItem{
		ID:      e.ID,
		At:      e.At,
		Summary: pendingSummary(e.Text, opts),
		Options: opts,
		Check:   e.Check,
		Task:    entryTask(e),
	}
}

// replyLocked is your message that settles the --opt/--check message at
// Entries[i], by rules 2 and 3 above; nil while there is none.
func (sp *Super) replyLocked(i int) *Entry {
	e, otherCollie := sp.st.Entries[i], false
	for _, next := range sp.st.Entries[i+1:] {
		if next.Thread != "" && next.Thread != mainThread {
			continue
		}
		switch next.Role {
		case "user":
			reID := ""
			if next.Re != nil {
				reID = next.Re.Entry
			}
			if reID == e.ID || (reID == "" && !otherCollie && !e.Check) {
				return next
			}
		case "collie":
			otherCollie = true
		}
	}
	return nil
}

var (
	pendingFenceRe = regexp.MustCompile("(?s)```.*?```")
	pendingImgRe   = regexp.MustCompile(`\[image:[^\]]+\]`)
	pendingLinkRe  = regexp.MustCompile(`\[([^\]\n]+)\]\([^)\n]+\)`)
	pendingListRe  = regexp.MustCompile(`^\s*(?:[#>*•\-]+|\d+[.)])\s+`)
)

func pendingSummary(text string, opts []string) string {
	s := pendingFenceRe.ReplaceAllString(text, " ")
	s = pendingImgRe.ReplaceAllString(s, " ")
	s = pendingLinkRe.ReplaceAllString(s, "$1")
	var parts []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(pendingListRe.ReplaceAllString(l, ""))
		l = strings.TrimSpace(mdMarks.Replace(l))
		if l != "" {
			parts = append(parts, l)
		}
	}
	out := oneLine(strings.Join(parts, " "))
	if out == "" {
		out = strings.Join(opts, " / ")
	}
	if out == "" {
		out = oneLine(text)
	}
	return clip(out, 120)
}

// humanText is your message as you wrote it: without the attachment list and
// the notes the hub adds for Collie.
func humanText(t string) string {
	if i := strings.Index(t, "\n\n[The human picked"); i >= 0 {
		t = t[:i]
	}
	if i := strings.LastIndex(t, "附件：\n"); i >= 0 {
		t = strings.TrimSpace(t[:i]) + " 📎"
	}
	return strings.TrimSpace(t)
}
