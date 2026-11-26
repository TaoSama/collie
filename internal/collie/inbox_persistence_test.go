package collie

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

func TestExtReceiveCommitsMessageAndTriggerBeforeAck(t *testing.T) {
	sp, h, _ := newExtTestHub(t)
	r := &extRun{h: h, man: h.manifest("fakew")}
	params := extReceiveParams(t, "m1")
	if _, err := r.handle(t.Context(), extproto.MChannelReceive, params); err != nil {
		t.Fatal(err)
	}

	// Reload only persisted state, with no Super flush between ACK and restart.
	st, err := loadState(sp.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Triggers) != 1 || st.Triggers[0].Ref != "m1" || st.Triggers[0].Status != "queued" {
		t.Fatalf("reloaded triggers = %+v, want the queued request m1", st.Triggers)
	}
	if !newExtStore(sp.db).hasMsg("fakew:c1", "m1") {
		t.Fatal("the accepted message is missing")
	}
	if th := st.Threads["ext:fakew:c1"]; th == nil || len(th.Triggers) != 1 || th.Triggers[0] != st.Triggers[0].ID {
		t.Fatalf("reloaded thread = %+v, want the accepted trigger", th)
	}

	restarted := &Super{db: sp.db, st: st}
	r.h = &extHost{sp: restarted, store: newExtStore(sp.db)}
	if _, err := r.handle(t.Context(), extproto.MChannelReceive, params); err != nil {
		t.Fatal(err)
	}
	if got := len(extTriggers(restarted, "ext:fakew")); got != 1 {
		t.Fatalf("replaying the accepted message left %d requests, want 1", got)
	}
}

