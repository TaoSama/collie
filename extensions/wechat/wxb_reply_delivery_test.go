package wechat

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const wxbMultipartBudget = 1000

// wxbReplyFixture is a client whose sendmessage records texts and client
// ids; fail decides the answer to the n-th send.
type wxbReplyFixture struct {
	t     *testing.T
	path  string
	state *StateManager
	loop  *loop
	mu    sync.Mutex
	sent  []string
	ids   []string
	fail  func(n int) (*http.Response, error)
}

func newWxbReplyFixture(t *testing.T) *wxbReplyFixture {
	f := &wxbReplyFixture{t: t, path: filepath.Join(t.TempDir(), stateFileName), loop: &loop{}}
	f.state = f.reload()
	f.state.SetContextToken(testChat, testContext)
	_, err := f.state.RecordInboundContext("synthetic-inbound-key", testChat, i64(nowMs()-1000), true, "")
	mustNoErr(t, err)
	return f
}

func (f *wxbReplyFixture) reload() *StateManager {
	s, err := NewStateManager(testAccountFor(testToken), f.path, PolicyOptions{SendBudget: wxbMultipartBudget})
	mustNoErr(f.t, err)
	return s
}

func (f *wxbReplyFixture) fetch(req *http.Request, body []byte) (*http.Response, error) {
	switch endpoint(req) {
	case "getconfig":
		return jsonResponse(200, map[string]any{"ret": 0, "typing_ticket": "synthetic-ticket"}), nil
	case "sendtyping":
		return jsonResponse(200, map[string]any{"ret": 0}), nil
	}
	if endpoint(req) != "sendmessage" {
		f.t.Errorf("unexpected %s", req.URL)
	}
	msg := decodeBody(f.t, body)["msg"].(map[string]any)
	item := msg["item_list"].([]any)[0].(map[string]any)
	f.mu.Lock()
	f.sent = append(f.sent, item["text_item"].(map[string]any)["text"].(string))
	f.ids = append(f.ids, msg["client_id"].(string))
	n, fail := len(f.sent), f.fail
	f.mu.Unlock()
	if fail != nil {
		if resp, err := fail(n); resp != nil || err != nil {
			return resp, err
		}
	}
	return jsonResponse(200, map[string]any{"ret": 0}), nil
}

func (f *wxbReplyFixture) client(s *StateManager) *ILinkClient {
	return newTestClient(f.t, s, f.loop, fakeHTTP(f.fetch))
}

func (f *wxbReplyFixture) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func wxbValidChunks(t *testing.T, chunks []string, budget int) {
	t.Helper()
	for _, c := range chunks {
		if utf16Len(c) > budget || !utf8.ValidString(c) || strings.ContainsRune(c, utf8.RuneError) {
			t.Fatalf("bad chunk %q", c)
		}
	}
}

// from: gateway/tests/wechat/reply-delivery.test.ts: default delivery formats the complete body and preserves over-budget code and emoji
func TestWxbDefaultDeliveryVerbatim(t *testing.T) {
	f := newWxbReplyFixture(t)
	original := strings.Repeat("中", 3799) + "🚀\n\n```ts\n  **literal**\n```\n\t"
	n, err := f.client(f.state).SendMarkdownReply(context.Background(), testChat, original, ReplyOptions{DeliveryKey: "default-budget"})
	mustNoErr(t, err)
	sent := f.texts()
	// Sent as written (E1): the chunks rebuild the Markdown source, not a rendering.
	if n != 2 || utf16Len(sent[0]) != 3799 || strings.Join(sent, "") != original {
		t.Fatalf("n %d, first %d units", n, utf16Len(sent[0]))
	}
	wxbValidChunks(t, sent, 3800)
	if f.state.ReplySentChunks("default-budget") != n {
		t.Fatal("progress not recorded")
	}
}

