package wechat

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

// gwgPaged serves one message of chat "chat" per page, client id m<page>.
func gwgPaged(h *harness, timestamp int64) *atomic.Int64 {
	var page atomic.Int64
	page.Store(1)
	h.setHooks(func(k *harnessHooks) {
		k.poll = func() (*http.Response, error) {
			p := page.Load()
			msg := inbound(testChat, "m"+strconv.FormatInt(p, 10), "request", timestamp+p)
			msg["context_token"] = "context-" + strconv.FormatInt(p, 10)
			return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "cursor-" + strconv.FormatInt(p, 10), "msgs": []any{msg}}), nil
		}
	})
	return &page
}

// gwgIs reports whether a handed-over message is client id mid.
func gwgIs(m extproto.Message, mid string) bool { return strings.HasSuffix(m.ID, `"`+mid+`"]`) }

// from: gateway/tests/gateway/reply-outbox.test.ts: real local HTTP: budget defers, new inbound refreshes, restart GET resumes frozen chunks without re-POST
func TestGwgReplyOutboxBudgetDefersAndResumesFrozenChunks(t *testing.T) {
	policy := PolicyOptions{SendBudget: 2, OutboxLimit: 1}
	h := newHarness(t, harnessOptions{noWindow: true, policy: policy, splitMax: 2})
	page := gwgPaged(h, time.Now().UnixMilli()-1000)
	answer := func(m extproto.Message) string {
		if gwgIs(m, "m1") {
			return "abcdef"
		}
		return ""
	}
	gwgAnswerWith(h, answer)
	gwgPoll(t, h)
	key := h.state.ComputeDeliveryKey(testChat, "m1")
	gwgEqual(t, "sent", h.sentTexts(), []string{"ab", "cd"})
	h.do(func() {
		s := h.state
		if s.SyncBuf != "cursor-1" || s.Outbox[key].Status != OutboxPending || s.ReplyDeliveries[key].AcknowledgedChunks != 2 ||
			s.ReplyDeliveries[key].InFlight || s.Outbox[key].SplitMaxChars != 2 {
			t.Fatalf("outbox %+v, plan %+v", s.Outbox[key], s.ReplyDeliveries[key])
		}
	})
	raw, err := os.ReadFile(filepath.Join(h.dir, stateFileName))
	mustNoErr(t, err)
	if strings.Contains(string(raw), "abcdef") {
		t.Fatal("state file holds the reply body")
	}
	page.Store(2)
	restarted := gwgReopen(t, h, policy, 1)
	gwgAnswerWith(restarted, answer)
	restarted.do(func() {
		restarted.state.failSave = func() error {
			if _, ok := restarted.state.Outbox[key]; !ok && restarted.state.ReplyDeliveries[key].AcknowledgedChunks == 3 {
				return errors.New("injected reference cleanup failure")
			}
			return nil
		}
	})
	if _, err := gwgPollErr(restarted); err == nil || !strings.Contains(err.Error(), "cleanup failure") {
		t.Fatalf("poll error = %v", err)
	}
	gwgEqual(t, "sent", h.sentTexts(), []string{"ab", "cd", "ef"})
	restarted.do(func() {
		// Collie's answers record their receipt intent before cleanup, so the reference waits as receipt, not pending.
		if item := restarted.state.Outbox[key]; item.Status != OutboxReceipt || item.ReceiptStatus != "accepted" ||
			restarted.state.ReplyDeliveries[key].AcknowledgedChunks != 3 || restarted.state.SyncBuf != "cursor-1" {
			t.Fatalf("cleanup failure lost the reference or moved the cursor: %+v %+v %s", restarted.state.Outbox[key], restarted.state.ReplyDeliveries[key], restarted.state.SyncBuf)
		}
	})
	completed := gwgReopen(t, h, PolicyOptions{SendBudget: 1}, 1)
	gwgAnswerWith(completed, answer)
	completed.do(func() {
		if completed.state.CanSendToChat(testChat, SendFinal) {
			t.Fatal("a lowered budget must count the earlier sends")
		}
	})
	gwgPoll(t, completed)
	completed.do(func() {
		if _, ok := completed.state.Outbox[key]; ok || completed.state.SyncBuf != "cursor-2" {
			t.Fatal("completed reply not cleaned up")
		}
	})
	gwgEqual(t, "sent", h.sentTexts(), []string{"ab", "cd", "ef"})
	// Collie got each message once; the frozen answer was never asked for again.
	if n := len(h.host.messages()); n != 2 {
		t.Fatalf("%d hand-overs", n)
	}
}

