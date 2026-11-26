package collie

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestFlushRetainsQueuedTriggersBeyondHistoryWindow(t *testing.T) {
	sp, db, _ := newPendingTestEnv(t)
	sp.st.Policies = append(sp.st.Policies, Policy{Source: "webhook:ignored", Action: "ignore"})
	first := sp.Ingest("webhook:github", "github:issue", "", "first pending", "", "", "")
	oldHistory := sp.Ingest("webhook:ignored", "history", "", "old history", "", "", "")
	second := sp.Ingest("webhook:github", "github:issue", "", "second pending", "", "", "")
	var recent *Trigger
	for range 600 {
		recent = sp.Ingest("webhook:ignored", "history", "", "recent history", "", "", "")
	}

	checkBacklog := func(st *State) {
		t.Helper()
		if len(st.Triggers) != 602 || st.Triggers[0].ID != first.ID || st.Triggers[1].ID != second.ID ||
			st.trigger(oldHistory.ID) != nil || st.trigger(recent.ID) == nil {
			t.Fatalf("working set has %d triggers, first pending=%t, second pending=%t, old history=%t, recent history=%t",
				len(st.Triggers), st.trigger(first.ID) != nil, st.trigger(second.ID) != nil,
				st.trigger(oldHistory.ID) != nil, st.trigger(recent.ID) != nil)
		}
	}
	sp.flush()
	checkBacklog(sp.st)

	reloaded, err := loadState(db)
	if err != nil {
		t.Fatal(err)
	}
	checkBacklog(reloaded)

	// Once a backlog item is delivered, persisting that update lets it age out.
	sp.st = reloaded
	sp.mu.Lock()
	sp.st.trigger(first.ID).Status = "delivered"
	sp.changed()
	sp.mu.Unlock()
	sp.flush()
	if sp.st.trigger(first.ID) != nil || sp.st.trigger(second.ID) == nil {
		t.Fatal("flush should retire the delivered trigger and retain the pending one")
	}
}

func TestRestartRoutesEventsToOlderThreadSubscribers(t *testing.T) {
	f := newFakeHerdr(t)
	sp := newRouterTestSuper(t, f)
	now := time.Now()
	const subscribed, unsubscribed, worker = "webhook:subscribed", "webhook:unsubscribed", "watcher"
	pane := f.add("wA", "workspace", worker, "claude", "/")
	sp.st.Workers[worker] = &Worker{Name: worker, Kind: "claude", Status: "idle", PaneID: pane, CreatedAt: now}
	sp.st.Threads[subscribed] = &Thread{ID: subscribed, LastAt: now.Add(-time.Hour), Subscribers: []string{worker}}
	sp.st.Threads[unsubscribed] = &Thread{ID: unsubscribed, LastAt: now.Add(-time.Hour)}
	for i := range 500 {
		id := fmt.Sprintf("recent:%d", i)
		sp.st.Threads[id] = &Thread{ID: id, LastAt: now}
	}
	// Pause delivery after target assignment so the test can inspect routing.
	sp.st.Settings.Paused = true
	if err := saveState(sp.db, sp.st, nil); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadState(sp.db)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Threads[unsubscribed] != nil || len(reloaded.Threads) != 501 {
		t.Errorf("loaded %d threads, want the 500 recent threads and the older subscribed thread", len(reloaded.Threads))
	}
	sp.st = reloaded
	event := sp.Ingest("webhook:github", subscribed, "sender", "new activity", "", "", "")
	sp.tick(t.Context())
	if !slices.Equal(event.Targets, []string{worker}) {
		t.Fatalf("older thread event routed to %v, want [%s]", event.Targets, worker)
	}
}
