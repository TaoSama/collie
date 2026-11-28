package collie

import (
	"testing"
	"time"
)

func TestTaskFollowupOutageDoesNotDisturbActiveReview(t *testing.T) {
	sp, _, _ := newPendingTestEnv(t)
	now := time.Now()
	worker := &Worker{Name: "verify", Machine: "node", Status: "working", CreatedAt: now}
	sp.st.Workers[worker.Name] = worker
	start := changeTask(t, sp, now, TaskUpdate{Action: "start", Worker: worker.Name, At: now.Add(time.Second)})
	first := taskTick(sp, start.NextAt, map[string]bool{worker.Machine: true})
	reviewed := changeTask(t, sp, start.NextAt, TaskUpdate{Action: "review", Version: first.Version})
	second := taskTick(sp, reviewed.NextAt, map[string]bool{worker.Machine: true})
	brief := reviewed.NextAt.Add(time.Second)

	sp.unanswered = map[string]time.Time{worker.Machine: brief}
	for _, answered := range []map[string]bool{nil, {"": true}} {
		got := taskTick(sp, brief.Add(3*time.Second), answered)
		if !got.OfflineAt.IsZero() || got.Version != second.Version || got.Missed != 1 || got.Review != second.Review {
			t.Fatalf("transient outage changed acknowledged history or check: %+v", got)
		}
	}
	delete(sp.unanswered, worker.Machine)
	cleared := taskTick(sp, brief.Add(4*time.Second), map[string]bool{worker.Machine: true})
	if cleared.Review != second.Review || len(sp.st.Triggers) != 2 {
		t.Fatalf("transient recovery created another review: %+v", cleared)
	}

	sp.unanswered[worker.Machine] = brief
	off := taskTick(sp, brief.Add(awayGrace), map[string]bool{"": true})
	if off.OfflineAt.IsZero() || off.Review != second.Review || off.Version != second.Version {
		t.Fatalf("confirmed outage invalidated review in flight: %+v", off)
	}
	delete(sp.unanswered, worker.Machine)
	recovered := taskTick(sp, brief.Add(awayGrace+time.Second), map[string]bool{worker.Machine: true})
	if !recovered.OfflineAt.IsZero() || recovered.Review != second.Review || recovered.Version != second.Version {
		t.Fatalf("returning node invalidated review in flight: %+v", recovered)
	}
}

func TestTaskFollowupNoReconnectEventForChatPending(t *testing.T) {
	for _, check := range []bool{false, true} {
		sp, _, _ := newPendingTestEnv(t)
		now := time.Now()
		worker := &Worker{Name: "verify", Machine: "node", Status: "working", CreatedAt: now}
		sp.st.Workers[worker.Name] = worker
		changeTask(t, sp, now, TaskUpdate{Action: "start", Worker: worker.Name})
		var options []string
		if !check {
			options = []string{"继续"}
		}
		entry, _ := sp.Reply(mainThread, "等你处理", "", options, check)
		wait := changeTask(t, sp, now, TaskUpdate{Action: "wait", Decision: entry.ID})
		sp.unanswered = map[string]time.Time{worker.Machine: now}
		off := taskTick(sp, now.Add(taskOfflineReview), map[string]bool{"": true})
		delete(sp.unanswered, worker.Machine)
		recovered := taskTick(sp, now.Add(taskOfflineReview+time.Second), map[string]bool{worker.Machine: true})
		if off.Decision != entry.ID || !off.OfflineAt.IsZero() || recovered.Version != wait.Version ||
			recovered.Review != "" || len(sp.st.Triggers) != 0 {
			t.Fatalf("machine state disturbed independent chat pending: offline=%+v recovered=%+v", off, recovered)
		}

		// The question may have been posted after the outage began.
		sp.st.TaskFollowups[followupTestPath].OfflineAt = now.Add(taskOfflineReview)
		quiet := taskTick(sp, now.Add(taskOfflineReview+2*time.Second), map[string]bool{worker.Machine: true})
		if !quiet.OfflineAt.IsZero() || quiet.Review != "" || quiet.Version != wait.Version {
			t.Fatalf("reconnect while waiting on chat was not cleared silently: %+v", quiet)
		}
	}
}

