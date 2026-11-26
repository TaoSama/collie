package wechat

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

var gwgFairReplies = map[string]string{"m1": "AB", "m2": "C", "m3": "D", "m4": "E", "other": "Z"}

func gwgFairAnswer(m extproto.Message) string {
	for mid, text := range gwgFairReplies {
		if gwgIs(m, mid) {
			return text
		}
	}
	return ""
}

func gwgFairness(t *testing.T, policy PolicyOptions) (*harness, func(int64), int64) {
	if policy.SendBudget == 0 {
		policy.SendBudget = 1
	}
	h := newHarness(t, harnessOptions{noWindow: true, policy: policy, splitMax: 1})
	timestamp := time.Now().UnixMilli() - 1000
	page := gwgPaged(h, timestamp)
	return h, page.Store, timestamp
}

// from: gateway/tests/gateway/reply-fairness.test.ts: fresh nonempty replies yield budget to deferred chunks, including after restart
func TestGwgFreshRepliesYieldToDeferredChunks(t *testing.T) {
	h, setPage, timestamp := gwgFairness(t, PolicyOptions{})
	gwgAnswerWith(h, gwgFairAnswer)
	gwgPoll(t, h)
	oldKey := h.state.ComputeDeliveryKey(testChat, "m1")
	gwgEqual(t, "sent", h.sentTexts(), []string{"A"})
	h.do(func() {
		if h.state.ReplyDeliveries[oldKey].AcknowledgedChunks != 1 || h.state.Outbox[oldKey].Status != OutboxPending {
			t.Fatal("deferred chunk not pending")
		}
	})
	restarted := gwgReopen(t, h, PolicyOptions{SendBudget: 1}, 1)
	gwgAnswerWith(restarted, gwgFairAnswer)
	restarted.do(func() {
		_, err := restarted.state.RecordInboundContext(restarted.state.ComputeDeliveryKey(testChat, "m2"), testChat, i64(timestamp+2), true, "context-2")
		mustNoErr(t, err)
	})
	for i, expected := range []string{"B", "C", "D"} {
		setPage(int64(i + 2))
		before := len(h.sent())
		gwgPoll(t, restarted)
		if got := h.sentTexts(); len(got)-before != 1 || got[len(got)-1] != expected {
			t.Fatalf("pass %d sent %v; each inbound grants only one send", i, got)
		}
		restarted.do(func() {
			if restarted.state.RemainingSendBudget(testChat) != 0 {
				t.Fatal("budget left over")
			}
		})
	}
	gwgEqual(t, "sent", h.sentTexts(), []string{"A", "B", "C", "D"})
	if n := len(h.host.messages()); n != 4 {
		t.Fatalf("%d hand-overs", n)
	}
	restarted.do(func() {
		if _, ok := restarted.state.Outbox[oldKey]; ok || restarted.state.ReplyDeliveries[oldKey].AcknowledgedChunks != 2 || restarted.state.SyncBuf != "cursor-4" {
			t.Fatal("old reply not finished in turn")
		}
	})
	mustNoErr(t, gwgFlush(restarted, ""))
	gwgEqual(t, "sent", h.sentTexts(), []string{"A", "B", "C", "D"})
}

// from: gateway/tests/gateway/reply-fairness.test.ts: a failed admission engine acknowledgement admits new work and retries only its receipt
func TestGwgFailedAdmissionAckRetriesReceiptOnly(t *testing.T) {
	h, setPage, _ := gwgFairness(t, PolicyOptions{})
	gwgAnswerWith(h, gwgFairAnswer)
	flaky := gwgFlaky(h)
	oldAcks := 0
	oldKey := h.state.ComputeDeliveryKey(testChat, "m1")
	flaky.fail = func(d extproto.Delivery) error {
		if d.Ref == oldKey {
			oldAcks++
			if oldAcks == 1 {
				return errors.New("synthetic receipt persistence failure")
			}
		}
		return nil
	}
	gwgPoll(t, h)
	setPage(2)
	gwgPoll(t, h)
	if oldAcks != 2 {
		t.Fatalf("%d old acknowledgements; the failed one is retried without reading or sending again", oldAcks)
	}
	gwgEqual(t, "sent", h.sentTexts(), []string{"A", "B"})
	if n := len(h.host.messages()); n != 2 {
		t.Fatalf("%d hand-overs", n)
	}
	h.do(func() {
		s := h.state
		m2 := s.ComputeDeliveryKey(testChat, "m2")
		if _, ok := s.Outbox[oldKey]; ok || s.Outbox[m2].Status != OutboxPending || !s.IsMessageProcessed(m2) || s.SyncBuf != "cursor-2" {
			t.Fatal("new work not admitted or old receipt not finished")
		}
	})
}

