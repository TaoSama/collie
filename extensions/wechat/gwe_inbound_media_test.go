package wechat

import (
	"crypto/aes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

const gweMediaKey = "2b7e151628aed2a6abf7158809cf4f3c"

var gweMediaPNG = append([]byte{137, 80, 78, 71, 13, 10, 26, 10}, []byte("pixels")...)

func gweEncrypted() []byte {
	key, _ := hex.DecodeString(gweMediaKey)
	block, _ := aes.NewCipher(key)
	pad := aesBlockBytes - len(gweMediaPNG)%aesBlockBytes
	plain := append(append([]byte(nil), gweMediaPNG...), []byte(strings.Repeat(string(rune(pad)), pad))...)
	out := make([]byte, len(plain))
	for i := 0; i < len(plain); i += aesBlockBytes {
		block.Encrypt(out[i:i+aesBlockBytes], plain[i:i+aesBlockBytes])
	}
	return out
}

var gweImageItem = map[string]any{"type": 2, "image_item": map[string]any{"aeskey": gweMediaKey, "media": map[string]any{"encrypt_query_param": "img"}}}

func gweMediaMessage(id string, items ...any) map[string]any {
	return map[string]any{"message_type": 1, "from_user_id": testChat, "client_id": id, "context_token": "ctx-" + id,
		"create_time_ms": nowMs() - 1000, "item_list": items}
}

// gweMediaHarness polls msg once and answers CDN downloads with cdn.
func gweMediaHarness(t *testing.T, msg map[string]any, cdn func() (*http.Response, error)) *harness {
	h := newHarness(t, harnessOptions{})
	var once sync.Once
	h.setHooks(func(k *harnessHooks) {
		k.poll = func() (*http.Response, error) {
			msgs := []any{}
			once.Do(func() { msgs = append(msgs, msg) })
			return jsonResponse(200, map[string]any{"ret": 0, "msgs": msgs, "get_updates_buf": "next"}), nil
		}
		k.cdn = func(*http.Request, []byte) (*http.Response, error) { return cdn() }
	})
	return h
}

// from: gateway/tests/gateway/inbound-media.test.ts: an image alone is stored as chat context without any WeChat reply
func TestGweImageAloneHeldAsContext(t *testing.T) {
	h := gweMediaHarness(t, gweMediaMessage("img", gweImageItem), func() (*http.Response, error) {
		return bytesResponse(200, gweEncrypted(), nil), nil
	})
	summary, err := gwePoll(t, h.gateway)
	mustNoErr(t, err)
	if summary.ProcessedCount != 1 || len(h.sent()) != 0 || len(h.host.messages()) != 0 {
		t.Fatalf("poll %+v, %d sends, %d hand-overs", summary, len(h.sent()), len(h.host.messages()))
	}
	h.do(func() {
		held := h.gateway.stash.chats[testChat]
		if len(held) != 1 || held[0].Kind != KindImage || held[0].Mime != "image/png" {
			t.Fatalf("held %+v", held)
		}
		if !h.state.IsMessageProcessed(h.state.ComputeDeliveryKey(testChat, "img")) {
			t.Fatal("message not processed")
		}
	})
}

// from: gateway/tests/gateway/inbound-media.test.ts: a failed image download falls back to the fixed notice
func TestGweFailedImageDownloadFixedNotice(t *testing.T) {
	h := gweMediaHarness(t, gweMediaMessage("img", gweImageItem), func() (*http.Response, error) {
		return rawResponse(404, "gone"), nil
	})
	_, err := gwePoll(t, h.gateway)
	mustNoErr(t, err)
	h.do(func() {
		if len(h.gateway.stash.chats[testChat]) != 0 {
			t.Fatal("failed image stored")
		}
	})
	if texts := h.sentTexts(); len(texts) != 1 || texts[0] != UnsupportedMediaReply {
		t.Fatalf("texts %q", texts)
	}
}

// from: gateway/tests/gateway/inbound-media.test.ts: text with an image is forwarded after storing it, and a failed image is flagged to the leader
func TestGweTextWithImageForwarded(t *testing.T) {
	text := map[string]any{"type": 1, "text_item": map[string]any{"text": "帮我看看这张图"}}
	ok := gweMediaHarness(t, gweMediaMessage("both", gweImageItem, text), func() (*http.Response, error) {
		return bytesResponse(200, gweEncrypted(), nil), nil
	})
	_, err := gwePoll(t, ok.gateway)
	mustNoErr(t, err)
	msgs := ok.host.messages()
	if len(msgs) != 1 || msgs[0].Text != "帮我看看这张图" || len(msgs[0].Images) != 1 || msgs[0].Images[0].Mime != "image/png" ||
		msgs[0].Images[0].Data != base64.StdEncoding.EncodeToString(gweMediaPNG) {
		t.Fatalf("handed over %+v", msgs)
	}
	_, err = ok.send(extproto.Send{Ref: "reply", Text: "看到了", Kind: extproto.KindAnswer, ReplyTo: msgs[0].ID})
	mustNoErr(t, err)
	if texts := ok.sentTexts(); len(texts) != 1 || texts[0] != "看到了" {
		t.Fatalf("texts %q", texts)
	}
	failed := gweMediaHarness(t, gweMediaMessage("both", gweImageItem, text), func() (*http.Response, error) {
		return nil, errors.New("network")
	})
	_, err = gwePoll(t, failed.gateway)
	mustNoErr(t, err)
	if msgs := failed.host.messages(); len(msgs) != 1 || msgs[0].Text != "帮我看看这张图"+MediaFailedNote || len(msgs[0].Images) != 0 {
		t.Fatalf("handed over %+v", msgs)
	}
}

// from: gateway/tests/gateway/inbound-steering.test.ts: polling deduplicates repeated root and supplemental messages while steering a running query
func TestGwePollingDedupesAndSteers(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	started, complete := make(chan struct{}), make(chan struct{})
	var polls int
	var mu sync.Mutex
	msg := func(id, text string) map[string]any {
		m := inbound(testChat, id, text, nowMs())
		m["context_token"] = "ctx-" + id
		return m
	}
	h.setHooks(func(k *harnessHooks) {
		k.poll = func() (*http.Response, error) {
			mu.Lock()
			polls++
			n := polls
			mu.Unlock()
			switch n {
			case 1:
				one := msg("one", "查一下部署情况")
				return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "one", "msgs": []any{one, one}}), nil
			case 2:
				<-started
				two, three := msg("two", "只看今天"), msg("three", "按服务分组")
				return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "two", "msgs": []any{two, two, three, three}}), nil
			}
			time.Sleep(time.Millisecond)
			return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "done", "msgs": []any{}}), nil
		}
	})
	var root string
	var budgets []int
	var appended []string
	progress := func(text string) extproto.SendResult {
		res, err := h.send(extproto.Send{Ref: "progress-" + newUUID(), Text: text, Kind: extproto.KindProgress, ReplyTo: root})
		if err != nil {
			t.Errorf("progress %q: %v", text, err)
		}
		return res
	}
	remaining := func() int {
		var q *QueryProgress
		h.do(func() { q = h.gateway.progress[jsonText([]string{testChat, root})] })
		var n int
		h.do(func() { n = q.Remaining() })
		return n
	}
	h.host.receive = func(m extproto.Message) error {
		if root == "" {
			// The root request: it reports progress, then works until its supplements arrived.
			root = m.ID
			progress("已找到相关部署记录，正在核对时间。")
			budgets = append(budgets, remaining())
			close(started)
			<-complete
			return nil
		}
		// A supplement steered into the running request.
		appended = append(appended, m.Text)
		progress("已结合补充要求：" + m.Text)
		budgets = append(budgets, remaining())
		if len(appended) == 2 {
			_, err := h.send(extproto.Send{Ref: "progress-extra", Text: "这一条不应发送", Kind: extproto.KindProgress, ReplyTo: root})
			if !extproto.IsCode(err, extproto.ErrCannotSend) || remaining() != 0 {
				t.Errorf("fourth progress: %v", err)
			}
			close(complete)
		}
		return nil
	}
	done := make(chan error, 1)
	var failures []error
	go func() {
		done <- h.gateway.RunLoop(1, func(err error) { failures = append(failures, err) }, nil)
	}()
	<-complete
	eventually(t, "all handed over", func() bool { return len(h.host.messages()) == 3 })
	_, err := h.send(extproto.Send{Ref: "final", Text: "今天，按服务分组的最终结果", Kind: extproto.KindAnswer, ReplyTo: root})
	mustNoErr(t, err)
	h.gateway.Stop()
	mustNoErr(t, <-done)
	if len(failures) != 0 || polls < 2 {
		t.Fatalf("failures %v polls %d", failures, polls)
	}
	if strings.Join(appended, ",") != "只看今天,按服务分组" || len(budgets) != 3 || budgets[0] != 2 || budgets[1] != 1 || budgets[2] != 0 {
		t.Fatalf("appended %v budgets %v", appended, budgets)
	}
	texts := h.sentTexts()
	if len(texts) != 4 || texts[3] != "今天，按服务分组的最终结果" {
		t.Fatalf("texts %q", texts)
	}
	h.do(func() {
		list, err := h.gateway.inbox.List()
		mustNoErr(t, err)
		// The harness's own window key is not a message; the three polled ones are.
		if len(h.state.ProcessedMessageKeys) != 3 || len(list) != 0 {
			t.Fatalf("processed %d inbox %d", len(h.state.ProcessedMessageKeys), len(list))
		}
	})
}
