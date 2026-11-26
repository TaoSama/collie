package wechat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var rtdUUID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func rtdTraceWith(chat, message string, events *[]map[string]any, mu *sync.Mutex) *MessageTrace {
	t := createMessageTrace(chat, message)
	t.sink = func(e map[string]any) {
		if mu != nil {
			mu.Lock()
			defer mu.Unlock()
		}
		*events = append(*events, e)
	}
	return t
}

// rtdSwapSink points the default sink at w for one test.
func rtdSwapSink(t *testing.T, w io.Writer) {
	traceSinkMu.Lock()
	previous, warned := traceSink, traceWarned
	traceSink, traceWarned = w, false
	traceSinkMu.Unlock()
	t.Cleanup(func() {
		traceSinkMu.Lock()
		traceSink, traceWarned = previous, warned
		traceSinkMu.Unlock()
	})
}

// from: gateway/tests/runtime/telemetry.test.ts: message identity remains stable across attempts without logging chat or message contents
func TestRtdTelemetryStableIdentity(t *testing.T) {
	var events []map[string]any
	first := rtdTraceWith("private-chat", "private-message", &events, nil)
	retry := createMessageTrace("private-chat", "private-message")
	other := createMessageTrace("other-chat", "private-message")
	if first.TraceID != retry.TraceID || first.AttemptID == retry.AttemptID || first.TraceID == other.TraceID ||
		!digestPattern.MatchString(first.TraceID) || !rtdUUID.MatchString(first.AttemptID) {
		t.Fatalf("trace ids: %+v %+v", first, retry)
	}
	traceEvent(withTrace(context.Background(), first), "message.received", Fields{"chars": 3})
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	e := events[0]
	if e["traceId"] != first.TraceID || e["attemptId"] != first.AttemptID || e["service"] != "gateway" || e["schema"] != 1 {
		t.Fatalf("event = %v", e)
	}
	if _, err := time.Parse(time.RFC3339Nano, e["timestamp"].(string)); err != nil {
		t.Fatalf("timestamp: %v", err)
	}
	if _, ok := e["elapsedMs"].(float64); !ok {
		t.Fatalf("elapsedMs = %T", e["elapsedMs"])
	}
	if raw := jsonText(events); strings.Contains(raw, "private-chat") || strings.Contains(raw, "private-message") {
		t.Fatal("events expose the chat or message")
	}
}

// from: gateway/tests/runtime/telemetry.test.ts: telemetry drops non-allowlisted secrets and invalid numeric values
func TestRtdTelemetryDropsSecrets(t *testing.T) {
	var events []map[string]any
	trace := rtdTraceWith("chat", "message", &events, nil)
	traceEvent(withTrace(context.Background(), trace), "fixture", Fields{
		"text": "secret-message-body", "token": "secret-token", "authorization": "secret-authorization",
		"arguments": "secret-tool-arguments", "output": "secret-tool-output", "contextToken": "secret-context",
		"traceId": "overridden-trace", "service": "overridden-service", "timestamp": "overridden-time",
		"durationMs": nanValue(), "budgetMs": infValue(), "chunkCount": nil,
		"httpStatus": 200, "failed": false, "reason": nil, "purpose": strings.Repeat("x", 300),
		// The engine's fields (tool, tokens) left the allowlist with the engine.
		"tool": strings.Repeat("x", 300), "inputTokens": 5,
	})
	e := events[0]
	for _, key := range []string{"text", "token", "authorization", "arguments", "output", "contextToken", "durationMs", "budgetMs",
		"chunkCount", "tool", "inputTokens"} {
		if _, ok := e[key]; ok {
			t.Fatalf("%s kept", key)
		}
	}
	if e["traceId"] != trace.TraceID || e["service"] != "gateway" || e["timestamp"] == "overridden-time" ||
		e["httpStatus"] != 200 || e["failed"] != false || e["purpose"] != strings.Repeat("x", 128) {
		t.Fatalf("event = %v", e)
	}
	// A nil value is dropped like undefined: Go fields cannot tell null from absent.
	if _, ok := e["reason"]; ok {
		t.Fatal("nil reason kept")
	}
	if strings.Contains(jsonText(events), "secret-") {
		t.Fatal("events expose a secret")
	}
}