func TestTaskFollowupDeliveredReviewKeepsItsVersion(t *testing.T) {
	sp, db, _ := newPendingTestEnv(t)
	now := time.Now()
	start := changeTask(t, sp, now, TaskUpdate{Action: "start"})
	review := taskTick(sp, start.NextAt, nil)
	sp.mu.Lock()
	sp.finishDeliveryLocked(routerName, []*Trigger{sp.st.trigger(review.Review)}, false, "")
	sp.mu.Unlock()
	sp.st = restoreTasks(t, db)
	evicted := sp.st.Triggers
	sp.st.Triggers = nil // a delivered trigger may have fallen outside the history window
	later := start.NextAt.Add(4 * time.Hour)
	same := taskTick(sp, later, nil)
	if same.Review != review.Review || same.Version != review.Version || len(sp.st.Triggers) != 0 {
		t.Fatalf("delivered review was reissued while the Router was investigating: %+v", same)
	}
	sp.st.Triggers = evicted
	saved := changeTask(t, sp, later, TaskUpdate{Action: "review", Version: review.Version, Detail: "review finished", Activity: true})
	if saved.Review != "" || saved.Detail != "review finished" {
		t.Fatalf("Router could not finish its delivered check: %+v", saved)
	}
}

func TestTaskFollowupDoesNotDuplicateWorkerLifecycleEvents(t *testing.T) {
	for _, status := range []string{"blocked", "gone", "failed"} {
		t.Run(status, func(t *testing.T) {
			sp, _, _ := newPendingTestEnv(t)
			now := time.Now()
			w := &Worker{Name: "verify", Status: "working", CreatedAt: now}
			sp.st.Workers[w.Name] = w
			task := changeTask(t, sp, now, TaskUpdate{Action: "start", Worker: w.Name})
			w.Status, w.Ended = status, status != "blocked"
			got := taskTick(sp, now.Add(time.Second), map[string]bool{"": true})
			if got.Version != task.Version || got.Review != "" || len(sp.st.Triggers) != 0 {
				t.Fatalf("worker %s also triggered an unexpected follow-up: %+v", status, got)
			}
			if status == "blocked" {
				got = taskTick(sp, task.NextAt, map[string]bool{"": true})
				if got.Review != "" {
					t.Fatalf("pending trust prompt also triggered a due check: %+v", got)
				}
				w.Status = "working"
				got = taskTick(sp, task.NextAt.Add(time.Second), map[string]bool{"": true})
				if got.ReviewKind != "due" {
					t.Fatal("regular check did not resume once worker was unblocked")
				}
			}
		})
	}
}

func TestTaskFollowupResumesAfterNativeDialogAnswer(t *testing.T) {
	for _, long := range []bool{false, true} {
		sp, db, _ := newPendingTestEnv(t)
		now := time.Now()
		w := &Worker{Name: "verify", Status: "blocked", PaneID: "w1:p1", CreatedAt: now}
		sp.st.Workers[w.Name] = w
		changeTask(t, sp, now, TaskUpdate{Action: "start", Worker: w.Name, Long: long})
		sp.mu.Lock()
		askID := sp.addAskLocked(w, nil)
		sp.mu.Unlock()
		wait := changeTask(t, sp, now, TaskUpdate{Action: "wait", Decision: askID})
		if !wait.Waiting || wait.Decision != askID {
			t.Fatalf("native dialog wasn't linked: %+v", wait)
		}

		sp.mu.Lock()
		sp.settleAskLocked(w.PaneID, "允许", byYou)
		w.Status = "working"
		sp.mu.Unlock()
		answer := now.Add(time.Second)
		after := taskTick(sp, answer, map[string]bool{"": true})
		interval := 2 * time.Hour
		if long {
			interval = time.Hour
		}
		if after.Waiting || after.Status != "active" || after.Decision != "" || after.Review != "" ||
			!after.NextAt.Equal(answer.Add(interval)) {
			t.Fatalf("dialog answer kept task waiting or reset the wrong timer: %+v", after)
		}
		sp.st = restoreTasks(t, db)
		if sp.st.TaskFollowups[followupTestPath].Waiting || sp.st.Entries[0].Ask.Done == nil {
			t.Fatal("dialog answer and task continuation were not saved together")
		}
	}
}

