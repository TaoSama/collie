package wechat

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

// gwePollOne makes every poll answer one message of chat (client id) with
// a fresh context token.
func gwePollOne(h *harness, chat, clientID string, createdAt int64) {
	h.setHooks(func(k *harnessHooks) {
		k.poll = func() (*http.Response, error) {
			m := inbound(chat, clientID, "hello", createdAt)
			m["context_token"] = "new-context"
			return jsonResponse(200, map[string]any{"ret": 0, "msgs": []any{m}, "get_updates_buf": "next"}), nil
		}
	})
}

func gweResumes(t *testing.T, h *harness) (out []ResumeIntent) {
	t.Helper()
	h.do(func() {
		var err error
		out, err = h.gateway.resultOutbox.PendingResumes()
		mustNoErr(t, err)
	})
	return out
}

// from: gateway/tests/results/gateway-wiring.test.ts: a failed resume checkpoint retries the same admitted generation without renewing its quota
func TestGweFailedResumeCheckpointRetriesSameGeneration(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	gwePollOne(h, testChat, "message", nowMs())
	// The result outbox file cannot be replaced while a directory stands in its place.
	blocker := filepath.Join(h.dir, resultOutboxFileName)
	mustNoErr(t, os.MkdirAll(filepath.Join(blocker, "x"), 0o700))
	var err error
	h.do(func() { _, err = h.gateway.pollOnce(context.Background(), true) })
	if err == nil {
		t.Fatal("pollOnce succeeded despite the failed resume checkpoint")
	}
	if len(h.host.messages()) != 0 {
		t.Fatal("message handed over before its resume intent was saved")
	}
	h.do(func() { mustNoErr(t, h.state.ConsumeSendBudget(testChat, SendFinal)) })
	mustNoErr(t, os.RemoveAll(blocker))
	h.do(func() { _, err = h.gateway.pollOnce(context.Background(), true) })
	mustNoErr(t, err)
	msgs := h.host.messages()
	if len(msgs) != 1 {
		t.Fatalf("%d messages handed over", len(msgs))
	}
	// The answer arrives from Collie afterwards and spends one more send.
	_, err = h.send(extproto.Send{Ref: "reply", Text: "reply", Kind: extproto.KindAnswer, ReplyTo: msgs[0].ID})
	mustNoErr(t, err)
	if r := gweRemaining(h, h.state); r != 8 {
		t.Fatalf("remaining %d: the retry must not reset 9 back to 10 before its reply", r)
	}
	if r := gweResumes(t, h); len(r) != 1 || r[0].ChatID != testChat || r[0].Generation != msgs[0].ID {
		t.Fatalf("resumes %+v", r)
	}
	if h.state.SyncBuf != "next" {
		t.Fatalf("cursor %q", h.state.SyncBuf)
	}
}

// from: gateway/tests/results/gateway-wiring.test.ts: an inbox checkpoint failure does not release result resumes before admission
func TestGweInboxFailureDoesNotReleaseResumes(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	gwePollOne(h, testChat, "inbox-admission", nowMs())
	var before ChatContextWindow
	h.do(func() { before = h.state.ContextWindows[testChat] })
	blocker := filepath.Join(h.dir, inboxFileName)
	mustNoErr(t, os.MkdirAll(blocker, 0o700))
	var err error
	h.do(func() { _, err = h.gateway.pollOnce(context.Background(), false) })
	if err == nil {
		t.Fatal("admission succeeded despite the failed inbox checkpoint")
	}
	h.do(func() {
		if h.state.ContextWindows[testChat] != before || h.state.ContextTokens[testChat] != testContext {
			t.Fatal("failed inbox admission refreshed the window or replaced the token")
		}
	})
	if r := gweResumes(t, h); len(r) != 0 {
		t.Fatalf("resumes %+v before admission", r)
	}
	mustNoErr(t, os.RemoveAll(blocker))
	h.do(func() { _, err = h.gateway.pollOnce(context.Background(), false) })
	mustNoErr(t, err)
	var key string
	h.do(func() { key = h.state.ComputeDeliveryKey(testChat, "inbox-admission") })
	if r := gweResumes(t, h); len(r) != 1 || r[0].Generation != key {
		t.Fatalf("resumes %+v", r)
	}
	if h.state.SyncBuf != "next" {
		t.Fatalf("cursor %q", h.state.SyncBuf)
	}
}