func nanValue() float64 { var z float64; return z / z }
func infValue() float64 { var z float64; return 1 / z }

// from: gateway/tests/runtime/telemetry.test.ts: async trace contexts remain separate and restore the caller context
func TestRtdTelemetrySeparateContexts(t *testing.T) {
	var events []map[string]any
	var mu sync.Mutex
	first := rtdTraceWith("chat-a", "message", &events, &mu)
	second := rtdTraceWith("chat-b", "message", &events, &mu)
	if currentTrace(context.Background()) != nil {
		t.Fatal("background has a trace")
	}
	var wg sync.WaitGroup
	for _, tr := range []*MessageTrace{first, second} {
		wg.Add(1)
		go func(tr *MessageTrace) {
			defer wg.Done()
			ctx := withTrace(context.Background(), tr)
			time.Sleep(time.Millisecond)
			if currentTrace(ctx) != tr {
				t.Error("trace context mixed")
			}
			traceEvent(ctx, "isolated", nil)
		}(tr)
	}
	wg.Wait()
	ids := map[any]bool{}
	for _, e := range events {
		ids[e["traceId"]] = true
	}
	if len(ids) != 2 || !ids[first.TraceID] || !ids[second.TraceID] {
		t.Fatalf("trace ids = %v", ids)
	}
}

// from: gateway/tests/runtime/telemetry.test.ts: spans pair success and failure without swallowing the original error
func TestRtdTelemetrySpansPair(t *testing.T) {
	var events []map[string]any
	ctx := withTrace(context.Background(), rtdTraceWith("chat", "message", &events, nil))
	failure := errors.New("private-provider-error")
	if v, err := traceSpan(ctx, "success", nil, func() (int, error) { return 42, nil }); v != 42 || err != nil {
		t.Fatalf("success span = %d %v", v, err)
	}
	if err := traceStep(ctx, "failure", nil, func() error { return failure }); err != failure {
		t.Fatalf("failure span = %v", err)
	}
	var stages []string
	for _, e := range events {
		stages = append(stages, e["stage"].(string))
	}
	rtdEqual(t, stages, []string{"success.start", "success.end", "failure.start", "failure.end"}, "stages")
	if events[0]["spanId"] != events[1]["spanId"] || events[2]["spanId"] != events[3]["spanId"] || events[0]["spanId"] == events[2]["spanId"] ||
		events[1]["outcome"] != "ok" || events[3]["outcome"] != "error" {
		t.Fatalf("spans = %v", events)
	}
	for _, e := range []map[string]any{events[1], events[3]} {
		if d, _ := e["durationMs"].(float64); d < 0 {
			t.Fatalf("duration %v", e["durationMs"])
		}
	}
	if strings.Contains(jsonText(events), failure.Error()) {
		t.Fatal("events expose the error")
	}
}

// from: gateway/tests/runtime/telemetry.test.ts: a throwing logger cannot turn a successful action into failure or replace an action failure
func TestRtdTelemetryThrowingLogger(t *testing.T) {
	rtdSwapSink(t, io.Discard)
	traceSinkMu.Lock()
	traceWarned = true // keeps the one-time warning off the test output
	traceSinkMu.Unlock()
	trace := createMessageTrace("chat", "message")
	trace.sink = func(map[string]any) { panic("private-sink-error") }
	ctx := withTrace(context.Background(), trace)
	failure := errors.New("original-action-failure")
	traceEvent(ctx, "fixture", nil)
	if v, err := traceSpan(ctx, "success", nil, func() (string, error) { return "accepted", nil }); v != "accepted" || err != nil {
		t.Fatalf("success = %q %v", v, err)
	}
	if err := traceStep(ctx, "failure", nil, func() error { return failure }); err != failure {
		t.Fatalf("failure = %v", err)
	}
}

type rtdBrokenWriter struct{ writes int }

func (w *rtdBrokenWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, errors.New("private-stderr-error")
}

