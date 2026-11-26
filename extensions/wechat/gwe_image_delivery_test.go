package wechat

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/TaoSama/collie/internal/extproto"
)

var gwePNG = []byte{137, 80, 78, 71, 13, 10, 26, 10}

func gweImage(data []byte) extproto.Blob {
	return extproto.Blob{Mime: "image/png", Data: base64.StdEncoding.EncodeToString(data)}
}

// gweImageFixture polls n messages from the bound chat and records the
// item type of every sendmessage; fail picks one to fail.
type gweImageFixture struct {
	*harness
	mu    sync.Mutex
	types []int
	fail  string // image | text
}

func newGweImageFixture(t *testing.T, n int, o harnessOptions) *gweImageFixture {
	f := &gweImageFixture{harness: newHarness(t, o)}
	f.setHooks(func(k *harnessHooks) {
		k.poll = func() (*http.Response, error) {
			var msgs []any
			for i := 0; i < n; i++ {
				msgs = append(msgs, inbound(testChat, fmt.Sprintf("message-%d", i), "截图", nowMs()-1000))
			}
			return jsonResponse(200, map[string]any{"ret": 0, "msgs": msgs, "get_updates_buf": "next"}), nil
		}
		k.send = func(msg map[string]any) (*http.Response, error) {
			n, _ := safeInt(msg["item_list"].([]any)[0].(map[string]any)["type"])
			f.mu.Lock()
			f.types = append(f.types, int(n))
			fail := f.fail
			f.mu.Unlock()
			if n == ItemTypeImage && fail == "image" {
				return nil, errors.New("ambiguous image send")
			}
			if n == ItemTypeText && fail == "text" {
				return jsonResponse(200, map[string]any{"ret": -1}), nil
			}
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		}
	})
	return f
}

func (f *gweImageFixture) sentTypes() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.types...)
}

func (f *gweImageFixture) setFail(v string) {
	f.mu.Lock()
	f.fail = v
	f.mu.Unlock()
}

func (f *gweImageFixture) answer(ref, text string, images ...[]byte) (extproto.SendResult, error) {
	var blobs []extproto.Blob
	for _, img := range images {
		blobs = append(blobs, gweImage(img))
	}
	return f.send(extproto.Send{Ref: ref, Text: text, Kind: extproto.KindAnswer, Images: blobs})
}

func gweInFlight(s *StateManager) int {
	n := 0
	for _, d := range s.ReplyDeliveries {
		if d.InFlight {
			n++
		}
	}
	return n
}

func gweTypes(got []int, want ...int) bool { return fmt.Sprint(got) == fmt.Sprint(want) }

// from: gateway/tests/gateway/image-delivery.test.ts: poll persists the command before delivery, sends image before completion text and checkpoints the cursor
func TestGweImageBeforeTextAfterDurableCommand(t *testing.T) {
	f := newGweImageFixture(t, 1, harnessOptions{})
	var durable []bool
	inner := f.hooks.send
	f.setHooks(func(k *harnessHooks) {
		k.send = func(msg map[string]any) (*http.Response, error) {
			persisted, err := NewStateManager(testAccountFor(testToken), filepath.Join(f.dir, stateFileName), PolicyOptions{})
			mustNoErr(t, err)
			durable = append(durable, persisted.IsMessageProcessed(persisted.ComputeDeliveryKey(testChat, "message-0")) &&
				persisted.Outbox["answer"].Status == OutboxPending)
			return inner(msg)
		}
	})
	summary, err := gwePoll(t, f.gateway)
	mustNoErr(t, err)
	if summary.ProcessedCount != 1 || f.state.SyncBuf != "next" {
		t.Fatalf("poll %+v", summary)
	}
	_, err = f.answer("answer", "截图已发送", gwePNG)
	mustNoErr(t, err)
	if !gweTypes(f.sentTypes(), 2, 1) || fmt.Sprint(durable) != "[true true]" {
		t.Fatalf("types %v durable %v", f.sentTypes(), durable)
	}
	var reqs []string
	f.mu.Lock()
	for _, r := range f.requests {
		if r == "upload" || r == "sendmessage" {
			reqs = append(reqs, r)
		}
	}
	f.mu.Unlock()
	if fmt.Sprint(reqs) != "[upload sendmessage sendmessage]" {
		t.Fatalf("order %v", reqs)
	}
	f.do(func() {
		if len(f.state.Outbox) != 0 {
			t.Fatal("outbox not empty")
		}
	})
	_, err = gwePoll(t, f.gateway)
	mustNoErr(t, err)
	if !gweTypes(f.sentTypes(), 2, 1) || len(f.host.messages()) != 1 {
		t.Fatal("repeated poll handed over or sent again")
	}
}

