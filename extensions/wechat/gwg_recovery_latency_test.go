package wechat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/TaoSama/collie/internal/extproto"
)

// gwgLatencyHarness: one chat with a window and a private context, no trace
// noise but the gateway's own.
func gwgLatencyHarness(t *testing.T) *harness {
	h := newHarness(t, harnessOptions{noWindow: true, account: &Account{Token: "private-token", AccountID: "account", UserID: testChat}})
	h.do(func() {
		_, err := h.state.RecordInboundContext("window", testChat, i64(nowMs()), true, "private-context")
		mustNoErr(t, err)
	})
	return h
}

func gwgFlushIn(h *harness, trace *MessageTrace) error {
	var err error
	h.do(func() { err = h.gateway.flushRepliesOnce(withTrace(context.Background(), trace), "", "") })
	return err
}

// from: gateway/tests/gateway/recovery-latency.test.ts: ${source} frozen reads, sends and receipts share an old-message recovery attempt rather than the triggering inbound
func TestGwgRecoveryAttemptBelongsToOldMessage(t *testing.T) {
	// Only the engine source remains: Collie's answers are frozen here; the Controller GET variant has no counterpart.
	h := gwgLatencyHarness(t)
	first := gwgPutEngine(t, h, testChat, "old-first", OutboxPending, "private-frozen-reply")
	second := gwgPutEngine(t, h, testChat, "old-second", OutboxPending, "private-frozen-reply")
	log := captureLog(t)
	inbound := createMessageTrace(testChat, "new-inbound")
	mustNoErr(t, gwgFlushIn(h, inbound))
	h.do(func() {
		if _, ok := h.state.Outbox[first]; ok {
			t.Fatal("first reply not finished")
		}
		if h.state.Outbox[second].Status != OutboxPending {
			t.Fatal("only one frozen pending reply is resumed per cycle")
		}
	})
	if ev := h.host.events(); len(ev) != 1 || ev[0].Ref != first {
		t.Fatalf("acknowledgements = %+v", ev)
	}
	events := gwgTraceEvents(log)
	expected := createMessageTrace(testChat, "old-first").TraceID
	attempts := map[any]bool{}
	for _, e := range events {
		if e["traceId"] != expected {
			t.Fatalf("event %v belongs to another trace", e["stage"])
		}
		attempts[e["attemptId"]] = true
		if e["attemptId"] == inbound.AttemptID {
			t.Fatal("recovery inherited the triggering inbound's attempt")
		}
	}
	if len(events) == 0 || len(attempts) != 1 {
		t.Fatalf("%d events, %d attempts", len(events), len(attempts))
	}
	if r, d := gwgStageIndex(events, "delivery.recovery.read.start"), gwgStageIndex(events, "delivery.start"); r < 0 || d < 0 || r > d {
		t.Fatalf("read at %d, delivery at %d", r, d)
	}
	if e := gwgStage(events, "delivery.recovery.end"); e == nil || e["outcome"] != "ok" {
		t.Fatalf("recovery end = %v", e)
	}
	if e := gwgStage(events, "delivery.end"); e == nil || e["receiptStatus"] != "wechat_api_accepted" {
		t.Fatalf("delivery end = %v", e)
	}
	for _, private := range []string{"private-token", "private-context", "private-frozen-reply"} {
		if strings.Contains(log.String(), private) {
			t.Fatalf("trace exposes %s", private)
		}
	}
	mustNoErr(t, gwgFlushIn(h, inbound))
	h.do(func() {
		if _, ok := h.state.Outbox[second]; ok {
			t.Fatal("second reply not finished")
		}
	})
	secondTrace := createMessageTrace(testChat, "old-second").TraceID
	found := false
	for _, e := range gwgTraceEvents(log) {
		found = found || e["traceId"] == secondTrace
	}
	if !found {
		t.Fatal("second recovery has no trace of its own")
	}
}