// from: gateway/tests/wechat/reply-delivery.test.ts: transport renders Markdown before splitting and resumes the same display after restart
func TestWxbVerbatimSplitResumesAfterRestart(t *testing.T) {
	f := newWxbReplyFixture(t)
	original := " \n# 标题🚀\n\n**重点** [文档](https://example.test/a_b)\n```ts\n  a_b += 1;\n```\n\t"
	o := ReplyOptions{DeliveryKey: "full", MaxCharsPerChunk: 9}
	n, err := f.client(f.state).SendMarkdownReply(context.Background(), testChat, original, o)
	mustNoErr(t, err)
	sent := f.texts()
	if n != len(sent) || strings.Join(sent, "") != original {
		t.Fatalf("n %d sent %q", n, sent)
	}
	wxbValidChunks(t, sent, 9)
	reloaded := f.reload()
	if reloaded.ReplySentChunks("full") != n {
		t.Fatal("progress lost on restart")
	}
	again, err := f.client(reloaded).SendMarkdownReply(context.Background(), testChat, original, o)
	mustNoErr(t, err)
	if again != n || len(f.texts()) != n {
		t.Fatal("completed chunks replayed after restart")
	}
}

// from: gateway/tests/wechat/reply-delivery.test.ts: explicitly rejected chunk resumes after restart without replaying acknowledged chunks
func TestWxbRejectedChunkResumes(t *testing.T) {
	f := newWxbReplyFixture(t)
	f.fail = func(n int) (*http.Response, error) {
		if n == 2 {
			return jsonResponse(200, map[string]any{"ret": -1}), nil
		}
		return nil, nil
	}
	o := ReplyOptions{DeliveryKey: "retry", MaxCharsPerChunk: 2}
	if _, err := f.client(f.state).SendMarkdownReply(context.Background(), testChat, "a\nb\nc", o); err == nil || !strings.Contains(err.Error(), "RejectedRet") {
		t.Fatalf("err = %v", err)
	}
	if f.state.ReplySentChunks("retry") != 1 {
		t.Fatal("first chunk not acknowledged")
	}
	f.fail = nil
	resumed := f.reload()
	_, err := f.client(resumed).SendMarkdownReply(context.Background(), testChat, "a\nb\nc", o)
	mustNoErr(t, err)
	if strings.Join(f.texts(), "|") != "a\n|b\n|b\n|c" || f.ids[1] != f.ids[2] || resumed.ReplySentChunks("retry") != 3 {
		t.Fatalf("sent %q ids %v", f.texts(), f.ids)
	}
}

// from: gateway/tests/wechat/reply-delivery.test.ts: ambiguous network outcome persists pending intent and forbids replay after restart
func TestWxbAmbiguousNetworkForbidsReplay(t *testing.T) {
	f := newWxbReplyFixture(t)
	f.fail = func(n int) (*http.Response, error) {
		if n == 2 {
			return nil, errors.New("synthetic network failure")
		}
		return nil, nil
	}
	o := ReplyOptions{DeliveryKey: "uncertain", MaxCharsPerChunk: 2}
	if _, err := f.client(f.state).SendMarkdownReply(context.Background(), testChat, "a\nb\nc", o); err == nil || !strings.Contains(err.Error(), "network failure") {
		t.Fatalf("err = %v", err)
	}
	resumed := f.reload()
	if resumed.ReplySentChunks("uncertain") != 1 || !resumed.ReplyDeliveries["uncertain"].InFlight {
		t.Fatal("pending intent not persisted")
	}
	f.fail = nil
	if _, err := f.client(resumed).SendMarkdownReply(context.Background(), testChat, "a\nb\nc", o); err == nil || !strings.Contains(err.Error(), "delivery uncertain") {
		t.Fatalf("replay err = %v", err)
	}
	if len(f.texts()) != 2 {
		t.Fatalf("sent %d", len(f.texts()))
	}
}

