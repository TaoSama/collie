package wechat

import (
	"context"
	"errors"
	"os"
	"testing"
)

// from: gateway/tests/results/dispatcher.test.ts: a ${terminal} deferred plan cannot starve later results for the same chat
func TestRtdDispatcherTerminalDeferredPlanNoStarve(t *testing.T) {
	for _, terminal := range []string{"superseded", "purged"} {
		t.Run(terminal, func(t *testing.T) {
			f := rtdDispatchSetup(t)
			f.svc.loss, f.allowRunner = "defer", false
			f.mustFail(f.dispatcher(nil), "defer response lost")
			original := f.svc.records[f.id]
			next := original
			next.ID, next.Key, next.Status = newUUID(), "next", ResultReady
			next.LastDeferredAttemptID, next.LastDeferReason, next.LastDeferredAt, next.SubmittedMessages = "", "", 0, 0
			f.svc.records[next.ID] = next
			f.svc.listOrder = append(f.svc.listOrder, next.ID)
			if terminal == "superseded" {
				original.Status = ResultSuperseded
				f.svc.records[f.id] = original
			} else {
				delete(f.svc.records, f.id)
			}
			f.now += 60_000
			f.allowRunner = true
			if s := f.mustDeliver(f.restart()); s != (ResultDispatchSummary{Accepted: 1, Skipped: 1}) {
				t.Fatalf("summary %+v", s)
			}
			if f.svc.records[next.ID].Status != ResultAccepted || f.sends != 1 || f.svc.claims != 2 || len(f.refs()) != 0 {
				t.Fatalf("status %s sends %d claims %d", f.svc.records[next.ID].Status, f.sends, f.svc.claims)
			}
		})
	}
}

// from: gateway/tests/results/dispatcher.test.ts: a matching receipt intent reconciles an expired uncertain plan without another send
func TestRtdDispatcherReceiptIntentReconciles(t *testing.T) {
	f := rtdDispatchSetup(t)
	f.svc.onRecordReceipt = func(string, string, string) (ResultDelivery, error) {
		return ResultDelivery{}, errors.New("receipt unavailable before commit")
	}
	f.mustFail(f.dispatcher(nil), "receipt unavailable before commit")
	if f.refs()[0].Status != RefReceipt {
		t.Fatalf("refs %+v", f.refs())
	}
	item := f.svc.records[f.id]
	item.Status = ResultUncertain
	f.svc.records[f.id] = item
	f.now += 500_001
	f.svc.onRecordReceipt = nil
	if s := f.mustDeliver(f.restart()); s != (ResultDispatchSummary{Accepted: 1}) {
		t.Fatalf("summary %+v", s)
	}
	if f.sends != 1 || f.svc.claims != 1 || f.svc.receipts != 1 || len(f.refs()) != 0 {
		t.Fatalf("sends %d claims %d receipts %d", f.sends, f.svc.claims, f.svc.receipts)
	}
}

// from: gateway/tests/results/dispatcher.test.ts: a receipt request 404 keeps its intent and never qualifies as a purged GET
func TestRtdDispatcherReceipt404KeepsIntent(t *testing.T) {
	f := rtdDispatchSetup(t)
	f.svc.onRecordReceipt = func(string, string, string) (ResultDelivery, error) { return ResultDelivery{}, rtdHTTP404 }
	f.mustFail(f.dispatcher(nil), "HTTP 404")
	f.mustFail(f.restart(), "HTTP 404")
	if f.refs()[0].Status != RefReceipt || f.sends != 1 || f.svc.claims != 1 {
		t.Fatalf("refs %+v sends %d claims %d", f.refs(), f.sends, f.svc.claims)
	}
}

// from: gateway/tests/results/dispatcher.test.ts: unknown transport send writes uncertain receipt and is never automatically replayed
func TestRtdDispatcherUnknownSendUncertain(t *testing.T) {
	f := rtdDispatchSetup(t)
	f.unknown = true
	if s := f.mustDeliver(f.dispatcher(nil)); s != (ResultDispatchSummary{Uncertain: 1}) {
		t.Fatalf("summary %+v", s)
	}
	f.mustDeliver(f.restart())
	if f.sends != 1 || f.svc.records[f.id].Status != ResultUncertain {
		t.Fatalf("sends %d status %s", f.sends, f.svc.records[f.id].Status)
	}
}

// from: gateway/tests/results/dispatcher.test.ts: durable subordinate acceptance recovers a crash before receipt intent was written
func TestRtdDispatcherAcceptanceBeforeReceiptIntent(t *testing.T) {
	f := rtdDispatchSetup(t)
	f.store.failUpdate = func(next ResultReference) error {
		if next.Status == RefReceipt {
			return errors.New("intent persistence unavailable")
		}
		return nil
	}
	f.mustFail(f.dispatcher(nil), "intent persistence unavailable")
	if f.sends != 1 || f.svc.records[f.id].Status != ResultClaimed {
		t.Fatalf("sends %d status %s", f.sends, f.svc.records[f.id].Status)
	}
	f.mustDeliver(f.restart())
	if f.sends != 1 || f.svc.records[f.id].Status != ResultAccepted {
		t.Fatalf("sends %d status %s", f.sends, f.svc.records[f.id].Status)
	}
}

