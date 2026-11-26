package wechat

import (
	"bytes"
	"crypto/aes"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

// from: gateway/tests/gateway/leader-engine.test.ts: Codex reply engine ${allowed ? 'handles duplicate page messages once' : 'skips duplicate unauthorized messages'}
func TestGwgLeaderDuplicatePageMessages(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		t.Run(map[bool]string{true: "allowed", false: "unauthorized"}[allowed], func(t *testing.T) {
			// Collie is the engine now; an unauthorized sender is anyone but the bound person.
			sender := testChat
			if !allowed {
				sender = "stranger"
			}
			h := newHarness(t, harnessOptions{noWindow: true})
			msg := inbound(sender, "message", "记住我喜欢简短回复", time.Now().UnixMilli()-1000)
			h.setHooks(func(k *harnessHooks) {
				k.poll = func() (*http.Response, error) {
					return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "next", "msgs": []any{msg, msg}}), nil
				}
			})
			gwgAnswerWith(h, func(m extproto.Message) string {
				if m.Text != "记住我喜欢简短回复" || m.ChatID != testChat || m.Owner == nil || !*m.Owner {
					t.Errorf("handed over %+v", m)
				}
				return "已经记下简短回复的偏好。"
			})
			first := gwgPoll(t, h)
			var want []string
			processed, skipped := 0, 2
			if allowed {
				want, processed, skipped = []string{"已经记下简短回复的偏好。"}, 1, 1
			}
			gwgEqual(t, "replies", h.sentTexts(), want)
			if first.PolledCount != 2 || first.ProcessedCount != processed || first.SkippedCount != skipped || first.NextSyncBuf != "next" {
				t.Fatalf("first = %+v", first)
			}
			gwgPoll(t, h)
			if n := len(h.host.messages()); n != processed {
				t.Fatalf("%d hand-overs, want %d", n, processed)
			}
		})
	}
}

var gwgPNG = append([]byte{137, 80, 78, 71, 13, 10, 26, 10}, []byte("pixels")...)

const gwgKey = "2b7e151628aed2a6abf7158809cf4f3c"

func gwgEncrypted() []byte {
	key, _ := hex.DecodeString(gwgKey)
	return gwgEncrypt(gwgPNG, key)
}

func gwgEncrypt(plain, key []byte) []byte {
	block, _ := aes.NewCipher(key)
	pad := 16 - len(plain)%16
	padded := append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(padded))
	for i := 0; i < len(padded); i += 16 {
		block.Encrypt(out[i:i+16], padded[i:i+16])
	}
	return out
}

func gwgMessage(id string, items ...any) map[string]any {
	return map[string]any{"message_type": 1, "from_user_id": testChat, "client_id": id, "context_token": "ctx-" + id,
		"create_time_ms": time.Now().UnixMilli(), "item_list": items}
}

var gwgImageItem = map[string]any{"type": 2, "image_item": map[string]any{"aeskey": gwgKey, "media": map[string]any{"encrypt_query_param": "img"}}}

func gwgTextItem(text string) map[string]any {
	return map[string]any{"type": 1, "text_item": map[string]any{"text": text}}
}

// gwgImageRun runs the real poll loop; the second page arrives only after
// Collie received the first.
func gwgImageRun(t *testing.T, pages [2][]any, onReceive func(h *harness, m extproto.Message)) (*harness, []error) {
	h := newHarness(t, harnessOptions{noWindow: true})
	started := make(chan struct{})
	var startOnce sync.Once
	polls := 0
	h.setHooks(func(k *harnessHooks) {
		k.poll = func() (*http.Response, error) {
			polls++
			switch polls {
			case 1:
				return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "one", "msgs": pages[0]}), nil
			case 2:
				select {
				case <-started:
				case <-time.After(time.Second):
				}
				return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "two", "msgs": pages[1]}), nil
			}
			time.Sleep(time.Millisecond)
			return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "done", "msgs": []any{}}), nil
		}
		k.cdn = func(req *http.Request, _ []byte) (*http.Response, error) {
			return bytesResponse(200, gwgEncrypted(), nil), nil
		}
		k.send = func(map[string]any) (*http.Response, error) {
			go h.gateway.Stop()
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		}
	})
	h.host.mu.Lock()
	h.host.receive = func(m extproto.Message) error {
		startOnce.Do(func() { close(started) })
		onReceive(h, m)
		return nil
	}
	h.host.mu.Unlock()
	var failures []error
	var mu sync.Mutex
	done := make(chan error, 1)
	go func() {
		done <- h.gateway.RunLoop(1, func(err error) {
			mu.Lock()
			failures = append(failures, err)
			mu.Unlock()
			go h.gateway.Stop()
		}, nil)
	}()
	select {
	case err := <-done:
		mustNoErr(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("run loop did not stop")
	}
	mu.Lock()
	defer mu.Unlock()
	return h, failures
}

func gwgImages(m extproto.Message) []bool {
	var out []bool
	for _, img := range m.Images {
		data, _ := base64.StdEncoding.DecodeString(img.Data)
		out = append(out, img.Mime == "image/png" && bytes.Equal(data, gwgPNG))
	}
	return out
}

// from: gateway/tests/gateway/leader-images.test.ts: an image sent right after its question joins the running leader turn as pixels
func TestGwgImageAfterQuestionJoinsRunningRequest(t *testing.T) {
	var askID string
	h, failures := gwgImageRun(t, [2][]any{{gwgMessage("ask", gwgTextItem("帮我看下这张截图"))}, {gwgMessage("picture", gwgImageItem)}},
		func(h *harness, m extproto.Message) {
			if m.Text != "" {
				askID = m.ID // the request is still being answered when the picture arrives
				return
			}
			_, _ = h.send(extproto.Send{Ref: "answer", Text: "截图里是部署失败的报错", Kind: extproto.KindAnswer, ReplyTo: askID})
		})
	if len(failures) > 0 {
		t.Fatalf("failures: %v", failures)
	}
	got := h.host.messages()
	if len(got) != 2 || got[0].Text != "帮我看下这张截图" || got[1].Text != "" {
		t.Fatalf("handed over %d messages: %+v", len(got), got)
	}
	gwgEqual(t, "pixels", gwgImages(got[1]), []bool{true})
	gwgEqual(t, "sent", h.sentTexts(), []string{"截图里是部署失败的报错"})
	h.do(func() {
		if n := len(h.state.ProcessedMessageKeys); n != 2 {
			t.Fatalf("%d processed keys", n)
		}
	})
}

// from: gateway/tests/gateway/leader-images.test.ts: an image sent before its question is staged for the leader turn that answers it
func TestGwgImageBeforeQuestionJoinsItsRequest(t *testing.T) {
	h, failures := gwgImageRun(t, [2][]any{{gwgMessage("picture", gwgImageItem)}, {gwgMessage("ask", gwgTextItem("图里是什么？"))}},
		func(h *harness, m extproto.Message) {
			_, _ = h.send(extproto.Send{Ref: "answer", Text: "一张截图", Kind: extproto.KindAnswer, ReplyTo: m.ID})
		})
	if len(failures) > 0 {
		t.Fatalf("failures: %v", failures)
	}
	got := h.host.messages()
	// The picture alone is held; it reaches Collie with the question that follows.
	if len(got) != 1 || got[0].Text != "图里是什么？" {
		t.Fatalf("handed over %+v", got)
	}
	gwgEqual(t, "pixels", gwgImages(got[0]), []bool{true})
	gwgEqual(t, "sent", h.sentTexts(), []string{"一张截图"})
}