// from: gateway/tests/wechat/reply-delivery.test.ts: unconfirmed receipt forbids replay rather than treating unknown shape as rejection
func TestWxbUnconfirmedReceiptForbidsReplay(t *testing.T) {
	f := newWxbReplyFixture(t)
	f.fail = func(int) (*http.Response, error) {
		return jsonResponse(200, map[string]any{"error": "synthetic-private"}), nil
	}
	c := f.client(f.state)
	if _, err := c.SendMarkdownReply(context.Background(), testChat, "body", ReplyOptions{DeliveryKey: "receipt"}); err == nil || !strings.Contains(err.Error(), "UnsupportedShape") {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.SendMarkdownReply(context.Background(), testChat, "body", ReplyOptions{DeliveryKey: "receipt"}); err == nil || !strings.Contains(err.Error(), "delivery uncertain") {
		t.Fatalf("replay err = %v", err)
	}
	if len(f.texts()) != 1 {
		t.Fatalf("sent %d", len(f.texts()))
	}
}

// from: gateway/tests/wechat/reply-delivery.test.ts: zero-byte HTTP send response persists uncertainty and forbids replay after restart
func TestWxbZeroByteSendResponse(t *testing.T) {
	f := newWxbReplyFixture(t)
	attempts := 0
	f.fail = func(int) (*http.Response, error) { attempts++; return rawResponse(200, ""), nil }
	_, err := f.client(f.state).SendMarkdownReply(context.Background(), testChat, "body", ReplyOptions{DeliveryKey: "empty-body"})
	var receipt *SendReceiptError
	if !errors.As(err, &receipt) || receipt.Reason != ReceiptEmptyResponse {
		t.Fatalf("err = %v", err)
	}
	for _, s := range []*StateManager{f.state, f.reload()} {
		if s.ReplySentChunks("empty-body") != 0 || !s.ReplyDeliveries["empty-body"].InFlight {
			t.Fatal("uncertainty not persisted")
		}
	}
	if _, err := f.client(f.reload()).SendMarkdownReply(context.Background(), testChat, "body", ReplyOptions{DeliveryKey: "empty-body"}); err == nil || !strings.Contains(err.Error(), "delivery uncertain") {
		t.Fatalf("replay err = %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts %d", attempts)
	}
}

// from: gateway/tests/wechat/reply-delivery.test.ts: JSON receipt ${JSON.stringify(response)} accepts delivery and prevents replay after restart
func TestWxbJSONReceiptAccepts(t *testing.T) {
	for _, response := range []string{`{}`, `{"errmsg":""}`} {
		t.Run(response, func(t *testing.T) {
			f := newWxbReplyFixture(t)
			f.fail = func(int) (*http.Response, error) { return rawResponse(200, response), nil }
			n, err := f.client(f.state).SendMarkdownReply(context.Background(), testChat, "body", ReplyOptions{DeliveryKey: "receipt-accepted"})
			mustNoErr(t, err)
			if n != 1 || f.state.ReplySentChunks("receipt-accepted") != 1 {
				t.Fatal("not acknowledged")
			}
			f.fail = nil
			n, err = f.client(f.reload()).SendMarkdownReply(context.Background(), testChat, "body", ReplyOptions{DeliveryKey: "receipt-accepted"})
			mustNoErr(t, err)
			if n != 1 || len(f.texts()) != 1 {
				t.Fatal("replayed")
			}
		})
	}
}

// from: gateway/tests/wechat/reply-delivery.test.ts: confirmed send followed by failed checkpoint remains uncertain in memory and on disk
func TestWxbFailedConfirmCheckpoint(t *testing.T) {
	f := newWxbReplyFixture(t)
	// The confirmation's checkpoint fails: the write that would record an acknowledged chunk.
	f.state.failSave = func() error {
		if f.state.ReplyDeliveries["checkpoint"].AcknowledgedChunks > 0 {
			return errors.New("checkpoint failed")
		}
		return nil
	}
	c := f.client(f.state)
	if _, err := c.SendMarkdownReply(context.Background(), testChat, "body", ReplyOptions{DeliveryKey: "checkpoint"}); err == nil || !strings.Contains(err.Error(), "checkpoint failed") {
		t.Fatalf("err = %v", err)
	}
	f.state.failSave = nil
	if _, err := c.SendMarkdownReply(context.Background(), testChat, "body", ReplyOptions{DeliveryKey: "checkpoint"}); err == nil || !strings.Contains(err.Error(), "delivery uncertain") {
		t.Fatalf("replay err = %v", err)
	}
	if !f.reload().ReplyDeliveries["checkpoint"].InFlight || len(f.texts()) != 1 {
		t.Fatal("uncertainty lost")
	}
}

// from: gateway/tests/wechat/reply-delivery.test.ts: changed reply body, recipient or chunk budget fails closed
func TestWxbChangedPlanFailsClosed(t *testing.T) {
	f := newWxbReplyFixture(t)
	c := f.client(f.state)
	_, err := c.SendMarkdownReply(context.Background(), testChat, "body", ReplyOptions{DeliveryKey: "fixed", MaxCharsPerChunk: 2})
	mustNoErr(t, err)
	for _, change := range []struct {
		chat, body string
		budget     int
	}{{testChat, "else", 2}, {testChat, "body", 3}, {"other", "body", 2}} {
		if _, err := c.SendMarkdownReply(context.Background(), change.chat, change.body, ReplyOptions{DeliveryKey: "fixed", MaxCharsPerChunk: change.budget}); err == nil ||
			!strings.Contains(err.Error(), "content or chunk plan changed") {
			t.Fatalf("%+v: %v", change, err)
		}
	}
	if len(f.texts()) != 2 {
		t.Fatalf("sent %d", len(f.texts()))
	}
}

// from: gateway/tests/wechat/reply-delivery.test.ts: typing failure leaves formatted body delivery available
func TestWxbTypingFailureLeavesDelivery(t *testing.T) {
	f := newWxbReplyFixture(t)
	c := newTestClient(t, f.state, f.loop, fakeHTTP(func(req *http.Request, body []byte) (*http.Response, error) {
		if endpoint(req) != "sendmessage" {
			return nil, errors.New("private-ticket-fetch-error")
		}
		return f.fetch(req, body)
	}))
	n, err := c.SendMarkdownReply(context.Background(), testChat, "**原文**", ReplyOptions{DeliveryKey: "typing-failed"})
	mustNoErr(t, err)
	// Verbatim (E1): the Markdown goes out as written.
	if n != 1 || strings.Join(f.texts(), "|") != "**原文**" {
		t.Fatalf("sent %q", f.texts())
	}
}

// from: gateway/tests/wechat/reply-delivery.test.ts: formatted body delivery completes while ${endpoint} remains unresolved
func TestWxbDeliveryCompletesWhileTypingBlocked(t *testing.T) {
	for _, blocked := range []string{"getconfig", "sendtyping"} {
		t.Run(blocked, func(t *testing.T) {
			f := newWxbReplyFixture(t)
			gate, began := make(chan struct{}), make(chan struct{})
			var once sync.Once
			c := newTestClient(t, f.state, f.loop, fakeHTTP(func(req *http.Request, body []byte) (*http.Response, error) {
				if endpoint(req) == blocked {
					once.Do(func() { close(began) })
					select {
					case <-gate:
					case <-req.Context().Done():
					}
				}
				return f.fetch(req, body)
			}))
			key := "background-" + blocked
			done := make(chan error, 1)
			go func() {
				_, err := c.SendMarkdownReply(context.Background(), testChat, "**原文**", ReplyOptions{DeliveryKey: key})
				done <- err
			}()
			<-began
			defer close(gate)
			mustNoErr(t, <-done)
			sent := 0
			f.loop.do(func() { sent = f.state.ReplySentChunks(key) })
			if strings.Join(f.texts(), "|") != "**原文**" || sent != 1 {
				t.Fatalf("sent %q, %d chunks", f.texts(), sent)
			}
		})
	}
}

// from: gateway/tests/wechat/reply-delivery.test.ts: frozen result fragments remain verbatim and occupy exactly their planned message
func TestWxbResultFragmentVerbatim(t *testing.T) {
	f := newWxbReplyFixture(t)
	original := "**原文**\n①" + strings.Repeat("正文", 1895)
	if utf16Len(original) > 3800 {
		t.Fatal("fixture too long")
	}
	_, err := f.client(f.state).SendMarkdownReply(context.Background(), testChat, original, ReplyOptions{DeliveryKey: "result-fragment"})
	mustNoErr(t, err)
	if sent := f.texts(); len(sent) != 1 || sent[0] != original {
		t.Fatalf("sent %d messages", len(sent))
	}
}

// from: gateway/tests/wechat/reply-delivery.test.ts: an unknown old Markdown send remains quarantined through the renderer upgrade
func TestWxbUnknownSendStaysQuarantined(t *testing.T) {
	// There is no older renderer to upgrade from (E2): the plan is frozen with
	// the current digest, and its unknown chunk still blocks any replay.
	f := newWxbReplyFixture(t)
	original, key := "**已开始**", "legacy-unknown"
	digest := hashJSON([]any{f.state.LoginFingerprint(), testChat, DefaultMessageMaxChars, original, verbatimDigestVersion, original})
	_, err := f.state.PrepareReplyDelivery(key, digest, 1)
	mustNoErr(t, err)
	mustNoErr(t, f.state.BeginReplyChunk(key, 0))
	if _, err := f.client(f.reload()).SendMarkdownReply(context.Background(), testChat, original, ReplyOptions{DeliveryKey: key}); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("err = %v", err)
	}
	if len(f.texts()) != 0 {
		t.Fatal("quarantined send replayed")
	}
}