// from: gateway/tests/gateway/reply-outbox.test.ts: unknown legacy context queues reply without creating window or blocking another chat
func TestGwgReplyOutboxLegacyContextQueuesReply(t *testing.T) {
	// One person is served now, so the three chats of the original become three messages of that chat.
	h := newHarness(t, harnessOptions{noWindow: true})
	h.do(func() { h.state.SetContextToken(testChat, "old-context") })
	h.setHooks(func(k *harnessHooks) {
		k.poll = func() (*http.Response, error) {
			legacy := inbound(testChat, "old", "legacy", 0)
			delete(legacy, "create_time_ms")
			delete(legacy, "context_token")
			return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "next", "msgs": []any{legacy}}), nil
		}
	})
	gwgAnswerWith(h, func(extproto.Message) string { return "reply" })
	gwgPoll(t, h)
	if n := len(h.host.messages()); n != 1 {
		t.Fatalf("%d hand-overs", n)
	}
	if len(h.sent()) != 0 {
		t.Fatal("a reply was sent without a window")
	}
	h.do(func() {
		s := h.state
		if _, ok := s.ContextWindows[testChat]; ok || s.CanSendToChat(testChat, SendFinal) || s.SyncBuf != "next" || s.ContextTokens[testChat] != "old-context" {
			t.Fatal("an untimed message opened a window or changed the token")
		}
		if len(s.Outbox) != 1 {
			t.Fatalf("outbox = %+v", s.Outbox)
		}
		for _, item := range s.Outbox {
			if item.ChatID != testChat || item.Status != OutboxPending {
				t.Fatalf("queued %+v", item)
			}
		}
	})
}

// from: gateway/tests/gateway/reply-outbox.test.ts: persistently rejected reply stops after bounded attempts without blocking the chat
func TestGwgReplyOutboxPersistentRefusalExpires(t *testing.T) {
	h := newHarness(t, harnessOptions{noWindow: true, splitMax: 2})
	page := gwgPaged(h, time.Now().UnixMilli()-1000)
	h.setHooks(func(k *harnessHooks) {
		k.send = func(msg map[string]any) (*http.Response, error) {
			if gwgText(msg) == "cd" {
				return jsonResponse(200, map[string]any{"ret": -2}), nil
			}
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		}
	})
	answer := func(m extproto.Message) string {
		if gwgIs(m, "m1") {
			return "abcdef"
		}
		return "ok"
	}
	gwgAnswerWith(h, answer)
	// The refusal answers channel.send (deferred); the poll's own recovery pass meets it again and fails like the original.
	if _, err := gwgPollErr(h); !IsRejectedSendReceipt(err) {
		t.Fatalf("poll error = %v", err)
	}
	key := h.state.ComputeDeliveryKey(testChat, "m1")
	h.do(func() {
		if h.state.Outbox[key].Status != OutboxPending {
			t.Fatal("a single refusal may be transient and stays retryable")
		}
	})
	for pass := 0; pass < 15; pass++ {
		_ = gwgFlush(h, "")
	}
	gwgEqual(t, "sent", h.sentTexts(), []string{"ab", "cd", "cd", "cd"})
	h.do(func() {
		if h.state.Outbox[key].Status != OutboxExpired || h.state.AccountBudget.Remaining != 6 {
			t.Fatalf("outbox %+v, remaining %d", h.state.Outbox[key], h.state.AccountBudget.Remaining)
		}
	})
	if ev := h.host.events(); len(ev) != 1 || ev[0].State != DeliveryExpired {
		t.Fatalf("events = %+v", ev)
	}
	page.Store(2)
	restarted := gwgReopen(t, h, PolicyOptions{}, 2)
	gwgAnswerWith(restarted, answer)
	gwgPoll(t, restarted)
	gwgEqual(t, "sent", h.sentTexts(), []string{"ab", "cd", "cd", "cd", "ok"})
	restarted.do(func() {
		_, ok := restarted.state.Outbox[restarted.state.ComputeDeliveryKey(testChat, "m2")]
		if restarted.state.Outbox[key].Status != OutboxExpired || ok {
			t.Fatal("a new message replayed the refused reply")
		}
	})
}

