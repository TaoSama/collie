package wechat

import (
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// pass is one deliverOnce, a minute after the previous one (the per-chat
// dispatch interval of the merged queue).
func (f *gwgRecovery) pass(maxChars int, active func() bool) (ResultDispatchSummary, error) {
	f.now.Add(ResultDispatchMs + 1)
	if maxChars == 0 {
		maxChars = 3000
	}
	s := f.state
	c := newTestClient(f.t, s, f.loop, fakeHTTP(f.fetch))
	canDispatch := func(chat, kind string) bool { return s.ContextTokens[chat] != "" && s.CanSendToChat(chat, kind) }
	deferReason := func(chat, kind string) string {
		if r := s.SendDeferReason(chat, kind); r != "" {
			return r
		}
		return DeferContextChanged
	}
	runner := NewResultDeliveryRunner(f.loop, ResultRunnerOptions{State: s, Transport: gwgTransport{c, maxChars},
		Budget: func(chat, kind string) ResultBudget {
			if f.budgetHook != nil {
				f.budgetHook()
			}
			return ResultBudget{Allowed: canDispatch(chat, kind), Remaining: s.RemainingSendBudget(chat), Reason: deferReason(chat, kind)}
		},
		ContextFingerprint: func(chat string) string {
			return jsonText([]string{s.ContextTokens[chat], s.ContextWindows[chat].LastInboundKey, c.budgetEpoch()})
		},
		IsReplyPending: f.replyPending})
	if active == nil {
		active = func() bool { return true }
	}
	d := NewResultDispatcher(ResultDispatcherOptions{Service: f.svc, Store: f.store, Runner: runner, State: s, CanDispatch: canDispatch,
		DeferReason: deferReason, IsReplyPending: f.replyPending, Active: active})
	var chats []string
	for _, p := range f.svc.plans {
		if !containsString(chats, p.ChatID) {
			chats = append(chats, p.ChatID)
		}
	}
	sort.Strings(chats)
	var summary ResultDispatchSummary
	var err error
	f.loop.do(func() { summary, err = d.DeliverOnce(chats) })
	return summary, err
}

func (f *gwgRecovery) mustPass(maxChars int) ResultDispatchSummary {
	f.t.Helper()
	s, err := f.pass(maxChars, nil)
	mustNoErr(f.t, err)
	return s
}

func gwgWantErr(t *testing.T, err error, text string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), text) {
		t.Fatalf("error = %v, want %q", err, text)
	}
}

func gwgStepKey(p ResultDelivery) string { return resultStepKey(p, 0) }

// gwgOutboxPath keeps the result outbox in a directory of its own, so its
// checkpoints can be failed apart from the state file's.
func gwgOutboxPath(dir string) string { return filepath.Join(dir, "results", resultOutboxFileName) }

// gwgFailOutboxWrites fails result-outbox checkpoints, before they replace
// the file, while when() holds.
func gwgFailOutboxWrites(t *testing.T, f *gwgRecovery, when func() bool, err error) {
	target := filepath.Dir(gwgOutboxPath(f.dir))
	checkpointStep = func(step, path string) error {
		if step == "open-file" && filepath.Dir(path) == target && when() {
			return err
		}
		return nil
	}
	t.Cleanup(func() { checkpointStep = nil })
}

