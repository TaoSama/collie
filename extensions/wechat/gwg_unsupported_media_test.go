package wechat

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

// gwgImageOnly is an image item without a usable media reference: content
// that cannot be passed on.
func gwgImageOnly(id string, created *int64) map[string]any {
	m := map[string]any{"message_type": 1, "from_user_id": testChat, "client_id": id, "context_token": "ctx-" + id,
		"item_list": []any{map[string]any{"type": 2, "image_item": map[string]any{"mid_size": 16}}}}
	if created != nil {
		m["create_time_ms"] = *created
	}
	return m
}

func gwgPagesHarness(t *testing.T, o harnessOptions) (*harness, *gwgPages) {
	h := newHarness(t, o)
	p := &gwgPages{}
	h.setHooks(func(k *harnessHooks) { k.poll = p.poll })
	return h, p
}

// from: gateway/tests/gateway/unsupported-media.test.ts: image-only inbound gets one fixed notice without a Controller turn
func TestGwgUnsupportedMediaFixedNotice(t *testing.T) {
	h, pages := gwgPagesHarness(t, harnessOptions{noWindow: true})
	created := i64(time.Now().UnixMilli() - 1000)
	pages.set([]any{gwgImageOnly("img", created), gwgImageOnly("img", created)}, []any{gwgImageOnly("img", created)})
	if s := gwgPoll(t, h); s.ProcessedCount != 1 {
		t.Fatalf("first = %+v", s)
	}
	if s := gwgPoll(t, h); s.ProcessedCount != 0 {
		t.Fatalf("second = %+v", s)
	}
	gwgEqual(t, "sent", h.sentTexts(), []string{UnsupportedMediaReply})
	if n := len(h.host.messages()); n != 0 {
		t.Fatalf("%d messages handed to Collie", n)
	}
	h.do(func() {
		key := h.state.ComputeDeliveryKey(testChat, "img")
		if _, ok := h.state.Outbox[key]; !h.state.IsMessageProcessed(key) || ok || h.state.ContextTokens[testChat] != "ctx-img" {
			t.Fatal("notice not finished, or the real inbound did not refresh the context")
		}
	})
}

// from: gateway/tests/gateway/unsupported-media.test.ts: a deferred notice is recovered once after restart and never re-sent for a replay
func TestGwgUnsupportedMediaDeferredNoticeRecovered(t *testing.T) {
	h, pages := gwgPagesHarness(t, harnessOptions{noWindow: true})
	// No timestamp opens no window, so the notice is deferred (the original mocked a deferral).
	pages.set([]any{gwgImageOnly("img", nil)})
	gwgPoll(t, h)
	var key string
	h.do(func() {
		key = h.state.ComputeDeliveryKey(testChat, "img")
		if item := h.state.Outbox[key]; item.Source != SourceUnsupportedMedia || item.Status != OutboxPending {
			t.Fatalf("outbox = %+v", item)
		}
	})
	if len(h.sent()) != 0 {
		t.Fatal("deferred notice was sent")
	}
	restarted := gwgReopen(t, h, PolicyOptions{}, 0)
	restarted.do(func() {
		_, err := restarted.state.RecordInboundContext("window", testChat, i64(nowMs()-500), true, "ctx-img")
		mustNoErr(t, err)
	})
	mustNoErr(t, gwgFlush(restarted, ""))
	gwgEqual(t, "sent", h.sentTexts(), []string{UnsupportedMediaReply})
	restarted.do(func() {
		if _, ok := restarted.state.Outbox[key]; ok {
			t.Fatal("outbox kept after delivery")
		}
	})
	pages.set([]any{gwgImageOnly("img", nil)})
	gwgPoll(t, restarted)
	mustNoErr(t, gwgFlush(restarted, ""))
	gwgEqual(t, "sent", h.sentTexts(), []string{UnsupportedMediaReply})
	if len(h.host.messages()) != 0 {
		t.Fatal("notice content was handed to Collie")
	}
}