func TestTaskFollowupShowsNextDayAfterDailyBatch(t *testing.T) {
	sp, _, _ := newPendingTestEnv(t)
	asked := time.Date(2026, 11, 28, 15, 0, 0, 0, time.Local)
	changeTask(t, sp, asked, TaskUpdate{Action: "start"})
	sp.mu.Lock()
	entry := sp.appendEntryLocked(&Entry{
		ID: sp.st.nextID("e"), Role: "collie", Thread: mainThread, At: asked, Options: []string{"继续"},
	})
	sp.mu.Unlock()
	changeTask(t, sp, asked, TaskUpdate{Action: "wait", Decision: entry.ID})
	dueDay := dailyTaskTime(asked.AddDate(0, 0, 1))
	sp.st.RemindedAt = dueDay
	for _, now := range []time.Time{dueDay.Add(time.Hour), dueDay.Add(6 * time.Hour)} {
		task := sp.taskViewLocked(sp.st.TaskFollowups[followupTestPath], now)
		if task.Decision != entry.ID || !task.NextAt.Equal(dueDay.AddDate(0, 0, 1)) {
			t.Fatalf("card claimed it would send a second reminder that day: %+v", task)
		}
	}
	future := dueDay.AddDate(0, 0, 2).Add(5 * time.Hour)
	changeTask(t, sp, dueDay, TaskUpdate{Action: "remind", Version: sp.st.TaskFollowups[followupTestPath].Version, At: future})
	if got := sp.taskViewLocked(sp.st.TaskFollowups[followupTestPath], dueDay); !got.NextAt.Equal(future) {
		t.Fatalf("future manual reminder changed: %+v", got)
	}
}

func TestTaskFollowupCanLinkAReopenedNativeDialog(t *testing.T) {
	sp, _, _ := newPendingTestEnv(t)
	now := time.Now()
	w := &Worker{Name: "verify", Status: "blocked", PaneID: "w1:p1", CreatedAt: now}
	sp.st.Workers[w.Name] = w
	changeTask(t, sp, now, TaskUpdate{Action: "start", Worker: w.Name})

	approval := &Approval{Choices: []Choice{{Label: "允许"}, {Label: "拒绝"}}}
	sp.mu.Lock()
	askID := sp.addAskLocked(w, approval)
	sp.mu.Unlock()
	changeTask(t, sp, now, TaskUpdate{Action: "wait", Decision: askID})

	sp.mu.Lock()
	w.Status = "working"
	sp.settleAskLocked(w.PaneID, "", "")
	sp.mu.Unlock()
	resumed := taskTick(sp, now.Add(time.Second), map[string]bool{"": true})
	if resumed.Waiting {
		t.Fatal("cleared dialog did not resume the task")
	}

	sp.mu.Lock()
	w.Status = "blocked"
	reopened := sp.addAskLocked(w, approval)
	sp.mu.Unlock()
	if reopened != askID {
		t.Fatalf("dialog redraw should reuse the same entry, got %s and %s", askID, reopened)
	}
	if got := changeTask(t, sp, now.Add(2*time.Second), TaskUpdate{Action: "wait", Decision: reopened}); !got.Waiting || got.Decision != askID {
		t.Fatalf("reopened native dialog is not linked: %+v", got)
	}
}
