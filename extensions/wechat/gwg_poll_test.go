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

// from: gateway/tests/gateway/poll.test.ts: pollOnce deduplicates within and across pages while isolating chats${blockedEndpoint ?
func TestGwgPollDeduplicatesAndIsolatesChats(t *testing.T) {
	for _, blocked := range []string{"", "getconfig", "sendtyping"} {
		t.Run("blocked="+blocked, func(t *testing.T) {
			created := time.Now().UnixMilli() - 1000
			release := make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			var mu sync.Mutex
			var configs, sent []string
			page := func() []any {
				a := inbound(testChat, "same-client-id", "status on macair", created)
				// Only the bound person is served: another chat with the same client id is isolated and skipped.
				b := inbound("chat-b", "same-client-id", "hello from b", created)
				return []any{a, a, b, b}
			}
			h := gwgCustom(t, harnessOptions{noWindow: true}, func(req *http.Request, body []byte) (*http.Response, error) {
				name := endpoint(req)
				if name == blocked {
					<-release
				}
				switch name {
				case "getupdates":
					return jsonResponse(200, map[string]any{"ret": 0, "errcode": 0, "get_updates_buf": "cursor-after-batch-1", "msgs": page()}), nil
				case "getconfig":
					mu.Lock()
					configs = append(configs, decodeBody(t, body)["ilink_user_id"].(string))
					mu.Unlock()
					return jsonResponse(200, map[string]any{"ret": 0, "typing_ticket": "ticket"}), nil
				case "sendtyping":
					return jsonResponse(200, map[string]any{"ret": 0}), nil
				case "sendmessage":
					mu.Lock()
					sent = append(sent, gwgText(decodeBody(t, body)["msg"].(map[string]any)))
					mu.Unlock()
					return jsonResponse(200, map[string]any{"ret": 0, "errcode": 0}), nil
				}
				return nil, errors.New("unexpected URL")
			})
			gwgAnswerWith(h, func(m extproto.Message) string { return "**Reply** to " + m.ChatID })
			first := gwgPoll(t, h)
			// Queries reach Collie and the body is delivered while typing may still be blocked.
			if first.PolledCount != 4 || first.ProcessedCount != 1 || first.SkippedCount != 3 || first.NextSyncBuf != "cursor-after-batch-1" {
				t.Fatalf("first poll = %+v", first)
			}
			gwgEqual(t, "received", gwgReceivedChats(h), []string{testChat})
			mu.Lock()
			gwgEqual(t, "sent", sent, []string{"**Reply** to chat"})
			mu.Unlock()
			once.Do(func() { close(release) })
			h.do(func() {
				if h.state.SyncBuf != "cursor-after-batch-1" || !h.state.IsMessageProcessed(h.state.ComputeDeliveryKey("chat-b", "same-client-id")) {
					t.Fatal("cursor or isolated key not recorded")
				}
			})
			second := gwgPoll(t, h)
			mu.Lock()
			defer mu.Unlock()
			if second.PolledCount != 0 || len(h.host.messages()) != 1 || len(sent) != 1 {
				t.Fatalf("second poll = %+v, %d received, %d sent", second, len(h.host.messages()), len(sent))
			}
			for _, c := range configs {
				if c != testChat {
					t.Fatalf("typing for a chat that is not served: %v", configs)
				}
			}
		})
	}
}