// from: gateway/tests/gateway/unsupported-media.test.ts: a notice whose send completed before a crash is not delivered again
func TestGwgUnsupportedMediaCrashAfterSend(t *testing.T) {
	h, pages := gwgPagesHarness(t, harnessOptions{noWindow: true})
	pages.set([]any{gwgImageOnly("img", i64(time.Now().UnixMilli()-1000))})
	key := h.state.ComputeDeliveryKey(testChat, "img")
	h.do(func() {
		h.state.failSave = func() error {
			// The cleanup checkpoint: the reference is gone and its reply complete.
			if _, ok := h.state.Outbox[key]; !ok && h.state.ReplyDeliveries[key].TotalChunks == 1 && h.state.ReplyDeliveries[key].AcknowledgedChunks == 1 {
				return errors.New("injected crash before outbox cleanup")
			}
			return nil
		}
	})
	if _, err := gwgPollErr(h); err == nil || err.Error() != "injected crash before outbox cleanup" {
		t.Fatalf("poll error = %v", err)
	}
	gwgEqual(t, "sent", h.sentTexts(), []string{UnsupportedMediaReply})
	restarted := gwgReopen(t, h, PolicyOptions{}, 0)
	mustNoErr(t, gwgFlush(restarted, ""))
	restarted.do(func() {
		if _, ok := restarted.state.Outbox[key]; ok {
			t.Fatal("outbox kept")
		}
	})
	gwgEqual(t, "sent", h.sentTexts(), []string{UnsupportedMediaReply})
}

// from: gateway/tests/gateway/unsupported-media.test.ts: unauthorized chats and baseline history never receive the notice
func TestGwgUnsupportedMediaUnauthorizedAndBaseline(t *testing.T) {
	denied, pages := gwgPagesHarness(t, harnessOptions{noWindow: true, account: &Account{Token: testToken, AccountID: testAccount, UserID: "someone-else"}})
	pages.set([]any{gwgImageOnly("img", i64(time.Now().UnixMilli()-1000))})
	if s := gwgPoll(t, denied); s.SkippedCount != 1 {
		t.Fatalf("denied = %+v", s)
	}
	if len(denied.sent()) != 0 {
		t.Fatal("notice sent to an unauthorized chat")
	}
	denied.do(func() {
		if !denied.state.IsMessageProcessed(denied.state.ComputeDeliveryKey(testChat, "img")) {
			t.Fatal("unauthorized message not marked processed")
		}
	})
	history, hp := gwgPagesHarness(t, harnessOptions{noWindow: true, noBase: true})
	hp.set([]any{gwgImageOnly("old", i64(time.Now().UnixMilli()-1000))}, []any{})
	history.do(func() {
		_, err := history.gateway.establishBaseline(context.Background(), 0)
		mustNoErr(t, err)
	})
	hp.set([]any{gwgImageOnly("old", i64(time.Now().UnixMilli()-1000))})
	gwgPoll(t, history)
	if len(history.sent()) != 0 {
		t.Fatal("baseline history got a notice")
	}
}

