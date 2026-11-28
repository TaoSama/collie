package collie

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const followupTestPath = "tasks/verify-release.md"

func changeTask(t *testing.T, sp *Super, now time.Time, update TaskUpdate) TaskFollowup {
	t.Helper()
	if update.Path == "" {
		update.Path = followupTestPath
	}
	task, err := sp.updateTaskAt(update, now)
	if err != nil {
		t.Fatalf("%s task: %v", update.Action, err)
	}
	return *task
}

func taskTick(sp *Super, now time.Time, answered map[string]bool) TaskFollowup {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.tickTaskFollowupsLocked(now, answered)
	return sp.taskViewLocked(sp.st.TaskFollowups[followupTestPath], now)
}

func TestTaskFollowupMilestonesAndTwoUnproductiveChecks(t *testing.T) {
	sp, _, _ := newPendingTestEnv(t)
	now := time.Now()
	task := changeTask(t, sp, now, TaskUpdate{Action: "start"})
	if !task.NextAt.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("first check = %v", task.NextAt)
	}
	if got := taskTick(sp, task.NextAt.Add(-time.Millisecond), nil); got.Review != "" {
		t.Fatalf("early review: %+v", got)
	}
	first := taskTick(sp, task.NextAt, nil)
	if first.Review == "" || first.Status != "review" || first.Missed != 0 {
		t.Fatalf("first review: %+v", first)
	}
	task = changeTask(t, sp, task.NextAt, TaskUpdate{Action: "review", Version: first.Version, Detail: "no new test result"})
	if task.Missed != 1 || task.Status == "stalled" {
		t.Fatalf("one unsuccessful review: %+v", task)
	}
	second := taskTick(sp, task.NextAt, nil)
	task = changeTask(t, sp, task.NextAt, TaskUpdate{Action: "review", Version: second.Version, Detail: "logs still unchanged"})
	if task.Missed != 2 || task.Status != "stalled" {
		t.Fatalf("two unsuccessful reviews: %+v", task)
	}
	next := taskTick(sp, task.NextAt, nil)
	task = changeTask(t, sp, task.NextAt, TaskUpdate{Action: "review", Version: next.Version, Activity: true, Detail: "CI resumed"})
	if task.Missed != 0 || task.Status != "active" {
		t.Fatalf("verified activity should clear suspected stall: %+v", task)
	}
	task = changeTask(t, sp, task.NextAt, TaskUpdate{Action: "progress", Detail: "first 200 iterations passed"})
	duplicate := changeTask(t, sp, task.LastProgressAt.Add(time.Minute), TaskUpdate{Action: "progress", Detail: "first 200 iterations passed"})
	if duplicate.Version != task.Version || !duplicate.NextAt.Equal(task.NextAt) {
		t.Fatalf("duplicate result reset the time: %+v", duplicate)
	}
	if len(sp.st.Entries) != 0 || len(sp.notifyBuf) != 0 {
		t.Fatal("routine follow-ups created a user notification")
	}

	long := changeTask(t, sp, now, TaskUpdate{Path: "tasks/long.md", Action: "start", Long: true})
	if !long.NextAt.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("first long check = %v", long.NextAt)
	}
	bind := changeTask(t, sp, now.Add(time.Minute), TaskUpdate{Path: long.Path, Action: "review"})
	if !bind.NextAt.Equal(long.NextAt) {
		t.Fatalf("preflight review postponed the first long check: %+v", bind)
	}
	phase := changeTask(t, sp, now.Add(30*time.Minute), TaskUpdate{Path: long.Path, Action: "progress", Detail: "23/40 passed"})
	if !phase.NextAt.Equal(now.Add(90 * time.Minute)) {
		t.Fatalf("long task progression = %+v", phase)
	}
}

