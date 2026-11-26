package wechat

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

type rtdDiag struct {
	events []map[string]any
	d      *NotificationCycleDiagnostics
}

func rtdDiagFixture() *rtdDiag {
	f := &rtdDiag{}
	f.d = NewNotificationCycleDiagnostics(1000, func(e map[string]any) { f.events = append(f.events, e) }, nil, nil)
	return f
}

func (f *rtdDiag) run(action func(context.Context) error) error {
	return f.d.Run(context.Background(), action, func() bool { return true }, func(error) bool { return true })
}

// rtdSendClient is an iLink client whose sendmessage goes to fetch.
func rtdSendClient(t *testing.T, fetch fetchFunc) (*ILinkClient, *loop) {
	s := newTestState(t, t.TempDir(), PolicyOptions{})
	openWindow(t, s, testChat)
	l := &loop{}
	return newTestClient(t, s, l, fakeHTTP(fetch)), l
}

// rtdSendItem sends one text item under ctx, with the loop held.
func rtdSendItem(ctx context.Context, c *ILinkClient, l *loop, clientID string) (err error) {
	l.do(func() {
		_, err = c.sendMessageItem(ctx, testChat, map[string]any{"type": ItemTypeText, "text_item": map[string]any{"text": "x"}}, clientID)
	})
	return err
}

// from: gateway/tests/runtime/notification-diagnostics.test.ts: cause facts are bounded, handle aggregate/cyclic causes and never serialize arbitrary values
func TestRtdDiagnosticsCauseFactsBounded(t *testing.T) {
	// Go errors have no reference cycles or custom names; an aggregate of a
	// DNS failure and an unknown code stands for the original chain.
	err := &net.OpError{Op: "dial", Err: errors.Join(&net.DNSError{Err: rtdPrivate, Name: rtdPrivate, IsNotFound: true}, syscall.Errno(9999))}
	name, codes := safeErrorFacts(err)
	sort.Strings(codes)
	if name != "TypeError" || jsonText(codes) != `["ENOTFOUND","OtherCode"]` {
		t.Fatalf("facts = %s %v", name, codes)
	}
	if strings.Contains(jsonText([]any{name, codes}), rtdPrivate) {
		t.Fatal("facts expose a private value")
	}
	name, codes = safeErrorFacts(errors.New(rtdPrivate))
	if name != "Error" || len(codes) != 0 {
		t.Fatalf("plain error facts = %s %v", name, codes)
	}
	_, codes = safeErrorFacts(syscall.Errno(9999))
	rtdEqual(t, codes, []string{"OtherCode"}, "unknown code")
}

// from: gateway/tests/runtime/notification-diagnostics.test.ts: request failure preserves error identity and reveals only fixed request fields and cause codes
func TestRtdDiagnosticsRequestFailureFacts(t *testing.T) {
	f := rtdDiagFixture()
	original := rtdSocketError()
	c, l := rtdSendClient(t, func(*http.Request, []byte) (*http.Response, error) { return nil, original })
	// The iLink send request stands for the Controller client of the original.
	err := f.run(func(ctx context.Context) error { return rtdSendItem(ctx, c, l, "client") })
	if !errors.Is(err, original) {
		t.Fatalf("error identity lost: %v", err)
	}
	e := f.events[0]
	if e["event"] != "notification_cycle_failed" || e["operation"] != "wechat_sendmessage" || e["phase"] != "fetch" ||
		e["errorType"] != "NetworkError" || e["requestAttempts"] != 1 || e["requestMaxAttempts"] != 1 ||
		e["retryScheduled"] != true || e["retryDelayMs"] != int64(1000) || e["timedOut"] != false {
		t.Fatalf("event = %v", e)
	}
	rtdEqual(t, e["causeCodes"], []string{"ECONNRESET"}, "cause codes")
	raw, _ := json.Marshal(e)
	if strings.Contains(string(raw), rtdPrivate) || strings.Contains(string(raw), "ilinkai") {
		t.Fatalf("event exposes a private value: %s", raw)
	}
}

type rtdFailingBody struct{ err error }

func (b rtdFailingBody) Read([]byte) (int, error) { return 0, b.err }
func (rtdFailingBody) Close() error               { return nil }

