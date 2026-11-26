package wechat

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

func gwgTypings(h *harness) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.typings)
}

func gwgWaitTypings(t *testing.T, h *harness, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for gwgTypings(h) < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if got := gwgTypings(h); got != n {
		t.Fatalf("%d typings, want %d", got, n)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ty := range h.typings {
		if ty["status"] != float64(typingOn) {
			t.Fatalf("typing status %v", ty["status"])
		}
	}
}

// from: gateway/tests/gateway/progress-delivery.test.ts: notification worker checks for new progress while getupdates remains in flight
func TestGwgNotificationWorkerRunsWhilePollInFlight(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.pushResult("result-1", "进度结果")
	pollStarted := make(chan struct{})
	finishPoll := make(chan struct{})
	var once, startOnce sync.Once
	h.setHooks(func(k *harnessHooks) {
		k.poll = func() (*http.Response, error) {
			startOnce.Do(func() { close(pollStarted) })
			<-finishPoll
			return jsonResponse(200, map[string]any{"ret": 0, "msgs": []any{}, "get_updates_buf": "cursor"}), nil
		}
		k.send = func(map[string]any) (*http.Response, error) {
			select {
			case <-pollStarted:
			default:
				t.Error("result sent before getupdates was in flight")
			}
			go h.gateway.Stop()
			once.Do(func() { close(finishPoll) })
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		}
	})
	done := make(chan error, 1)
	go func() {
		done <- h.gateway.RunLoop(1, func(err error) { t.Errorf("poll error: %v", err) }, func(error) {})
	}()
	select {
	case err := <-done:
		mustNoErr(t, err)
	case <-time.After(10 * time.Second):
		once.Do(func() { close(finishPoll) })
		t.Fatal("run loop did not stop")
	}
	if n := len(h.sent()); n != 1 {
		t.Fatalf("%d sends", n)
	}
}

// from: gateway/tests/gateway/progress-delivery.test.ts: three progress events and the final result each trigger typing and preserve original Markdown
func TestGwgProgressAndFinalPreserveMarkdown(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	texts := []string{"**进度 1**\n`代码🚀`", "**进度 2**\n`代码🚀`", "**进度 3**\n`代码🚀`", "# 最终结果\n\n完整的 **原文**。"}
	// Progress comes from Collie as channel.send{kind: progress}; the final result goes through the result queue.
	for i, text := range texts[:3] {
		res, err := h.send(extproto.Send{Ref: "progress-" + string(rune('1'+i)), Text: text, Kind: extproto.KindProgress, ReplyTo: "request"})
		mustNoErr(t, err)
		if res.MessageID == "" && res.Deferred {
			t.Fatalf("progress %d = %+v", i, res)
		}
		h.do(func() {
			if b := h.state.AccountBudget; b.Remaining != h.state.Policy.SendBudget-int64(i)-1 || b.ProgressSent != int64(i+1) {
				t.Fatalf("budget after progress %d = %+v", i, b)
			}
		})
		// Typing runs in the background; one in flight absorbs the next, so wait for each.
		gwgWaitTypings(t, h, i+1)
	}
	h.pushResult("final", texts[3])
	summary, err := h.deliverNotificationsOnce()
	mustNoErr(t, err)
	if summary.Accepted != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	h.do(func() {
		if b := h.state.AccountBudget; b.Remaining != h.state.Policy.SendBudget-4 || b.ProgressSent != 3 {
			t.Fatalf("budget = %+v", b)
		}
	})
	gwgWaitTypings(t, h, 4)
	gwgEqual(t, "sent", h.sentTexts(), texts)
	if _, err := h.deliverNotificationsOnce(); err != nil || len(h.sent()) != 4 {
		t.Fatalf("second pass: %v, %d sends", err, len(h.sent()))
	}
	gwgWaitTypings(t, h, 4)
}

// from: gateway/tests/gateway/progress-delivery.test.ts: terminal outbox parts retain pure whitespace continuations without truncation or summaries
func TestGwgResultKeepsWhitespaceContinuation(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	// Collie hands the whole result at once; its parts are joined and sent verbatim.
	parts := []string{"# 原始结果\n```ts\n  run();\n```", " \n\t\n  "}
	h.pushResult("result", parts[0]+parts[1])
	summary, err := h.deliverNotificationsOnce()
	mustNoErr(t, err)
	if summary.Accepted != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	joined := ""
	for _, s := range h.sentTexts() {
		joined += s
	}
	if joined != parts[0]+parts[1] {
		t.Fatalf("sent %q", joined)
	}
	gwgWaitTypings(t, h, 1)
	if ev := h.host.events(); len(ev) != 1 || ev[0].State != DeliverySent {
		t.Fatalf("events = %+v", ev)
	}
}
