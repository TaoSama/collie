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

func TestExtReceiveUpdateRetriesOnDBWriteError(t *testing.T) {
	sp, h, _ := newExtTestHub(t)
	r := &extRun{h: h, man: h.manifest("fakew")}
	if _, err := r.handle(t.Context(), extproto.MChannelReceive, extReceiveParams(t, "m1")); err != nil {
		t.Fatal(err)
	}
	owner := true
	upd, err := json.Marshal(extproto.Receive{Message: extproto.Message{
		ID: "m1", ChatID: "c1", Owner: &owner, Text: "updated card", Updated: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.db.Exec(`CREATE TRIGGER reject_msg_update BEFORE UPDATE ON ext_msgs BEGIN SELECT RAISE(ABORT, 'update failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.handle(t.Context(), extproto.MChannelReceive, upd); !extproto.IsCode(err, extproto.ErrHubUnavailable) {
		t.Fatalf("update error = %v, want retryable hub_unavailable", err)
	}
	if _, err := sp.db.Exec(`DROP TRIGGER reject_msg_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.handle(t.Context(), extproto.MChannelReceive, upd); err != nil {
		t.Fatal(err)
	}
	msgs := h.store.msgs("fakew:c1", time.Time{})
	if len(msgs) != 1 || msgs[0].Text != "updated card" {
		t.Fatalf("messages after update retry = %+v, want updated card", msgs)
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

func TestExtDedicatedTopicRespectsPolicy(t *testing.T) {
	for _, policy := range []struct {
		name, action, want string
		allow              []string
	}{
		{"ignored", "ignore", "ignored", nil},
		{"not allowed", "respond", "ignored", []string{"another sender"}},
		{"allowed", "respond", "queued", []string{"you"}},
	} {
		for _, state := range []string{"absent", "ended", "running"} {
			t.Run(policy.name+"/"+state, func(t *testing.T) {
				sp, h, _ := newExtTestHub(t)
				man := *h.manifest("fakew")
				man.Defaults.Handler = handlerAgent
				sp.mu.Lock()
				sp.st.Policies = []Policy{{Source: "ext:fakew", Action: policy.action, Allow: policy.allow}}
				if state != "absent" {
					sp.st.Workers["dedicated"] = &Worker{Name: "dedicated", Conv: "fakew:c1#topic", Ended: state == "ended"}
				}
				sp.mu.Unlock()

				_, trigger, err := h.fileIncoming(&man, incomingMsg{
					Chat: "c1", ChatKind: chatGroup, Topic: "topic", ID: "m1",
					Owner: true, Text: "/ai hello", At: time.Now(), Live: true,
				})
				if err != nil {
					t.Fatal(err)
				}
				if trigger == nil || trigger.Status != policy.want || trigger.WaitForConvWorker != (policy.want == "queued") {
					t.Fatalf("trigger = %+v, want status %s", trigger, policy.want)
				}
			})
		}
	}
}

func TestExtPolicySaveReleasesSchedulerBeforeUpdatingExtensions(t *testing.T) {
	sp, h, _ := newExtTestHub(t)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cat["fakew"].Defaults.Machine = machineRouter

	req := httptest.NewRequest("POST", "/api/super/settings",
		strings.NewReader(`{"policies":[{"source":"ext:fakew","action":"ignore"}]}`))
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	t.Cleanup(func() { <-done })
	go func() {
		defer close(done)
		sp.s.superSaveSettings(rec, req)
	}()

	// A status read owns the extension lock and needs the scheduler lock for
	// the default machine. Wait for the save to publish while that read owns it.
	waitFor(t, "the policy saved while an extension status read holds the extension lock", func() bool {
		if !sp.mu.TryLock() {
			return false
		}
		defer sp.mu.Unlock()
		return slices.ContainsFunc(sp.st.Policies, func(p Policy) bool {
			return p.Source == "ext:fakew" && p.Action == "ignore"
		})
	})
	h.settingLocked("fakew")
}

func TestExtTopicSnapshotsFollowCommit(t *testing.T) {
	for _, step := range []string{"commit", "duplicate", "rollback"} {
		t.Run(step, func(t *testing.T) {
			sp, h, _ := newExtTestHub(t)
			man := h.manifest("fakew")
			in := incomingMsg{Chat: "c1", ChatKind: chatGroup, Topic: "topic", ID: "m1",
				Owner: true, SenderName: "original", Text: "/ai hello", At: time.Now(), Live: true}
			if _, _, err := h.fileIncoming(man, in); err != nil {
				t.Fatal(err)
			}
			before, _ := h.store.conv("fakew:c1")
			want := "original"
			in.SenderName = "changed"
			switch step {
			case "commit":
				in.ID, want = "m2", in.SenderName
			case "rollback":
				in.ID = "m2"
				if _, err := sp.db.Exec(`CREATE TRIGGER reject_topic BEFORE INSERT ON triggers BEGIN SELECT RAISE(ABORT, 'trigger write failed'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := h.fileIncoming(man, in); (err != nil) != (step == "rollback") {
				t.Fatalf("%s: unexpected write result: %v", step, err)
			}
			current, _ := h.store.conv("fakew:c1")
			if got := current.askOf("topic").AskBy; got != want {
				t.Errorf("current topic owner = %q, want %q", got, want)
			}
			if got := before.askOf("topic").AskBy; got != "original" {
				t.Errorf("previous snapshot changed to %q", got)
			}
		})
	}
}

func TestExtTopicSnapshotCanBeEncodedWhileReceiving(t *testing.T) {
	_, h, _ := newExtTestHub(t)
	man := h.manifest("fakew")
	in := incomingMsg{Chat: "c1", ChatKind: chatGroup, Topic: "topic", ID: "m1",
		Owner: true, Text: "/ai hello", At: time.Now(), Live: true}
	if _, _, err := h.fileIncoming(man, in); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := h.store.conv("fakew:c1")
	done := make(chan struct{})
	t.Cleanup(func() { <-done })
	go func() {
		defer close(done)
		for range 100 {
			if _, err := json.Marshal(snapshot); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	in.ID = "m2"
	if _, _, err := h.fileIncoming(man, in); err != nil {
		t.Fatal(err)
	}
}

// A receive extension running on a remote node survives a hub restart window:
// messages (including /ai requests) sent while the hub is down are queued on
// disk, replayed once the restarted hub reconnects, and trigger once without
// duplication even if replayed again.
func TestExtReceiveSurvivesHubRestartWindow(t *testing.T) {
	dir := t.TempDir()
	nodeDir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "herd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// Simulated remote node process manager that outlives hub restarts.
	nodeProcs := &extProcs{byID: map[string]*extProc{}}
	man := fakeManifest("faker", "receive")
	man.Defaults.Conversations = "" // Feishu-like: /ai triggers from you; closed chats keep previews only

	startHub := func() (*Super, *extHost, context.CancelFunc) {
		st, err := loadState(db)
		if err != nil {
			t.Fatal(err)
		}
		srv := &Server{herdr: newHerdr("", "hub", dir), home: dir}
		sp := &Super{s: srv, db: db, dir: dir, bin: "collie", st: st, subs: map[chan struct{}]bool{}}
		srv.sup = sp
		h := newExtHost(srv, sp, dir)
		h.cat = map[string]*ExtManifest{"faker": man}
		srv.ext = h
		srv.herdr.attach(&Machine{
			ID: "node1", Name: "node1", Online: true,
			ext: func(ctx context.Context) (net.Conn, error) {
				a, b := net.Pipe()
				go nodeProcs.serve(a, nodeDir)
				return b, nil
			},
		})
		ctx, cancel := context.WithCancel(context.Background())
		go h.Run(ctx)
		waitFor(t, "hub host running", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.ctx != nil })
		return sp, h, cancel
	}

	t.Cleanup(func() {
		nodeProcs.mu.Lock()
		p := nodeProcs.byID["faker"]
		delete(nodeProcs.byID, "faker")
		nodeProcs.mu.Unlock()
		if p != nil {
			p.shutdown()
		}
	})

	// Hub #1 starts the extension on node1, opens c_open, and receives a baseline message.
	sp1, h1, cancel1 := startHub()
	if _, err := h1.configure("faker", func(x *ExtSetting) { x.Enabled = true; x.Machine = "node1" }); err != nil {
		t.Fatal(err)
	}
	var r1 *extRun
	waitFor(t, "faker running on hub 1", func() bool { r1 = h1.run("faker"); return r1 != nil && r1.running() })
	login(t, r1)
	feed(t, nodeDir, "faker", fakeMsg{Chat: "c_open", ChatName: "已开群", ID: "m0", Sender: "u2", SenderName: "甲", Text: "重启前"})
	waitFor(t, "baseline conv on hub 1", func() bool { _, ok := h1.store.conv("faker:c_open"); return ok })
	h1.store.update("faker:c_open", func(c *InboxConv) { c.On = true; c.Handed = time.Now().Add(-time.Minute) })

	// Hub #1 goes down while the node extension process keeps running.
	cancel1()
	waitFor(t, "node detached from hub 1", func() bool {
		nodeProcs.mu.Lock()
		p := nodeProcs.byID["faker"]
		nodeProcs.mu.Unlock()
		if p == nil {
			return false
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.hub == nil
	})
	_ = sp1

	// Messages arrive during the hub restart window: one in an open chat, one /ai request, one in a closed chat.
	now := time.Now().Format(time.RFC3339)
	feed(t, nodeDir, "faker",
		fakeMsg{Chat: "c_open", ChatName: "已开群", ID: "m_open", Sender: "u2", SenderName: "甲", Text: "窗口期普通消息", At: now},
		fakeMsg{Chat: "c_closed", ChatName: "未开群", ID: "m_ai", Sender: "me", SenderName: "我", Mine: true, Text: "/ai 窗口期指令", At: now},
		fakeMsg{Chat: "c_closed", ChatName: "未开群", ID: "m_closed", Sender: "u2", SenderName: "乙", Text: "窗口期预览", At: now},
	)
	waitFor(t, "messages queued locally while hub is down", func() bool {
		b, err := os.ReadFile(filepath.Join(nodeDir, "ext", "faker", "data", "pending.json"))
		return err == nil && strings.Contains(string(b), "m_open") && strings.Contains(string(b), "m_ai") && strings.Contains(string(b), "m_closed")
	})

	// Hub #2 starts from the same database and reconnects to the still-running node extension.
	sp2, h2, cancel2 := startHub()
	defer cancel2()
	waitFor(t, "all queued messages delivered to hub 2", func() bool {
		return h2.store.hasMsg("faker:c_open", "m_open") &&
			h2.store.hasMsg("faker:c_closed", "m_ai") &&
			len(extTriggers(sp2, "ext:faker")) == 1
	})
	ts := extTriggers(sp2, "ext:faker")
	if len(ts) != 1 || ts[0].Ref != "m_ai" || ts[0].Thread != "ext:faker:c_closed" || !strings.Contains(ts[0].Text, "窗口期指令") {
		t.Fatalf("triggers after restart = %+v, want m_ai once", ts)
	}
	if c, ok := h2.store.conv("faker:c_closed"); !ok || !strings.Contains(c.Preview, "窗口期预览") {
		t.Fatalf("closed conv preview after restart = %+v %v", c, ok)
	}

	// Replaying the same /ai message ID (e.g. lost ACK) must not create a second trigger.
	owner := true
	dup, _ := json.Marshal(extproto.Receive{Message: extproto.Message{
		ID: "m_ai", ChatID: "c_closed", Chat: "未开群", ChatKind: "dm", Sender: "me", SenderName: "我", Owner: &owner, Text: "/ai 窗口期指令", At: now,
	}})
	if _, err := h2.run("faker").handle(t.Context(), extproto.MChannelReceive, dup); err != nil {
		t.Fatal(err)
	}
	if got := len(extTriggers(sp2, "ext:faker")); got != 1 {
		t.Fatalf("duplicate replay produced %d triggers, want 1", got)
	}
}

// When the hub tunnel drops while a program->hub request is already in flight,
// extProc answers it with hub_unavailable immediately instead of leaving the
// extension blocked until timeout (or forever).
func TestExtProcFailsInflightOnHubDetach(t *testing.T) {
	var hubBuf, progBuf syncBuffer
	hub := &lineWriter{w: &hubBuf}
	prog := &lineWriter{w: &progBuf}
	p := &extProc{id: "x", hub: hub, prog: prog, inflight: map[string]struct{}{}}

	p.fromProgram(prog, extproto.Frame{ID: "p1", Method: extproto.MChannelReceive, Params: json.RawMessage(`{}`)})
	if !strings.Contains(hubBuf.String(), `"p1"`) {
		t.Fatalf("request did not reach hub: %s", hubBuf.String())
	}
	if progBuf.String() != "" {
		t.Fatalf("unexpected early error: %s", progBuf.String())
	}

	p.detach(hub)
	var f extproto.Frame
	if err := json.Unmarshal([]byte(strings.TrimSpace(progBuf.String())), &f); err != nil {
		t.Fatalf("unmarshal detach reply %q: %v", progBuf.String(), err)
	}
	if f.ID != "p1" || f.Error == nil || f.Error.Code != extproto.ErrHubUnavailable {
		t.Fatalf("detach reply = %+v, want hub_unavailable for p1", f)
	}
}

// When a node reconnects after a hub restart, extHost wakes extensions waiting
// on extRedialWait instead of waiting the full 10 seconds.
func TestExtHostWakesMachineOnReconnect(t *testing.T) {
	_, h, _ := newExtTestHub(t)
	h.mu.Lock()
	h.cat["rem"] = fakeManifest("rem", "receive")
	h.mu.Unlock()
	if _, err := h.configure("rem", func(x *ExtSetting) { x.Enabled = true; x.Machine = "devbox" }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.configure("rem", func(x *ExtSetting) { x.Enabled = false }) })
	waitFor(t, "initial dial failed while machine offline", func() bool {
		return h.view("rem").Problem != ""
	})

	dialed := make(chan struct{}, 1)
	h.s.herdr.attach(&Machine{
		ID: "devbox", Name: "devbox", Online: true,
		ext: func(ctx context.Context) (net.Conn, error) {
			select {
			case dialed <- struct{}{}:
			default:
			}
			a, b := net.Pipe()
			t.Cleanup(func() { a.Close(); b.Close() })
			return b, nil
		},
	})
	h.wakeMachine("devbox")
	select {
	case <-dialed:
	case <-time.After(2 * time.Second):
		t.Fatal("extRun did not wake promptly when its node reconnected")
	}
}
