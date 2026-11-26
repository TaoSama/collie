package collie

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// chatter adds n event lines, enough to push older entries out of memory.
func chatter(sp *Super, n int) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	for i := 0; i < n; i++ {
		sp.addEntryLocked("event", mainThread, fmt.Sprintf("worker-%d 完成了一轮", i), "worker-a", "done")
	}
}

// A --check outlives the entries kept in memory, and a hub restart.
func TestPendingHeldPastTheWindow(t *testing.T) {
	sp, db, call := newPendingTestEnv(t)
	check, _ := sp.Reply(mainThread, "验收报告放好了，请过目。", "", nil, true)
	opt, _ := sp.Reply(mainThread, "部署要现在排队吗？", "", []string{"排", "等等"})
	quoted, _ := sp.Reply(mainThread, "这条稍后被引用回复。", "", nil, true)
	chatter(sp, keepEntries+50)
	sp.flush()
	sp.mu.Lock()
	inMemory := slices.ContainsFunc(sp.st.Entries, func(e *Entry) bool { return e.ID == check.ID })
	sp.mu.Unlock()
	if inMemory {
		t.Fatal("the test needs the --check out of memory")
	}
	want := []string{check.ID, opt.ID, quoted.ID}
	if got := pendingIDs(t, call); !slices.Equal(got, want) {
		t.Fatalf("pending = %v, want %v", got, want)
	}

	// A restart: only the last entries come back, the held ones with them.
	st, err := loadState(db)
	if err != nil {
		t.Fatal(err)
	}
	sp.mu.Lock()
	sp.st, sp.heldCache = st, nil
	sp.mu.Unlock()
	if got := pendingIDs(t, call); !slices.Equal(got, want) {
		t.Fatalf("pending after restart = %v, want %v", got, want)
	}
	// Settled by a quote or by 已看, even from out of memory.
	call("POST", "/api/super/say", map[string]any{"text": "看过了", "re": quoted.ID})
	sp.Dismiss(opt.ID)
	if got := pendingIDs(t, call); !slices.Equal(got, []string{check.ID}) {
		t.Fatalf("pending after a quote and a dismiss = %v, want [%s]", got, check.ID)
	}
	sp.mu.Lock()
	d := sp.decisionLocked(quoted.ID)
	open := sp.decisionLocked(check.ID)
	sp.mu.Unlock()
	if d == nil || d.Status != decisionDone || d.Answer == nil || d.Answer.Choice != "看过了" {
		t.Errorf("the quoted one: %+v", d)
	}
	if open == nil || open.Status != decisionOpen {
		t.Errorf("the held --check opens as still waiting: %+v", open)
	}
	// Once settled, it is let go at the next save; the quote may scroll away.
	chatter(sp, keepEntries+10)
	sp.flush()
	sp.mu.Lock()
	held := slices.Clone(sp.st.Held)
	sp.mu.Unlock()
	if !slices.Equal(held, []string{check.ID}) {
		t.Errorf("held = %v, want [%s]", held, check.ID)
	}
	if got := pendingIDs(t, call); !slices.Equal(got, []string{check.ID}) {
		t.Fatalf("pending = %v, want [%s]", got, check.ID)
	}
}

func TestDailyReminder(t *testing.T) {
	sp, _, _ := newPendingTestEnv(t)
	day := time.Date(2026, 11, 26, 0, 0, 0, 0, time.Local)
	sp.mu.Lock()
	for i, text := range []string{"审查结果 103 条请过目", "部署窗口要不要定在今晚", "这条才问了一小时"} {
		age := 30 * time.Hour
		if i == 2 {
			age = time.Hour
		}
		sp.appendEntryLocked(&Entry{ID: sp.st.nextID("e"), Role: "collie", Thread: mainThread, Text: text,
			At: day.Add(11*time.Hour - age), Check: true})
	}
	reminders := func() []*Entry {
		var out []*Entry
		for _, e := range sp.st.Entries {
			if e.Tone == toneRemind {
				out = append(out, e)
			}
		}
		return out
	}
	sp.remindPendingLocked(day.Add(9 * time.Hour)) // before the hour
	if n := len(reminders()); n != 0 {
		t.Fatalf("reminded at 09:00: %d", n)
	}
	sp.remindPendingLocked(day.Add(11 * time.Hour))
	sp.remindPendingLocked(day.Add(15 * time.Hour)) // the same day: no second one
	got := reminders()
	if len(got) != 1 {
		t.Fatalf("reminders = %d, want 1", len(got))
	}
	if r := got[0]; !strings.HasPrefix(r.Text, "有 2 件事等你超过一天：") || !strings.Contains(r.Text, "审查结果 103 条请过目") ||
		strings.Contains(r.Text, "才问了一小时") || r.Ref != sp.st.Entries[0].ID {
		t.Errorf("reminder: %+v", r)
	}
	sp.remindPendingLocked(day.Add(34 * time.Hour)) // the next day, 10:00
	if n := len(reminders()); n != 2 {
		t.Fatalf("next day: reminders = %d, want 2", n)
	}
	for _, e := range sp.st.Entries {
		if e.Role == "collie" {
			sp.dismissLocked(e.ID)
		}
	}
	sp.remindPendingLocked(day.Add(58 * time.Hour))
	if n := len(reminders()); n != 2 {
		t.Errorf("nothing waits, yet reminded: %d", n)
	}
	sp.mu.Unlock()

	n := noticeNote(notice{tone: toneRemind, text: got[0].Text, remind: got[0].Ref})
	if n == nil || n.Tag != "remind" || n.Hash != "#/d/"+got[0].Ref || n.Answer != nil || len(n.Choices) != 0 {
		t.Errorf("push: %+v", n)
	}
}