// from: gateway/tests/gateway/reply-outbox.test.ts: a restart after the final recorded refusal expires the reply without another send
func TestGwgReplyOutboxRestartAfterFinalRefusal(t *testing.T) {
	h := newHarness(t, harnessOptions{noWindow: true, splitMax: 2})
	key := h.state.ComputeDeliveryKey(testChat, "m1")
	h.do(func() {
		_, err := h.state.RecordInboundContext(key, testChat, i64(nowMs()-1000), true, "context")
		mustNoErr(t, err)
	})
	gwgPutEngine(t, h, testChat, "m1", OutboxPending, "ab")
	h.do(func() {
		_, err := h.state.PrepareReplyDelivery(key, strings.Repeat("a", 64), 1)
		mustNoErr(t, err)
		// A crash after the final refusal was persisted, before the outbox expired.
		for attempt := 0; attempt < MaxRejectedReplyAttempts; attempt++ {
			mustNoErr(t, h.state.BeginReplyChunk(key, 0))
			mustNoErr(t, h.state.RejectReplyChunk(key, 0, false))
		}
	})
	restarted := gwgReopen(t, h, PolicyOptions{}, 2)
	mustNoErr(t, gwgFlush(restarted, ""))
	if len(h.sent()) != 0 {
		t.Fatal("refused reply sent again")
	}
	restarted.do(func() {
		if restarted.state.Outbox[key].Status != OutboxExpired {
			t.Fatal("not expired")
		}
	})
}

// gwgUnavailable is the reply-unavailable fixture: a legacy reference whose
// reply no store holds, and two chats with windows.
func gwgUnavailable(t *testing.T) (*harness, string) {
	h := newHarness(t, harnessOptions{noWindow: true})
	key := h.state.ComputeDeliveryKey(testChat, "old-message")
	h.do(func() {
		for _, chat := range []string{testChat, "other-chat"} {
			_, err := h.state.RecordInboundContext("prior:"+chat, chat, i64(nowMs()-2000), true, "context:"+chat)
			mustNoErr(t, err)
		}
		mustNoErr(t, h.state.PutOutbox(DeliveryOutboxItem{Kind: "reply", DeliveryKey: key, ChatID: testChat, MessageID: "old-message",
			SplitMaxChars: 100, Status: OutboxPending}))
	})
	return h, key
}

// from: gateway/tests/gateway/reply-unavailable.test.ts: HTTP ${status} expires its reply, unblocks same-chat progress and survives restart with acknowledgements
func TestGwgUnavailableReplyExpires(t *testing.T) {
	// The Controller's 403/404/410 are one case now: a reply no store holds.
	h, key := gwgUnavailable(t)
	deliveries := map[string]ReplyDeliveryState{}
	chunks := map[string]int{}
	h.do(func() {
		for _, k := range []string{key, imageReplyPrefix + key} {
			_, err := h.state.PrepareReplyDelivery(k, strings.Repeat("a", 64), 2)
			mustNoErr(t, err)
			mustNoErr(t, h.state.BeginReplyChunk(k, 0))
			mustNoErr(t, h.state.ConfirmReplyChunk(k, 0))
		}
		for _, k := range []string{key, imageReplyPrefix + key} {
			deliveries[k], chunks[k] = h.state.ReplyDeliveries[k], h.state.SentReplyProgress[k]
		}
	})
	h.pushResult("progress", "进度：已完成一半")
	summary, err := h.deliverNotificationsOnce()
	mustNoErr(t, err)
	if summary.Accepted != 1 || summary.Uncertain != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	gwgEqual(t, "sent", h.sentTexts(), []string{"进度：已完成一半"})
	h.do(func() {
		if h.state.Outbox[key].Status != OutboxExpired || h.sent()[0]["to_user_id"] != testChat {
			t.Fatal("unavailable reply not expired")
		}
		for k, d := range deliveries {
			if h.state.ReplyDeliveries[k] != d || h.state.SentReplyProgress[k] != chunks[k] {
				t.Fatalf("%s evidence changed", k)
			}
		}
	})
	mustNoErr(t, gwgFlush(h, ""))
	restarted := gwgReopen(t, h, PolicyOptions{}, 0)
	restarted.do(func() {
		if restarted.state.Outbox[key].Status != OutboxExpired {
			t.Fatal("expiry lost on restart")
		}
		for k, d := range deliveries {
			if restarted.state.ReplyDeliveries[k] != d {
				t.Fatalf("%s evidence changed on restart", k)
			}
		}
	})
	mustNoErr(t, gwgFlush(restarted, ""))
	h.setHooks(func(k *harnessHooks) {
		k.poll = func() (*http.Response, error) {
			return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "next-cursor", "msgs": []any{
				inbound(testChat, "fresh-message", "fresh request", time.Now().UnixMilli()-1000)}}), nil
		}
	})
	gwgAnswerWith(restarted, func(extproto.Message) string { return "fresh reply" })
	if s := gwgPoll(t, restarted); s.ProcessedCount != 1 {
		t.Fatalf("poll = %+v", s)
	}
	restarted.do(func() {
		if restarted.state.SyncBuf != "next-cursor" || restarted.state.Outbox[key].Status != OutboxExpired {
			t.Fatal("cursor or expiry wrong")
		}
	})
	gwgEqual(t, "sent", h.sentTexts(), []string{"进度：已完成一半", "fresh reply"})
	if n := len(restarted.host.messages()); n != 1 {
		t.Fatalf("%d hand-overs", n)
	}
}