func TestTaskFollowupOfflineAndRecovery(t *testing.T) {
	sp, db, _ := newPendingTestEnv(t)
	now := time.Now()
	w := &Worker{Name: "verify", Machine: "devbox", Status: "working", CreatedAt: now}
	sp.st.Workers[w.Name] = w
	sp.changed()
	sp.flush()
	changeTask(t, sp, now, TaskUpdate{Action: "start", Worker: w.Name})
	sp.unanswered = map[string]time.Time{w.Machine: now.Add(time.Minute)}
	off := taskTick(sp, now.Add(time.Minute+awayGrace), map[string]bool{"": true})
	if off.Status != "offline" || !off.NextAt.Equal(now.Add(16*time.Minute)) || off.Review != "" {
		t.Fatalf("offline task = %+v", off)
	}
	if !sp.taskWorkerOfflineLocked(w, map[string]bool{"": true}) {
		t.Fatal("offline worker identity was not protected until reconnect")
	}
	sp.st = restoreTasks(t, db)
	early := taskTick(sp, off.NextAt.Add(-time.Second), map[string]bool{"": true})
	if early.Review != "" || early.Status != "offline" {
		t.Fatalf("offline check ran before 15m: %+v", early)
	}
	check := taskTick(sp, off.NextAt, map[string]bool{"": true})
	if check.ReviewKind != "offline" {
		t.Fatalf("offline check = %+v", check)
	}
	recovered := taskTick(sp, off.NextAt.Add(time.Second), map[string]bool{"": true, "devbox": true})
	if recovered.Review == check.Review || recovered.ReviewKind != "reconnected" || !recovered.OfflineAt.IsZero() {
		t.Fatalf("reconnected task = %+v", recovered)
	}
	if sp.taskTriggerCurrentLocked(sp.st.trigger(check.Review)) {
		t.Fatal("old offline check can still be delivered")
	}
	changeTask(t, sp, off.NextAt.Add(2*time.Second), TaskUpdate{Action: "progress", Version: recovered.Version, Detail: "same run, new test result"})
	if len(sp.st.Entries) != 0 {
		t.Fatal("reconnect notified the human")
	}
}

func TestTaskFollowupBusinessDecisionOutlivesNativePending(t *testing.T) {
	for _, reason := range []string{"expired", "seen", "unrelated"} {
		t.Run(reason, func(t *testing.T) {
			sp, db, call := newPendingTestEnv(t)
			now := time.Now()
			start := changeTask(t, sp, now, TaskUpdate{Action: "start"})
			var entry *Entry
			if reason == "seen" {
				entry, _ = sp.Reply(mainThread, "看一下回归报告，之后还需决定是否放行。", "", nil, true)
			} else {
				entry, _ = sp.Reply(mainThread, "要放行吗？", "", []string{"放行", "等等"})
			}
			wait := changeTask(t, sp, now, TaskUpdate{Action: "wait", Decision: entry.ID, Version: start.Version})
			sp.st = restoreTasks(t, db)
			if view := sp.taskFollowups()[start.Path]; view.Decision != entry.ID {
				t.Fatalf("decision not available after commit/restart: %+v", view)
			}

			switch reason {
			case "seen":
				rec := call("POST", "/api/super/dismiss", map[string]string{"id": entry.ID})
				if rec.Code != http.StatusOK {
					t.Fatalf("dismiss = %d %s", rec.Code, rec.Body)
				}
			case "unrelated":
				rec := call("POST", "/api/super/say", map[string]string{"text": "另一个事，首页也看一下"})
				if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
					t.Fatalf("say = %d %s", rec.Code, rec.Body)
				}
			case "expired":
				now = now.Add(8 * 24 * time.Hour)
			}
			if wait.NextAt.After(now) {
				now = wait.NextAt
			}
			task := taskTick(sp, now, nil)
			if task.Decision != "" || task.LastDecision != entry.ID || !task.Waiting || task.ReviewKind != "decision" {
				t.Fatalf("%s business decision = %+v", reason, task)
			}
			updated := changeTask(t, sp, now, TaskUpdate{Action: "resolve", Version: task.Version, Detail: "用户明确同意放行"})
			if updated.Waiting || updated.Decision != "" || updated.Review != "" {
				t.Fatalf("resolved task = %+v", updated)
			}
		})
	}
}

