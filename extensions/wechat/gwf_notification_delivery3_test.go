package wechat

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

// from: gateway/tests/gateway/notification-delivery.test.ts: notification failure diagnostic is sanitized and follows permanent receipt: ${errorType}
func TestGwfFailureDiagnosticSanitized(t *testing.T) {
	const private = "private token/context/text/chatId"
	for _, c := range []struct {
		err       error
		errorType string
	}{
		{errors.New("private network " + private), "NetworkError"},
		{errors.New("private timeout " + private), "TimeoutError"},
		{errors.New("WeChat sendmessage failed: " + private), "ILinkSendMessageError"},
	} {
		t.Run(c.errorType, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			h.setHooks(gwfSendError(c.err))
			text := "计划结果正文"
			id := gwfPlant(t, h, "result-1", testChat, text)
			log := captureLog(t)
			if s := gwfSummary(t, h); s != (ResultDispatchSummary{Uncertain: 1}) {
				t.Fatalf("summary %+v", s)
			}
			failed := logEvents(log, "notification_send_failed")
			if len(failed) != 1 || failed[0]["notificationId"] != id || failed[0]["errorType"] != c.errorType ||
				failed[0]["attemptId"] != gwfPlan(h, id).AttemptID || len(failed[0]) != 4 {
				t.Fatalf("diagnostics %v", failed)
			}
			for _, secret := range []string{private, text, testToken + `"`, testContext, `"chat"`} {
				if strings.Contains(log.String(), secret) {
					t.Fatalf("diagnostics expose %q", secret)
				}
			}
			gwfSummary(t, h)
			if len(h.sent()) != 1 || len(logEvents(log, "notification_send_failed")) != 1 {
				t.Fatal("resent or reported twice")
			}
		})
	}
}

// gwfBrokenSink is a log sink that always fails.
type gwfBrokenSink struct{}

func (gwfBrokenSink) Write([]byte) (int, error) {
	return 0, errors.New("private diagnostic sink failure")
}