// from: gateway/tests/results/dispatcher.test.ts: dispatch cooldown persists across restart and prevents repeated ticks from draining backlog
func TestRtdDispatcherCooldownPersists(t *testing.T) {
	f := rtdDispatchSetup(t)
	second := f.svc.records[f.id]
	second.ID, second.Key, second.CreatedAt = newUUID(), "second", second.CreatedAt-1
	f.svc.records[second.ID] = second
	f.svc.listOrder = append(f.svc.listOrder, second.ID)
	f.mustDeliver(f.dispatcher(nil))
	if f.sends != 1 {
		t.Fatalf("sends %d", f.sends)
	}
	f.mustDeliver(f.restart())
	if f.sends != 1 {
		t.Fatalf("sends %d after restart", f.sends)
	}
	f.now += 60_000
	f.mustDeliver(f.dispatcher(nil))
	if f.sends != 2 {
		t.Fatalf("sends %d after the interval", f.sends)
	}
}

// from: gateway/tests/results/dispatcher.test.ts: pending ordinary reply keeps active results queued without claiming
func TestRtdDispatcherPendingReplyQueues(t *testing.T) {
	f := rtdDispatchSetup(t)
	f.pendingReply = true
	f.mustDeliver(f.dispatcher(nil))
	if f.svc.claims != 0 || f.sends != 0 {
		t.Fatalf("claims %d sends %d", f.svc.claims, f.sends)
	}
}

// from: gateway/tests/results/dispatcher.test.ts: API outbox is private, bounded references only, and isolated by account
func TestRtdDispatcherOutboxPrivateIsolated(t *testing.T) {
	f := rtdDispatchSetup(t)
	f.svc.loss = "claim"
	f.mustFail(f.dispatcher(nil), "claim response lost")
	st, err := os.Stat(f.outboxPath)
	mustNoErr(t, err)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	other, err := NewFileResultOutbox(f.outboxPath, func() string { return "different-account" })
	mustNoErr(t, err)
	if refs, err := other.References(); err != nil || len(refs) != 0 {
		t.Fatalf("another account sees %d references (%v)", len(refs), err)
	}
}

// from: gateway/tests/results/dispatcher.test.ts: a chat with no usable context marks ready results waiting without claiming or sending
func TestRtdDispatcherNoContextMarksWaiting(t *testing.T) {
	f := rtdDispatchSetup(t)
	d := f.dispatcher(func(o *ResultDispatcherOptions) {
		o.CanDispatch = func(string, string) bool { return false }
		o.DeferReason = func(string, string) string { return DeferWindowExpired }
	})
	f.mustDeliver(d)
	item := f.svc.records[f.id]
	if item.Status != ResultWaitingForUser || item.DeferReason != DeferWindowExpired || f.svc.claims != 0 || f.sends != 0 || f.svc.blocked != 1 {
		t.Fatalf("item %+v claims %d blocked %d", item, f.svc.claims, f.svc.blocked)
	}
	f.mustDeliver(d)
	if f.svc.blocked != 1 {
		t.Fatal("repeated ticks do not repeatedly mark the same blocked chat")
	}
}

// from: gateway/tests/results/dispatcher.test.ts: one blocked chat never prevents an independent authorized chat from delivering
func TestRtdDispatcherBlockedChatIndependent(t *testing.T) {
	f := rtdDispatchSetup(t)
	other := f.svc.records[f.id]
	other.ID, other.Key, other.ChatID = newUUID(), "other", "other"
	other.Artifact.ID, other.Artifact.ChatID = newUUID(), "other"
	f.svc.records[other.ID] = other
	f.svc.listOrder = append(f.svc.listOrder, other.ID)
	d := f.dispatcher(func(o *ResultDispatcherOptions) { o.CanDispatch = func(chat, _ string) bool { return chat == "other" } })
	f.mustDeliver(d, "chat", "other")
	if f.svc.records[f.id].Status != ResultWaitingForUser || f.svc.records[other.ID].Status != ResultAccepted || f.sends != 1 {
		t.Fatalf("statuses %s %s sends %d", f.svc.records[f.id].Status, f.svc.records[other.ID].Status, f.sends)
	}
}

