package wechat

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
	"time"
)

// Trace fields allowed out: counts, durations and fixed categories, never a
// message body, chat or token.
var traceFieldNames = map[string]bool{
	"durationMs": true, "budgetMs": true, "outcome": true, "spanId": true, "status": true,
	"batchId": true, "batchIndex": true, "batchCount": true, "batchWaitMs": true, "platformCreatedAtMs": true,
	"platformToReceivedMs": true, "clockSkew": true, "chars": true, "chunkIndex": true, "chunkCount": true,
	"acknowledgedChunks": true, "attempt": true, "httpStatus": true, "source": true, "receiptStatus": true,
	"recovery": true, "reason": true, "purpose": true, "queued": true, "aborted": true, "failed": true,
	"kind": true, "pendingRuns": true,
}

const maxTraceStringChars = 128

// traceSink takes one JSON line; stderr unless a test replaced it.
var (
	traceSinkMu sync.Mutex
	traceSink   io.Writer = os.Stderr
	traceWarned bool
	instanceID  = newUUID()
)

// MessageTrace follows one message (or one poll) through the gateway: the
// id is a hash of chat and message, so a retry after a restart joins it.
type MessageTrace struct {
	TraceID   string
	AttemptID string
	started   time.Time
	sink      func(map[string]any)
}

type traceKey struct{}

func createMessageTrace(chatID, messageID string) *MessageTrace {
	return &MessageTrace{TraceID: hashJSON([]string{chatID, messageID}), AttemptID: newUUID(), started: time.Now()}
}

func withTrace(ctx context.Context, t *MessageTrace) context.Context {
	return context.WithValue(ctx, traceKey{}, t)
}

func currentTrace(ctx context.Context) *MessageTrace {
	t, _ := ctx.Value(traceKey{}).(*MessageTrace)
	return t
}

// Fields are a trace event's values.
type Fields map[string]any

// traceEvent writes one message_latency line with only allowed fields.
func traceEvent(ctx context.Context, stage string, fields Fields) {
	t := currentTrace(ctx)
	if t == nil {
		return
	}
	event := map[string]any{"schema": 1, "event": "message_latency", "service": "gateway", "instanceId": instanceID,
		"traceId": t.TraceID, "attemptId": t.AttemptID, "stage": stage, "timestamp": isoTime(nowMs()),
		"elapsedMs": float64(time.Since(t.started).Microseconds()) / 1000}
	for k, v := range fields {
		if !traceFieldNames[k] || v == nil {
			continue
		}
		switch x := v.(type) {
		case string:
			v = utf16Prefix(x, maxTraceStringChars)
		case float64:
			if math.IsNaN(x) || math.IsInf(x, 0) {
				continue
			}
		}
		event[k] = v
	}
	if t.sink != nil {
		// A failing sink never changes the traced action's outcome.
		defer func() {
			if recover() != nil {
				warnTraceSinkOnce("[telemetry] event sink failed\n")
			}
		}()
		t.sink(event)
		return
	}
	writeLogLine(event, "[telemetry] event sink failed\n")
}

// writeLogLine writes one JSON line to the trace sink; a failing sink is
// reported once and never changes a delivery's outcome.
func writeLogLine(event map[string]any, warning string) {
	line, err := json.Marshal(event)
	traceSinkMu.Lock()
	defer traceSinkMu.Unlock()
	if err == nil {
		_, err = traceSink.Write(append(line, '\n'))
	}
	if err != nil && !traceWarned {
		traceWarned = true
		// Both logging channels may be unavailable; the delivery result stands.
		_, _ = os.Stderr.WriteString(warning)
	}
}

func warnTraceSinkOnce(warning string) {
	traceSinkMu.Lock()
	defer traceSinkMu.Unlock()
	if !traceWarned {
		traceWarned = true
		_, _ = os.Stderr.WriteString(warning)
	}
}

// traceSpan wraps a step in .start and .end events.
func traceSpan[T any](ctx context.Context, stage string, fields Fields, action func() (T, error)) (T, error) {
	started := time.Now()
	spanID := newUUID()
	with := func(extra Fields) Fields {
		out := Fields{"spanId": spanID}
		for k, v := range fields {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	traceEvent(ctx, stage+".start", with(nil))
	value, err := action()
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	traceEvent(ctx, stage+".end", with(Fields{"durationMs": msSince(started), "outcome": outcome}))
	return value, err
}

// traceStep is traceSpan for a step without a value.
func traceStep(ctx context.Context, stage string, fields Fields, action func() error) error {
	_, err := traceSpan(ctx, stage, fields, func() (struct{}, error) { return struct{}{}, action() })
	return err
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("wechat: no randomness: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
