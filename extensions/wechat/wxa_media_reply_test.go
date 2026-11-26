package wechat

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// The original file-delivery tests inject upload, budget and send into one
// function; here they run through sendFileReply over the fake iLink, where
// those are the CDN upload, the budget ledger and sendmessage.

var wxaResultMeta = fileMetadataFor("result.md", wxaReplyFile)

func wxaHangReq(req *http.Request) (*http.Response, error) { return nil, wxaHang(req.Context()) }

// from: gateway/tests/wechat/file-delivery.test.ts: file final send consumes quota after upload and persists acknowledgement before returning
func TestWxaFileDeliveryOrder(t *testing.T) {
	f := wxaNewMediaFixture(t)
	f.sendHook = func(*http.Request) (*http.Response, error) {
		if f.restart().ReplyDeliveries["file-delivery"].InFlight != true {
			t.Error("intent is not durable before sendmessage")
		}
		if n := f.remaining(); n != 9 {
			t.Errorf("budget not charged before the send: %d", n)
		}
		return jsonResponse(200, map[string]any{"ret": 0}), nil
	}
	if n, err := f.sendFile(f.client(f.state, 0), wxaResultMeta, FileReplyOptions{DeliveryKey: "file-delivery"}); n != 1 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if got := strings.Join(f.callList(), ","); got != "/ilink/bot/getuploadurl,/c2c/upload,/ilink/bot/sendmessage" {
		t.Fatalf("calls %s", got)
	}
	if r := f.restart().ReplyDeliveries["file-delivery"]; r.AcknowledgedChunks != 1 || r.InFlight {
		t.Fatalf("%+v", r)
	}
	mustNoErr(t, wxaErr(f.sendFile(f.client(f.restart(), 0), wxaResultMeta, FileReplyOptions{DeliveryKey: "file-delivery"})))
	if len(f.callList()) != 3 {
		t.Fatal("restart reuploaded an acknowledged file")
	}
}

// from: gateway/tests/wechat/file-delivery.test.ts: unknown final outcome survives restart and refuses every upload or send replay
func TestWxaFileDeliveryUnknownOutcome(t *testing.T) {
	f := wxaNewMediaFixture(t)
	f.sendHook = func(*http.Request) (*http.Response, error) { return nil, errors.New("synthetic response loss") }
	if _, err := f.sendFile(f.client(f.state, 0), wxaResultMeta, FileReplyOptions{DeliveryKey: "file-delivery"}); err == nil || !strings.Contains(err.Error(), "response loss") {
		t.Fatalf("%v", err)
	}
	if !f.restart().ReplyDeliveries["file-delivery"].InFlight {
		t.Fatal("not durable")
	}
	if _, err := f.sendFile(f.client(f.restart(), 0), wxaResultMeta, FileReplyOptions{DeliveryKey: "file-delivery"}); err == nil || !strings.Contains(err.Error(), "delivery uncertain") {
		t.Fatalf("%v", err)
	}
	if got := strings.Join(f.callList(), ","); got != "/ilink/bot/getuploadurl,/c2c/upload,/ilink/bot/sendmessage" {
		t.Fatalf("replayed: %s", got)
	}
}

// from: gateway/tests/wechat/file-delivery.test.ts: explicit rejection is retryable with the same message identity
func TestWxaFileDeliveryRejectionRetry(t *testing.T) {
	f := wxaNewMediaFixture(t)
	first := true
	f.sendHook = func(*http.Request) (*http.Response, error) {
		if first {
			first = false
			return jsonResponse(200, map[string]any{"ret": 1}), nil
		}
		return jsonResponse(200, map[string]any{"ret": 0}), nil
	}
	c := f.client(f.state, 0)
	if _, err := f.sendFile(c, wxaResultMeta, FileReplyOptions{DeliveryKey: "file-delivery"}); err == nil || !strings.Contains(err.Error(), "RejectedRet") {
		t.Fatalf("%v", err)
	}
	if f.state.ReplyDeliveries["file-delivery"].InFlight {
		t.Fatal("rejection left uncertainty")
	}
	if n, err := f.sendFile(c, wxaResultMeta, FileReplyOptions{DeliveryKey: "file-delivery"}); n != 1 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if s := f.sent(); len(s) != 2 || s[0]["client_id"] != s[1]["client_id"] {
		t.Fatal("retry changed the message identity")
	}
}