// from: gateway/tests/gateway/reply-unavailable.test.ts: unavailable queue head leaves another chat's reply reachable in the next bounded flush
func TestGwgUnavailableHeadLeavesOtherChatReachable(t *testing.T) {
	h, key := gwgUnavailable(t)
	healthy := gwgPutEngine(t, h, "other-chat", "healthy-message", OutboxPending, "healthy reply")
	mustNoErr(t, gwgFlush(h, ""))
	h.do(func() {
		if h.state.Outbox[key].Status != OutboxExpired || h.state.Outbox[healthy].Status != OutboxPending {
			t.Fatal("first flush must only expire the head")
		}
	})
	if len(h.sent()) != 0 {
		t.Fatal("sent in the same pass")
	}
	mustNoErr(t, gwgFlush(h, ""))
	h.do(func() {
		if _, ok := h.state.Outbox[healthy]; ok {
			t.Fatal("healthy reply not delivered")
		}
	})
	if s := h.sent(); len(s) != 1 || s[0]["to_user_id"] != "other-chat" {
		t.Fatalf("sent = %v", s)
	}
}

// from: gateway/tests/gateway/reply-unavailable.test.ts: ${source} uncertain evidence stays quarantined before unavailable lookup
func TestGwgUncertainEvidenceQuarantinedBeforeLookup(t *testing.T) {
	for _, source := range []string{"text", "image"} {
		t.Run(source, func(t *testing.T) {
			h, key := gwgUnavailable(t)
			evidenceKey := key
			if source == "image" {
				evidenceKey = imageReplyPrefix + key
			}
			var evidence ReplyDeliveryState
			h.do(func() {
				_, err := h.state.PrepareReplyDelivery(evidenceKey, strings.Repeat("a", 64), 2)
				mustNoErr(t, err)
				mustNoErr(t, h.state.BeginReplyChunk(evidenceKey, 0))
				evidence = h.state.ReplyDeliveries[evidenceKey]
			})
			mustNoErr(t, gwgFlush(h, ""))
			h.do(func() {
				if h.state.Outbox[key].Status != OutboxUncertain || h.state.ReplyDeliveries[evidenceKey] != evidence {
					t.Fatal("uncertain evidence not quarantined")
				}
			})
			restarted := gwgReopen(t, h, PolicyOptions{}, 0)
			mustNoErr(t, gwgFlush(restarted, ""))
			restarted.do(func() {
				if restarted.state.Outbox[key].Status != OutboxUncertain || restarted.state.ReplyDeliveries[evidenceKey] != evidence {
					t.Fatal("quarantine lost on restart")
				}
			})
			if len(h.sent()) != 0 {
				t.Fatal("uncertain reply sent")
			}
		})
	}
}