// from: gateway/tests/gateway/notification-recovery.test.ts: claim intent is durable before dispatch and lost response resumes the same attempt via GET
func TestGwgRecoveryClaimIntentDurable(t *testing.T) {
	f := gwgRecoverySetup(t)
	item := gwgNotification(1, testChat)
	f.svc.claim = func(id string) error {
		ref, ok := f.durableRef(item.ID)
		if !ok || ref.Status != RefClaiming || ref.AttemptID != f.svc.plans[id].AttemptID {
			t.Errorf("claim intent not durable before the claim: %+v", ref)
		}
		return errors.New("claim response lost")
	}
	_, err := f.pass(0, nil)
	gwgWantErr(t, err, "claim response lost")
	if len(f.sent()) != 0 {
		t.Fatal("sent after a lost claim")
	}
	attempt := f.svc.get(item.ID).AttemptID
	f.svc.claim = nil
	f.restart()
	f.svc.resetCalls()
	if s := f.mustPass(0); s.Accepted != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if calls := f.svc.callList(); calls[0] != "get:"+item.ID || f.svc.has("claim:") {
		t.Fatalf("calls = %v", calls)
	}
	if f.svc.get(item.ID).AttemptID != attempt || len(f.sent()) != 1 {
		t.Fatal("recovery did not resume the same attempt once")
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: recovery rejects changed notification ${field}
func TestGwgRecoveryRejectsChangedIdentity(t *testing.T) {
	for _, field := range []string{"source", "text"} {
		t.Run(field, func(t *testing.T) {
			f := gwgRecoverySetup(t)
			item := gwgNotification(1, testChat)
			f.svc.claim = func(string) error { return errors.New("claim response lost") }
			_, err := f.pass(0, nil)
			gwgWantErr(t, err, "claim response lost")
			current := f.svc.get(item.ID)
			if field == "source" {
				current.Source.Type = "goal"
			} else {
				current.Messages = []string{"tampered notification"}
			}
			f.svc.set(current)
			f.svc.claim = nil
			f.restart()
			_, err = f.pass(0, nil)
			gwgWantErr(t, err, "identity mismatch")
			if ref, _ := f.durableRef(item.ID); len(f.sent()) != 0 || ref.Status != RefClaiming {
				t.Fatalf("sent %d, ref %+v", len(f.sent()), ref)
			}
		})
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: receipt intent survives restart and retries only the receipt
func TestGwgRecoveryReceiptIntentSurvivesRestart(t *testing.T) {
	f := gwgRecoverySetup(t)
	item := gwgNotification(1, testChat)
	f.svc.receipt = func() error {
		if ref, _ := f.durableRef(item.ID); ref.Status != RefReceipt || ref.ReceiptStatus != ResultAccepted {
			t.Errorf("receipt intent not durable: %+v", ref)
		}
		return errors.New("receipt unavailable")
	}
	_, err := f.pass(0, nil)
	gwgWantErr(t, err, "receipt unavailable")
	if len(f.sent()) != 1 {
		t.Fatal("not sent once")
	}
	f.svc.receipt = nil
	f.restart()
	f.svc.resetCalls()
	if s := f.mustPass(2); s.Accepted != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if calls := f.svc.callList(); calls[0] != "get:"+item.ID || len(f.sent()) != 1 {
		t.Fatalf("calls %v, %d sends", calls, len(f.sent()))
	}
	if _, ok := f.durableRef(item.ID); ok || f.state.ReplyDeliveries[gwgStepKey(item)].AcknowledgedChunks != 1 {
		t.Fatal("receipt not finished or evidence lost")
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: partial budget exhaustion defers durably and continues frozen chunks after refreshed context
func TestGwgRecoveryPartialBudgetDefersAndContinues(t *testing.T) {
	item := gwgWithText(gwgNotification(1, testChat), "abcdefghijkl")
	f := gwgRecoverySetup(t, item)
	f.setRemaining(1)
	if s := f.mustPass(4); s.Skipped != 1 {
		t.Fatalf("summary = %+v", s)
	}
	gwgEqual(t, "sent", f.texts(), []string{"abcd"})
	// Not retryable: the result waits for the next message instead of a pending reference.
	if p := f.svc.get(item.ID); p.LastDeferReason != DeferBudgetExhausted || p.Status != ResultWaitingForUser {
		t.Fatalf("plan = %+v", p)
	}
	f.restart()
	f.inbound(testChat, "")
	if s := f.mustPass(4); s.Accepted != 1 {
		t.Fatalf("summary = %+v", s)
	}
	gwgEqual(t, "sent", f.texts(), []string{"abcd", "efgh", "ijkl"})
	ids := map[string]bool{}
	for _, s := range f.sent() {
		ids[s.clientID] = true
	}
	if len(ids) != 3 {
		t.Fatal("chunks reused a client id")
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: lost defer response recovers matching remote marker without reusing the old attempt
func TestGwgRecoveryLostDeferResponse(t *testing.T) {
	item := gwgWithText(gwgNotification(1, testChat), "abcdefgh")
	f := gwgRecoverySetup(t, item)
	f.setRemaining(1)
	f.svc.defer_ = func() error {
		if ref, _ := f.durableRef(item.ID); ref.Status != RefDefer || ref.DeferReason != DeferBudgetExhausted {
			t.Errorf("defer intent not durable: %+v", ref)
		}
		return errors.New("defer response lost")
	}
	_, err := f.pass(4, nil)
	gwgWantErr(t, err, "defer response lost")
	previous, _ := f.durableRef(item.ID)
	f.svc.defer_ = nil
	f.restart()
	f.inbound(testChat, "")
	f.svc.resetCalls()
	// The matching marker finishes the old intent; the refreshed window then sends in the same pass.
	if s := f.mustPass(4); s.Accepted != 1 {
		t.Fatalf("summary = %+v", s)
	}
	calls := f.svc.callList()
	get, claim := -1, -1
	for i, c := range calls {
		if c == "get:"+item.ID && get < 0 {
			get = i
		}
		if c == "claim:"+item.ID && claim < 0 {
			claim = i
		}
	}
	if get < 0 || claim < get {
		t.Fatalf("calls = %v", calls)
	}
	if f.svc.get(item.ID).AttemptID == previous.AttemptID {
		t.Fatal("old attempt reused")
	}
	gwgEqual(t, "sent", f.texts(), []string{"abcd", "efgh"})
}

// from: gateway/tests/gateway/notification-recovery.test.ts: defer marker mismatch fails closed and preserves the durable intent
func TestGwgRecoveryDeferMarkerMismatch(t *testing.T) {
	item := gwgWithText(gwgNotification(1, testChat), "abcdefgh")
	f := gwgRecoverySetup(t, item)
	f.setRemaining(1)
	f.svc.defer_ = func() error { return errors.New("lost") }
	_, err := f.pass(4, nil)
	gwgWantErr(t, err, "lost")
	tampered := item
	tampered.LastDeferredAttemptID, tampered.LastDeferReason = "00000000-0000-4000-8000-999999999999", DeferBudgetExhausted
	f.svc.set(tampered)
	f.svc.defer_ = nil
	f.restart()
	_, err = f.pass(4, nil)
	gwgWantErr(t, err, "defer recovery attempt mismatch")
	if ref, _ := f.durableRef(item.ID); ref.Status != RefDefer || len(f.sent()) != 1 {
		t.Fatalf("ref %+v, %d sends", ref, len(f.sent()))
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: claimed recovery with exhausted local budget defers without another network send
func TestGwgRecoveryClaimedWithExhaustedBudgetDefers(t *testing.T) {
	f := gwgRecoverySetup(t)
	item := gwgNotification(1, testChat)
	f.svc.claim = func(string) error { return errors.New("lost claim response") }
	_, err := f.pass(0, nil)
	gwgWantErr(t, err, "lost claim")
	f.svc.claim = nil
	f.restart()
	f.setRemaining(0)
	if s := f.mustPass(0); s.Skipped != 1 {
		t.Fatalf("summary = %+v", s)
	}
	// The merged queue parks the result until the next message instead of a pending notification.
	if p := f.svc.get(item.ID); len(f.sent()) != 0 || p.Status != ResultWaitingForUser || p.LastDeferReason != DeferBudgetExhausted {
		t.Fatalf("plan %+v, %d sends", p, len(f.sent()))
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: in-flight uncertainty is sealed before any budget defer can reopen the attempt
func TestGwgRecoveryInFlightUncertaintySealed(t *testing.T) {
	f := gwgRecoverySetup(t)
	item := gwgNotification(1, testChat)
	f.sendHook = func() error { return errors.New("send response lost") }
	f.svc.receipt = func() error { return errors.New("receipt lost") }
	_, err := f.pass(0, nil)
	gwgWantErr(t, err, "receipt lost")
	if s, _ := f.durable(); !s.HasUncertainReply(gwgStepKey(item)) {
		t.Fatal("in-flight intent not durable")
	}
	f.sendHook, f.svc.receipt = nil, nil
	f.restart()
	f.setRemaining(0)
	if s := f.mustPass(0); s.Uncertain != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if len(f.sent()) != 1 || f.svc.get(item.ID).Status != ResultUncertain {
		t.Fatal("not sealed as uncertain")
	}
	f.inbound(testChat, "")
	f.mustPass(0)
	if len(f.sent()) != 1 || f.svc.get(item.ID).Status != ResultUncertain {
		t.Fatal("uncertain send replayed")
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: expired partial delivery keeps outbox and acknowledged reply evidence
func TestGwgRecoveryExpiredPartialKeepsEvidence(t *testing.T) {
	setNow(t, 1_800_000_000_000)
	item := gwgWithText(gwgNotification(1, testChat), "abcdefgh")
	item.ExpiresAt = nowMs() + 60_000
	f := gwgRecoverySetup(t, item)
	// gwgRecoverySetup sets its own clock: keep the plan's short expiry relative to it.
	item.CreatedAt, item.ExpiresAt = nowMs()-1000, nowMs()+2*ResultDispatchMs
	f.svc.set(item)
	f.setRemaining(1)
	f.mustPass(4)
	f.now.Add(2*ResultDispatchMs + 1)
	f.restart()
	f.mustPass(0)
	if s, _ := f.durable(); s.ReplyDeliveries[gwgStepKey(item)].AcknowledgedChunks != 1 {
		t.Fatal("acknowledged evidence lost")
	}
	if p := f.svc.get(item.ID); p.Status == ResultAccepted || len(f.sent()) != 1 {
		t.Fatalf("plan %+v, %d sends", p, len(f.sent()))
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: an exhausted account blocks every notification until a real inbound refreshes the shared allowance
func TestGwgRecoveryExhaustedAccountBlocksAll(t *testing.T) {
	f := gwgRecoverySetup(t, gwgNotification(1, "a"), gwgNotification(2, "b"))
	f.setRemaining(0)
	if s := f.mustPass(0); s.Accepted != 0 || len(f.sent()) != 0 {
		t.Fatalf("summary = %+v", s)
	}
	f.inbound("b", "context-b")
	w := f.state.ContextWindows["a"]
	w.RefreshedAt -= f.state.Policy.ContextWindowMs
	f.state.ContextWindows["a"] = w
	if s := f.mustPass(0); s.Accepted != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if s := f.sent(); len(s) != 1 || s[0].chat != "b" {
		t.Fatalf("sent = %+v", s)
	}
	if f.svc.has("claim:00000000-0000-4000-8000-000000000001") {
		t.Fatal("blocked chat claimed")
	}
	var lists []string
	for _, c := range f.svc.callList() {
		if strings.HasPrefix(c, "list:") {
			lists = append(lists, c)
		}
	}
	gwgEqual(t, "lists", lists, []string{"list:b"})
}