// from: gateway/tests/wechat/file-delivery.test.ts: upload failure, local budget rejection and context changes never leave unknown delivery
func TestWxaFileDeliveryFailuresAreSafe(t *testing.T) {
	for _, phase := range []string{"upload", "budget", "context"} {
		f := wxaNewMediaFixture(t)
		c := f.client(f.state, 0)
		switch phase {
		case "upload":
			f.upload = func(*http.Request) (*http.Response, error) { return bytesResponse(400, nil, nil), nil }
		case "budget":
			c.SetBeforeSendCheck(func(string, string) error { return errors.New("budget exhausted") })
		case "context":
			f.upload = func(*http.Request) (*http.Response, error) {
				f.loop.do(func() { f.state.SetContextToken("chat", "new-context") })
				return bytesResponse(200, nil, http.Header{"X-Encrypted-Param": {"d"}}), nil
			}
		}
		if _, err := f.sendFile(c, wxaResultMeta, FileReplyOptions{DeliveryKey: "file-delivery"}); err == nil {
			t.Fatalf("%s: sent", phase)
		}
		if f.state.ReplyDeliveries["file-delivery"].InFlight || len(f.sent()) != 0 {
			t.Fatalf("%s: unknown delivery", phase)
		}
		if phase != "budget" && f.remaining() != 10 {
			t.Fatalf("%s: charged", phase)
		}
	}
}

// from: gateway/tests/wechat/file-delivery.test.ts: concurrent same-key file requests upload independently but submit only once under the shared chat lock
func TestWxaFileDeliveryConcurrentSameKey(t *testing.T) {
	f := wxaNewMediaFixture(t)
	var arrived sync.WaitGroup
	arrived.Add(2)
	f.upload = func(*http.Request) (*http.Response, error) {
		// Both requests are past their first plan check before either sends.
		arrived.Done()
		arrived.Wait()
		return bytesResponse(200, nil, http.Header{"X-Encrypted-Param": {"d"}}), nil
	}
	c := f.client(f.state, 0)
	results := make([]int, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n, err := f.sendFile(c, wxaResultMeta, FileReplyOptions{DeliveryKey: "file-delivery"})
			mustNoErr(t, err)
			results[i] = n
		}(i)
	}
	wg.Wait()
	if results[0] != 1 || results[1] != 1 || f.count("/c2c/upload") != 2 || len(f.sent()) != 1 || f.remaining() != 9 {
		t.Fatalf("results %v, uploads %d, sends %d, left %d", results, f.count("/c2c/upload"), len(f.sent()), f.remaining())
	}
}

// from: gateway/tests/wechat/file-delivery.test.ts: file reply overall deadline bounds uploads and unknown sends that ignore cancellation
func TestWxaFileDeliveryDeadline(t *testing.T) {
	for _, phase := range []string{"upload", "send"} {
		f := wxaNewMediaFixture(t)
		// A Go transport that ignored its context would hang for good; these wait for it.
		if phase == "upload" {
			f.upload = wxaHangReq
		} else {
			f.sendHook = wxaHangReq
		}
		_, err := f.sendFile(f.client(f.state, 0), wxaResultMeta, FileReplyOptions{DeliveryKey: "file-delivery", TimeoutMs: 15})
		if err == nil || !strings.Contains(err.Error(), "file reply timed out") {
			t.Fatalf("%s: %v", phase, err)
		}
		if f.state.ReplyDeliveries["file-delivery"].InFlight != (phase == "send") {
			t.Fatalf("%s: in flight %v", phase, f.state.ReplyDeliveries["file-delivery"].InFlight)
		}
	}
}