// from: gateway/tests/gateway/notification-delivery.test.ts: notification diagnostic write failure keeps uncertainty permanent and never resends
// from: gateway/tests/gateway/notification-delivery.test.ts: a short diagnostic write stops the run loop without its stderr error callback or another send
func TestGwfDiagnosticSinkFailureNeverResends(t *testing.T) {
	traceSinkMu.Lock()
	previous := traceSink
	traceSink = gwfBrokenSink{}
	traceSinkMu.Unlock()
	t.Cleanup(func() { traceSinkMu.Lock(); traceSink = previous; traceSinkMu.Unlock() })
	h := newHarness(t, harnessOptions{})
	h.setHooks(gwfSendError(errors.New("private send failure")))
	id := gwfPlant(t, h, "result-1", testChat, "结果")
	// Diagnostics are best-effort stderr lines now: a broken sink neither fails
	// the cycle nor stops the loop, and the uncertain receipt stays final.
	done := make(chan error, 1)
	go func() {
		done <- h.gateway.RunLoop(1, func(err error) { t.Errorf("poll error %v", err) }, func(err error) { t.Errorf("cycle error %v", err) })
	}()
	eventually(t, "uncertain receipt", func() bool { return gwfPlan(h, id).Status == ResultUncertain })
	time.Sleep(50 * time.Millisecond)
	h.gateway.Stop()
	mustNoErr(t, <-done)
	if len(h.sent()) != 1 {
		t.Fatalf("%d sends", len(h.sent()))
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: failed notification receipt persistence does not emit a persisted-failure diagnostic or resend
func TestGwfFailedReceiptPersistenceNoDiagnostic(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	id := gwfPlant(t, h, "result-1", testChat, "结果")
	log := captureLog(t)
	var stop func()
	h.setHooks(func(k *harnessHooks) {
		k.send = func(map[string]any) (*http.Response, error) {
			stop = gwfFailCheckpoint(t, resultOutboxFileName)
			return nil, errors.New("private send failure")
		}
	})
	for i := 0; i < 2; i++ {
		if _, err := h.deliverNotificationsOnce(); err == nil || !strings.Contains(err.Error(), "receipt unavailable") {
			t.Fatalf("cycle %d error = %v", i, err)
		}
	}
	if gwfPlan(h, id).Status != ResultClaimed || len(logEvents(log, "notification_send_failed")) != 0 {
		t.Fatalf("status %s", gwfPlan(h, id).Status)
	}
	stop()
	if s := gwfSummary(t, h); s.Uncertain != 1 || gwfPlan(h, id).Status != ResultUncertain {
		t.Fatalf("summary %+v", s)
	}
	if len(logEvents(log, "notification_send_failed")) != 0 || len(h.sent()) != 1 {
		t.Fatal("diagnostic after recovery or resent")
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: run loop reports a sanitized send failure even without consuming the delivery summary
func TestGwfRunLoopReportsSanitizedFailure(t *testing.T) {
	const private = "private token/context/text/chatId"
	h := newHarness(t, harnessOptions{})
	h.setHooks(gwfSendError(errors.New(private)))
	id := gwfPlant(t, h, "result-1", testChat, "结果")
	log := captureLog(t)
	var cycleErrors []error
	var mu sync.Mutex
	record := func(err error) { mu.Lock(); cycleErrors = append(cycleErrors, err); mu.Unlock() }
	done := make(chan error, 1)
	go func() { done <- h.gateway.RunLoop(1, record, record) }()
	eventually(t, "uncertain receipt", func() bool { return gwfPlan(h, id).Status == ResultUncertain })
	h.gateway.Stop()
	mustNoErr(t, <-done)
	if len(cycleErrors) != 0 {
		t.Fatalf("cycle errors %v", cycleErrors)
	}
	failed := logEvents(log, "notification_send_failed")
	if len(failed) != 1 || failed[0]["errorType"] != "NetworkError" || strings.Contains(log.String(), private) {
		t.Fatalf("diagnostics %v", failed)
	}
	for _, e := range logEvents(log, "message_latency") {
		if e["schema"] != float64(1) || e["service"] != "gateway" {
			t.Fatalf("telemetry %v", e)
		}
	}
	if len(logEvents(log, "message_latency")) == 0 || len(h.sent()) != 1 {
		t.Fatal("no telemetry or resent")
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: accepted send with exhausted receipt retries leaves claim queryable, never sends again
func TestGwfAcceptedSendReceiptRecovery(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	id := gwfPlant(t, h, "result-1", testChat, "结果")
	var stop func()
	h.setHooks(func(k *harnessHooks) {
		k.send = func(map[string]any) (*http.Response, error) {
			stop = gwfFailCheckpoint(t, resultOutboxFileName)
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		}
	})
	for i := 0; i < 2; i++ {
		if _, err := h.deliverNotificationsOnce(); err == nil || !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("cycle %d error = %v", i, err)
		}
	}
	if len(h.sent()) != 1 || gwfPlan(h, id).Status != ResultClaimed {
		t.Fatalf("%d sends, %s", len(h.sent()), gwfPlan(h, id).Status)
	}
	stop()
	if s := gwfSummary(t, h); s.Accepted != 1 || gwfPlan(h, id).Status != ResultAccepted || len(h.sent()) != 1 {
		t.Fatalf("summary %+v", s)
	}
}

// gwfBreakReplyRecovery leaves a pending answer whose frozen plan no longer
// matches, so reply recovery fails. Its chat "peer" has a window but no
// token, so it does not count as a reply holding results back.
func gwfBreakReplyRecovery(t *testing.T, h *harness) {
	t.Helper()
	h.do(func() {
		g := h.gateway
		_, err := h.state.RecordInboundContext("fixture:inbound:peer", "peer", i64(nowMs()-1000), true, "")
		mustNoErr(t, err)
		_, err = g.answers.put(FrozenReply{Ref: "stale", ChatID: "peer", Kind: extproto.KindAnswer, Text: "旧回复", Identity: "stale"}, nil, nil)
		mustNoErr(t, err)
		mustNoErr(t, h.state.PutOutbox(DeliveryOutboxItem{Kind: "reply", Source: SourceEngine, ChatID: "peer", MessageID: "stale",
			DeliveryKey: "stale", SplitMaxChars: DefaultMessageMaxChars, Status: OutboxPending}))
		h.state.ReplyDeliveries["stale"] = ReplyDeliveryState{Digest: strings.Repeat("0", 64), TotalChunks: 1}
	})
}

// from: gateway/tests/gateway/notification-delivery.test.ts: ordinary reply recovery failure still delivers pending notifications and preserves the original error
func TestGwfReplyRecoveryFailureStillDeliversResults(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	gwfBreakReplyRecovery(t, h)
	id := gwfPlant(t, h, "result-1", testChat, "结果")
	_, err := h.deliverNotificationsOnce()
	if err == nil || !strings.Contains(err.Error(), "content or chunk plan changed") {
		t.Fatalf("error = %v", err)
	}
	if gwfPlan(h, id).Status != ResultAccepted || len(h.sent()) != 1 {
		t.Fatalf("status %s, %d sends", gwfPlan(h, id).Status, len(h.sent()))
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: run loop starts notification work alongside inbound polling and isolates notification errors
func TestGwfRunLoopIsolatesNotificationErrors(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	// A result whose stored identity changed fails only the notification cycle.
	id := gwfPlant(t, h, "result-1", testChat, "结果")
	gwfClaimed(t, h, id)
	h.do(func() { h.gateway.results.plans[id].Source.Type = "goal" })
	var mu sync.Mutex
	var errs []error
	polls := 0
	h.setHooks(func(k *harnessHooks) {
		k.poll = func() (*http.Response, error) {
			mu.Lock()
			polls++
			stop := polls >= 2 && len(errs) > 0
			mu.Unlock()
			if stop {
				h.gateway.Stop()
			}
			return jsonResponse(200, map[string]any{"ret": 0, "msgs": []any{}, "get_updates_buf": "new-cursor"}), nil
		}
	})
	err := h.gateway.RunLoop(1, func(err error) { t.Errorf("inbound poll failed: %v", err) },
		func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() })
	mustNoErr(t, err)
	var cursor string
	h.do(func() { cursor = h.state.SyncBuf })
	if len(errs) == 0 || polls < 2 || cursor != "new-cursor" || len(h.sent()) != 0 {
		t.Fatalf("errors %v, polls %d, cursor %s", errs, polls, cursor)
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: stop remains terminal when runLoop is called after stopping
func TestGwfStopIsTerminal(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.gateway.Stop()
	mustNoErr(t, h.gateway.RunLoop(0, nil, nil))
	h.mu.Lock()
	n := len(h.requests)
	h.mu.Unlock()
	if n != 0 || len(h.sent()) != 0 {
		t.Fatalf("%d requests after stop", n)
	}
}

// gwfBlockSend holds the first sendmessage until release is closed.
func gwfBlockSend(h *harness) (entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.setHooks(func(k *harnessHooks) {
		k.send = func(map[string]any) (*http.Response, error) {
			once.Do(func() { close(entered) })
			<-release
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		}
	})
	return entered, release
}

// from: gateway/tests/gateway/notification-delivery.test.ts: notification work does not create a processing phase or hide stale inbound health
// from: gateway/tests/gateway/notification-delivery.test.ts: notification cleanup does not erase a concurrent inbound polling health phase
func TestGwfNotificationsLeaveHealthPhaseAlone(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	var before GatewayHealth
	h.do(func() {
		h.gateway.health.StartRun()
		h.gateway.health.RecordPollSuccess()
		before, _ = h.gateway.health.Snapshot()
	})
	gwfPlant(t, h, "result-1", testChat, "结果")
	entered, release := gwfBlockSend(h)
	done := make(chan struct{})
	go func() { _, _ = h.deliverNotificationsOnce(); close(done) }()
	<-entered
	var during GatewayHealth
	h.do(func() { during, _ = h.gateway.health.Snapshot() })
	if during.Phase != nil || during.LastPollSucceededAt != before.LastPollSucceededAt || during.ProcessedMessageCount != 0 {
		t.Fatalf("during %+v", during)
	}
	// A poll starting meanwhile keeps its phase after the notification work ends.
	h.do(func() { mustNoErr(t, h.gateway.health.RecordPollStart(DefaultPollTimeoutMs)) })
	var phase *Phase
	h.do(func() { s, _ := h.gateway.health.Snapshot(); phase = s.Phase })
	close(release)
	<-done
	var after GatewayHealth
	h.do(func() { after, _ = h.gateway.health.Snapshot() })
	if after.Phase == nil || *after.Phase != *phase || after.ProcessedMessageCount != 0 || after.LastPollSucceededAt != before.LastPollSucceededAt {
		t.Fatalf("after %+v", after)
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: stop after the first notification chunk cancels the remaining chunks and records uncertainty
func TestGwfStopAfterFirstChunk(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	id := gwfPlant(t, h, "result-1", testChat, "# 结果\n"+strings.Repeat("完整输出🚀\n", 700))
	h.setHooks(func(k *harnessHooks) {
		k.send = func(map[string]any) (*http.Response, error) {
			h.gateway.Stop()
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		}
	})
	_, err := h.deliverNotificationsOnce()
	mustNoErr(t, err)
	if len(h.sent()) != 1 {
		t.Fatalf("%d sends", len(h.sent()))
	}
	if p := gwfPlan(h, id); p.Status != ResultUncertain {
		t.Fatalf("plan %+v", p)
	}
	r := gwfRestart(t, h)
	gwfSummary(t, r)
	if len(h.sent()) != 1 {
		t.Fatal("first message repeated after restart")
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: blocked notification work does not prevent inbound polling or replace its health phase
func TestGwfBlockedNotificationDoesNotBlockPolling(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.do(func() { h.gateway.health.StartRun() })
	gwfPlant(t, h, "result-1", testChat, "结果")
	entered, release := gwfBlockSend(h)
	var polls int
	var phaseName string
	h.setHooks(func(k *harnessHooks) {
		k.poll = func() (*http.Response, error) {
			<-entered
			polls++
			h.do(func() {
				if s, ok := h.gateway.health.Snapshot(); ok && s.Phase != nil {
					phaseName = s.Phase.Name
				}
				h.gateway.stop()
			})
			close(release)
			return jsonResponse(200, map[string]any{"ret": 0, "msgs": []any{}, "get_updates_buf": "c"}), nil
		}
	})
	mustNoErr(t, h.gateway.RunLoop(0, func(error) {}, func(error) {}))
	if polls != 1 || phaseName != "polling" {
		t.Fatalf("polls %d, phase %q", polls, phaseName)
	}
}
