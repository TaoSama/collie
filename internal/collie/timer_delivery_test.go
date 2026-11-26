package collie

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestTimerDeliveryPersistsScheduleBeforeRestart(t *testing.T) {
	f, sp, _ := simAgent(t)
	const thread = "timer:delivery"
	sp.st.Workers["worker-1"] = &Worker{Name: "worker-1", Kind: "claude", Thread: thread, Status: "idle", PaneID: "wA:p1"}
	sp.st.Threads[thread] = &Thread{ID: thread, Subscribers: []string{"worker-1"}}
	timer := &Timer{
		ID: sp.st.nextID("m"), Spec: "every 1h", Thread: thread, Text: "check status",
		NextRun: time.Now().Add(-time.Minute), Enabled: true,
	}
	sp.st.Timers = []*Timer{timer}
	sp.changed()
	sp.flush()

	sp.tick(context.Background())
	if got := f.count("agent.prompt"); got != 1 {
		t.Fatalf("initial submit count = %d, want 1", got)
	}
	// Simulate a crash after sending and before the next periodic flush.
	restarted, err := loadState(sp.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.Triggers) != 1 || restarted.Triggers[0].Status != "delivered" {
		t.Fatalf("restart restored triggers %+v, want one delivered trigger", restarted.Triggers)
	}
	id := restarted.Triggers[0].ID
	if restarted.Seq < int(idNum(id)) {
		t.Errorf("restart restored sequence %d, below delivered trigger %s", restarted.Seq, id)
	}
	if th := restarted.Threads[thread]; th == nil || !slices.Equal(th.Subscribers, []string{"worker-1"}) || !slices.Contains(th.Triggers, id) {
		t.Errorf("restart restored thread %+v, want original subscriber and trigger %s", th, id)
	}
	if len(restarted.Timers) != 1 || restarted.Timers[0].LastRun == nil || !restarted.Timers[0].NextRun.Equal(timer.NextRun) {
		t.Errorf("restart did not restore the advanced schedule for timer %s", timer.ID)
	}

	sp.st, sp.dirty = restarted, false
	sp.tick(context.Background())
	if got := f.count("agent.prompt"); got != 1 {
		t.Errorf("total submit count after restart = %d, want 1", got)
	}
}
