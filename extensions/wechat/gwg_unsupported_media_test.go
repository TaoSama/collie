package wechat

import (
	"context"
	"encoding/base64"
	"net/http"
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

// Content that can't be passed on goes to Collie, named in Unsupported,
// once: the extension sends nothing of its own.
func TestGwgUnsupportedContentGoesToCollie(t *testing.T) {
	h, pages := gwgPagesHarness(t, harnessOptions{noWindow: true})
	created := i64(time.Now().UnixMilli() - 1000)
	pages.set([]any{gwgImageOnly("img", created), gwgImageOnly("img", created)}, []any{gwgImageOnly("img", created)})
	if s := gwgPoll(t, h); s.ProcessedCount != 1 {
		t.Fatalf("first = %+v", s)
	}
	if s := gwgPoll(t, h); s.ProcessedCount != 0 {
		t.Fatalf("second = %+v", s)
	}
	if len(h.sent()) != 0 {
		t.Fatalf("the extension answered by itself: %q", h.sentTexts())
	}
	got := h.host.messages()
	if len(got) != 1 || got[0].Text != "" || len(got[0].Unsupported) == 0 {
		t.Fatalf("handed over %+v", got)
	}
	h.do(func() {
		key := h.state.ComputeDeliveryKey(testChat, "img")
		if _, ok := h.state.Outbox[key]; !h.state.IsMessageProcessed(key) || ok || h.state.ContextTokens[testChat] != "ctx-img" {
			t.Fatal("not finished, or the real inbound did not refresh the context")
		}
	})
}

// from: gateway/tests/gateway/unsupported-media.test.ts: unauthorized chats and baseline history never receive the notice
func TestGwgUnsupportedMediaUnauthorizedAndBaseline(t *testing.T) {
	denied, pages := gwgPagesHarness(t, harnessOptions{noWindow: true, account: &Account{Token: testToken, AccountID: testAccount, UserID: "someone-else"}})
	pages.set([]any{gwgImageOnly("img", i64(time.Now().UnixMilli()-1000))})
	if s := gwgPoll(t, denied); s.SkippedCount != 1 {
		t.Fatalf("denied = %+v", s)
	}
	if len(denied.sent()) != 0 || len(denied.host.messages()) != 0 {
		t.Fatal("an unauthorized chat was answered or handed over")
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
	if len(history.sent()) != 0 || len(history.host.messages()) != 0 {
		t.Fatal("baseline history was answered or handed over")
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

// A message id handed to Collie before with other content (a crash between
// handing over and marking it done, then the same id with new text) is
// ignored and logged: no fixed reply, nothing handed over again.
func TestGwgReusedIDWithOtherContentIgnored(t *testing.T) {
	h, pages := gwgPagesHarness(t, harnessOptions{noWindow: true})
	key := h.state.ComputeDeliveryKey(testChat, "same")
	h.do(func() { mustNoErr(t, h.state.recordReceived(key, "content handed over before")) })
	pages.set([]any{inbound(testChat, "same", "新的内容", time.Now().UnixMilli()-1000)})
	if s := gwgPoll(t, h); s.SkippedCount != 1 {
		t.Fatalf("poll = %+v", s)
	}
	if len(h.sent()) != 0 || len(h.host.messages()) != 0 {
		t.Fatalf("sent %q, handed over %+v", h.sentTexts(), h.host.messages())
	}
	h.do(func() {
		if !h.state.IsMessageProcessed(key) {
			t.Fatal("not marked processed: it would come back every poll")
		}
	})
}
