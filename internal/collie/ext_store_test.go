package collie

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

func TestOutboxMaintenance(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "herd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	st := newExtStore(db)
	cutoff := time.Unix(1000, 500)
	older := cutoff.Add(-time.Second)
	sameSecondBefore := cutoff.Add(-time.Nanosecond)
	sameSecondAfter := cutoff.Add(time.Nanosecond)

	t.Run("expiration", func(t *testing.T) {
		for _, o := range []OutboxItem{
			{Ref: "expire-older", At: older, State: outPending},
			{Ref: "expire-before", At: sameSecondBefore, State: outPending},
			{Ref: "expire-equal", At: cutoff, State: outPending},
			{Ref: "expire-after", At: sameSecondAfter, State: outPending},
			{Ref: "expire-sent", At: older, State: extproto.DeliverySent},
		} {
			st.putOut(&o)
		}
		st.expireStale(cutoff)
		for ref, want := range map[string]string{
			"expire-older": "expired", "expire-before": "expired",
			"expire-equal": outPending, "expire-after": outPending, "expire-sent": extproto.DeliverySent,
		} {
			if o := st.out(ref); o == nil || o.State != want {
				t.Errorf("%s = %+v, want state %s", ref, o, want)
			}
		}
	})

	t.Run("retention", func(t *testing.T) {
		dir := t.TempDir()
		for _, tt := range []struct {
			ref, state     string
			at             time.Time
			wantRow        bool
			wantAttachment bool
		}{
			{"trim-older", extproto.DeliverySent, older, false, false},
			{"trim-before", extproto.DeliverySent, sameSecondBefore, true, false},
			{"trim-equal", extproto.DeliverySent, cutoff, true, true},
			{"trim-after", extproto.DeliverySent, sameSecondAfter, true, true},
			{"trim-pending", outPending, older, true, true},
		} {
			t.Run(tt.ref, func(t *testing.T) {
				path := filepath.Join(dir, tt.ref)
				if err := os.WriteFile(path, []byte("attachment"), 0o600); err != nil {
					t.Fatal(err)
				}
				st.putOut(&OutboxItem{Ref: tt.ref, At: tt.at, State: tt.state, Files: []OutFile{{Path: path}}})
				st.trimOutbox(cutoff.Add(outboxKeep))
				if got := st.out(tt.ref) != nil; got != tt.wantRow {
					t.Errorf("row kept = %t, want %t", got, tt.wantRow)
				}
				_, err := os.Stat(path)
				if tt.wantAttachment && err != nil || !tt.wantAttachment && !os.IsNotExist(err) {
					t.Errorf("attachment stat error = %v, want file present = %t", err, tt.wantAttachment)
				}
			})
		}
	})

	t.Run("retry", func(t *testing.T) {
		for _, o := range []OutboxItem{
			{Ref: "retry-new", Conv: "fake:new", At: cutoff, State: outPending},
			{Ref: "retry-other", Conv: "fakew:old", At: older, State: outPending},
			{Ref: "retry-case", Conv: "Fake:old", At: older, State: outPending},
			{Ref: "retry-sent", Conv: "fake:sent", At: older, State: extproto.DeliverySent},
			{Ref: "retry-old", Conv: "fake:old", At: older, State: outPending},
		} {
			st.putOut(&o)
		}
		var refs []string
		for _, o := range st.pendingOutsFor("fake") {
			refs = append(refs, o.Ref)
		}
		if want := []string{"retry-old", "retry-new"}; !slices.Equal(refs, want) {
			t.Errorf("retry candidates = %v, want %v", refs, want)
		}
	})
}