// from: gateway/tests/gateway/unsupported-media.test.ts: unsupported content arriving during a running turn is not steered and is answered after it
func TestGwgUnsupportedMediaNotSteered(t *testing.T) {
	h := newHarness(t, harnessOptions{noWindow: true})
	started := make(chan struct{})
	var startOnce sync.Once
	polls := 0
	var sendMu sync.Mutex
	sends := 0
	h.setHooks(func(k *harnessHooks) {
		k.poll = func() (*http.Response, error) {
			polls++
			now := time.Now().UnixMilli() - 1000
			switch polls {
			case 1:
				return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "one", "msgs": []any{inbound(testChat, "text", "查一下部署", now)}}), nil
			case 2:
				<-started
				return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "two", "msgs": []any{gwgImageOnly("img", &now)}}), nil
			}
			time.Sleep(time.Millisecond)
			return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "done", "msgs": []any{}}), nil
		}
		k.send = func(map[string]any) (*http.Response, error) {
			sendMu.Lock()
			sends++
			n := sends
			sendMu.Unlock()
			if n == 1 {
				// The notice went out while Collie is still answering; now the answer arrives.
				go func() {
					_, _ = h.send(extproto.Send{Ref: "answer", Text: "部署正常", Kind: extproto.KindAnswer})
				}()
			}
			if n == 2 {
				go h.gateway.Stop()
			}
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		}
	})
	h.host.mu.Lock()
	h.host.receive = func(extproto.Message) error { startOnce.Do(func() { close(started) }); return nil }
	h.host.mu.Unlock()
	var failures []error
	var fmu sync.Mutex
	done := make(chan error, 1)
	go func() {
		done <- h.gateway.RunLoop(1, func(err error) { fmu.Lock(); failures = append(failures, err); fmu.Unlock(); go h.gateway.Stop() }, nil)
	}()
	select {
	case err := <-done:
		mustNoErr(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("run loop did not stop")
	}
	if len(failures) > 0 {
		t.Fatalf("failures: %v", failures)
	}
	// Not steered: Collie only has the text. The notice no longer waits behind the turn, since
	// the turn runs at Collie; it goes out first and the answer follows.
	if got := h.host.messages(); len(got) != 1 || got[0].Text != "查一下部署" {
		t.Fatalf("handed over %+v", got)
	}
	gwgEqual(t, "sent", h.sentTexts(), []string{UnsupportedMediaReply, "部署正常"})
	h.do(func() {
		if !h.state.IsMessageProcessed(h.state.ComputeDeliveryKey(testChat, "img")) {
			t.Fatal("image not processed")
		}
	})
	inbox, err := NewFileInboundInbox(filepath.Join(h.dir, inboxFileName), h.state.LoginFingerprint, 1000)
	mustNoErr(t, err)
	if list, err := inbox.List(); err != nil || len(list) != 0 {
		t.Fatalf("inbox = %v, %v", list, err)
	}
}

// from: gateway/tests/gateway/steered-image.test.ts: steered image downloads use the follow-up identity while final delivery belongs to the root query
func TestGwgSteeredImageBelongsToRootReply(t *testing.T) {
	h := newHarness(t, harnessOptions{noWindow: true})
	h.setHooks(func(k *harnessHooks) {
		k.poll = func() (*http.Response, error) {
			return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "next", "msgs": []any{
				inbound(testChat, "root-message", "先查看状态", time.Now().UnixMilli())}}), nil
		}
	})
	const final = "已结合补充要求取得截图。"
	// The picture comes frozen with Collie's answer to the root message, whatever message produced it.
	h.host.mu.Lock()
	h.host.receive = func(m extproto.Message) error {
		_, err := h.gateway.HandleSend(context.Background(), extproto.Send{Ref: m.ID, Chat: m.ChatID, Text: final, Kind: extproto.KindAnswer,
			ReplyTo: m.ID, Images: []extproto.Blob{{Name: "follow-up-screenshot.png", Mime: "image/png", Data: base64.StdEncoding.EncodeToString(gwgPNG)}}})
		return err
	}
	h.host.mu.Unlock()
	if s := gwgPoll(t, h); s.ProcessedCount != 1 {
		t.Fatalf("summary = %+v", s)
	}
	var types []float64
	for _, msg := range h.sent() {
		types = append(types, msg["item_list"].([]any)[0].(map[string]any)["type"].(float64))
	}
	gwgEqual(t, "sent types", types, []float64{2, 1})
	if texts := h.sentTexts(); len(texts) != 1 || texts[0] != final {
		t.Fatalf("texts = %v", texts)
	}
	rootKey := h.state.ComputeDeliveryKey(testChat, "root-message")
	events := h.host.events()
	if len(events) != 1 || events[0].Ref != rootKey || events[0].State != DeliverySent {
		t.Fatalf("events = %+v", events)
	}
	h.do(func() {
		s := h.state
		if _, ok := s.Outbox[rootKey]; !s.IsMessageProcessed(rootKey) || s.ReplyDeliveries[imageReplyPrefix+rootKey].AcknowledgedChunks != 1 ||
			s.ReplyDeliveries[rootKey].AcknowledgedChunks != 1 || ok {
			t.Fatal("root reply not delivered as one reply with its picture")
		}
	})
}