// from: gateway/tests/runtime/telemetry.test.ts: failed event and diagnostic sinks cannot interrupt actions or replace their original failures
func TestRtdTelemetryBrokenSinks(t *testing.T) {
	broken := &rtdBrokenWriter{}
	rtdSwapSink(t, broken)
	// The fallback warning goes to stderr: capture it.
	r, w, err := os.Pipe()
	mustNoErr(t, err)
	stderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = stderr })
	ctx := withTrace(context.Background(), createMessageTrace("chat", "message"))
	failure := errors.New("original-action-error")
	completed := 0
	traceEvent(ctx, "fixture", nil)
	if v, err := traceSpan(ctx, "success", nil, func() (string, error) { completed++; return "accepted", nil }); v != "accepted" || err != nil {
		t.Fatalf("success = %q %v", v, err)
	}
	if err := traceStep(ctx, "failure", nil, func() error { return failure }); err != failure {
		t.Fatalf("failure = %v", err)
	}
	os.Stderr = stderr
	w.Close()
	warnings, err := io.ReadAll(r)
	mustNoErr(t, err)
	if completed != 1 || broken.writes != 5 {
		t.Fatalf("completed %d, sink writes %d, want five events", completed, broken.writes)
	}
	if string(warnings) != "[telemetry] event sink failed\n" {
		t.Fatalf("a broken sink must warn exactly once, got %q", warnings)
	}
}

// from: gateway/tests/runtime/telemetry.test.ts: calling telemetry without a context produces no event
func TestRtdTelemetryWithoutContext(t *testing.T) {
	var b bytes.Buffer
	rtdSwapSink(t, &b)
	traceEvent(context.Background(), "outside-context", Fields{"chars": 1})
	if b.Len() != 0 {
		t.Fatalf("output = %q", b.String())
	}
}

// TestRtdTelemetryHelperProcess runs in a child process for the stdout and
// stderr test; it does nothing in a normal run.
func TestRtdTelemetryHelperProcess(t *testing.T) {
	mode := os.Getenv("RTD_TELEMETRY_HELPER")
	if mode == "" {
		return
	}
	traceSink = os.Stderr
	if mode == "closed" {
		_ = syscall.Close(2)
	}
	ctx := withTrace(context.Background(), createMessageTrace("chat", "message"))
	result, _ := traceSpan(ctx, "success", nil, func() (string, error) { return "accepted", nil })
	original := errors.New("private-action-failure")
	preserved := traceStep(ctx, "failure", nil, func() error { return original }) == original
	out, _ := json.Marshal(map[string]any{"result": result, "originalErrorPreserved": preserved})
	_, _ = os.Stdout.Write(out)
	os.Exit(0)
}

// from: gateway/tests/runtime/telemetry.test.ts: default telemetry preserves stdout JSON${closedStderr ? " with stderr closed" : " and writes JSON events to stderr"}
func TestRtdTelemetryDefaultSinkKeepsStdout(t *testing.T) {
	for _, closed := range []bool{false, true} {
		mode := "open"
		if closed {
			mode = "closed"
		}
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestRtdTelemetryHelperProcess$")
			cmd.Env = append(os.Environ(), "RTD_TELEMETRY_HELPER="+mode)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			mustNoErr(t, cmd.Run())
			if stdout.String() != `{"originalErrorPreserved":true,"result":"accepted"}` {
				t.Fatalf("stdout = %q", stdout.String())
			}
			if closed {
				if stderr.Len() != 0 {
					t.Fatalf("stderr = %q", stderr.String())
				}
				return
			}
			var stages []string
			for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
				var e map[string]any
				mustNoErr(t, json.Unmarshal([]byte(line), &e))
				if e["service"] != "gateway" {
					t.Fatalf("event = %v", e)
				}
				stages = append(stages, e["stage"].(string))
			}
			rtdEqual(t, stages, []string{"success.start", "success.end", "failure.start", "failure.end"}, "stages")
			if strings.Contains(stderr.String(), "private-action-failure") {
				t.Fatal("stderr exposes the error")
			}
		})
	}
}