// from: gateway/tests/runtime/notification-diagnostics.test.ts: HTTP200 with a failing response body records body phase, never recovery
func TestRtdDiagnosticsBodyPhase(t *testing.T) {
	f := rtdDiagFixture()
	original := rtdSocketError()
	c, l := rtdSendClient(t, func(*http.Request, []byte) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: rtdFailingBody{original}}, nil
	})
	if err := f.run(func(ctx context.Context) error { return rtdSendItem(ctx, c, l, "client") }); !errors.Is(err, original) {
		t.Fatalf("error = %v", err)
	}
	if e := f.events[0]; e["operation"] != "wechat_sendmessage" || e["phase"] != "body" || e["httpStatus"] != 200 {
		t.Fatalf("event = %v", e)
	}
	mustNoErr(t, f.run(func(context.Context) error { return nil }))
	if len(f.events) != 1 {
		t.Fatal("a skipped cycle claimed recovery")
	}
}

// from: gateway/tests/runtime/notification-diagnostics.test.ts: timeout facts differ from a caller abort and response byte-budget rejection
func TestRtdDiagnosticsTimeoutFacts(t *testing.T) {
	f := rtdDiagFixture()
	c, l := rtdSendClient(t, func(req *http.Request, _ []byte) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	request := func(ctx context.Context, timeoutMs int64) (err error) {
		l.do(func() {
			_, err = c.request(ctx, endpointSendMessage, map[string]any{}, testToken, timeoutMs, newRequestDiagnostics("wechat_sendmessage", "x"))
		})
		return err
	}
	if err := f.run(func(ctx context.Context) error { return request(ctx, 5) }); err == nil {
		t.Fatal("timeout did not fail")
	}
	if f.events[0]["timedOut"] != true || f.events[0]["parentAborted"] != false {
		t.Fatalf("timeout event = %v", f.events[0])
	}
	aborted, cancel := context.WithCancelCause(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel(errors.New(rtdPrivate)) }()
	if err := f.d.Run(aborted, func(ctx context.Context) error { return request(ctx, 5000) }, func() bool { return true },
		func(error) bool { return true }); err == nil {
		t.Fatal("abort did not fail")
	}
	if f.events[1]["timedOut"] != false || f.events[1]["parentAborted"] != true {
		t.Fatalf("abort event = %v", f.events[1])
	}
	big, bl := rtdSendClient(t, func(*http.Request, []byte) (*http.Response, error) {
		return bytesResponse(200, make([]byte, maxILinkResponseBytes+1), nil), nil
	})
	if err := f.run(func(ctx context.Context) error { return rtdSendItem(ctx, big, bl, "client") }); err == nil {
		t.Fatal("oversized answer accepted")
	}
	if f.events[2]["timedOut"] != false {
		t.Fatalf("budget event = %v", f.events[2])
	}
}

// from: gateway/tests/runtime/notification-diagnostics.test.ts: successful skipped cycles, another recipient, and partial cycles cannot claim request recovery
func TestRtdDiagnosticsNoFalseRecovery(t *testing.T) {
	f := rtdDiagFixture()
	pending := func(ctx context.Context, recipient string, fails bool) error {
		d := newRequestDiagnostics("result_pending", recipient)
		if fails {
			return d.failure(ctx, rtdSocketError(), 1, 1, false, false)
		}
		d.success(ctx)
		return nil
	}
	rtdFails(t, f.run(func(ctx context.Context) error { return pending(ctx, "recipient-a", true) }))
	mustNoErr(t, f.run(func(context.Context) error { return nil }))
	mustNoErr(t, f.run(func(ctx context.Context) error { return pending(ctx, "recipient-b", false) }))
	if len(f.events) != 1 {
		t.Fatalf("events = %d", len(f.events))
	}
	rtdFails(t, f.run(func(ctx context.Context) error {
		mustNoErr(t, pending(ctx, "recipient-a", false))
		return newRequestDiagnostics("result_get", "later-stage").failure(ctx, rtdSocketError(), 1, 1, false, false)
	}))
	if len(f.events) != 2 {
		t.Fatal("a successful request cannot hide a later stage failure")
	}
	mustNoErr(t, f.run(func(context.Context) error { return nil }))
	if len(f.events) != 2 {
		t.Fatal("skipped cycle recovered")
	}
	mustNoErr(t, f.run(func(ctx context.Context) error {
		newRequestDiagnostics("result_get", "later-stage").success(ctx)
		return nil
	}))
	if f.events[2]["event"] != "notification_cycle_recovered" {
		t.Fatalf("event = %v", f.events[2])
	}
	rtdEqual(t, f.events[2]["recoveredOperations"], []string{"result_pending", "result_get"}, "recovered operations")
	for i := 0; i < 5; i++ {
		mustNoErr(t, f.run(func(context.Context) error { return nil }))
	}
	if len(f.events) != 3 {
		t.Fatal("no successful per-second noise after recovery")
	}
}

// from: gateway/tests/runtime/notification-diagnostics.test.ts: separate outstanding requests must all succeed before a complete cycle emits recovery
func TestRtdDiagnosticsAllOutstandingMustSucceed(t *testing.T) {
	f := rtdDiagFixture()
	rtdFails(t, f.run(func(ctx context.Context) error { return rtdRequest(ctx, "a", true) }))
	rtdFails(t, f.run(func(ctx context.Context) error { return rtdRequest(ctx, "b", true) }))
	mustNoErr(t, f.run(func(ctx context.Context) error { return rtdRequest(ctx, "a", false) }))
	if len(f.events) != 2 {
		t.Fatalf("events = %d", len(f.events))
	}
	mustNoErr(t, f.run(func(ctx context.Context) error { return rtdRequest(ctx, "b", false) }))
	if f.events[2]["event"] != "notification_cycle_recovered" {
		t.Fatalf("event = %v", f.events[2])
	}
}

// from: gateway/tests/runtime/notification-diagnostics.test.ts: all failures from one cycle survive firstError propagation and skipped work
func TestRtdDiagnosticsAllFailuresOfOneCycle(t *testing.T) {
	f := rtdDiagFixture()
	rtdFails(t, f.run(func(ctx context.Context) error {
		first := newRequestDiagnostics("result_get", "a").failure(ctx, rtdSocketError(), 1, 1, false, false)
		_ = newRequestDiagnostics("result_get", "b").failure(ctx, rtdSocketError(), 1, 1, false, false)
		return first
	}))
	if f.events[0]["unresolvedFailures"] != 2 {
		t.Fatalf("event = %v", f.events[0])
	}
	mustNoErr(t, f.run(func(ctx context.Context) error { return rtdRequest(ctx, "a", false) }))
	mustNoErr(t, f.run(func(context.Context) error { return nil }))
	if len(f.events) != 1 {
		t.Fatalf("events = %d", len(f.events))
	}
	mustNoErr(t, f.run(func(ctx context.Context) error { return rtdRequest(ctx, "b", false) }))
	if f.events[1]["event"] != "notification_cycle_recovered" {
		t.Fatalf("event = %v", f.events[1])
	}
}

// from: gateway/tests/runtime/notification-diagnostics.test.ts: schema and identity validation stay in their request; skipped cycles cannot recover them or retry a receipt
func TestRtdDiagnosticsValidationPhase(t *testing.T) {
	// The iLink receipt validation stands for the Controller schema checks;
	// a refused receipt is never sent again on its own.
	f := rtdDiagFixture()
	bad, calls := true, 0
	c, l := rtdSendClient(t, func(*http.Request, []byte) (*http.Response, error) {
		calls++
		if bad {
			return jsonResponse(200, map[string]any{"items": "bad"}), nil
		}
		return jsonResponse(200, map[string]any{"ret": 0}), nil
	})
	rtdFails(t, f.run(func(ctx context.Context) error { return rtdSendItem(ctx, c, l, "client") }))
	if f.events[0]["phase"] != "validate" || f.events[0]["operation"] != "wechat_sendmessage" {
		t.Fatalf("event = %v", f.events[0])
	}
	mustNoErr(t, f.run(func(context.Context) error { return nil }))
	if len(f.events) != 1 {
		t.Fatal("skipped cycle recovered a validation failure")
	}
	bad = false
	mustNoErr(t, f.run(func(ctx context.Context) error { return rtdSendItem(ctx, c, l, "client") }))
	if f.events[1]["event"] != "notification_cycle_recovered" || calls != 2 {
		t.Fatalf("event = %v, calls = %d", f.events[1], calls)
	}
}

// from: gateway/tests/runtime/notification-diagnostics.test.ts: an uninstrumented local-stage failure is left unproven by a skipped cycle
func TestRtdDiagnosticsLocalStageUnproven(t *testing.T) {
	f := rtdDiagFixture()
	rtdFails(t, f.run(func(context.Context) error { return errors.New(rtdPrivate) }))
	mustNoErr(t, f.run(func(context.Context) error { return nil }))
	if len(f.events) != 1 {
		t.Fatalf("events = %d", len(f.events))
	}
}

// from: gateway/tests/runtime/notification-diagnostics.test.ts: readiness is logged once; shutdown aborts and fatal diagnostic errors are not retried diagnostics
func TestRtdDiagnosticsReadinessOnce(t *testing.T) {
	f := rtdDiagFixture()
	mustNoErr(t, f.run(func(context.Context) error { return nil }))
	mustNoErr(t, f.run(func(context.Context) error { return nil }))
	rtdEqual(t, []string{f.events[0]["event"].(string)}, []string{"notification_cycle_ready"}, "events")
	fail := func(context.Context) error { return rtdSocketError() }
	rtdFails(t, f.d.Run(context.Background(), fail, func() bool { return false }, func(error) bool { return true }))
	rtdFails(t, f.d.Run(context.Background(), fail, func() bool { return true }, func(error) bool { return false }))
	if len(f.events) != 1 {
		t.Fatalf("events = %d", len(f.events))
	}
}

// from: gateway/tests/runtime/notification-diagnostics.test.ts: actual independent Gateway loop retries and reports recovery while inbound health remains separate
func TestRtdDiagnosticsGatewayLoopRecovery(t *testing.T) {
	var events []map[string]any
	var h *harness
	var healthAtRecovery GatewayHealth
	diag := NewNotificationCycleDiagnostics(1, func(e map[string]any) {
		events = append(events, e)
		if e["event"] == "notification_cycle_recovered" {
			healthAtRecovery, _ = h.gateway.health.Snapshot()
			h.gateway.stop() // the sink runs on the loop
		}
	}, nil, nil)
	h = newHarness(t, harnessOptions{diagnosis: diag})
	h.gateway.health.StartRun()
	sends := 0
	h.setHooks(func(k *harnessHooks) {
		k.send = func(map[string]any) (*http.Response, error) {
			sends++
			if sends <= 2 {
				// An explicit refusal keeps the answer pending for the next cycle.
				return jsonResponse(200, map[string]any{"ret": -1}), nil
			}
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		}
	})
	// The poll stays open until the stop, so every retry happens in the notification cycle.
	h.setHooks(func(k *harnessHooks) {
		k.poll = func() (*http.Response, error) { <-h.gateway.ctx.Done(); return nil, context.Canceled }
	})
	res, err := h.send(extproto.Send{Ref: "answer-1", Text: "回复", Kind: extproto.KindAnswer})
	mustNoErr(t, err)
	if !res.Deferred {
		t.Fatalf("answer = %+v, want it retried", res)
	}
	var notificationErrors []error
	go func() {
		time.Sleep(5 * time.Second)
		h.gateway.Stop()
	}()
	done := make(chan error, 1)
	go func() {
		done <- h.gateway.RunLoop(1, func(error) {}, func(err error) { notificationErrors = append(notificationErrors, err) })
	}()
	<-done
	h.do(func() {
		if len(notificationErrors) != 1 {
			t.Fatalf("notification errors = %d", len(notificationErrors))
		}
		if len(events) < 2 || events[0]["stage"] != StageReplyRecovery || events[0]["operation"] != "wechat_sendmessage" ||
			events[1]["event"] != "notification_cycle_recovered" {
			t.Fatalf("events = %v", events)
		}
		if notificationFailure(notificationErrors[0]).Request.Operation != "wechat_sendmessage" {
			t.Fatal("the reported error lost its request facts")
		}
		// Read before the stop: the open poll's cancellation is not part of the cycle.
		if healthAtRecovery.TotalErrorCount != 0 {
			t.Fatalf("notification failures were counted as poll errors: %+v", healthAtRecovery)
		}
	})
	if sends != 3 {
		t.Fatalf("sends = %d, want one refused hand-over, one refused retry and the delivery", sends)
	}
}