func TestExtReceiveRollsBackMessageWhenTriggerWriteFails(t *testing.T) {
	sp, h, _ := newExtTestHub(t)
	r := &extRun{h: h, man: h.manifest("fakew")}
	params := extReceiveParams(t, "m1")
	if _, err := sp.db.Exec(`CREATE TRIGGER reject_trigger BEFORE INSERT ON triggers BEGIN SELECT RAISE(ABORT, 'trigger write failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.handle(t.Context(), extproto.MChannelReceive, params); !extproto.IsCode(err, extproto.ErrHubUnavailable) {
		t.Fatalf("receive error = %v, want a retryable rejection", err)
	}
	if h.store.hasMsg("fakew:c1", "m1") {
		t.Fatal("the failed request left a deduplication record")
	}
	if _, ok := h.store.conv("fakew:c1"); ok {
		t.Fatal("the failed request left an in-memory conversation")
	}
	if n := len(extTriggers(sp, "ext:fakew")); n != 0 {
		t.Fatalf("the failed request published %d trigger(s)", n)
	}
	if _, err := sp.db.Exec(`DROP TRIGGER reject_trigger`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.handle(t.Context(), extproto.MChannelReceive, params); err != nil {
		t.Fatal(err)
	}
	if n := len(extTriggers(sp, "ext:fakew")); n != 1 {
		t.Fatalf("retry queued %d trigger(s), want 1", n)
	}
}

func TestExtDedicatedRequestSurvivesRestartBeforeWorkerLaunch(t *testing.T) {
	sp, h, dir := newExtTestHub(t)
	man := *h.manifest("fakew")
	man.Defaults.Handler = handlerAgent
	r := &extRun{h: h, man: &man}
	if _, err := r.handle(t.Context(), extproto.MChannelReceive, extReceiveParams(t, "m1")); err != nil {
		t.Fatal(err)
	}
	// A second request arrives after Spawn allocated the worker in memory,
	// before that worker or its subscription was persisted.
	sp.mu.Lock()
	sp.st.Workers["dedicated"] = &Worker{Name: "dedicated", Conv: "fakew:c1", Status: "starting"}
	sp.mu.Unlock()
	if _, err := r.handle(t.Context(), extproto.MChannelReceive, extReceiveParams(t, "m2")); err != nil {
		t.Fatal(err)
	}
	st, err := loadState(sp.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Triggers) != 2 || st.Triggers[0].Ref != "m1" || st.Triggers[1].Ref != "m2" {
		t.Fatalf("reloaded triggers = %+v, want both dedicated requests", st.Triggers)
	}
	if len(st.Workers) != 0 {
		t.Fatalf("reloaded workers = %+v, want none after interrupted launch", st.Workers)
	}
	srv := &Server{herdr: newHerdr("", "test", dir)}
	restarted := &Super{s: srv, db: sp.db, st: st}
	srv.ext = &extHost{s: srv, sp: restarted, store: newExtStore(sp.db),
		sets: map[string]ExtSetting{"fakew": {Machine: "unconnected"}},
	}

	restarted.mu.Lock()
	early := restarted.routeExtensionLocked(st.Triggers[0], nil, nil, false)
	restarted.mu.Unlock()
	if early {
		t.Fatal("dedicated request was routable before its worker came up")
	}
	waitFor(t, "the offline worker launch to defer", func() bool {
		_, busy := srv.ext.starting.Load("fakew:c1")
		return !busy
	})

	restarted.mu.Lock()
	defer restarted.mu.Unlock()
	// A recovered worker can be linked by its conversation even before
	// the inbox conversation's Worker field has been updated.
	st.Workers["dedicated"] = &Worker{Name: "dedicated", Conv: "fakew:c1"}
	live := map[string]liveAgent{"dedicated": {Ready: true}}
	for _, trigger := range st.Triggers {
		if trigger.Status != "queued" {
			t.Fatalf("request %s is %s after restart with the node offline", trigger.Ref, trigger.Status)
		}
		if !restarted.routeExtensionLocked(trigger, live, nil, false) || len(trigger.Targets) != 1 || trigger.Targets[0] != "dedicated" {
			t.Fatalf("recovered request %s targets = %v, want only the dedicated worker", trigger.Ref, trigger.Targets)
		}
	}
	trigger := st.Triggers[0]
	trigger.Deliveries = map[string]deliveryAttempt{"old-worker": {Pane: "old-pane"}}
	trigger.Targets = []string{"old-worker"}
	if restarted.routeExtensionLocked(trigger, live, nil, false) || trigger.Targets[0] != "old-worker" {
		t.Fatal("dedicated request changed targets before its previous delivery was resolved")
	}
}

func TestExtArchiveRetriesPageWhenMessageWriteFails(t *testing.T) {
	sp, h, _ := newExtTestHub(t)
	const cursorKey = "ext_cursor:fake"
	h.store.setKV(cursorKey, "1")
	r := &extRun{h: h, man: h.manifest("fake")}
	client, server := net.Pipe()
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	r.peer = extproto.NewPeer(client, client, "h", nil, nil)
	remote := extproto.NewPeer(server, server, "p", func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		var req struct{ Cursor string }
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if method != extproto.MArchiveChanges || req.Cursor != "1" {
			t.Errorf("archive request = %s (cursor %q), want page after 1", method, req.Cursor)
		}
		return extproto.ArchiveChanges{Next: "3", Batches: []extproto.ArchiveBatch{{
			Chat: extproto.ArchiveChat{ID: "c1"},
			Messages: []extproto.ArchiveMessage{
				{ID: "m1", Mine: true, Text: "/ai first", At: time.Now().Add(-time.Second).Format(time.RFC3339)},
				{ID: "m2", Mine: true, Text: "/ai second", At: time.Now().Format(time.RFC3339)},
			},
		}}}, nil
	}, nil)
	go r.peer.Serve(t.Context())
	go remote.Serve(t.Context())
	if _, err := sp.db.Exec(`CREATE TRIGGER reject_second_message BEFORE INSERT ON ext_msgs WHEN NEW.id = 'm2' BEGIN SELECT RAISE(ABORT, 'message write failed'); END`); err != nil {
		t.Fatal(err)
	}
	if !r.pullArchive(t.Context()) {
		t.Error("storage failure did not ask for an early retry")
	}
	if got := h.store.kv(cursorKey); got != "1" {
		t.Fatalf("cursor advanced to %q through a failed write", got)
	}
	if _, err := sp.db.Exec(`DROP TRIGGER reject_second_message`); err != nil {
		t.Fatal(err)
	}
	if r.pullArchive(t.Context()) {
		t.Error("successful archive replay requested another retry")
	}
	if got := h.store.kv(cursorKey); got != "3" {
		t.Fatalf("cursor after retry = %q, want 3", got)
	}
	triggers := extTriggers(sp, "ext:fake")
	if len(triggers) != 2 || triggers[0].Ref != "m1" || triggers[1].Ref != "m2" {
		t.Fatalf("retry triggers = %+v, want m1 and m2 once each", triggers)
	}
}

func extReceiveParams(t *testing.T, id string) json.RawMessage {
	t.Helper()
	owner := true
	body, err := json.Marshal(extproto.Receive{Message: extproto.Message{
		ID: id, ChatID: "c1", Owner: &owner, Text: "/ai hello",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}