// from: gateway/tests/gateway/image-delivery.test.ts: uncertain image is quarantined with evidence and never emits completion text or repeats after restart
func TestGweUncertainImageQuarantined(t *testing.T) {
	f := newGweImageFixture(t, 1, harnessOptions{})
	f.setFail("image")
	summary, err := gwePoll(t, f.gateway)
	mustNoErr(t, err)
	if summary.ProcessedCount != 1 || summary.SkippedCount != 0 || f.state.SyncBuf != "next" {
		t.Fatalf("poll %+v", summary)
	}
	_, err = f.answer("answer", "截图已发送", gwePNG)
	if !extproto.IsCode(err, extproto.ErrCannotSend) {
		t.Fatalf("answer error %v, want cannot_send for an unknown send", err)
	}
	f.do(func() {
		if !gweTypes(f.sentTypes(), 2) || gweInFlight(f.state) != 1 || f.state.Outbox["answer"].Status != OutboxUncertain {
			t.Fatalf("types %v", f.sentTypes())
		}
	})
	if e := f.host.events(); len(e) != 1 || e[0].State != DeliveryUncertain {
		t.Fatalf("events %v", e)
	}
	f.setFail("")
	resumed, g := gweReopen(t, f.harness)
	_, err = gwePoll(t, g)
	mustNoErr(t, err)
	g.loop.do(func() {
		if gweInFlight(resumed) != 1 || resumed.Outbox["answer"].Status != OutboxUncertain {
			t.Fatal("evidence lost after restart")
		}
	})
	if !gweTypes(f.sentTypes(), 2) || len(f.host.messages()) != 1 {
		t.Fatalf("repeated after restart: %v", f.sentTypes())
	}
}

// from: gateway/tests/gateway/image-delivery.test.ts: an uncertain image cannot block a following chat in the same batch
func TestGweUncertainImageDoesNotBlockFollowing(t *testing.T) {
	// Only the bound person is served, so the following message is theirs too.
	f := newGweImageFixture(t, 2, harnessOptions{})
	f.setFail("image")
	summary, err := gwePoll(t, f.gateway)
	mustNoErr(t, err)
	if summary.ProcessedCount != 2 || summary.SkippedCount != 0 || f.state.SyncBuf != "next" {
		t.Fatalf("poll %+v", summary)
	}
	if _, err := f.answer("answer-0", "截图已发送", gwePNG); err == nil {
		t.Fatal("uncertain image reported as sent")
	}
	_, err = f.answer("answer-1", "正常回复")
	mustNoErr(t, err)
	f.do(func() {
		if !gweTypes(f.sentTypes(), 2, 1) || gweInFlight(f.state) != 1 {
			t.Fatalf("types %v", f.sentTypes())
		}
	})
}

// from: gateway/tests/gateway/image-delivery.test.ts: text rejection resumes text without redownloading or replaying accepted image
func TestGweTextRejectionResumesText(t *testing.T) {
	f := newGweImageFixture(t, 1, harnessOptions{})
	f.setFail("text")
	_, err := gwePoll(t, f.gateway)
	mustNoErr(t, err)
	res, err := f.answer("answer", "截图已发送", gwePNG)
	mustNoErr(t, err)
	if !res.Deferred {
		t.Fatalf("rejected text answered %+v", res)
	}
	f.do(func() {
		if f.state.Outbox["answer"].Status != OutboxPending {
			t.Fatal("rejected reply not pending")
		}
	})
	f.setFail("")
	state, g := gweReopen(t, f.harness)
	_, err = gwePoll(t, g)
	mustNoErr(t, err)
	if !gweTypes(f.sentTypes(), 2, 1, 1) || len(f.host.messages()) != 1 {
		t.Fatalf("types %v", f.sentTypes())
	}
	g.loop.do(func() {
		if len(state.Outbox) != 0 || state.SyncBuf != "next" {
			t.Fatal("reply not finished")
		}
	})
}

// gweDropBlob removes one stored reply picture.
func gweDropBlob(t *testing.T, h *harness, data []byte) {
	t.Helper()
	mustNoErr(t, os.Remove(filepath.Join(h.dir, answersBlobDir, sha256Hex(string(data)))))
}

