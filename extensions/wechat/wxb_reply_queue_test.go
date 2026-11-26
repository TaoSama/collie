package wechat

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const wxbQueuedReplyTimeoutMs = 100

// wxbQueue is a client whose sendmessage of a held text waits until it is
// released (or its request is cancelled).
type wxbQueue struct {
	t     *testing.T
	path  string
	state *StateManager
	loop  *loop
	mu    sync.Mutex
	sent  []string
	held  map[string]*wxbGate
}

type wxbGate struct {
	began, release chan struct{}
	once, rel      sync.Once
}

func newWxbQueue(t *testing.T) *wxbQueue {
	q := &wxbQueue{t: t, path: filepath.Join(t.TempDir(), stateFileName), loop: &loop{}, held: map[string]*wxbGate{}}
	q.state = q.reload()
	q.state.SetContextToken(testChat, testContext)
	_, err := q.state.RecordInboundContext("synthetic-inbound-key", testChat, i64(nowMs()), true, "")
	mustNoErr(t, err)
	t.Cleanup(func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		for _, g := range q.held {
			g.rel.Do(func() { close(g.release) })
		}
	})
	return q
}

func (q *wxbQueue) reload() *StateManager {
	s, err := NewStateManager(testAccountFor(testToken), q.path, PolicyOptions{SendBudget: 100})
	mustNoErr(q.t, err)
	return s
}

func (q *wxbQueue) client(s *StateManager) *ILinkClient {
	return newTestClient(q.t, s, q.loop, fakeHTTP(func(req *http.Request, body []byte) (*http.Response, error) {
		switch endpoint(req) {
		case "getconfig":
			return jsonResponse(200, map[string]any{"ret": 0, "typing_ticket": "synthetic-ticket"}), nil
		case "sendtyping":
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		}
		text := decodeBody(q.t, body)["msg"].(map[string]any)["item_list"].([]any)[0].(map[string]any)["text_item"].(map[string]any)["text"].(string)
		q.mu.Lock()
		q.sent = append(q.sent, text)
		g := q.held[text]
		q.mu.Unlock()
		if g != nil {
			g.once.Do(func() { close(g.began) })
			select {
			case <-g.release:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		return jsonResponse(200, map[string]any{"ret": 0}), nil
	}))
}

func (q *wxbQueue) hold(text string) *wxbGate {
	g := &wxbGate{began: make(chan struct{}), release: make(chan struct{})}
	q.mu.Lock()
	q.held[text] = g
	q.mu.Unlock()
	return g
}

func (q *wxbQueue) release(g *wxbGate) { g.rel.Do(func() { close(g.release) }) }

func (q *wxbQueue) texts() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.sent...)
}

func (q *wxbQueue) assertRetryable(s *StateManager, key string) {
	q.t.Helper()
	q.loop.do(func() {
		if s.ReplyDeliveries[key].InFlight || s.ReplySentChunks(key) != 0 {
			q.t.Errorf("%s is not retryable", key)
		}
	})
}

func wxbWaitQueued(q *wxbQueue, key string) {
	// The queued call has frozen its plan once it waits on the chat lock.
	for i := 0; i < 500; i++ {
		ok := false
		q.loop.do(func() { _, ok = q.state.ReplyDeliveries[key] })
		if ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	q.t.Fatal("queued reply never planned")
}

// from: gateway/tests/wechat/reply-queue.test.ts: queued text ${cancellation} sends no HTTP request and remains retryable after restart
func TestWxbQueuedTextCancellation(t *testing.T) {
	for _, cancellation := range []string{"timeout", "abort"} {
		t.Run(cancellation, func(t *testing.T) {
			q := newWxbQueue(t)
			c := q.client(q.state)
			blocker := q.hold("blocker")
			first := make(chan error, 1)
			go func() { first <- c.SendSingleText(context.Background(), testChat, "blocker", "") }()
			<-blocker.began
			key := "queued-" + cancellation
			var before int64
			q.loop.do(func() { before = q.state.RemainingSendBudget(testChat) })
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			o := ReplyOptions{DeliveryKey: key}
			if cancellation == "timeout" {
				o.TimeoutMs = wxbQueuedReplyTimeoutMs
			}
			pending := make(chan error, 1)
			go func() { _, err := c.SendMarkdownReply(ctx, testChat, "queued body", o); pending <- err }()
			wxbWaitQueued(q, key)
			q.assertRetryable(q.state, key)
			want := "reply timed out"
			if cancellation == "abort" {
				cancel(errors.New("synthetic cancellation"))
				want = "synthetic cancellation"
			}
			if err := <-pending; err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v", err)
			}
			if strings.Join(q.texts(), "|") != "blocker" {
				t.Fatalf("sent %q", q.texts())
			}
			q.assertRetryable(q.state, key)
			q.assertRetryable(q.reload(), key)
			q.loop.do(func() {
				if q.state.RemainingSendBudget(testChat) != before {
					t.Error("queued reply spent budget")
				}
			})
			q.release(blocker)
			mustNoErr(t, <-first)
			n, err := q.client(q.reload()).SendMarkdownReply(context.Background(), testChat, "queued body", ReplyOptions{DeliveryKey: key})
			mustNoErr(t, err)
			if n != 1 || strings.Join(q.texts(), "|") != "blocker|queued body" || q.reload().ReplySentChunks(key) != 1 {
				t.Fatalf("resume sent %q", q.texts())
			}
			_, err = q.client(q.reload()).SendMarkdownReply(context.Background(), testChat, "queued body", ReplyOptions{DeliveryKey: key})
			mustNoErr(t, err)
			if len(q.texts()) != 2 {
				t.Fatal("acknowledged retry repeated")
			}
		})
	}
}

