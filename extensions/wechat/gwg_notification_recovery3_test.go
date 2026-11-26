package wechat

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// from: gateway/tests/gateway/notification-recovery.test.ts: chat lookup network failure still gives an independent chat a delivery turn
func TestGwgRecoveryLookupFailureIsolated(t *testing.T) {
	f := gwgRecoverySetup(t, gwgNotification(1, "a"), gwgNotification(2, "b"))
	f.svc.list = func(chat string) error {
		if chat == "a" {
			return errors.New("chat a unavailable")
		}
		return nil
	}
	_, err := f.pass(0, nil)
	gwgWantErr(t, err, "chat a unavailable")
	if s := f.sent(); len(s) != 1 || s[0].chat != "b" {
		t.Fatalf("sent = %+v", s)
	}
}

func gwgPutReply(t *testing.T, f *gwgRecovery, key string) {
	mustNoErr(t, f.state.PutOutbox(DeliveryOutboxItem{Kind: "reply", DeliveryKey: key, ChatID: testChat, MessageID: "message", SplitMaxChars: 100, Status: OutboxPending}))
}

// from: gateway/tests/gateway/notification-recovery.test.ts: pending ordinary reply reserves its chat budget against progress notifications
func TestGwgRecoveryPendingReplyBlocksProgress(t *testing.T) {
	item := gwgProgress(2, gwgNotification(1, testChat))
	f := gwgRecoverySetup(t, item)
	gwgPutReply(t, f, "ordinary-reply")
	f.mustPass(0)
	if len(f.sent()) != 0 || f.svc.has("claim:") {
		t.Fatal("progress went ahead of a pending reply")
	}
	mustNoErr(t, f.state.RemoveOutbox("ordinary-reply"))
	if s := f.mustPass(0); s.Accepted != 1 || f.state.AccountBudget.ProgressSent != 1 {
		t.Fatalf("summary = %+v, budget %+v", s, f.state.AccountBudget)
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: pending ordinary reply does not block a final notification
func TestGwgRecoveryPendingReplyThenFinal(t *testing.T) {
	f := gwgRecoverySetup(t)
	gwgPutReply(t, f, "ordinary-reply")
	// Merged queue: any pending reply of the account goes first, a final result included.
	f.mustPass(0)
	if len(f.sent()) != 0 {
		t.Fatal("result went ahead of the pending reply")
	}
	mustNoErr(t, f.state.RemoveOutbox("ordinary-reply"))
	if s := f.mustPass(0); s.Accepted != 1 || f.state.AccountBudget.ProgressSent != 0 {
		t.Fatalf("summary = %+v", s)
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: an existing blocked progress reference does not hide a new final notification
func TestGwgRecoveryBlockedProgressDoesNotHideFinal(t *testing.T) {
	final := gwgNotification(1, testChat)
	progress := gwgProgress(2, final)
	f := gwgRecoverySetup(t, progress)
	gwgPutReply(t, f, "ordinary")
	f.mustPass(0)
	f.svc.set(final)
	mustNoErr(t, f.state.RemoveOutbox("ordinary"))
	if s := f.mustPass(0); s.Accepted != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if s := f.sent(); len(s) != 1 || s[0].text != final.Messages[0] {
		t.Fatalf("sent = %+v; the final result outranks the progress", s)
	}
	if p := f.svc.get(progress.ID); p.Status != ResultReady {
		t.Fatalf("progress = %+v", p)
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: crash before receipt intent preserves in-flight evidence and never defers an ambiguous send
func TestGwgRecoveryCrashBeforeReceiptIntent(t *testing.T) {
	f := gwgRecoverySetup(t)
	item := gwgNotification(1, testChat)
	sentOnce := false
	f.sendHook = func() error { sentOnce = true; return errors.New("ambiguous send") }
	gwgFailOutboxWrites(t, f, func() bool { return sentOnce }, errors.New("receipt intent disk failure"))
	_, err := f.pass(0, nil)
	gwgWantErr(t, err, "receipt intent disk failure")
	if ref, _ := f.durableRef(item.ID); ref.Status != RefClaimed {
		t.Fatalf("ref = %+v", ref)
	}
	if s, _ := f.durable(); !s.HasUncertainReply(gwgStepKey(item)) {
		t.Fatal("in-flight evidence lost")
	}
	checkpointStep = nil
	f.sendHook = nil
	f.restart()
	f.setRemaining(0)
	if s := f.mustPass(0); s.Uncertain != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if len(f.sent()) != 1 || f.svc.has("defer:") || f.svc.get(item.ID).Status != ResultUncertain {
		t.Fatal("ambiguous send deferred or replayed")
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: failed durable claim intent prevents the Controller claim request
func TestGwgRecoveryFailedClaimIntent(t *testing.T) {
	f := gwgRecoverySetup(t)
	item := gwgNotification(1, testChat)
	writes := 0
	gwgFailOutboxWrites(t, f, func() bool { writes++; return writes == 2 }, errors.New("claim intent disk failure"))
	_, err := f.pass(0, nil)
	gwgWantErr(t, err, "claim intent disk failure")
	checkpointStep = nil
	if ref, _ := f.durableRef(item.ID); f.svc.has("claim:") || ref.Status != RefPending || len(f.sent()) != 0 {
		t.Fatalf("ref %+v, calls %v", ref, f.svc.callList())
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: progress claim recovery yields to a pending ordinary reply through durable defer
func TestGwgRecoveryProgressYieldsToReply(t *testing.T) {
	progress := gwgProgress(2, gwgNotification(1, testChat))
	f := gwgRecoverySetup(t, progress)
	f.svc.claim = func(string) error { return errors.New("claim response lost") }
	_, err := f.pass(0, nil)
	gwgWantErr(t, err, "claim response lost")
	f.svc.claim = nil
	f.restart()
	gwgPutReply(t, f, "ordinary")
	f.mustPass(0)
	// The merged queue names the reason reply_pending, which retries once the reply is gone.
	if p := f.svc.get(progress.ID); len(f.sent()) != 0 || p.Status != ResultReady || p.LastDeferReason != "reply_pending" {
		t.Fatalf("plan %+v, %d sends", p, len(f.sent()))
	}
	if _, ok := f.durableRef(progress.ID); ok {
		t.Fatal("defer did not finish its intent")
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: claim cancellation before send produces a durable defer without an uncertain receipt
func TestGwgRecoveryClaimCancellationDefers(t *testing.T) {
	f := gwgRecoverySetup(t)
	item := gwgNotification(1, testChat)
	running := true
	f.svc.claim = func(string) error { running = false; return nil }
	if s, err := f.pass(0, func() bool { return running }); err != nil || s.Skipped != 1 {
		t.Fatalf("summary = %+v, %v", s, err)
	}
	if p := f.svc.get(item.ID); p.Status == ResultClaimed || p.LastDeferReason != DeferContextChanged || len(f.sent()) != 0 || f.svc.has("receipt:") {
		t.Fatalf("plan %+v, calls %v", p, f.svc.callList())
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: context changes during typing safely defer before any message request
func TestGwgRecoveryContextChangeDefers(t *testing.T) {
	f := gwgRecoverySetup(t)
	item := gwgNotification(1, testChat)
	// Typing never waits here; the token changes after the attempt began, before the first message.
	changed := false
	f.budgetHook = func() {
		if !changed {
			changed = true
			f.state.SetContextToken(testChat, "new-context")
		}
	}
	if s := f.mustPass(0); s.Skipped != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if p := f.svc.get(item.ID); p.LastDeferReason != DeferContextChanged || len(f.sent()) != 0 || f.svc.has("receipt:") {
		t.Fatalf("plan %+v, %d sends", p, len(f.sent()))
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: TTL expiry between chunks retains acknowledged partial evidence without fabricating a receipt
func TestGwgRecoveryTTLBetweenChunks(t *testing.T) {
	item := gwgWithText(gwgNotification(1, testChat), "abcdefgh")
	f := gwgRecoverySetup(t, item)
	item.ExpiresAt = nowMs() + 2*ResultDispatchMs
	f.svc.set(item)
	f.sendHook = func() error { f.now.Add(2*ResultDispatchMs + 1); return nil }
	if s := f.mustPass(4); s.Skipped != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if s, _ := f.durable(); s.ReplyDeliveries[gwgStepKey(item)].AcknowledgedChunks != 1 || len(f.sent()) != 1 || f.svc.has("receipt:") {
		t.Fatal("partial evidence lost or a receipt fabricated")
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: complete chunk acknowledgements recover a failed receipt-intent write with exhausted budget${expiredLease ? " and projected uncertainty" : ""}
func TestGwgRecoveryCompleteChunksRecoverReceiptIntent(t *testing.T) {
	for _, expiredLease := range []bool{false, true} {
		t.Run(fmt.Sprint(expiredLease), func(t *testing.T) {
			f := gwgRecoverySetup(t)
			item := gwgNotification(1, testChat)
			f.setRemaining(1)
			sent := false
			f.sendHook = func() error { sent = true; return nil }
			gwgFailOutboxWrites(t, f, func() bool { return sent }, errors.New("receipt intent disk failure"))
			_, err := f.pass(0, nil)
			gwgWantErr(t, err, "receipt intent disk failure")
			s, _ := f.durable()
			if ref, _ := f.durableRef(item.ID); ref.Status != RefClaimed || s.ReplyDeliveries[gwgStepKey(item)].AcknowledgedChunks != 1 || s.AccountBudget.Remaining != 0 {
				t.Fatalf("ref %+v", ref)
			}
			checkpointStep = nil
			f.restart()
			if expiredLease {
				f.now.Add(deliveryAttemptMs + 1)
			}
			if s := f.mustPass(0); s.Accepted != 1 {
				t.Fatalf("summary = %+v", s)
			}
			if _, ok := f.durableRef(item.ID); ok || len(f.sent()) != 1 || f.svc.get(item.ID).Status != ResultAccepted || f.svc.has("defer:") {
				t.Fatal("complete acknowledgements not turned into the receipt")
			}
		})
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: accepted receipt intent survives TTL expiry and projected uncertainty without another send
func TestGwgRecoveryAcceptedReceiptSurvivesTTL(t *testing.T) {
	f := gwgRecoverySetup(t)
	item := gwgNotification(1, testChat)
	f.svc.receipt = func() error { return errors.New("receipt network failure") }
	_, err := f.pass(0, nil)
	gwgWantErr(t, err, "receipt network failure")
	f.svc.receipt = nil
	f.now.Add(86_400_001)
	f.restart()
	if s := f.mustPass(0); s.Accepted != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if len(f.sent()) != 1 || f.svc.get(item.ID).Status != ResultAccepted {
		t.Fatal("receipt not recorded once")
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: a successful final chunk crossing TTL records its real receipt without resending
func TestGwgRecoveryFinalChunkCrossingTTL(t *testing.T) {
	item := gwgNotification(1, testChat)
	f := gwgRecoverySetup(t, item)
	item.ExpiresAt = nowMs() + 2*ResultDispatchMs
	f.svc.set(item)
	f.sendHook = func() error { f.now.Add(2*ResultDispatchMs + 1); return nil }
	if s := f.mustPass(0); s.Accepted != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if len(f.sent()) != 1 || f.svc.get(item.ID).Status != ResultAccepted {
		t.Fatal("real receipt not recorded")
	}
}

// from: gateway/tests/gateway/notification-recovery.test.ts: an unsendable pending reference skips large Controller GET bodies until the budget is refreshed
func TestGwgRecoveryUnsendableSkipsReads(t *testing.T) {
	item := gwgWithText(gwgNotification(1, testChat), "abcdefgh")
	f := gwgRecoverySetup(t, item)
	f.setRemaining(1)
	f.mustPass(4)
	f.svc.resetCalls()
	f.mustPass(4)
	// Nothing is read or claimed while the chat cannot send; the chat is only marked waiting.
	for _, c := range f.svc.callList() {
		if !strings.HasPrefix(c, "block:") {
			t.Fatalf("calls = %v", f.svc.callList())
		}
	}
	f.inbound(testChat, "")
	if s := f.mustPass(4); s.Accepted != 1 {
		t.Fatalf("summary = %+v", s)
	}
	gwgEqual(t, "sent", f.texts(), []string{"abcd", "efgh"})
}

// from: gateway/tests/gateway/notification-recovery.test.ts: a stale progress claim conflict cannot starve a final notification in the same chat
func TestGwgRecoveryStaleProgressCannotStarveFinal(t *testing.T) {
	final := gwgNotification(1, testChat)
	progress := gwgProgress(2, final)
	f := gwgRecoverySetup(t, progress)
	gwgPutReply(t, f, "ordinary")
	f.mustPass(0)
	mustNoErr(t, f.state.RemoveOutbox("ordinary"))
	f.svc.set(final)
	f.svc.claim = func(id string) error {
		if id == progress.ID {
			return errors.New("Result request HTTP 409")
		}
		return nil
	}
	// The final result outranks the progress in each pass, so the conflict never comes first.
	if s := f.mustPass(0); s.Accepted != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if f.svc.get(final.ID).Status != ResultAccepted {
		t.Fatal("final not accepted")
	}
	gwgEqual(t, "sent", f.texts(), []string{final.Messages[0]})
}

// from: gateway/tests/gateway/notification-recovery.test.ts: a notification with little send time remaining is delivered while typing stays pending
func TestGwgRecoveryLittleTimeDeliveredWhileTypingPending(t *testing.T) {
	item := gwgNotification(1, testChat)
	item.CreatedAt = nowMs() - 60_000
	f := gwgRecoverySetup(t, item)
	item.CreatedAt = nowMs() - 60_000
	f.svc.set(item)
	f.svc.claim = func(id string) error {
		p := f.svc.plans[id]
		p.AttemptedAt = nowMs() - 29_990
		p.AttemptExpiresAt = p.AttemptedAt + 60_000
		f.svc.plans[id] = p
		return nil
	}
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f.typingHook = func() { <-release }
	if s := f.mustPass(0); s.Accepted != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if len(f.sent()) != 1 || f.svc.get(item.ID).Status != ResultAccepted {
		t.Fatal("not delivered while typing was pending")
	}
	once.Do(func() { close(release) })
}