// from: gateway/tests/gateway/image-delivery.test.ts: expired or unavailable attachments persist failure evidence and cannot block following chats or replay after restart
func TestGweUnavailableAttachmentFailsPermanently(t *testing.T) {
	// Pictures are frozen with the answer, so the one failure left is a stored picture gone (unavailable).
	f := newGweImageFixture(t, 2, harnessOptions{noWindow: true})
	res, err := f.answer("answer-0", "截图已发送", gwePNG)
	mustNoErr(t, err)
	if !res.Deferred {
		t.Fatalf("answer without a window: %+v", res)
	}
	gweDropBlob(t, f.harness, gwePNG)
	summary, err := gwePoll(t, f.gateway)
	mustNoErr(t, err)
	if summary.ProcessedCount != 2 || summary.SkippedCount != 0 || f.state.SyncBuf != "next" {
		t.Fatalf("poll %+v", summary)
	}
	_, err = f.answer("answer-1", "正常回复")
	mustNoErr(t, err)
	if !gweTypes(f.sentTypes(), 1) {
		t.Fatalf("types %v: the failed image's text must stay suppressed", f.sentTypes())
	}
	resumed, g := gweReopen(t, f.harness)
	g.loop.do(func() {
		found := false
		for _, d := range resumed.ReplyDeliveries {
			found = found || d.FailureReason == FailureAttachmentUnavailable
			if d.InFlight {
				t.Fatal("in-flight evidence")
			}
		}
		if !found || resumed.Outbox["answer-0"].Status != OutboxExpired {
			t.Fatal("failure evidence lost")
		}
	})
	_, err = gwePoll(t, g)
	mustNoErr(t, err)
	if !gweTypes(f.sentTypes(), 1) || len(f.host.messages()) != 2 {
		t.Fatalf("replayed: %v", f.sentTypes())
	}
}

// from: gateway/tests/gateway/image-delivery.test.ts: an expired second image preserves the first receipt and allows the following chat without completion text or replay
func TestGweMissingSecondImageKeepsFirstReceipt(t *testing.T) {
	second := append(append([]byte(nil), gwePNG...), 'x')
	f := newGweImageFixture(t, 2, harnessOptions{noWindow: true})
	_, err := f.answer("answer-0", "截图已发送", gwePNG, second)
	mustNoErr(t, err)
	gweDropBlob(t, f.harness, second)
	summary, err := gwePoll(t, f.gateway)
	mustNoErr(t, err)
	if summary.ProcessedCount != 2 {
		t.Fatalf("poll %+v", summary)
	}
	_, err = f.answer("answer-1", "正常回复")
	mustNoErr(t, err)
	if !gweTypes(f.sentTypes(), 2, 1) {
		t.Fatalf("types %v", f.sentTypes())
	}
	key := imageReplyPrefix + "answer-0"
	f.do(func() {
		d := f.state.ReplyDeliveries[key]
		if d.TotalChunks != 2 || d.AcknowledgedChunks != 1 || d.InFlight || d.FailureReason != FailureAttachmentUnavailable {
			t.Fatalf("plan %+v", d)
		}
	})
	resumed, g := gweReopen(t, f.harness)
	_, err = gwePoll(t, g)
	mustNoErr(t, err)
	g.loop.do(func() {
		if d := resumed.ReplyDeliveries[key]; d.AcknowledgedChunks != 1 || d.FailureReason != FailureAttachmentUnavailable {
			t.Fatalf("plan after restart %+v", d)
		}
	})
	if !gweTypes(f.sentTypes(), 2, 1) {
		t.Fatalf("replayed: %v", f.sentTypes())
	}
}