// from: gateway/tests/results/gateway-wiring.test.ts: result failure still attempts one ${renewal} renewal for another chat and preserves the first error
func TestGweResultFailureStillAttemptsRenewal(t *testing.T) {
	for _, renewal := range []string{"accepted", "failed"} {
		t.Run(renewal, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			if renewal == "failed" {
				h.setHooks(func(k *harnessHooks) {
					k.send = func(map[string]any) (*http.Response, error) {
						return nil, errors.New("renewal transport response lost")
					}
				})
			}
			h.do(func() {
				s := h.state
				s.SetContextToken("broken", "broken-context")
				_, err := s.RecordInboundContext("broken-inbound", "broken", i64(nowMs()-1000), true, "")
				mustNoErr(t, err)
				s.SetContextToken("renewal", "renewal-context")
				_, err = s.RecordInboundContext("renewal-inbound", "renewal", i64(nowMs()-22*gweHour-1000), true, "")
				mustNoErr(t, err)
				s.AccountBudget.RefreshedAt = s.ContextWindows["renewal"].RefreshedAt
				s.AccountBudget.Remaining = 2
				mustNoErr(t, s.SaveState())
			})
			// The result service failing: a reference that no longer matches its plan.
			res, err := h.send(extproto.Send{Ref: "broken-result", Chat: "broken", Text: "结果", Kind: extproto.KindResult})
			if err != nil || !res.Deferred {
				t.Fatalf("result %+v %v", res, err)
			}
			h.do(func() {
				for id, p := range h.gateway.results.plans {
					mustNoErr(t, h.gateway.resultOutbox.Put(ResultReference{ID: id, ChatID: p.ChatID, Identity: strings.Repeat("f", 64), Status: RefPending}))
				}
			})
			h.gateway.SetInterest([]string{"renewal"})
			for pass := 0; pass < 2; pass++ {
				_, err := h.deliverNotificationsOnce()
				if !errors.Is(err, errRecoveryIdentity) {
					t.Fatalf("pass %d error = %v, want the result failure first", pass, err)
				}
				sent := h.sent()
				if len(sent) != 1 || sent[0]["to_user_id"] != "renewal" {
					t.Fatalf("pass %d sends %v", pass, sent)
				}
			}
		})
	}
}

// from: gateway/tests/results/gateway-wiring.test.ts: a ready result yields shared quota to another recipient's active inbound reply
func TestGweReadyResultYieldsToActiveInbound(t *testing.T) {
	// The bound person is "peer" here so that the result for "chat" is another recipient.
	h := newHarness(t, harnessOptions{account: &Account{Token: testToken, AccountID: testAccount, UserID: "peer"}})
	h.pushResult("ready", "ready task result")
	began, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.host.receive = func(extproto.Message) error {
		once.Do(func() { close(began) })
		<-release
		return nil
	}
	gwePollOne(h, "peer", "peer-message", nowMs())
	var poll PollCycleSummary
	var err error
	h.do(func() { poll, err = h.gateway.pollOnce(context.Background(), false) })
	mustNoErr(t, err)
	if poll.ProcessedCount != 1 {
		t.Fatalf("poll %+v", poll)
	}
	<-began
	summary, err := h.deliverNotificationsOnce()
	mustNoErr(t, err)
	if summary != (ResultDispatchSummary{}) || len(h.sent()) != 0 {
		t.Fatalf("the ready task consumed an active reply's quota: %+v, %d sends", summary, len(h.sent()))
	}
	close(release)
	eventually(t, "hand-over", func() bool { return len(h.host.messages()) == 1 })
	// Collie's answer to the peer goes first.
	_, err = h.send(extproto.Send{Ref: "peer-reply", Chat: "peer", Text: "ordinary peer reply", Kind: extproto.KindAnswer,
		ReplyTo: h.host.messages()[0].ID})
	mustNoErr(t, err)
	eventually(t, "runner idle", func() bool {
		idle := false
		h.do(func() { idle = !h.gateway.isReplyPending("peer") })
		return idle
	})
	summary, err = h.deliverNotificationsOnce()
	mustNoErr(t, err)
	var to []any
	for _, m := range h.sent() {
		to = append(to, m["to_user_id"])
	}
	if summary.Accepted != 1 || len(to) != 2 || to[0] != "peer" || to[1] != testChat || gweRemaining(h, h.state) != 8 {
		t.Fatalf("summary %+v, recipients %v", summary, to)
	}
}

// from: gateway/tests/results/gateway-wiring.test.ts: an expired recipient's pending reply does not block another recipient's ready result
func TestGweExpiredRecipientDoesNotBlockResult(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.pushResult("ready", "ready task result")
	h.do(func() {
		_, err := h.state.RecordInboundContext("expired-peer-inbound", "expired-peer", i64(nowMs()-24*gweHour), true, "expired-token")
		mustNoErr(t, err)
	})
	gweFreeze(t, h.gateway, "expired-peer-reply", "expired-peer", "old", 2000)
	h.do(func() {
		if h.state.CanSendToChat("expired-peer", SendFinal) {
			t.Fatal("expired peer can still send")
		}
	})
	summary, err := h.deliverNotificationsOnce()
	mustNoErr(t, err)
	sent := h.sent()
	if summary.Accepted != 1 || len(sent) != 1 || sent[0]["to_user_id"] != testChat {
		t.Fatalf("summary %+v sends %v", summary, sent)
	}
	h.do(func() {
		if h.state.Outbox["expired-peer-reply"].Status != OutboxPending {
			t.Fatal("expired peer's reply changed")
		}
	})
}

