package wechat

import (
	"context"
	"testing"
)

// Off (the default outside tests), a trace writes nothing; errors still do.
func TestTraceOffByDefault(t *testing.T) {
	b := captureLog(t)
	traceOn = false
	t.Cleanup(func() { traceOn = true })
	ctx := withTrace(context.Background(), createMessageTrace("chat", "msg"))
	traceEvent(ctx, "poll.start", Fields{"durationMs": 1.0})
	if err := traceStep(ctx, "message.host", nil, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if b.Len() != 0 {
		t.Fatalf("trace lines while off: %s", b)
	}
	writeLogLine(map[string]any{"event": "poll_failed"}, "")
	if len(logEvents(b, "poll_failed")) != 1 {
		t.Fatalf("an error line must still go out: %s", b)
	}
}