// from: gateway/tests/gateway/image-delivery.test.ts: permanent image failure remains quarantined after its processed key is evicted and state is reloaded
func TestGwePermanentImageFailureStaysQuarantined(t *testing.T) {
	f := newGweImageFixture(t, 1, harnessOptions{noWindow: true})
	_, err := f.answer("answer", "截图已发送", gwePNG)
	mustNoErr(t, err)
	gweDropBlob(t, f.harness, gwePNG)
	_, err = gwePoll(t, f.gateway)
	mustNoErr(t, err)
	key := f.state.ComputeDeliveryKey(testChat, "message-0")
	f.do(func() {
		for i := 0; i < MaxProcessedMessageKeys; i++ {
			f.state.ProcessedMessageKeys = append(f.state.ProcessedMessageKeys, fmt.Sprintf("later-message-%d", i))
		}
		mustNoErr(t, f.state.RecordProcessedMessageKey("last-message", false))
		if len(f.state.ProcessedMessageKeys) != MaxProcessedMessageKeys || f.state.IsMessageProcessed(key) {
			t.Fatal("key not evicted")
		}
	})
	resumed, g := gweReopen(t, f.harness)
	if resumed.IsMessageProcessed(key) || !resumed.HasPermanentReplyFailure(imageReplyPrefix+"answer") {
		t.Fatal("reload lost the failure evidence")
	}
	before := len(f.sentTypes())
	_, err = gwePoll(t, g)
	mustNoErr(t, err)
	// The replayed message reaches Collie again under its id, which Collie takes only once;
	// the answer that failed for good is never sent again.
	g.loop.do(func() {
		if !resumed.HasPermanentReplyFailure(imageReplyPrefix+"answer") || !resumed.IsMessageProcessed(key) {
			t.Fatal("quarantine lost")
		}
	})
	var res extproto.SendResult
	res, err = g.HandleSend(context.Background(), extproto.Send{Ref: "answer", Chat: testChat, Text: "截图已发送", Kind: extproto.KindAnswer,
		Images: []extproto.Blob{gweImage(gwePNG)}})
	if !extproto.IsCode(err, extproto.ErrCannotSend) || len(f.sentTypes()) != before {
		t.Fatalf("repeated ref: %+v %v, sends %v", res, err, f.sentTypes())
	}
}

// from: gateway/tests/gateway/image-delivery.test.ts: a peer refresh during an ordinary image upload defers safely without stopping inbound processing or rerunning the model
func TestGweRefreshDuringUploadDefers(t *testing.T) {
	// The refresh comes from the bound person's next message: only they are served.
	f := newGweImageFixture(t, 0, harnessOptions{})
	uploaded, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	blocking := true
	f.setHooks(func(k *harnessHooks) {
		k.cdn = func(*http.Request, []byte) (*http.Response, error) {
			if blocking {
				once.Do(func() { close(uploaded) })
				<-release
			}
			return bytesResponse(200, nil, http.Header{"X-Encrypted-Param": {"download-param"}}), nil
		}
		k.poll = func() (*http.Response, error) {
			return jsonResponse(200, map[string]any{"ret": 0, "msgs": []any{inbound(testChat, "following-message", "正常任务", nowMs())},
				"get_updates_buf": "next"}), nil
		}
	})
	answered := make(chan extproto.SendResult, 1)
	go func() {
		res, err := f.answer("answer", "截图已发送", gwePNG)
		if err != nil {
			t.Error(err)
		}
		answered <- res
	}()
	<-uploaded
	var summary PollCycleSummary
	var err error
	f.do(func() { summary, err = f.gateway.pollOnce(context.Background(), false) })
	mustNoErr(t, err)
	if summary.ProcessedCount != 1 {
		t.Fatalf("poll %+v", summary)
	}
	f.do(func() {
		if f.state.AccountBudget.LastInboundKey != f.state.ComputeDeliveryKey(testChat, "following-message") {
			t.Fatal("new message did not refresh the budget")
		}
	})
	blocking = false
	close(release)
	if res := <-answered; !res.Deferred {
		t.Fatalf("interrupted image answered %+v", res)
	}
	// The new message's own admission flushes this chat's waiting reply in the new epoch:
	// the interrupted image spent nothing, so picture and text cost two of the fresh ten.
	eventually(t, "recovered reply", func() bool {
		done := false
		f.do(func() {
			mustNoErr(t, f.gateway.flushRepliesOnce(context.Background(), "", ""))
			done = len(f.state.Outbox) == 0
		})
		return done
	})
	// The runner hands the new message over on its own goroutine.
	eventually(t, "new message handed over", func() bool { return len(f.host.messages()) == 1 })
	f.do(func() {
		d := f.state.ReplyDeliveries[imageReplyPrefix+"answer"]
		if !gweTypes(f.sentTypes(), 2, 1) || d.InFlight || f.state.AccountBudget.Remaining != 8 || len(f.host.messages()) != 1 {
			t.Fatalf("types %v plan %+v remaining %d", f.sentTypes(), d, f.state.AccountBudget.Remaining)
		}
	})
	f.do(func() { summary, err = f.gateway.pollOnce(context.Background(), false) })
	mustNoErr(t, err)
	if summary.Interrupted {
		t.Fatal("a safe deferral must not halt the gateway")
	}
}
