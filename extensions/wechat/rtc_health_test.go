package wechat

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// from: gateway/tests/runtime/health.test.ts: Health tracker and checkGatewayHealth lifecycle and error sanitization
func TestRtcHealthTrackerLifecycle(t *testing.T) {
	// Health lives in memory and reaches Collie through connector.status: the
	// health.json reads, pid checks and staleness reasons of checkGatewayHealth
	// have no counterpart. The tracker's state machine and the sanitizer are ported.
	setNow(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli())
	tracker := &HealthTracker{}
	if _, ok := tracker.Snapshot(); ok {
		t.Fatal("health before StartRun")
	}
	tracker.StartRun()
	h, _ := tracker.Snapshot()
	if h.Status != HealthStarting {
		t.Fatalf("status = %s", h.Status)
	}
	tracker.RecordPollError(errors.New("WeChat iLink HTTP 502 on /ilink/bot/getupdates"))
	h, _ = tracker.Snapshot()
	if h.ConsecutiveErrors != 1 || h.LastErrorType != "ILinkHttp502" || h.Status != HealthDegraded {
		t.Fatalf("after error: %+v", h)
	}
	if got := SanitizeErrorType(errors.New("WeChat getupdates failed")); got != "ILinkGetUpdatesError" {
		t.Fatalf("getupdates = %s", got)
	}
	// The Controller categories went with the Controller; a custom error name never leaks.
	if got := SanitizeErrorType(errors.New("foo")); got != "OperationalError" {
		t.Fatalf("custom = %s", got)
	}
	tracker.RecordPollSuccess()
	h, _ = tracker.Snapshot()
	if h.Status != HealthHealthy || h.ConsecutiveErrors != 0 || h.LastPollSucceededAt == "" {
		t.Fatalf("after success: %+v", h)
	}
}

// from: gateway/tests/runtime/health.test.ts: Log error classification and output redaction verification
func TestRtcHealthErrorClassification(t *testing.T) {
	if got := SanitizeErrorType(errors.New(`WeChat iLink failed with sensitive_token_9999 and body: {"secret":123}`)); got != "OperationalError" {
		t.Fatalf("leak = %s", got)
	}
	if got := SanitizeErrorType(errors.New("Request to ilinkai.weixin.qq.com/getupdates timed out after 35000ms")); got != "TimeoutError" {
		t.Fatalf("timeout = %s", got)
	}
	// Without a Controller the HTTP status keeps only the generic category.
	if got := SanitizeErrorType(errors.New("Controller /v1/messages returned HTTP 500")); got != "Http500" {
		t.Fatalf("http = %s", got)
	}
	var v any
	parseErr := json.Unmarshal([]byte("{oops"), &v)
	if got := SanitizeErrorType(parseErr); got != "InvalidJsonSyntax" {
		t.Fatalf("syntax = %s", got)
	}
}