// from: gateway/tests/wechat/reply-queue.test.ts: a continuation invalidated while queued creates no send intent and can resume
func TestWxbQueuedContinuationInvalidated(t *testing.T) {
	q := newWxbQueue(t)
	c := q.client(q.state)
	blocker := q.hold("blocker")
	first := make(chan error, 1)
	go func() { first <- c.SendSingleText(context.Background(), testChat, "blocker", "") }()
	<-blocker.began
	var canContinue atomic.Bool
	canContinue.Store(true)
	key := "queued-stopped"
	pending := make(chan error, 1)
	go func() {
		_, err := c.SendMarkdownReply(context.Background(), testChat, "queued body", ReplyOptions{DeliveryKey: key, ShouldContinue: canContinue.Load})
		pending <- err
	}()
	wxbWaitQueued(q, key)
	q.assertRetryable(q.state, key)
	canContinue.Store(false)
	q.release(blocker)
	mustNoErr(t, <-first)
	if err := <-pending; err == nil || !strings.Contains(err.Error(), "interrupted before completion") {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(q.texts(), "|") != "blocker" {
		t.Fatalf("sent %q", q.texts())
	}
	q.assertRetryable(q.state, key)
	q.assertRetryable(q.reload(), key)
	n, err := q.client(q.reload()).SendMarkdownReply(context.Background(), testChat, "queued body", ReplyOptions{DeliveryKey: key})
	mustNoErr(t, err)
	if n != 1 || strings.Join(q.texts(), "|") != "blocker|queued body" {
		t.Fatalf("sent %q", q.texts())
	}
}

// from: gateway/tests/wechat/reply-queue.test.ts: canceling after HTTP submission preserves uncertainty and forbids replay after restart
func TestWxbCancelAfterSubmission(t *testing.T) {
	q := newWxbQueue(t)
	c := q.client(q.state)
	submitted := q.hold("submitted body")
	key := "submitted-abort"
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	pending := make(chan error, 1)
	go func() {
		_, err := c.SendMarkdownReply(ctx, testChat, "submitted body", ReplyOptions{DeliveryKey: key})
		pending <- err
	}()
	<-submitted.began
	if !q.reload().ReplyDeliveries[key].InFlight {
		t.Fatal("intent did not reach disk before HTTP submission")
	}
	cancel(errors.New("synthetic cancellation"))
	if err := <-pending; err == nil || !strings.Contains(err.Error(), "synthetic cancellation") {
		t.Fatalf("err = %v", err)
	}
	q.loop.do(func() {
		if !q.state.HasUncertainReply(key) {
			t.Error("not uncertain")
		}
	})
	resumed := q.reload()
	if !resumed.HasUncertainReply(key) {
		t.Fatal("uncertainty lost on restart")
	}
	if _, err := q.client(resumed).SendMarkdownReply(context.Background(), testChat, "submitted body", ReplyOptions{DeliveryKey: key}); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("replay err = %v", err)
	}
	if strings.Join(q.texts(), "|") != "submitted body" {
		t.Fatalf("sent %q", q.texts())
	}
}