// from: gateway/tests/gateway/reply-fairness.test.ts: global recovery skips another active chat's ${status} engine item until its query finishes
func TestGwgGlobalRecoverySkipsActiveChat(t *testing.T) {
	for _, status := range []string{OutboxPending, OutboxReceipt} {
		t.Run(status, func(t *testing.T) {
			// Budget for both the old reply and the new answer: what is checked is who may touch the active chat.
			h, _, _ := gwgFairness(t, PolicyOptions{SendBudget: 3})
			oldKey := gwgPutEngine(t, h, testChat, "old", status, "Q")
			flaky := gwgFlaky(h)
			oldTouches := 0
			var mu sync.Mutex
			// The admission recovery of the old item fails once, like the original's first operation.
			h.setHooks(func(k *harnessHooks) {
				k.send = func(msg map[string]any) (*http.Response, error) {
					mu.Lock()
					defer mu.Unlock()
					if gwgText(msg) == "Q" {
						oldTouches++
						if oldTouches == 1 {
							return jsonResponse(200, map[string]any{"ret": -2}), nil
						}
					}
					return jsonResponse(200, map[string]any{"ret": 0}), nil
				}
			})
			flaky.fail = func(d extproto.Delivery) error {
				if d.Ref == oldKey {
					mu.Lock()
					defer mu.Unlock()
					oldTouches++
					if oldTouches == 1 {
						return errors.New("synthetic admission recovery failure")
					}
				}
				return nil
			}
			started, release := make(chan struct{}), make(chan struct{})
			h.host.mu.Lock()
			h.host.receive = func(m extproto.Message) error {
				close(started)
				<-release
				_, _ = h.send(extproto.Send{Ref: m.ID, Text: "C", Kind: extproto.KindAnswer, ReplyTo: m.ID})
				return nil
			}
			h.host.mu.Unlock()
			polled := make(chan error, 1)
			go func() { _, err := gwgPollErr(h); polled <- err }()
			<-started
			mustNoErr(t, gwgFlush(h, "")) // global recovery cannot wait for, or touch, the active chat
			mu.Lock()
			touches := oldTouches
			mu.Unlock()
			if touches != 1 {
				t.Fatalf("old item touched %d times while its chat's query runs", touches)
			}
			h.do(func() {
				if h.state.Outbox[oldKey].Status != status {
					t.Fatal("old item changed")
				}
			})
			close(release)
			mustNoErr(t, <-polled)
			h.do(func() {
				if _, ok := h.state.Outbox[oldKey]; ok || h.state.SyncBuf != "cursor-1" {
					t.Fatalf("old item %+v left after the query finished", h.state.Outbox[oldKey])
				}
			})
			if status == OutboxPending {
				gwgEqual(t, "sent", h.sentTexts(), []string{"Q", "C", "Q"})
			} else {
				gwgEqual(t, "sent", h.sentTexts(), []string{"C"})
			}
		})
	}
}

// from: gateway/tests/gateway/reply-fairness.test.ts: ${pressure} backpressure recovery keeps the caller's active chat eligible
func TestGwgBackpressureRecoveryKeepsCallerEligible(t *testing.T) {
	// Outbox pressure only: Collie has no reply-store backpressure of its own to report.
	h, _, _ := gwgFairness(t, PolicyOptions{OutboxLimit: 1})
	oldKey := gwgPutEngine(t, h, testChat, "old", OutboxPending, "Q")
	flaky := gwgFlaky(h)
	acks := 0
	flaky.fail = func(d extproto.Delivery) error {
		if d.Ref == oldKey {
			if acks++; acks == 1 {
				return errors.New("synthetic admission recovery failure")
			}
		}
		return nil
	}
	released := false
	h.host.mu.Lock()
	h.host.receive = func(m extproto.Message) error {
		h.do(func() { _, still := h.state.Outbox[oldKey]; released = !still })
		_, _ = h.send(extproto.Send{Ref: m.ID, Text: "C", Kind: extproto.KindAnswer, ReplyTo: m.ID})
		return nil
	}
	h.host.mu.Unlock()
	gwgPoll(t, h)
	if !released || acks != 2 {
		t.Fatalf("released %v after %d acknowledgements: backpressure recovery must release the caller's own old reply", released, acks)
	}
	gwgEqual(t, "sent", h.sentTexts(), []string{"Q"})
	h.do(func() {
		m1 := h.state.ComputeDeliveryKey(testChat, "m1")
		if _, ok := h.state.Outbox[oldKey]; ok || h.state.Outbox[m1].Status != OutboxPending || !h.state.IsMessageProcessed(m1) || h.state.SyncBuf != "cursor-1" {
			t.Fatal("new message not admitted after pressure recovery")
		}
	})
}