// from: gateway/tests/wechat/file-delivery.test.ts: changed metadata cannot reuse a committed delivery identity
func TestWxaFileDeliveryChangedMetadata(t *testing.T) {
	f := wxaNewMediaFixture(t)
	c := f.client(f.state, 0)
	mustNoErr(t, wxaErr(f.sendFile(c, wxaResultMeta, FileReplyOptions{DeliveryKey: "file-delivery"})))
	changed := wxaResultMeta
	changed.FileName = "changed.md"
	if _, err := f.sendFile(c, changed, FileReplyOptions{DeliveryKey: "file-delivery"}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("%v", err)
	}
	if len(f.sent()) != 1 {
		t.Fatal("sent twice")
	}
}

var (
	wxaSmallPNG = mustHex("89504e470d0a1a0a00000000")
	wxaImageOne = ImageAttachment{ID: "image-one", Mime: "image/png", Size: len(wxaSmallPNG), SHA256: sha256Hex(string(wxaSmallPNG))}
)

func wxaPNGBytes(context.Context, ImageAttachment) ([]byte, error) { return wxaSmallPNG, nil }

func (f *wxaMediaFixture) sendImages(ctx context.Context, c *ILinkClient, images []ImageAttachment, o ImageReplyOptions) (n int, err error) {
	c.loop.do(func() { n, err = c.sendImageReply(ctx, "chat", images, o) })
	return n, err
}

func wxaImageFixture(t *testing.T) *wxaMediaFixture {
	f := wxaNewMediaFixture(t)
	f.authCheck = func(_ string, body map[string]any) {
		if info, _ := body["base_info"].(map[string]any); info["channel_version"] != ChannelVersion || body["media_type"] != float64(mediaTypeImage) {
			t.Errorf("image authorization %v", body)
		}
	}
	return f
}

// from: gateway/tests/wechat/image-client.test.ts: shared admission policy checks every image and yields a 3 -> 2 batch without an unknown send
func TestWxaImageClientYieldsBatch(t *testing.T) {
	f := wxaImageFixture(t)
	c := f.client(f.state, 0)
	wxaSetRemaining(t, f, 3)
	images := []ImageAttachment{wxaImageOne, wxaImageOne}
	images[1].ID = "image-two"
	c.SetBeforeSendCheck(func(chat, key string) error {
		if chat != "chat" || key != "priority-images" {
			t.Errorf("check %s %s", chat, key)
		}
		if f.state.RemainingSendBudget(chat) < 3 {
			return deferred(DeferBudgetExhausted)
		}
		return nil
	})
	_, err := f.sendImages(context.Background(), c, images, ImageReplyOptions{DeliveryKey: "priority-images", GetImageBytes: wxaPNGBytes})
	if _, ok := asDeferred(err); !ok {
		t.Fatalf("%v", err)
	}
	if len(f.sent()) != 1 || f.remaining() != 2 {
		t.Fatalf("%d sends, %d left", len(f.sent()), f.remaining())
	}
	restarted := f.restart()
	if r := restarted.ReplyDeliveries["priority-images"]; r.AcknowledgedChunks != 1 || r.InFlight {
		t.Fatalf("%+v", r)
	}
	mustNoErr(t, wxaErr(f.sendImages(context.Background(), f.client(restarted, 0), images, ImageReplyOptions{DeliveryKey: "priority-images", GetImageBytes: wxaPNGBytes})))
	if len(f.sent()) != 2 {
		t.Fatal("second picture not sent")
	}
}