// from: gateway/tests/results/dispatcher.test.ts: genuine inbound resume intent survives restart and waits until the ordinary reply finishes
func TestRtdDispatcherResumeIntentSurvivesRestart(t *testing.T) {
	f := rtdDispatchSetup(t)
	item := f.svc.records[f.id]
	item.Status = ResultWaitingForUser
	f.svc.records[f.id] = item
	mustNoErr(t, f.dispatcher(nil).NoteInbound("chat", "genuine-inbound-generation"))
	f.pendingReply = true
	f.mustDeliver(f.restart())
	resumes, err := f.store.PendingResumes()
	mustNoErr(t, err)
	if f.svc.records[f.id].Status != ResultWaitingForUser || len(resumes) != 1 {
		t.Fatalf("status %s resumes %d", f.svc.records[f.id].Status, len(resumes))
	}
	f.pendingReply = false
	f.mustDeliver(f.dispatcher(nil))
	resumes, err = f.store.PendingResumes()
	mustNoErr(t, err)
	if f.svc.records[f.id].Status != ResultAccepted || len(resumes) != 0 || f.sends != 1 || f.svc.lastResumedGeneration != "genuine-inbound-generation" {
		t.Fatalf("status %s resumes %d sends %d", f.svc.records[f.id].Status, len(resumes), f.sends)
	}
}

// from: gateway/tests/results/dispatcher.test.ts: partly accepted results pin shared evidence across capacity eviction and resume with only the file
func TestRtdDispatcherPartialAcceptancePinsEvidence(t *testing.T) {
	f := rtdDispatchSetup(t)
	item := f.svc.records[f.id]
	item.Mode, item.Messages = ModeAttachment, []string{"summary"}
	f.svc.records[f.id] = item
	f.state.SetReplyEvidenceProtection(func(k string) bool { return f.store.ProtectsDeliveryKey(k) })
	failUpload, textSends, fileSends := true, 0, 0
	accept := func(key, text string) {
		d, err := f.state.PrepareReplyDelivery(key, sha256Hex(text), 1)
		mustNoErr(t, err)
		if d.AcknowledgedChunks > 0 {
			return
		}
		mustNoErr(t, f.state.BeginReplyChunk(key, 0))
		mustNoErr(t, f.state.ConfirmReplyChunk(key, 0))
	}
	runner := NewResultDeliveryRunner(f.loop, ResultRunnerOptions{State: f.state, Now: func() int64 { return f.now },
		ContextFingerprint: func(string) string { return "context" },
		Budget:             func(string, string) ResultBudget { return ResultBudget{Allowed: true, Remaining: 10} },
		IsReplyPending:     func(string) bool { return false },
		DownloadArtifact:   func(context.Context, ResultArtifact) ([]byte, error) { return []byte("artifact"), nil },
		Transport: &rtdTransport{text: func(_ string, text string, o ResultSendOptions) error {
			accept(o.DeliveryKey, text)
			textSends++
			return nil
		},
			file: func(_ []byte, o ResultSendOptions) error {
				if failUpload {
					return errors.New("upload not submitted")
				}
				accept(o.DeliveryKey, "file")
				fileSends++
				return nil
			}}})
	withRunner := func(o *ResultDispatcherOptions) { o.Runner = runner }
	f.mustDeliver(f.dispatcher(withRunner))
	refs := f.refs()
	if f.svc.records[f.id].Status != ResultWaitingForUser || refs[0].Status != RefWaiting || refs[0].SubmittedMessages != 1 {
		t.Fatalf("status %s refs %+v", f.svc.records[f.id].Status, refs)
	}
	stepKey := "result:" + f.id + ":step:0"
	if f.state.ReplyDeliveries[stepKey].AcknowledgedChunks != 1 {
		t.Fatal("summary not acknowledged")
	}
	f.mustDeliver(f.restart())
	if f.refs()[0].Status != RefWaiting {
		t.Fatal("restart preserves the dormant pin")
	}
	// The first 999 completed plans are placed directly (a checkpoint each
	// would take a minute); the last two go through the API and evict.
	for i := 0; i < MaxReplyDeliveries-1; i++ {
		f.state.ReplyDeliveries["unrelated:"+itoa(i)] = ReplyDeliveryState{Digest: sha256Hex("text " + itoa(i)), TotalChunks: 1, AcknowledgedChunks: 1}
	}
	accept("unrelated:999", "text 999")
	accept("unrelated:1000", "text 1000")
	if len(f.state.ReplyDeliveries) != MaxReplyDeliveries || f.state.ReplyDeliveries[stepKey].AcknowledgedChunks != 1 {
		t.Fatal("capacity cleanup cannot erase the acknowledged summary")
	}
	failUpload = false
	f.now += 60_000
	mustNoErr(t, f.svc.resumeDeliveries("chat", ""))
	if s := f.mustDeliver(f.dispatcher(withRunner)); s != (ResultDispatchSummary{Accepted: 1}) {
		t.Fatalf("summary %+v", s)
	}
	if textSends != 1 || fileSends != 1 || len(f.refs()) != 0 {
		t.Fatalf("text %d file %d refs %d", textSends, fileSends, len(f.refs()))
	}
}
