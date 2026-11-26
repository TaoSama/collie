package wechat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// from: gateway/tests/gateway/notification-delivery.test.ts: over 128 contexts use per-chat lookups and rotate bounded cycles without starvation
func TestGwfManyChatsNoStarvation(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.do(func() {
		for i := 0; i < 130; i++ {
			chat := fmt.Sprintf("chat-%03d", i)
			h.state.SetContextToken(chat, "context")
			_, err := h.state.RecordInboundContext("fixture:inbound:"+chat, chat, i64(nowMs()-1000), true, "")
			mustNoErr(t, err)
		}
	})
	gwfPlant(t, h, "result-129", "chat-129", "结果")
	// Every chat is asked on its own each pass, so the last of 130 is reached at once.
	if s := gwfSummary(t, h); s.Accepted != 1 || h.sent()[0]["to_user_id"] != "chat-129" {
		t.Fatalf("summary %+v", s)
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: round robin gives each reachable chat one notification before returning to a busy chat
func TestGwfRoundRobinAcrossChats(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.do(func() {
		for _, chat := range []string{"chat-a", "chat-b"} {
			h.state.SetContextToken(chat, "context-"+chat)
			_, err := h.state.RecordInboundContext("fixture:inbound:"+chat, chat, i64(nowMs()-1000), true, "")
			mustNoErr(t, err)
		}
	})
	gwfPlant(t, h, "a-1", "chat-a", "A1")
	time.Sleep(2 * time.Millisecond)
	gwfPlant(t, h, "a-2", "chat-a", "A2")
	gwfPlant(t, h, "b-1", "chat-b", "B1")
	// One result per chat per pass; a busy chat's next waits out the per-chat interval.
	if s := gwfSummary(t, h); s.Accepted != 2 {
		t.Fatalf("first pass %+v", s)
	}
	if s := gwfSummary(t, h); s.Accepted != 0 {
		t.Fatalf("busy chat sent again inside its interval: %+v", s)
	}
	gwfAge(t, h, "chat-a")
	gwfAge(t, h, "chat-b")
	if s := gwfSummary(t, h); s.Accepted != 1 {
		t.Fatalf("third pass %+v", s)
	}
	var to []string
	for _, s := range h.sent() {
		to = append(to, s["to_user_id"].(string))
	}
	if fmt.Sprint(to) != "[chat-a chat-b chat-a]" && fmt.Sprint(to) != "[chat-b chat-a chat-a]" {
		t.Fatalf("order %v", to)
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: notification delivery is single-flight and limited to one outbox item per cycle
func TestGwfSingleFlightOnePerChat(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	for i := 1; i <= 5; i++ {
		gwfPlant(t, h, fmt.Sprintf("result-%d", i), testChat, fmt.Sprintf("结果 %d", i))
	}
	entered, release := make(chan struct{}), make(chan struct{})
	h.setHooks(func(k *harnessHooks) {
		k.send = func(map[string]any) (*http.Response, error) {
			close(entered)
			<-release
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		}
	})
	done := make(chan ResultDispatchSummary)
	go func() { s, _ := h.deliverNotificationsOnce(); done <- s }()
	<-entered
	if s := gwfSummary(t, h); s != (ResultDispatchSummary{}) {
		t.Fatalf("concurrent cycle did work: %+v", s)
	}
	close(release)
	if s := <-done; s.Accepted != 1 || len(h.sent()) != 1 {
		t.Fatalf("summary %+v, %d sends", s, len(h.sent()))
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: lost claim response resumes the existing unattempted send after restart without reclaiming
func TestGwfClaimedResultResumesAfterRestart(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	id := gwfPlant(t, h, "result-1", testChat, "结果")
	attempt := gwfClaimed(t, h, id)
	r := gwfRestart(t, h)
	if s := gwfSummary(t, r); s != (ResultDispatchSummary{Accepted: 1}) {
		t.Fatalf("summary = %+v", s)
	}
	p := gwfPlan(r, id)
	if len(h.sent()) != 1 || p.Status != ResultAccepted || p.AttemptID != attempt {
		t.Fatalf("%d sends, %s, attempt kept %v", len(h.sent()), p.Status, p.AttemptID == attempt)
	}
	gwfSummary(t, r)
	if len(h.sent()) != 1 {
		t.Fatal("resent")
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: post-claim ${mode} prevents sending and retains the appropriate evidence
func TestGwfPostClaimChangePreventsSending(t *testing.T) {
	for _, mode := range []string{"stop", "expiry", "attempt-expiry", "context", "account", "baseline", "budget"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			id := gwfPlant(t, h, "result-1", testChat, "结果")
			gwfClaimed(t, h, id)
			h.do(func() {
				s := h.state
				switch mode {
				case "stop":
					h.gateway.stop()
				case "expiry":
					gwfExpire(h, id)
				case "attempt-expiry":
					p := h.gateway.results.plans[id]
					p.AttemptedAt, p.AttemptExpiresAt = nowMs()-2, nowMs()-1
				case "context":
					delete(s.ContextTokens, testChat)
				case "account":
					s.Account.AccountID = "changed"
				case "baseline":
					s.BaselineEstablished = false
				case "budget":
					s.AccountBudget.Remaining = 0
				}
			})
			_, err := h.deliverNotificationsOnce()
			mustNoErr(t, err)
			states := fmt.Sprint(gwfEventStates(h))
			// Local cancellation or expiry before the first send never fabricates an unknown send.
			if strings.Contains(states, DeliveryUncertain) {
				t.Fatalf("events %s", states)
			}
			p := gwfPlan(h, id)
			switch mode {
			case "context", "budget":
				reason := DeferContextChanged
				if mode == "budget" {
					reason = DeferBudgetExhausted
				}
				if len(h.sent()) != 0 || p.Status != ResultWaitingForUser || p.LastDeferReason != reason || p.AttemptID != "" {
					t.Fatalf("%d sends, plan %+v", len(h.sent()), p)
				}
			case "attempt-expiry":
				// The attempt ran out before anything reached WeChat: it is claimed
				// again and delivered once rather than lost or marked uncertain.
				if len(h.sent()) != 1 || p.Status != ResultAccepted {
					t.Fatalf("%d sends, status %s", len(h.sent()), p.Status)
				}
			case "expiry":
				if len(h.sent()) != 0 || !strings.Contains(states, DeliveryExpired) {
					t.Fatalf("%d sends, events %s", len(h.sent()), states)
				}
				r := gwfRestart(t, h)
				gwfSummary(t, r)
				if len(h.sent()) != 0 {
					t.Fatal("expired result sent after restart")
				}
			default:
				// stop and baseline idle the queue; another account empties it.
				if len(h.sent()) != 0 {
					t.Fatal("sent")
				}
			}
		})
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: post-claim exhausted budget defers durably and resumes only after new inbound context
func TestGwfExhaustedBudgetWaitsForInbound(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	id := gwfPlant(t, h, "result-1", testChat, "结果")
	h.do(func() { h.state.AccountBudget.Remaining = 0 })
	gwfSummary(t, h)
	if len(h.sent()) != 0 || gwfPlan(h, id).Status != ResultWaitingForUser {
		t.Fatalf("status %s", gwfPlan(h, id).Status)
	}
	r := gwfRestart(t, h)
	gwfSummary(t, r)
	if len(h.sent()) != 0 {
		t.Fatal("sent without a new window")
	}
	r.do(func() {
		m := InboundMessage{ChatID: testChat, DeliveryKey: "fixture:new-inbound", ContextToken: testContext}
		_, err := r.state.RecordInboundContext(m.DeliveryKey, testChat, i64(nowMs()), true, testContext)
		mustNoErr(t, err)
		mustNoErr(t, r.gateway.noteAccountInbound(m))
	})
	if s := gwfSummary(t, r); s.Accepted != 1 || len(h.sent()) != 1 || gwfPlan(r, id).Status != ResultAccepted {
		t.Fatalf("summary %+v, %d sends", s, len(h.sent()))
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: post-claim ${field} mismatch fails closed
func TestGwfPostClaimMismatchFailsClosed(t *testing.T) {
	for _, field := range []string{"chatId", "text", "kind", "createdAt", "expiresAt", "attemptId"} {
		t.Run(field, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			id := gwfPlant(t, h, "result-1", testChat, "结果")
			gwfClaimed(t, h, id)
			h.do(func() {
				p := &h.gateway.results.plans[id].ResultDelivery
				switch field {
				case "chatId":
					p.ChatID = "changed"
				case "text":
					p.Messages = []string{"changed"}
				case "kind":
					p.Kind = "failed"
				case "createdAt":
					p.CreatedAt--
				case "expiresAt":
					p.ExpiresAt++
				case "attemptId":
					p.AttemptID = id
				}
			})
			if _, err := h.deliverNotificationsOnce(); err == nil || !strings.Contains(err.Error(), "mismatch") {
				t.Fatalf("error = %v", err)
			}
			if len(h.sent()) != 0 {
				t.Fatal("sent")
			}
		})
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: an item expired or context lost after listing is never claimed
func TestGwfExpiredOrUnreachableNeverClaimed(t *testing.T) {
	for _, mode := range []string{"expired", "context", "stop"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			id := gwfPlant(t, h, "result-1", testChat, "结果")
			h.do(func() {
				switch mode {
				case "expired":
					gwfExpire(h, id)
				case "context":
					delete(h.state.ContextTokens, testChat)
				case "stop":
					h.gateway.stop()
				}
			})
			_, err := h.deliverNotificationsOnce()
			mustNoErr(t, err)
			if p := gwfPlan(h, id); len(h.sent()) != 0 || p.AttemptID != "" || p.Status == ResultClaimed {
				t.Fatalf("%d sends, plan %+v", len(h.sent()), p)
			}
		})
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: notification rejects invalid or unrecognized receipts: ${JSON.stringify(response)}
func TestGwfInvalidReceiptsUncertain(t *testing.T) {
	for _, response := range []string{`{"base_resp":{"ret":0}}`, `{"ret":null}`, `{"ret":"0"}`, `{"ret":0,"errcode":null}`,
		`{"ret":0,"errcode":1}`, `{"errcode":0,"ret":false}`, `[]`} {
		t.Run(response, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			h.setHooks(gwfSendRaw(response))
			id := gwfPlant(t, h, "result-1", testChat, "结果")
			s := gwfSummary(t, h)
			gwfSummary(t, h)
			if len(h.sent()) != 1 {
				t.Fatalf("%d sends", len(h.sent()))
			}
			// An explicit refusal defers in the merged queue; every other bad receipt is uncertain.
			if response == `{"ret":0,"errcode":1}` {
				if s.Skipped != 1 || gwfPlan(h, id).Status != ResultWaitingForUser {
					t.Fatalf("summary %+v, status %s", s, gwfPlan(h, id).Status)
				}
				return
			}
			if s != (ResultDispatchSummary{Uncertain: 1}) || gwfPlan(h, id).Status != ResultUncertain {
				t.Fatalf("summary %+v, status %s", s, gwfPlan(h, id).Status)
			}
		})
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: notification accepts supported successful response: ${JSON.stringify(response)}
func TestGwfSuccessfulReceiptsAccepted(t *testing.T) {
	for _, response := range []string{`{}`, `{"message_id":"123"}`, `{"ret":0}`, `{"errcode":0}`, `{"ret":0,"errcode":0}`} {
		h := newHarness(t, harnessOptions{})
		h.setHooks(gwfSendRaw(response))
		gwfPlant(t, h, "result-1", testChat, "结果")
		if s := gwfSummary(t, h); s.Accepted != 1 {
			t.Fatalf("%s: %+v", response, s)
		}
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: ordinary replies share the empty-object success contract
func TestGwfOrdinaryReplyEmptyObjectSuccess(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.setHooks(gwfSendRaw(`{}`))
	mustNoErr(t, h.ilink.SendSingleText(context.Background(), testChat, "ordinary", ""))
	if len(h.sent()) != 1 {
		t.Fatal("no send")
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: network send errors record uncertainty and receipt retry never resends
func TestGwfNetworkErrorUncertainNoResend(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.setHooks(gwfSendError(errors.New("network response lost")))
	gwfPlant(t, h, "result-1", testChat, "结果")
	// Receipts are local now: no receipt retries, the uncertain outcome is reported once.
	if s := gwfSummary(t, h); s.Uncertain != 1 {
		t.Fatalf("summary %+v", s)
	}
	gwfSummary(t, h)
	if len(h.sent()) != 1 || fmt.Sprint(gwfEventStates(h)) != "[uncertain:send_unknown]" {
		t.Fatalf("%d sends, events %v", len(h.sent()), gwfEventStates(h))
	}
}