// from: gateway/tests/gateway/poll.test.ts: Poison batch: CHAT_NOT_ALLOWED skips unallowed chat and lets cursor advance for valid chat
func TestGwgPollPoisonBatch(t *testing.T) {
	h := newHarness(t, harnessOptions{noWindow: true})
	created := time.Now().UnixMilli() - 1000
	stranger := inbound("unauthorized-stranger", "poison-msg-1", "spam", created)
	delete(stranger, "context_token")
	h.setHooks(func(k *harnessHooks) {
		k.poll = func() (*http.Response, error) {
			return jsonResponse(200, map[string]any{"ret": 0, "errcode": 0, "get_updates_buf": "cursor-past-poison",
				"msgs": []any{stranger, inbound(testChat, "valid-msg-2", "legit task", created)}}), nil
		}
	})
	gwgAnswerWith(h, func(extproto.Message) string { return "ok" })
	s := gwgPoll(t, h)
	if s.PolledCount != 2 || s.ProcessedCount != 1 || s.SkippedCount != 1 || s.NextSyncBuf != "cursor-past-poison" {
		t.Fatalf("summary = %+v", s)
	}
	gwgEqual(t, "received", gwgReceivedChats(h), []string{testChat})
	time.Sleep(50 * time.Millisecond) // typing is best effort in the background
	h.mu.Lock()
	typings := append([]map[string]any(nil), h.typings...)
	h.mu.Unlock()
	if len(typings) == 0 {
		t.Fatal("no typing for the served chat")
	}
	for _, ty := range typings {
		if ty["ilink_user_id"] != testChat {
			t.Fatalf("typing to %v", ty["ilink_user_id"])
		}
	}
}

// from: gateway/tests/gateway/poll.test.ts: ambiguous multipart failure is isolated durably while later chats and the cursor advance
func TestGwgPollAmbiguousMultipartIsolated(t *testing.T) {
	h := newHarness(t, harnessOptions{noWindow: true, splitMax: 6})
	created := time.Now().UnixMilli() - 1000
	var mu sync.Mutex
	var attempted []string
	failOnce := true
	h.setHooks(func(k *harnessHooks) {
		k.poll = func() (*http.Response, error) {
			return jsonResponse(200, map[string]any{"ret": 0, "errcode": 0, "get_updates_buf": "cursor-next", "msgs": []any{
				inbound(testChat, "msg-multipart-1", "give long output", created), inbound(testChat, "later-message", "later request", created+1)}}), nil
		}
		k.send = func(msg map[string]any) (*http.Response, error) {
			text := msg["item_list"].([]any)[0].(map[string]any)["text_item"].(map[string]any)["text"].(string)
			mu.Lock()
			defer mu.Unlock()
			attempted = append(attempted, text)
			if text == "Part2\n" && failOnce {
				failOnce = false
				return nil, errors.New("simulated network drop on chunk 2")
			}
			return jsonResponse(200, map[string]any{"ret": 0, "errcode": 0}), nil
		}
	})
	// The later request now comes from the bound person too: only one chat is served.
	gwgAnswerWith(h, func(m extproto.Message) string {
		if strings.Contains(m.Text, "later") {
			return "later"
		}
		return "Part1\nPart2\nPart3"
	})
	first := gwgPoll(t, h)
	if first.SkippedCount != 0 || first.ProcessedCount != 2 {
		t.Fatalf("first = %+v", first)
	}
	restarted := gwgReopen(t, h, PolicyOptions{}, 6)
	if s := gwgPoll(t, restarted); s.PolledCount != 0 {
		t.Fatalf("after restart = %+v", s)
	}
	mu.Lock()
	gwgEqual(t, "attempted", attempted, []string{"Part1\n", "Part2\n", "later"})
	mu.Unlock()
	restarted.do(func() {
		s := restarted.state
		key := s.ComputeDeliveryKey(testChat, "msg-multipart-1")
		if !s.BaselineEstablished || s.SyncBuf != "cursor-next" || !s.IsMessageProcessed(key) || !s.HasUncertainReply(key) ||
			s.Outbox[key].Status != OutboxUncertain || s.ReplyDeliveries[key].AcknowledgedChunks != 1 {
			t.Fatalf("uncertain reply not isolated: outbox %+v, plan %+v", s.Outbox[key], s.ReplyDeliveries[key])
		}
	})
	if n := len(h.host.messages()); n != 2 {
		t.Fatalf("%d messages handed over, an uncertain delivery must never replay either", n)
	}
}