// from: gateway/tests/gateway/reply-fairness.test.ts: fresh budget quarantines uncertain old chunks without GET, resend or re-POST
func TestGwgFreshBudgetQuarantinesUncertainChunks(t *testing.T) {
	h, setPage, _ := gwgFairness(t, PolicyOptions{})
	gwgAnswerWith(h, gwgFairAnswer)
	gwgPoll(t, h)
	oldKey := h.state.ComputeDeliveryKey(testChat, "m1")
	h.do(func() { mustNoErr(t, h.state.BeginReplyChunk(oldKey, 1)) })
	restarted := gwgReopen(t, h, PolicyOptions{SendBudget: 1}, 1)
	gwgAnswerWith(restarted, gwgFairAnswer)
	setPage(2)
	gwgPoll(t, restarted)
	gwgEqual(t, "sent", h.sentTexts(), []string{"A", "C"})
	if n := len(h.host.messages()); n != 2 {
		t.Fatalf("%d hand-overs", n)
	}
	restarted.do(func() {
		s := restarted.state
		if s.Outbox[oldKey].Status != OutboxUncertain || !s.HasUncertainReply(oldKey) || s.ReplyDeliveries[oldKey].AcknowledgedChunks != 1 || s.SyncBuf != "cursor-2" {
			t.Fatal("uncertain chunk not quarantined")
		}
	})
}

// from: gateway/tests/gateway/reply-fairness.test.ts: a concurrent unrelated recovery cannot bypass refreshed chat's deferred reply
func TestGwgConcurrentRecoveryCannotBypassRefreshedChat(t *testing.T) {
	h, setPage, timestamp := gwgFairness(t, PolicyOptions{})
	gwgAnswerWith(h, gwgFairAnswer)
	gwgPoll(t, h)
	otherKey := gwgPutEngine(t, h, "other-chat", "other", OutboxPending, "Z")
	h.do(func() {
		_, err := h.state.RecordInboundContext(otherKey, "other-chat", i64(timestamp+1), true, "other-context")
		mustNoErr(t, err)
	})
	entered, release := make(chan struct{}), make(chan struct{})
	h.setHooks(func(k *harnessHooks) {
		k.send = func(msg map[string]any) (*http.Response, error) {
			if gwgText(msg) == "Z" {
				close(entered)
				<-release
			}
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		}
	})
	recovered := make(chan error, 1)
	go func() { recovered <- gwgFlush(h, "other-chat") }()
	<-entered
	setPage(2)
	polled := make(chan error, 1)
	go func() { _, err := gwgPollErr(h); polled <- err }()
	time.Sleep(50 * time.Millisecond)
	if n := len(h.host.messages()); n != 1 {
		t.Fatalf("%d hand-overs: the new message must wait for its fair recovery turn", n)
	}
	close(release)
	mustNoErr(t, <-recovered)
	mustNoErr(t, <-polled)
	// The unrelated send was charged before the refresh, so the refreshed
	// allowance goes to the chat's deferred reply.
	gwgEqual(t, "sent", h.sentTexts(), []string{"A", "Z", "B"})
	if n := len(h.host.messages()); n != 2 {
		t.Fatalf("%d hand-overs", n)
	}
	setPage(3)
	gwgPoll(t, h)
	gwgEqual(t, "sent", h.sentTexts(), []string{"A", "Z", "B", "C"})
	h.do(func() {
		if h.state.RemainingSendBudget(testChat) != 0 || h.state.RemainingSendBudget("other-chat") != 0 {
			t.Fatal("budget left over")
		}
	})
}
