package wechat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

const (
	gwePrivateText    = "private-inbound-body"
	gwePrivateToken   = "private-bot-token"
	gwePrivateContext = "private-context-token"
)

// gweLatency polls one message per chat (client ids message-<i>) with
// private text, token and context.
func gweLatency(t *testing.T, chats ...string) *harness {
	h := newHarness(t, harnessOptions{account: &Account{Token: gwePrivateToken, AccountID: testAccount, UserID: testChat}})
	createdAt := nowMs() - 1000
	h.setHooks(func(k *harnessHooks) {
		k.poll = func() (*http.Response, error) {
			var msgs []any
			for i, chat := range chats {
				m := inbound(chat, fmt.Sprintf("message-%d", i), gwePrivateText, createdAt)
				m["context_token"] = gwePrivateContext
				msgs = append(msgs, m)
			}
			return jsonResponse(200, map[string]any{"ret": 0, "errcode": 0, "get_updates_buf": "after-batch", "msgs": msgs}), nil
		}
	})
	return h
}

// gweTrace is every message_latency event logged so far.
func gweTrace(b *bytes.Buffer) []map[string]any {
	traceSinkMu.Lock()
	defer traceSinkMu.Unlock()
	return logEvents(bytes.NewBuffer(append([]byte(nil), b.Bytes()...)), "message_latency")
}

func gweStage(events []map[string]any, stage string) []map[string]any {
	var out []map[string]any
	for _, e := range events {
		if e["stage"] == stage {
			out = append(out, e)
		}
	}
	return out
}

func gweIndex(events []map[string]any, e map[string]any) int {
	for i, x := range events {
		if fmt.Sprint(x) == fmt.Sprint(e) {
			return i
		}
	}
	return -1
}

func gweNoPrivateData(t *testing.T, b *bytes.Buffer) {
	t.Helper()
	traceSinkMu.Lock()
	s := b.String()
	traceSinkMu.Unlock()
	for _, v := range []string{gwePrivateText, gwePrivateToken, gwePrivateContext, "synthetic-ticket"} {
		if strings.Contains(s, v) {
			t.Fatalf("telemetry contains %s", v)
		}
	}
}

// from: gateway/tests/gateway/latency.test.ts: every message in a batch is recorded before processing and later messages expose batch waiting
func TestGweBatchRecordedBeforeProcessing(t *testing.T) {
	// Only the bound person is served, so both messages of the batch are theirs.
	log := captureLog(t)
	h := gweLatency(t, testChat, testChat)
	h.host.receive = func(m extproto.Message) error {
		if len(h.host.messages()) == 0 {
			if n := len(gweStage(gweTrace(log), "message.received")); n != 2 {
				t.Errorf("%d received events before the first hand-over", n)
			}
			time.Sleep(20 * time.Millisecond)
		}
		return nil
	}
	summary, err := gwePoll(t, h.gateway)
	mustNoErr(t, err)
	if summary.ProcessedCount != 2 || summary.SkippedCount != 0 || summary.NextSyncBuf != "after-batch" || len(h.host.messages()) != 2 {
		t.Fatalf("poll %+v", summary)
	}
	events := gweTrace(log)
	received, processing := gweStage(events, "message.received"), gweStage(events, "message.processing.start")
	if len(received) != 2 || len(processing) != 2 || gweIndex(events, received[1]) > gweIndex(events, processing[0]) {
		t.Fatalf("received %d processing %d", len(received), len(processing))
	}
	if received[0]["batchIndex"] != float64(0) || received[1]["batchIndex"] != float64(1) || received[0]["batchId"] != received[1]["batchId"] ||
		received[0]["batchCount"] != float64(2) {
		t.Fatalf("received %v", received)
	}
	if w0, w1 := processing[0]["batchWaitMs"].(float64), processing[1]["batchWaitMs"].(float64); !(w1 > w0 && w1 > 0) {
		t.Fatalf("batch waits %v %v", w0, w1)
	}
	for i := range received {
		if received[i]["traceId"] != processing[i]["traceId"] {
			t.Fatal("trace ids differ")
		}
		var outcome any
		for _, e := range gweStage(events, "message.processing.end") {
			if e["traceId"] == received[i]["traceId"] {
				outcome = e["outcome"]
			}
		}
		if outcome != "processed" {
			t.Fatalf("outcome %v", outcome)
		}
	}
	gweNoPrivateData(t, log)
	summary, err = gwePoll(t, h.gateway)
	mustNoErr(t, err)
	if summary.PolledCount != 0 || len(h.host.messages()) != 2 {
		t.Fatal("observability must keep deduplication")
	}
}