// from: gateway/tests/gateway/recovery-latency.test.ts: terminal receipt retries each receive an independent trace and never re-read or re-send
func TestGwgTerminalReceiptRetriesOwnTrace(t *testing.T) {
	h := gwgLatencyHarness(t)
	accepted := gwgPutEngine(t, h, testChat, "old-accepted", OutboxReceipt, "")
	expired := gwgPutEngine(t, h, testChat, "old-expired", OutboxExpired, "")
	log := captureLog(t)
	mustNoErr(t, gwgFlushIn(h, createMessageTrace("new-chat", "new-message")))
	h.do(func() {
		if _, ok := h.state.Outbox[accepted]; ok || !h.state.Outbox[expired].EngineAcked {
			t.Fatal("terminal receipts not finished")
		}
	})
	ev := h.host.events()
	if len(ev) != 2 || ev[0].Ref != accepted || ev[0].State != DeliverySent || ev[1].Ref != expired || ev[1].State != DeliveryExpired {
		t.Fatalf("acknowledgements = %+v", ev)
	}
	events := gwgTraceEvents(log)
	acks, attempts := 0, map[any]bool{}
	for _, e := range events {
		attempts[e["attemptId"]] = true
		if e["stage"] == "delivery.recovery.ack.end" {
			acks++
		}
		if e["stage"] == "delivery.recovery.read.start" || e["stage"] == "delivery.start" {
			t.Fatal("terminal receipt was read or sent again")
		}
	}
	if acks != 2 || len(attempts) != 2 {
		t.Fatalf("%d acks over %d attempts", acks, len(attempts))
	}
	for _, mid := range []string{"old-accepted", "old-expired"} {
		id := createMessageTrace(testChat, mid).TraceID
		ok := false
		for _, e := range events {
			ok = ok || e["traceId"] == id && e["stage"] == "delivery.recovery.ack.end"
		}
		if !ok {
			t.Fatalf("%s acknowledged outside its own trace", mid)
		}
	}
	if len(h.sent()) != 0 {
		t.Fatal("terminal receipt was sent")
	}
}

// from: gateway/tests/gateway/recovery-latency.test.ts: ${status} recovery becomes silent after a durable engine acknowledgement, including after restart
func TestGwgTerminalRecoverySilentAfterAck(t *testing.T) {
	for _, status := range []string{OutboxUncertain, OutboxExpired} {
		t.Run(status, func(t *testing.T) {
			h := gwgLatencyHarness(t)
			key := gwgPutEngine(t, h, testChat, "terminal", status, "")
			var budget, windows, tokens string
			h.do(func() {
				budget, windows, tokens = jsonText(h.state.AccountBudget), jsonText(h.state.ContextWindows), jsonText(h.state.ContextTokens)
			})
			log := captureLog(t)
			mustNoErr(t, gwgFlush(h, ""))
			if n := len(h.host.events()); n != 1 {
				t.Fatalf("%d acknowledgements", n)
			}
			var terminal DeliveryOutboxItem
			h.do(func() { terminal = h.state.Outbox[key] })
			if !terminal.EngineAcked || terminal.Status != status {
				t.Fatalf("terminal = %+v", terminal)
			}
			acks := 0
			for _, e := range gwgTraceEvents(log) {
				if e["stage"] == "delivery.recovery.ack.end" {
					acks++
				}
			}
			if acks != 1 {
				t.Fatalf("%d ack spans", acks)
			}
			log.Reset()
			mustNoErr(t, gwgFlush(h, ""))
			mustNoErr(t, gwgFlush(h, ""))
			if n := len(gwgTraceEvents(log)); n != 0 {
				t.Fatalf("acknowledged audit entries made %d recovery events", n)
			}
			restarted := gwgReopen(t, h, PolicyOptions{}, 0)
			mustNoErr(t, gwgFlush(restarted, ""))
			mustNoErr(t, gwgFlush(restarted, ""))
			if n := len(gwgTraceEvents(log)); n != 0 || len(h.host.events()) != 1 {
				t.Fatal("restart did not keep the terminal state silent")
			}
			restarted.do(func() {
				s := restarted.state
				if jsonText(s.Outbox[key]) != jsonText(terminal) || jsonText(s.AccountBudget) != budget ||
					jsonText(s.ContextWindows) != windows || jsonText(s.ContextTokens) != tokens {
					t.Fatal("restart changed the terminal reference or the window")
				}
			})
		})
	}
}