func TestDeliveryAcknowledgesPersistedState(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "herd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	h := &extHost{store: newExtStore(db)}
	r := &extRun{h: h, man: &ExtManifest{ID: "wechat"}}
	ref := "reply"
	h.store.putOut(&OutboxItem{Ref: ref, Conv: "wechat:chat", At: time.Now(), State: extproto.DeliveryDeferred})
	raw, err := json.Marshal(extproto.Delivery{Ref: ref, State: extproto.DeliverySent, MessageID: "wx-message"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_save BEFORE UPDATE ON ext_outbox BEGIN SELECT RAISE(ABORT, 'write failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.handle(t.Context(), extproto.MChannelDelivery, raw); err == nil {
		t.Fatal("acknowledged a failed write")
	}
	if got := h.store.out(ref); got.State != extproto.DeliveryDeferred {
		t.Fatalf("after failed write: %+v", got)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_save`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.handle(t.Context(), extproto.MChannelDelivery, raw); err != nil {
		t.Fatal(err)
	}
	got := h.store.out(ref)
	if got.State != extproto.DeliverySent || got.MessageID != "wx-message" || got.DoneAt.IsZero() {
		t.Fatalf("after replay: %+v", got)
	}
	if _, err := r.handle(t.Context(), extproto.MChannelDelivery, raw); err != nil {
		t.Fatal(err)
	}
	if again := h.store.out(ref); !again.DoneAt.Equal(got.DoneAt) {
		t.Fatalf("duplicate changed the receipt: %+v", again)
	}
}

func TestOutboxWakeRetriesNotReadySend(t *testing.T) {
	_, h, _ := newExtTestHub(t)
	r := &extRun{h: h, man: h.manifest("fakew"), proc: extProcState{State: extRunning}, pull: make(chan struct{}, 1)}
	client, server := net.Pipe()
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	ready := make(chan struct{})
	r.peer = extproto.NewPeer(client, client, "h", nil, nil)
	remote := extproto.NewPeer(server, server, "p", func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method != extproto.MChannelSend {
			t.Errorf("unexpected request %s", method)
		}
		select {
		case <-ready:
			return extproto.SendResult{MessageID: "message"}, nil
		default:
			return nil, extproto.Errorf(extproto.ErrNotReady, "starting")
		}
	}, nil)
	go r.peer.Serve(t.Context())
	go remote.Serve(t.Context())

	conv := "fakew:chat"
	h.store.mu.Lock()
	h.store.convs[conv] = &InboxConv{ID: conv, Ext: "fakew", Chat: "chat"}
	h.store.mu.Unlock()
	o := &OutboxItem{Ref: "warming-up", Conv: conv, At: time.Now(), State: outPending}
	h.store.putOut(o)
	if h.trySend(r, o) || h.store.out(o.Ref).State != outPending {
		t.Fatal("starting extension must leave the send pending")
	}

	close(ready)
	r.event(extproto.EChanged, json.RawMessage(`{"scope":"outbox"}`))
	if len(r.pull) != 0 {
		t.Error("outbox event also scheduled an archive pull")
	}
	waitFor(t, "pending send retried by the outbox event", func() bool {
		got := h.store.out(o.Ref)
		return got.State == extproto.DeliverySent && got.MessageID == "message"
	})
}

func TestRouterReplyToClosedExtThreadWithSubscriberRetriesAfterNotReady(t *testing.T) {
	sp, h, _ := newExtTestHub(t)
	m := fakeManifest(larkxID, "receive")
	h.cat[larkxID] = m
	r := &extRun{h: h, man: m, proc: extProcState{State: extRunning}, pull: make(chan struct{}, 1)}
	h.mu.Lock()
	h.runs[larkxID] = r
	h.mu.Unlock()

	client, server := net.Pipe()
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	ready := make(chan struct{})
	firstTry := make(chan struct{}, 1)
	r.peer = extproto.NewPeer(client, client, "h", nil, nil)
	remote := extproto.NewPeer(server, server, "p", func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method != extproto.MChannelSend {
			t.Errorf("unexpected request %s", method)
		}
		select {
		case firstTry <- struct{}{}:
		default:
		}
		select {
		case <-ready:
			return extproto.SendResult{MessageID: "lark-msg-1"}, nil
		default:
			return nil, extproto.Errorf(extproto.ErrNotReady, "starting")
		}
	}, nil)
	go r.peer.Serve(t.Context())
	go remote.Serve(t.Context())

	conv := larkxID + ":c_dm"
	thread := "ext:" + conv
	h.store.mu.Lock()
	h.store.convs[conv] = &InboxConv{ID: conv, Ext: larkxID, Chat: "c_dm", On: false, Handler: handlerAgent, Worker: "larkx-sub"}
	h.store.mu.Unlock()
	sp.mu.Lock()
	sp.st.Workers["larkx-sub"] = &Worker{Name: "larkx-sub", Thread: thread, Status: "done", Conv: conv}
	sp.threadLocked(thread).Subscribers = []string{"larkx-sub"}
	sp.mu.Unlock()

	rec := httptest.NewRecorder()
	sp.s.superReply(rec, httptest.NewRequest("POST", "/api/super/reply", strings.NewReader(
		`{"thread":"`+thread+`","text":"六方分工表已建好"}`)))
	if rec.Code != 200 {
		t.Fatalf("superReply = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK   bool   `json:"ok"`
		Sent string `json:"sent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.OK || resp.Sent == "" {
		t.Fatalf("unexpected superReply response: %s", rec.Body.String())
	}
	<-firstTry
	if got := h.store.out(resp.Sent); got == nil || got.State != outPending {
		t.Fatalf("send should remain pending on not_ready, got %+v", got)
	}

	close(ready)
	r.event(extproto.EChanged, json.RawMessage(`{"scope":"outbox"}`))
	waitFor(t, "router reply delivered after outbox wake", func() bool {
		got := h.store.out(resp.Sent)
		return got != nil && got.State == extproto.DeliverySent && got.MessageID == "lark-msg-1"
	})
}

func TestConvWorkerInternalErrorsStayInCollie(t *testing.T) {
	oldRetries, oldWait := convReplyRetries, convReplyWait
	convReplyRetries, convReplyWait = 1, time.Millisecond
	t.Cleanup(func() { convReplyRetries, convReplyWait = oldRetries, oldWait })

	sp, h, _ := newExtTestHub(t)
	s, f, pane := chatServer(t, 0, "codex")
	s.home = t.TempDir()
	t.Setenv("CODEX_HOME", s.home)
	sp.s.herdr = s.herdr
	sp.s.home = s.home
	sp.transcripts = map[string]*transcript{}

	conv := larkxID + ":c_dm"
	thread := "ext:" + conv
	h.store.mu.Lock()
	h.store.convs[conv] = &InboxConv{ID: conv, Ext: larkxID, Chat: "c_dm", Name: "杜俊达"}
	h.store.mu.Unlock()

	w := &Worker{Name: "larkx-5edd68", Kind: "codex", Thread: thread, PaneID: pane, Conv: conv, Status: "done"}

	// 1. Missing transcript must notify Collie (chat:main) and never send to the external chat.
	f.panes[pane].title = "codex | me"
	sp.workerSettled(t.Context(), w, false)
	if outs := h.store.outs("conv = ?", conv); len(outs) != 0 {
		t.Fatalf("missing transcript leaked %d message(s) to external chat: %+v", len(outs), outs)
	}
	sp.mu.Lock()
	entries := append([]*Entry(nil), sp.st.Entries...)
	sp.mu.Unlock()
	if len(entries) != 1 || entries[0].Thread != mainThread || entries[0].Tone != "error" || !strings.Contains(entries[0].Text, "杜俊达") {
		t.Fatalf("expected error notice in chat:main, got %+v", entries)
	}

	// 2. Startup failure (failConvStart) must notify Collie (chat:main) and never send to the external chat.
	trig := &Trigger{ID: "t1", Thread: thread, Status: "queued"}
	sp.mu.Lock()
	sp.st.Triggers = append(sp.st.Triggers, trig)
	sp.mu.Unlock()
	h.failConvStart(InboxConv{ID: conv, Ext: larkxID, Chat: "c_dm", Name: "杜俊达"}, trig, "hub 不跑 agent")
	if outs := h.store.outs("conv = ?", conv); len(outs) != 0 {
		t.Fatalf("failConvStart leaked %d message(s) to external chat: %+v", len(outs), outs)
	}
	sp.mu.Lock()
	entries = append([]*Entry(nil), sp.st.Entries...)
	sp.mu.Unlock()
	if len(entries) != 2 || entries[1].Thread != mainThread || entries[1].Tone != "error" || !strings.Contains(entries[1].Text, "hub 不跑 agent") {
		t.Fatalf("expected startup failure notice in chat:main, got %+v", entries)
	}
}

func TestConvLastReplyRetriesUntilCodexIndexAppears(t *testing.T) {
	oldRetries, oldWait := convReplyRetries, convReplyWait
	convReplyRetries, convReplyWait = 5, 10*time.Millisecond
	t.Cleanup(func() { convReplyRetries, convReplyWait = oldRetries, oldWait })

	s, f, pane := chatServer(t, 0, "codex")
	s.home = t.TempDir()
	t.Setenv("CODEX_HOME", s.home)
	log := strings.Replace(zstRollout, `"payload":{}`, `"payload":{"cwd":"/tmp"}`, 1)
	writeFiles(t, s.home, map[string]string{
		"sessions/2026/11/26/rollout-date-id.jsonl": log,
	})
	f.panes[pane].title = "Topic | me"
	sp := &Super{s: s, transcripts: map[string]*transcript{}}
	w := &Worker{Name: "larkx-59c4de", Kind: "codex", PaneID: pane, Conv: "larkx:c_dm"}

	go func() {
		time.Sleep(15 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(s.home, "session_index.jsonl"), []byte(`{"id":"id","thread_name":"Topic"}`+"\n"), 0o600)
	}()

	if got := sp.convLastReply(t.Context(), w); got != "Two files." {
		t.Fatalf("convLastReply = %q, want Two files.", got)
	}
}

func TestFastTurnBetweenTicksSettlesWorker(t *testing.T) {
	s, f, pane := chatServer(t, 0, "codex")
	s.home = t.TempDir()
	t.Setenv("CODEX_HOME", s.home)
	log := strings.Replace(zstRollout, `"payload":{}`, `"payload":{"cwd":"/tmp"}`, 1)
	writeFiles(t, s.home, map[string]string{
		"session_index.jsonl":                       `{"id":"id","thread_name":"Topic"}` + "\n",
		"sessions/2026/11/26/rollout-date-id.jsonl": log,
	})
	f.panes[pane].name = "larkx-fast"
	f.panes[pane].title = "Topic | me"
	f.panes[pane].status = "done"

	sp, h, _ := newExtTestHub(t)
	sp.s.herdr = s.herdr
	sp.s.home = s.home
	sp.transcripts = map[string]*transcript{}

	conv := larkxID + ":c_fast"
	thread := "ext:" + conv
	h.store.mu.Lock()
	h.store.convs[conv] = &InboxConv{ID: conv, Ext: larkxID, Chat: "c_fast", Name: "王思宇"}
	h.store.mu.Unlock()
	w := &Worker{Name: "larkx-fast", Kind: "codex", Thread: thread, PaneID: pane, Conv: conv, Status: "idle", CreatedAt: time.Now()}
	sp.mu.Lock()
	sp.st.Workers[w.Name] = w
	sp.mu.Unlock()

	// Fast turn went idle -> working -> done between ticks: must settle and enqueue reply.
	sp.tick(t.Context())
	waitFor(t, "fast turn reply queued", func() bool {
		outs := h.store.outs("conv = ?", conv)
		return len(outs) == 1 && strings.Contains(outs[0].Text, "Two files.")
	})
}