// from: gateway/tests/results/notification-context-queue.test.ts: queued result notification ${newInbound ? "defers after a real inbound context change" : "sends when its context is unchanged"}
func TestGweQueuedResultContextCheck(t *testing.T) {
	const ordinaryText, noticeText = "ordinary message ahead of notification", "queued synthetic result notification"
	for _, newInbound := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "new-inbound"}[newInbound], func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			first, releaseFirst := make(chan struct{}), make(chan struct{})
			var calls int
			var mu sync.Mutex
			h.setHooks(func(k *harnessHooks) {
				k.send = func(map[string]any) (*http.Response, error) {
					mu.Lock()
					calls++
					n := calls
					mu.Unlock()
					if n == 1 {
						close(first)
						<-releaseFirst
					}
					return jsonResponse(200, map[string]any{"ret": 0, "errcode": 0}), nil
				}
				k.poll = func() (*http.Response, error) {
					m := inbound(testChat, "fresh-inbound", "fresh user input", nowMs())
					m["context_token"] = "fresh-poll-context"
					return jsonResponse(200, map[string]any{"ret": 0, "msgs": []any{m}, "get_updates_buf": "fresh-cursor"}), nil
				}
			})
			h.pushResult("notice", noticeText)
			var id string
			h.do(func() {
				for pid := range h.gateway.results.plans {
					id = pid
				}
			})
			ordinary := make(chan error, 1)
			go func() { ordinary <- h.ilink.SendSingleText(context.Background(), testChat, ordinaryText, "") }()
			<-first
			pending := make(chan ResultDispatchSummary, 1)
			go func() {
				s, err := h.deliverNotificationsOnce()
				if err != nil {
					t.Error(err)
				}
				pending <- s
			}()
			stepKey := "result:" + id + ":step:0"
			// The result is queued behind the ordinary send once its plan exists.
			eventually(t, "queued result", func() bool {
				ok := false
				h.do(func() { _, ok = h.state.ReplyDeliveries[stepKey] })
				return ok
			})
			if newInbound {
				var poll PollCycleSummary
				var err error
				h.do(func() { poll, err = h.gateway.pollOnce(context.Background(), true) })
				mustNoErr(t, err)
				if poll.ProcessedCount != 1 || h.state.ContextTokens[testChat] != "fresh-poll-context" {
					t.Fatalf("poll %+v", poll)
				}
			}
			close(releaseFirst)
			mustNoErr(t, <-ordinary)
			var summary ResultDispatchSummary
			select {
			case summary = <-pending:
			case <-time.After(10 * time.Second):
				t.Fatal("notification pass did not finish")
			}
			want := ResultDispatchSummary{Accepted: 1}
			if newInbound {
				want = ResultDispatchSummary{Skipped: 1}
			}
			if summary != want {
				t.Fatalf("summary %+v, want %+v", summary, want)
			}
			sent := h.sent()
			if n := len(sent); n != map[bool]int{false: 2, true: 1}[newInbound] {
				t.Fatalf("%d sends", n)
			}
			h.do(func() {
				p := h.gateway.results.plans[id]
				refs, err := h.gateway.resultOutbox.References()
				mustNoErr(t, err)
				d := h.state.ReplyDeliveries[stepKey]
				if len(refs) != 0 || d.InFlight {
					t.Fatalf("refs %+v plan %+v", refs, d)
				}
				if newInbound {
					if p.Status != ResultWaitingForUser || p.LastDeferReason != DeferContextChanged || p.SubmittedMessages != 0 ||
						d.AcknowledgedChunks != 0 || h.state.AccountBudget.Remaining != h.state.Policy.SendBudget {
						t.Fatalf("deferred plan %+v, step %+v", p.ResultDelivery, d)
					}
				} else {
					if p.Status != ResultAccepted || d.AcknowledgedChunks != 1 || sent[1]["context_token"] != testContext {
						t.Fatalf("plan %+v", p.ResultDelivery)
					}
					item := sent[1]["item_list"].([]any)[0].(map[string]any)["text_item"].(map[string]any)["text"]
					if item != noticeText {
						t.Fatalf("result text %v", item)
					}
				}
			})
		})
	}
}