// from: gateway/tests/gateway/recovery-latency.test.ts: ${status} retries after ${failureAt} acknowledgement failure and becomes silent only after persistence
func TestGwgTerminalAckFailureRetries(t *testing.T) {
	for _, status := range []string{OutboxUncertain, OutboxExpired} {
		for _, failureAt := range []string{"engine", "state"} {
			t.Run(status+"/"+failureAt, func(t *testing.T) {
				h := gwgLatencyHarness(t)
				key := gwgPutEngine(t, h, testChat, "terminal", status, "")
				failure := errors.New("synthetic acknowledgement failure")
				fail := true
				flaky := gwgFlaky(h)
				flaky.fail = func(extproto.Delivery) error {
					if fail && failureAt == "engine" {
						return failure
					}
					return nil
				}
				var budget string
				h.do(func() {
					budget = jsonText(h.state.AccountBudget)
					h.state.failSave = func() error {
						if fail && failureAt == "state" && h.state.Outbox[key].EngineAcked {
							return failure
						}
						return nil
					}
				})
				log := captureLog(t)
				if err := gwgFlush(h, ""); !errors.Is(err, failure) {
					t.Fatalf("flush error = %v", err)
				}
				h.do(func() {
					if h.state.Outbox[key].EngineAcked {
						t.Fatal("acknowledged without persistence")
					}
				})
				if e := gwgStage(gwgTraceEvents(log), "delivery.recovery.end"); e == nil || e["outcome"] != "error" {
					t.Fatalf("recovery end = %v", e)
				}
				fail = false
				mustNoErr(t, gwgFlush(h, ""))
				if flaky.count() != 2 {
					t.Fatalf("%d acknowledgement calls", flaky.count())
				}
				h.do(func() {
					if !h.state.Outbox[key].EngineAcked || h.state.Outbox[key].Status != status {
						t.Fatal("not acknowledged")
					}
				})
				log.Reset()
				mustNoErr(t, gwgFlush(h, ""))
				if len(gwgTraceEvents(log)) != 0 || flaky.count() != 2 {
					t.Fatal("acknowledged entry was retried")
				}
				h.do(func() {
					if jsonText(h.state.AccountBudget) != budget {
						t.Fatal("budget changed")
					}
				})
			})
		}
	}
}

// from: gateway/tests/gateway/recovery-latency.test.ts: acknowledged terminal audit entries do not block accepted cleanup or pending delivery
func TestGwgAcknowledgedAuditEntriesDoNotBlock(t *testing.T) {
	h := gwgLatencyHarness(t)
	uncertain := gwgPutEngine(t, h, testChat, "uncertain", OutboxUncertain, "")
	expired := gwgPutEngine(t, h, testChat, "expired", OutboxExpired, "")
	accepted := gwgPutEngine(t, h, testChat, "accepted", OutboxReceipt, "")
	pending := gwgPutEngine(t, h, testChat, "pending", OutboxPending, "frozen reply")
	var budget int64
	h.do(func() {
		for _, key := range []string{uncertain, expired, accepted} {
			mustNoErr(t, h.state.UpdateOutbox(key, func(i *DeliveryOutboxItem) { i.EngineAcked = true }))
		}
		budget = h.state.RemainingSendBudget(testChat)
	})
	log := captureLog(t)
	mustNoErr(t, gwgFlush(h, ""))
	h.do(func() {
		s := h.state
		_, a := s.Outbox[accepted]
		_, p := s.Outbox[pending]
		if a || p || s.Outbox[uncertain].Status != OutboxUncertain || s.Outbox[expired].Status != OutboxExpired || s.RemainingSendBudget(testChat) != budget-1 {
			t.Fatal("audit entries blocked cleanup or delivery")
		}
	})
	if ev := h.host.events(); len(ev) != 1 || ev[0].Ref != pending {
		t.Fatalf("acknowledgements = %+v", ev)
	}
	if n := len(h.sent()); n != 1 {
		t.Fatalf("%d sends", n)
	}
	terminal := map[any]bool{createMessageTrace(testChat, "uncertain").TraceID: true, createMessageTrace(testChat, "expired").TraceID: true}
	for _, e := range gwgTraceEvents(log) {
		if terminal[e["traceId"]] {
			t.Fatal("acknowledged terminal entry made a recovery event")
		}
	}
}
