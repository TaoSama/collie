package collie

import (
	"fmt"
	"log"
	"math"
	"slices"
	"strings"
)

const showThreadPage = 15

// showThreadLocked lists a thread's triggers older than trigger number
// `before` (0 for the newest). They come from the DB, since memory keeps only
// a thread's latest ids; the caller flushes first so the DB is current.
func (sp *Super) showThreadLocked(id string, before int64) (string, bool) {
	th := sp.st.Threads[id]
	var list []*Trigger
	var total int
	var more bool
	fromDB := false
	if sp.db != nil {
		var err error
		if list, total, more, err = sp.threadPageLocked(id, before); err != nil {
			log.Printf("show thread %s: %v", id, err)
		} else {
			fromDB = true
		}
	}
	if !fromDB && th != nil {
		list, total = sp.memoryThreadPageLocked(th), len(th.Triggers)
	}
	if th == nil && total == 0 {
		return "", false
	}
	var b strings.Builder
	var subscribers []string
	if th != nil {
		subscribers = th.Subscribers
	}
	fmt.Fprintf(&b, "thread %s\nsubscribers: %s\n", id, strings.Join(subscribers, ", "))
	if before > 0 {
		fmt.Fprintf(&b, "triggers: %d in total; before #t%d:\n", total, before)
	} else {
		fmt.Fprintf(&b, "triggers: %d in total; recent:\n", total)
	}
	for _, t := range list {
		fmt.Fprintf(&b, "  #%s %s %s: %s\n", t.ID, t.At.Format("01-02 15:04"), t.Source, clip(firstLine(t.Text), 90))
	}
	if more {
		fmt.Fprintf(&b, "older: collie show %s --before %s\n", id, list[0].ID)
	}
	return b.String(), true
}

// threadPageLocked reads one page from the DB, oldest first.
func (sp *Super) threadPageLocked(id string, before int64) (list []*Trigger, total int, more bool, err error) {
	counts, err := threadTriggerCounts(sp.db, []string{id})
	if err != nil {
		return nil, 0, false, err
	}
	if before <= 0 {
		before = math.MaxInt64
	}
	list, err = threadHistory(sp.db, id, before, showThreadPage+1)
	if err != nil {
		return nil, 0, false, err
	}
	if more = len(list) > showThreadPage; more {
		list = list[:showThreadPage]
	}
	slices.Reverse(list)
	return list, counts[id], more, nil
}

// memoryThreadPageLocked is the fallback without a DB: the latest ids memory has.
func (sp *Super) memoryThreadPageLocked(th *Thread) []*Trigger {
	ids := th.Triggers
	if len(ids) > showThreadPage {
		ids = ids[len(ids)-showThreadPage:]
	}
	var out []*Trigger
	for _, tid := range ids {
		if t := sp.st.trigger(tid); t != nil {
			out = append(out, t)
		}
	}
	return out
}