// from: gateway/tests/wechat/reply-delivery.test.ts: a legacy verbatim plan resumes its sanitized chunks without changing new deliveries
func TestWxbVerbatimIsTheOnlyPlan(t *testing.T) {
	// Only the second half has a counterpart (E2 has no legacy plans): a
	// verbatim send and a default send of the same text are one plan.
	f := newWxbReplyFixture(t)
	token := "citeturn6search1"
	original := "前" + token + "后 `代码 " + token + "` [文档 " + token + "](https://example.com/" + token + ") https://example.com/raw-" + token
	c := f.client(f.state)
	for i := 0; i < 2; i++ {
		_, err := c.SendMarkdownReply(context.Background(), testChat, original, ReplyOptions{DeliveryKey: "new-verbatim"})
		mustNoErr(t, err)
	}
	if sent := f.texts(); len(sent) != 1 || sent[0] != original {
		t.Fatalf("sent %q", sent)
	}
}

// from: gateway/tests/wechat/reply-delivery.test.ts: multipart timeout bounds an uncooperative send and persists its unknown outcome
func TestWxbReplyTimeoutPersistsUnknown(t *testing.T) {
	f := newWxbReplyFixture(t)
	attempts := 0
	c := newTestClient(t, f.state, f.loop, fakeHTTP(func(req *http.Request, body []byte) (*http.Response, error) {
		if endpoint(req) != "sendmessage" {
			return f.fetch(req, body)
		}
		attempts++
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))
	started := time.Now()
	if _, err := c.SendMarkdownReply(context.Background(), testChat, "body", ReplyOptions{DeliveryKey: "timeout", TimeoutMs: 100}); err == nil || !strings.Contains(err.Error(), "reply timed out") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(started) > 5*time.Second || !f.state.ReplyDeliveries["timeout"].InFlight {
		t.Fatal("timeout not bounded or outcome not persisted")
	}
	if _, err := c.SendMarkdownReply(context.Background(), testChat, "body", ReplyOptions{DeliveryKey: "timeout"}); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("replay err = %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts %d", attempts)
	}
}