// from: gateway/tests/gateway/latency.test.ts: poison-chat telemetry does not block valid messages or cursor progress
func TestGwePoisonChatDoesNotBlock(t *testing.T) {
	log := captureLog(t)
	h := gweLatency(t, "poison", testChat)
	summary, err := gwePoll(t, h.gateway)
	mustNoErr(t, err)
	if summary.ProcessedCount != 1 || summary.SkippedCount != 1 || h.state.SyncBuf != "after-batch" {
		t.Fatalf("poll %+v", summary)
	}
	if msgs := h.host.messages(); len(msgs) != 1 || msgs[0].ChatID != testChat {
		t.Fatalf("handed over %v", msgs)
	}
	events := gweTrace(log)
	received := gweStage(events, "message.received")
	for _, e := range events {
		if e["traceId"] == received[0]["traceId"] {
			if e["stage"] == "message.processing.end" && e["outcome"] != "skipped" {
				t.Fatalf("poison outcome %v", e["outcome"])
			}
			if e["stage"] == "message.host.start" {
				t.Fatal("poison message reached Collie")
			}
		}
	}
	gweNoPrivateData(t, log)
}

// from: gateway/tests/gateway/latency.test.ts: a failed Controller request retains the cursor and does not falsely record acceptance
func TestGweFailedHandOverRetainsCursor(t *testing.T) {
	log := captureLog(t)
	h := gweLatency(t, testChat, testChat)
	failure := errors.New("private-controller-failure-body")
	attempts := 0
	h.host.receive = func(extproto.Message) error { attempts++; return failure }
	before := h.state.SyncBuf
	_, err := gwePoll(t, h.gateway)
	if !errors.Is(err, failure) {
		t.Fatalf("error %v", err)
	}
	if h.state.SyncBuf != before || attempts != 1 || len(h.sent()) != 0 {
		t.Fatalf("cursor %q attempts %d", h.state.SyncBuf, attempts)
	}
	events := gweTrace(log)
	if len(gweStage(events, "message.received")) != 2 || len(gweStage(events, "message.processing.start")) != 1 ||
		gweStage(events, "message.processing.end")[0]["outcome"] != "error" {
		t.Fatal("trace does not show the failed attempt")
	}
	raw, _ := json.Marshal(events)
	if strings.Contains(string(raw), "wechat_api_accepted") || strings.Contains(string(raw), failure.Error()) {
		t.Fatal("trace records acceptance or the error text")
	}
	gweNoPrivateData(t, log)
}

type gweBrokenSink struct{}

func (gweBrokenSink) Write([]byte) (int, error) { return 0, errors.New("private-sink-failure") }

// from: gateway/tests/gateway/latency.test.ts: a throwing telemetry sink preserves real Gateway success and failure semantics
func TestGweBrokenTraceSinkKeepsSemantics(t *testing.T) {
	traceSinkMu.Lock()
	previous := traceSink
	traceSink = gweBrokenSink{}
	traceSinkMu.Unlock()
	t.Cleanup(func() {
		traceSinkMu.Lock()
		traceSink = previous
		traceSinkMu.Unlock()
	})
	ok := gweLatency(t, testChat)
	summary, err := gwePoll(t, ok.gateway)
	mustNoErr(t, err)
	if summary.ProcessedCount != 1 || len(ok.host.messages()) != 1 || ok.state.SyncBuf != "after-batch" {
		t.Fatalf("poll %+v", summary)
	}
	failed := gweLatency(t, testChat)
	failure := errors.New("original-controller-error")
	failed.host.receive = func(extproto.Message) error { return failure }
	before := failed.state.SyncBuf
	if _, err := gwePoll(t, failed.gateway); !errors.Is(err, failure) || failed.state.SyncBuf != before || len(failed.sent()) != 0 {
		t.Fatalf("error %v cursor %q", err, failed.state.SyncBuf)
	}
}