// from: gateway/tests/wechat/image-client.test.ts: image transport types, uploads encrypted bytes and confirms the final WeChat receipt
func TestWxaImageClientSends(t *testing.T) {
	f := wxaImageFixture(t)
	if n, err := f.sendImages(context.Background(), f.client(f.state, 0), []ImageAttachment{wxaImageOne}, ImageReplyOptions{DeliveryKey: "images", GetImageBytes: wxaPNGBytes}); n != 1 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	var calls []string
	for _, c := range f.callList() {
		if !strings.HasSuffix(c, "getconfig") && !strings.HasSuffix(c, "sendtyping") {
			calls = append(calls, c)
		}
	}
	if strings.Join(calls, ",") != "/ilink/bot/getuploadurl,/c2c/upload,/ilink/bot/sendmessage" {
		t.Fatalf("calls %v", calls)
	}
	sent := f.sent()[0]
	item := wxaSentItem(sent)
	img := item["image_item"].(map[string]any)
	if sent["to_user_id"] != "chat" || sent["context_token"] != "synthetic-context" || item["type"] != float64(ItemTypeImage) ||
		img["media"].(map[string]any)["encrypt_type"] != float64(1) || img["mid_size"] != float64(16) {
		t.Fatalf("sent %v", sent)
	}
	if r := f.state.ReplyDeliveries["images"]; r.AcknowledgedChunks != 1 || r.InFlight {
		t.Fatalf("%+v", r)
	}
}

// from: gateway/tests/wechat/image-client.test.ts: image upload and delivery complete while ${endpoint} remains unresolved
func TestWxaImageClientTypingDoesNotBlock(t *testing.T) {
	for _, ep := range []string{"getconfig", "sendtyping"} {
		t.Run(ep, func(t *testing.T) {
			f := wxaImageFixture(t)
			gate, began := make(chan struct{}), make(chan struct{})
			var once sync.Once
			f.typing = func(request string) {
				if request == ep {
					once.Do(func() { close(began) })
					<-gate
				}
			}
			defer close(gate)
			n, err := f.sendImages(context.Background(), f.client(f.state, 0), []ImageAttachment{wxaImageOne},
				ImageReplyOptions{DeliveryKey: "background-" + ep, GetImageBytes: wxaPNGBytes})
			if n != 1 || err != nil {
				t.Fatalf("%d %v", n, err)
			}
			if ep == "getconfig" {
				<-began
			}
			if len(f.sent()) != 1 || f.state.ReplySentChunks("background-"+ep) != 1 {
				t.Fatal("delivery waited for typing")
			}
		})
	}
}

// from: gateway/tests/wechat/image-client.test.ts: CDN acceptance followed by unknown WeChat receipt persists uncertainty and forbids all replay
func TestWxaImageClientUnknownReceipt(t *testing.T) {
	f := wxaImageFixture(t)
	f.sendHook = func(*http.Request) (*http.Response, error) { return nil, errors.New("synthetic response loss") }
	o := ImageReplyOptions{DeliveryKey: "unknown", GetImageBytes: wxaPNGBytes}
	if _, err := f.sendImages(context.Background(), f.client(f.state, 0), []ImageAttachment{wxaImageOne}, o); err == nil || !strings.Contains(err.Error(), "response loss") {
		t.Fatalf("%v", err)
	}
	if !f.state.ReplyDeliveries["unknown"].InFlight || f.state.ReplySentChunks("unknown") != 0 {
		t.Fatal("uncertainty not recorded")
	}
	if _, err := f.sendImages(context.Background(), f.client(f.restart(), 0), []ImageAttachment{wxaImageOne}, o); err == nil || !strings.Contains(err.Error(), "delivery uncertain") {
		t.Fatalf("%v", err)
	}
	if len(f.sent()) != 1 || f.count("/c2c/upload") != 1 {
		t.Fatal("replayed")
	}
}

// from: gateway/tests/wechat/image-client.test.ts: upload failure leaves a safely resumable image intent without claiming WeChat acceptance
func TestWxaImageClientUploadFailureResumes(t *testing.T) {
	f := wxaImageFixture(t)
	f.upload = func(*http.Request) (*http.Response, error) { return bytesResponse(400, nil, nil), nil }
	c := f.client(f.state, 0)
	o := ImageReplyOptions{DeliveryKey: "upload", GetImageBytes: wxaPNGBytes}
	if _, err := f.sendImages(context.Background(), c, []ImageAttachment{wxaImageOne}, o); err == nil || !strings.Contains(err.Error(), "upload failed HTTP 400") {
		t.Fatalf("%v", err)
	}
	if len(f.sent()) != 0 || f.state.ReplyDeliveries["upload"].InFlight || f.state.ReplySentChunks("upload") != 0 {
		t.Fatal("upload failure claimed acceptance")
	}
	f.set(func(f *wxaMediaFixture) { f.upload = nil })
	if n, err := f.sendImages(context.Background(), c, []ImageAttachment{wxaImageOne}, o); n != 1 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
}