func TestTaskFollowupDailyBatchAndRestart(t *testing.T) {
	sp, db, _ := newPendingTestEnv(t)
	base := time.Date(2026, 11, 28, 8, 0, 0, 0, time.Local)
	changeTask(t, sp, base, TaskUpdate{Action: "start"})
	sp.mu.Lock()
	q := sp.appendEntryLocked(&Entry{ID: sp.st.nextID("e"), Role: "collie", Thread: mainThread, Text: "回归是否放行？", Options: []string{"放行"}, At: base})
	other := sp.appendEntryLocked(&Entry{ID: sp.st.nextID("e"), Role: "collie", Thread: mainThread, Text: "请看另一份报告", Check: true, At: base})
	sp.mu.Unlock()
	changeTask(t, sp, base, TaskUpdate{Action: "wait", Decision: q.ID})

	day2 := base.AddDate(0, 0, 1)
	changeTask(t, sp, base, TaskUpdate{Action: "remind", Version: sp.taskFollowups()[followupTestPath].Version, At: day2.Add(12 * time.Hour)})
	sp.flush()
	remind := func(now time.Time) {
		sp.mu.Lock()
		defer sp.mu.Unlock()
		sp.remindPendingLocked(now)
	}
	remind(day2.Add(time.Hour)) // 09:00 local
	if len(sp.st.Entries) != 2 {
		t.Fatal("reminder before 10:00")
	}
	remind(day2.Add(2 * time.Hour))
	sp.st = restoreTasks(t, db) // crash before the normal flush
	tail := sp.st.Entries[len(sp.st.Entries)-1]
	if tail.Tone != toneRemind || tail.Ref != other.ID || strings.Contains(tail.Text, q.Text) {
		t.Fatalf("reminder should contain only the other question: %+v", tail)
	}
	remind(day2.Add(13 * time.Hour))
	if len(sp.st.Entries) != 3 {
		t.Fatal("a second reminder was emitted the same day")
	}
	remind(day2.AddDate(0, 0, 1).Add(2 * time.Hour))
	tail = sp.st.Entries[len(sp.st.Entries)-1]
	if tail.Tone != toneRemind || !strings.Contains(tail.Text, q.Text) || !strings.Contains(tail.Text, other.Text) {
		t.Fatalf("reminders were not batched: %+v", tail)
	}
}

func TestTaskFollowupConcurrentReschedule(t *testing.T) {
	sp, db, call := newPendingTestEnv(t)
	now := time.Now()
	task := changeTask(t, sp, now, TaskUpdate{Action: "start"})
	answers := make(chan int, 2)
	var group sync.WaitGroup
	for _, d := range []time.Duration{3 * time.Hour, 4 * time.Hour} {
		group.Go(func() {
			body := map[string]any{"path": task.Path, "version": task.Version, "at": now.Add(d).Format(time.RFC3339Nano)}
			answers <- call("POST", "/api/super/task/remind", body).Code
		})
	}
	group.Wait()
	codes := []int{<-answers, <-answers}
	slices.Sort(codes)
	if !slices.Equal(codes, []int{http.StatusOK, http.StatusConflict}) {
		t.Fatalf("two devices returned %v", codes)
	}
	saved := restoreTasks(t, db).TaskFollowups[task.Path]
	current := sp.taskFollowups()[task.Path]
	if !saved.NextAt.Equal(current.NextAt) || saved.Version != task.Version+1 || !saved.Manual {
		t.Fatalf("saved version or time = %+v", saved)
	}
	progress := changeTask(t, sp, now.Add(time.Minute), TaskUpdate{Action: "progress", Detail: "new artifact", At: now.Add(time.Hour)})
	if !progress.NextAt.Equal(saved.NextAt) {
		t.Fatal("routine progress replaced the manual time")
	}
}

func TestTaskFollowupRestartAndRouterDelivery(t *testing.T) {
	_, sp, _ := simAgent(t)
	now := time.Now()
	start := changeTask(t, sp, now, TaskUpdate{Action: "start", At: now.Add(time.Millisecond)})
	sp.st = restoreTasks(t, sp.db) // restart before the check
	first := taskTick(sp, start.NextAt, nil)
	sp.st = restoreTasks(t, sp.db) // restart after the check has been queued
	second := taskTick(sp, start.NextAt.Add(time.Second), nil)
	if first.Review == "" || first.Review != second.Review || first.Version != second.Version {
		t.Fatalf("restart duplicated the review: %+v → %+v", first, second)
	}
	trigger := sp.st.trigger(second.Review)
	if !slices.Equal(trigger.Targets, []string{routerName}) || trigger.Action != "respond" {
		t.Fatalf("check won't reach the new Router: %+v", trigger)
	}
	prompt := sp.deliveryPrompt(routerName, []*Trigger{trigger}, larkDelivery{}, true, 200)
	if !strings.Contains(prompt, "Session start") || !strings.Contains(prompt, fmt.Sprintf("%s (version %d)", start.Path, second.Version)) {
		t.Fatalf("new Router will not receive the review and version: %q", prompt)
	}
	sp.tick(context.Background())
	tail := restoreTasks(t, sp.db)
	if len(tail.TaskFollowups) != 1 || tail.TaskFollowups[start.Path].Review != first.Review {
		t.Fatalf("review did not survive a tick and next restart: %+v", tail.TaskFollowups)
	}
}