// from: gateway/tests/wechat/reply-delivery.test.ts: concurrent logical replies serialize individual sends while preserving both contents
func TestWxbConcurrentRepliesSerialize(t *testing.T) {
	f := newWxbReplyFixture(t)
	var mu sync.Mutex
	outstanding, maximum := 0, 0
	c := newTestClient(t, f.state, f.loop, fakeHTTP(func(req *http.Request, body []byte) (*http.Response, error) {
		if endpoint(req) != "sendmessage" {
			return f.fetch(req, body)
		}
		mu.Lock()
		outstanding++
		maximum = max(maximum, outstanding)
		mu.Unlock()
		time.Sleep(time.Millisecond)
		defer func() { mu.Lock(); outstanding--; mu.Unlock() }()
		return f.fetch(req, body)
	}))
	var wg sync.WaitGroup
	for _, r := range []struct{ key, body string }{{"first", "甲\n甲\n甲"}, {"second", "乙\n乙\n乙"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.SendMarkdownReply(context.Background(), testChat, r.body, ReplyOptions{DeliveryKey: r.key, MaxCharsPerChunk: 2})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	join := func(prefix string) string {
		var out []string
		for _, s := range f.texts() {
			if strings.HasPrefix(s, prefix) {
				out = append(out, s)
			}
		}
		return strings.Join(out, "")
	}
	if maximum != 1 || join("甲") != "甲\n甲\n甲" || join("乙") != "乙\n乙\n乙" {
		t.Fatalf("maximum %d sent %q", maximum, f.texts())
	}
}