// from: gateway/tests/wechat/image-client.test.ts: image send timeout is bounded even if its transport ignores abort
func TestWxaImageClientTimeout(t *testing.T) {
	f := wxaImageFixture(t)
	// A transport that ignored its context would hang for good in Go; this one waits for it.
	f.sendHook = wxaHangReq
	started := time.Now()
	_, err := f.sendImages(context.Background(), f.client(f.state, 0), []ImageAttachment{wxaImageOne},
		ImageReplyOptions{DeliveryKey: "timeout", GetImageBytes: wxaPNGBytes, TimeoutMs: 500})
	if err == nil || !strings.Contains(err.Error(), "image reply timed out") || time.Since(started) > 5*time.Second {
		t.Fatalf("%v", err)
	}
	if len(f.sent()) != 1 || !f.state.ReplyDeliveries["timeout"].InFlight {
		t.Fatal("unknown send not recorded")
	}
}

// from: gateway/tests/wechat/image-client.test.ts: a canceled image queue waiter cannot let a later text send bypass an active image upload
func TestWxaImageClientCanceledWaiterKeepsOrder(t *testing.T) {
	f := wxaImageFixture(t)
	c := f.client(f.state, 0)
	finish, began := make(chan struct{}), make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := f.sendImages(context.Background(), c, []ImageAttachment{wxaImageOne}, ImageReplyOptions{DeliveryKey: "first",
			GetImageBytes: func(context.Context, ImageAttachment) ([]byte, error) {
				close(began)
				c.loop.await(func() { <-finish })
				return wxaSmallPNG, nil
			}})
		firstDone <- err
	}()
	<-began
	ctx, cancel := context.WithCancelCause(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		_, err := f.sendImages(ctx, c, []ImageAttachment{wxaImageOne}, ImageReplyOptions{DeliveryKey: "second", GetImageBytes: wxaPNGBytes})
		secondDone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel(errors.New("synthetic cancellation"))
	if err := <-secondDone; err == nil || !strings.Contains(err.Error(), "synthetic cancellation") {
		t.Fatalf("second: %v", err)
	}
	textDone := make(chan error, 1)
	go func() { textDone <- c.SendSingleText(context.Background(), "chat", "after images", "") }()
	time.Sleep(20 * time.Millisecond)
	if len(f.sent()) != 0 {
		t.Fatal("text passed the running image delivery")
	}
	close(finish)
	mustNoErr(t, <-firstDone)
	mustNoErr(t, <-textDone)
	s := f.sent()
	if len(s) != 2 || wxaSentItem(s[0])["type"] != float64(ItemTypeImage) || wxaSentItem(s[1])["type"] != float64(ItemTypeText) {
		t.Fatalf("sends %v", s)
	}
}

// from: gateway/tests/wechat/image-client.test.ts: account or context changes during image download prevent upload and sending
func TestWxaImageClientChangeDuringDownload(t *testing.T) {
	for _, mode := range []string{"account", "context"} {
		f := wxaImageFixture(t)
		_, err := f.sendImages(context.Background(), f.client(f.state, 0), []ImageAttachment{wxaImageOne}, ImageReplyOptions{DeliveryKey: "changed-" + mode,
			GetImageBytes: func(context.Context, ImageAttachment) ([]byte, error) {
				if mode == "account" {
					f.state.Account.Token = "replacement"
				} else {
					f.state.SetContextToken("chat", "replacement-context")
				}
				return bytes.Clone(wxaSmallPNG), nil
			}})
		if err == nil || !strings.Contains(err.Error(), "image reply interrupted before") {
			t.Fatalf("%s: %v", mode, err)
		}
		if len(f.sent()) != 0 || f.count("/ilink/bot/getuploadurl") != 0 {
			t.Fatalf("%s: uploaded or sent", mode)
		}
	}
}