func TestTaskFollowupCloseHidesTaskBeforeArchive(t *testing.T) {
	sp, _, call := newPendingTestEnv(t)
	now := time.Now()
	task := changeTask(t, sp, now, TaskUpdate{Action: "start"})
	file := filepath.Join(sp.memDir(), task.Path)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("# Verify release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	check := taskTick(sp, task.NextAt, nil)
	changeTask(t, sp, task.NextAt, TaskUpdate{Action: "close", Version: check.Version, Detail: "reviewed artifact"})
	res := call("GET", "/api/super/memory?area=tasks", nil)
	var panel struct {
		Files    []memFile               `json:"files"`
		Followup map[string]TaskFollowup `json:"followup"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &panel); err != nil || len(panel.Files) != 0 || len(panel.Followup) != 0 {
		t.Fatalf("closed task in panel: %s, %v", res.Body, err)
	}
	if sp.taskTriggerCurrentLocked(sp.st.trigger(check.Review)) {
		t.Fatal("closed task's queued review can still be delivered")
	}
	reused := call("POST", "/api/super/task", TaskUpdate{Action: "start", Path: task.Path})
	if reused.Code != http.StatusConflict || !strings.Contains(reused.Body.String(), "choose a new slug") {
		t.Fatalf("closed slug reuse = %d %s", reused.Code, reused.Body)
	}
}

func restoreTasks(t *testing.T, db *sql.DB) *State {
	t.Helper()
	state, err := loadState(db)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestTaskFollowupDeliveryFailureStaysPrivate(t *testing.T) {
	sp, _, _ := newPendingTestEnv(t)
	now := time.Now()
	start := changeTask(t, sp, now, TaskUpdate{Action: "start"})
	check := taskTick(sp, start.NextAt, nil)
	sp.mu.Lock()
	event := sp.st.trigger(check.Review)
	sp.finishDeliveryLocked(routerName, []*Trigger{event}, false, "test: prompt not confirmed")
	sp.mu.Unlock()
	if event.Status != "failed" || len(sp.st.Entries) != 0 || len(sp.notifyBuf) != 0 {
		t.Fatalf("internal delivery failure became public: status=%s entries=%v notices=%v", event.Status, sp.st.Entries, sp.notifyBuf)
	}
	retry := taskTick(sp, start.NextAt.Add(taskReviewRetry), nil)
	if retry.Review == event.ID || retry.ReviewKind != "due" || retry.Version != check.Version {
		t.Fatalf("unconfirmed internal review was not retried on its original version: %+v", retry)
	}
	sp.mu.Lock()
	sp.finishDeliveryLocked(routerName, []*Trigger{sp.st.trigger(retry.Review)}, false, "test: also unconfirmed")
	sp.mu.Unlock()
	sp.st = restoreTasks(t, sp.db)
	after := taskTick(sp, start.NextAt.Add(2*taskReviewRetry), nil)
	if after.Review != retry.Review || after.Version != retry.Version {
		t.Fatalf("review retried a second time: %+v", after)
	}
}

func TestTaskFollowupOfflineDialogGetsReviewedInFifteenMinutes(t *testing.T) {
	sp, _, _ := newPendingTestEnv(t)
	now := time.Now()
	w := &Worker{Name: "verify", Machine: "node", Status: "blocked", PaneID: "w1:p1@node", CreatedAt: now}
	sp.st.Workers[w.Name] = w
	changeTask(t, sp, now, TaskUpdate{Action: "start", Worker: w.Name})
	sp.mu.Lock()
	ask := sp.appendEntryLocked(&Entry{
		ID: sp.st.nextID("e"), Role: "event", Ref: w.Name, Thread: mainThread, At: now,
		Ask: &DialogAsk{Pane: w.PaneID, Choices: []string{"允许"}},
	})
	sp.mu.Unlock()
	changeTask(t, sp, now, TaskUpdate{Action: "wait", Decision: ask.ID})

	sp.unanswered = map[string]time.Time{w.Machine: now.Add(time.Minute)}
	offline := taskTick(sp, now.Add(time.Minute+awayGrace), map[string]bool{"": true})
	if offline.Status != "offline" || offline.Decision != "" || !offline.NextAt.Equal(now.Add(16*time.Minute)) {
		t.Fatalf("disconnect left a worker dialog available or hid its next check: %+v", offline)
	}
	check := taskTick(sp, offline.NextAt, map[string]bool{"": true})
	if !check.Waiting || check.ReviewKind != "offline" {
		t.Fatalf("offline worker dialog did not get the offline follow-up: %+v", check)
	}
}
