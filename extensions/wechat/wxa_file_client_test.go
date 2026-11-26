package wechat

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	wxaReplyFile = []byte("# 完整任务成果\n\n这是下载并验证过的正文。\n")
	wxaReplyMeta = fileMetadataFor("研究成果.md", wxaReplyFile)
)

// wxaMediaFixture is the original file and image client fixture: one chat
// with a fresh window and a fake iLink and CDN.
type wxaMediaFixture struct {
	t     *testing.T
	dir   string
	loop  *loop
	state *StateManager

	mu        sync.Mutex
	calls     []string
	sends     []map[string]any
	sendHook  func(*http.Request) (*http.Response, error)
	upload    func(*http.Request) (*http.Response, error)
	typing    func(endpoint string)
	authCheck func(path string, body map[string]any)
}

func wxaNewMediaFixture(t *testing.T) *wxaMediaFixture {
	f := &wxaMediaFixture{t: t, dir: t.TempDir(), loop: &loop{}}
	f.state = f.restart()
	_, err := f.state.RecordInboundContext("inbound-key", "chat", i64(nowMs()-1000), true, "synthetic-context")
	mustNoErr(t, err)
	return f
}

func (f *wxaMediaFixture) restart() *StateManager {
	s, err := NewStateManager(&Account{Token: "synthetic-private", AccountID: "fixture", UserID: "chat"}, filepath.Join(f.dir, stateFileName), PolicyOptions{})
	mustNoErr(f.t, err)
	return s
}

func (f *wxaMediaFixture) fetch(req *http.Request, body []byte) (*http.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req.URL.Path)
	upload, send, typing, check := f.upload, f.sendHook, f.typing, f.authCheck
	f.mu.Unlock()
	if req.URL.Path == "/c2c/upload" {
		if req.URL.Hostname() != cdnUploadHost || req.Header.Get("Authorization") != "" {
			f.t.Errorf("CDN upload %s with %q", req.URL, req.Header.Get("Authorization"))
		}
		if upload != nil {
			return upload(req)
		}
		return bytesResponse(200, nil, http.Header{"X-Encrypted-Param": {"synthetic-download"}}), nil
	}
	// Typing runs on its own goroutine and may start after a test replaced the token.
	if auth := req.Header.Get("Authorization"); auth != "Bearer synthetic-private" &&
		!(auth == "Bearer replacement" && (endpoint(req) == "getconfig" || endpoint(req) == "sendtyping")) {
		f.t.Errorf("%s without the bot token", req.URL.Path)
	}
	payload := decodeBody(f.t, body)
	switch endpoint(req) {
	case "getconfig", "sendtyping":
		if typing != nil {
			typing(endpoint(req))
		}
		return jsonResponse(200, map[string]any{"ret": 0, "typing_ticket": "synthetic-ticket"}), nil
	case "getuploadurl":
		if check != nil {
			check(req.URL.Path, payload)
		}
		return jsonResponse(200, map[string]any{"ret": 0, "upload_param": "synthetic-upload"}), nil
	}
	msg, _ := payload["msg"].(map[string]any)
	f.mu.Lock()
	f.sends = append(f.sends, msg)
	f.mu.Unlock()
	if send != nil {
		return send(req)
	}
	return jsonResponse(200, map[string]any{"ret": 0}), nil
}

func (f *wxaMediaFixture) client(s *StateManager, maxFileBytes int64) *ILinkClient {
	c, err := NewILinkClient(s, f.loop, ILinkOptions{HTTP: fakeHTTP(f.fetch), MaxFileBytes: maxFileBytes})
	mustNoErr(f.t, err)
	return c
}

func (f *wxaMediaFixture) set(change func(*wxaMediaFixture)) {
	f.mu.Lock()
	change(f)
	f.mu.Unlock()
}

func (f *wxaMediaFixture) sent() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.sends...)
}

func (f *wxaMediaFixture) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *wxaMediaFixture) count(path string) int {
	n := 0
	for _, c := range f.callList() {
		if c == path {
			n++
		}
	}
	return n
}

// remaining reads the budget on the loop, as the gateway would.
func (f *wxaMediaFixture) remaining() (n int64) {
	f.loop.do(func() { n = f.state.AccountBudget.Remaining })
	return n
}

func (f *wxaMediaFixture) sendFile(c *ILinkClient, meta FileMetadata, o FileReplyOptions) (n int, err error) {
	c.loop.do(func() { n, err = c.sendFileReply(context.Background(), "chat", wxaReplyFile, meta, o) })
	return n, err
}

// wxaCheckUnspent asserts in getuploadurl that an upload alone spent nothing.
func (f *wxaMediaFixture) checkUnspent() {
	f.authCheck = func(_ string, body map[string]any) {
		if body["media_type"] != float64(mediaTypeFile) || body["rawsize"] != float64(len(wxaReplyFile)) {
			f.t.Errorf("authorization %v", body)
		}
		if n := f.remaining(); n != 10 {
			f.t.Errorf("upload alone consumed a send: %d left", n)
		}
	}
}

func wxaSentItem(msg map[string]any) map[string]any {
	return msg["item_list"].([]any)[0].(map[string]any)
}

// from: gateway/tests/wechat/file-client.test.ts: client authorizes FILE upload and confirms its type=4 send against the shared durable ledger
func TestWxaFileClientSendsFile(t *testing.T) {
	f := wxaNewMediaFixture(t)
	f.checkUnspent()
	if n, err := f.sendFile(f.client(f.state, 0), wxaReplyMeta, FileReplyOptions{DeliveryKey: "file"}); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if got := strings.Join(f.callList(), ","); got != "/ilink/bot/getuploadurl,/c2c/upload,/ilink/bot/sendmessage" {
		t.Fatalf("calls %s", got)
	}
	if f.remaining() != 9 {
		t.Fatalf("remaining %d", f.remaining())
	}
	sent := f.sent()[0]
	item := wxaSentItem(sent)
	fileItem := item["file_item"].(map[string]any)
	if sent["context_token"] != "synthetic-context" || item["type"] != float64(ItemTypeFile) || fileItem["file_name"] != wxaReplyMeta.FileName ||
		fileItem["len"] != strconv.Itoa(len(wxaReplyFile)) || fileItem["media"].(map[string]any)["encrypt_type"] != float64(1) {
		t.Fatalf("sent %v", sent)
	}
	if f.restart().ReplyDeliveries["file"].AcknowledgedChunks != 1 {
		t.Fatal("acknowledgement not durable")
	}
	f.authCheck = nil
	mustNoErr(t, wxaErr(f.sendFile(f.client(f.restart(), 0), wxaReplyMeta, FileReplyOptions{DeliveryKey: "file"})))
	if len(f.sent()) != 1 || len(f.callList()) != 3 {
		t.Fatal("completed files upload again after restart")
	}
}

func wxaErr(_ int, err error) error { return err }

// from: gateway/tests/wechat/file-client.test.ts: file upload failure and local size or integrity rejection never consume chat quota
func TestWxaFileClientFailuresKeepQuota(t *testing.T) {
	for _, phase := range []string{"upload", "size", "integrity"} {
		f := wxaNewMediaFixture(t)
		if phase == "upload" {
			f.upload = func(*http.Request) (*http.Response, error) { return bytesResponse(400, nil, nil), nil }
		}
		var max int64
		if phase == "size" {
			max = int64(len(wxaReplyFile) - 1)
		}
		meta := wxaReplyMeta
		if phase == "integrity" {
			meta.SHA256 = strings.Repeat("0", 64)
		}
		if _, err := f.sendFile(f.client(f.state, max), meta, FileReplyOptions{DeliveryKey: phase}); err == nil {
			t.Fatalf("%s: sent", phase)
		}
		if f.remaining() != 10 || len(f.sent()) != 0 || f.state.ReplyDeliveries[phase].InFlight {
			t.Fatalf("%s: quota or intent changed", phase)
		}
		if phase != "upload" && len(f.callList()) != 0 {
			t.Fatalf("%s: network calls %v", phase, f.callList())
		}
	}
}

// from: gateway/tests/wechat/file-client.test.ts: file response loss persists unknown outcome before restart and forbids every replay
func TestWxaFileClientResponseLoss(t *testing.T) {
	f := wxaNewMediaFixture(t)
	f.sendHook = func(*http.Request) (*http.Response, error) { return nil, errors.New("synthetic response loss") }
	if _, err := f.sendFile(f.client(f.state, 0), wxaReplyMeta, FileReplyOptions{DeliveryKey: "unknown"}); err == nil || !strings.Contains(err.Error(), "response loss") {
		t.Fatalf("%v", err)
	}
	if !f.restart().ReplyDeliveries["unknown"].InFlight {
		t.Fatal("unknown outcome not durable")
	}
	if _, err := f.sendFile(f.client(f.restart(), 0), wxaReplyMeta, FileReplyOptions{DeliveryKey: "unknown"}); err == nil || !strings.Contains(err.Error(), "delivery uncertain") {
		t.Fatalf("%v", err)
	}
	if len(f.sent()) != 1 || f.count("/c2c/upload") != 1 || f.restart().AccountBudget.Remaining != 9 {
		t.Fatal("replayed")
	}
}

// from: gateway/tests/wechat/file-client.test.ts: explicit file rejection clears uncertainty and retry preserves deterministic client ID
func TestWxaFileClientRejectionRetries(t *testing.T) {
	f := wxaNewMediaFixture(t)
	f.sendHook = func(*http.Request) (*http.Response, error) { return jsonResponse(200, map[string]any{"ret": -2}), nil }
	c := f.client(f.state, 0)
	if _, err := f.sendFile(c, wxaReplyMeta, FileReplyOptions{DeliveryKey: "rejected"}); err == nil || !strings.Contains(err.Error(), "RejectedRet") {
		t.Fatalf("%v", err)
	}
	if f.state.ReplyDeliveries["rejected"].InFlight {
		t.Fatal("rejection left uncertainty")
	}
	f.set(func(f *wxaMediaFixture) { f.sendHook = nil })
	mustNoErr(t, wxaErr(f.sendFile(c, wxaReplyMeta, FileReplyOptions{DeliveryKey: "rejected"})))
	s := f.sent()
	if s[0]["client_id"] != s[1]["client_id"] || f.state.ReplyDeliveries["rejected"].AcknowledgedChunks != 1 {
		t.Fatalf("client ids %v %v", s[0]["client_id"], s[1]["client_id"])
	}
}

func wxaSetRemaining(t *testing.T, f *wxaMediaFixture, n int64) {
	f.state.AccountBudget.Remaining = n
	mustNoErr(t, f.state.SaveState())
}

// from: gateway/tests/wechat/file-client.test.ts: file and ordinary text sends use one chat lock and cannot overspend the final shared send
func TestWxaFileClientSharedLastSend(t *testing.T) {
	f := wxaNewMediaFixture(t)
	wxaSetRemaining(t, f, 1)
	c := f.client(f.state, 0)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = f.sendFile(c, wxaReplyMeta, FileReplyOptions{DeliveryKey: "file"})
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = c.SendMarkdownReply(context.Background(), "chat", "普通回复优先", ReplyOptions{DeliveryKey: "text"})
	}()
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) || len(f.sent()) != 1 || f.remaining() != 0 {
		t.Fatalf("errors %v, %d sends, %d left", errs, len(f.sent()), f.remaining())
	}
}

// from: gateway/tests/wechat/file-client.test.ts: admission guard is checked before consuming the last non-reserved file send
func TestWxaFileClientGuardBeforeCharge(t *testing.T) {
	f := wxaNewMediaFixture(t)
	wxaSetRemaining(t, f, 4)
	n, err := f.sendFile(f.client(f.state, 0), wxaReplyMeta, FileReplyOptions{DeliveryKey: "guard",
		ShouldContinue: func() bool { return f.state.AccountBudget.Remaining > 3 }})
	if err != nil || n != 1 || f.remaining() != 3 || f.state.ReplyDeliveries["guard"].InFlight {
		t.Fatalf("%d %v, %d left", n, err, f.remaining())
	}
}

// from: gateway/tests/wechat/file-client.test.ts: file and text cannot overlap sendmessage requests for the same chat
func TestWxaFileClientNoOverlap(t *testing.T) {
	f := wxaNewMediaFixture(t)
	var active, maximum atomic.Int32
	f.sendHook = func(*http.Request) (*http.Response, error) {
		n := active.Add(1)
		for m := maximum.Load(); n > m && !maximum.CompareAndSwap(m, n); m = maximum.Load() {
		}
		time.Sleep(5 * time.Millisecond)
		active.Add(-1)
		return jsonResponse(200, map[string]any{"ret": 0}), nil
	}
	c := f.client(f.state, 0)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		mustNoErr(t, wxaErr(f.sendFile(c, wxaReplyMeta, FileReplyOptions{DeliveryKey: "file"})))
	}()
	go func() {
		defer wg.Done()
		mustNoErr(t, wxaErr(c.SendMarkdownReply(context.Background(), "chat", "普通回复", ReplyOptions{DeliveryKey: "text"})))
	}()
	wg.Wait()
	if maximum.Load() != 1 || len(f.sent()) != 2 || f.remaining() != 8 {
		t.Fatalf("max %d, %d sends, %d left", maximum.Load(), len(f.sent()), f.remaining())
	}
}

// from: gateway/tests/wechat/file-client.test.ts: new context or renewed window during CDN upload cancels the old file before final submission
func TestWxaFileClientChangeDuringUpload(t *testing.T) {
	for _, mutation := range []string{"context", "window", "account"} {
		f := wxaNewMediaFixture(t)
		f.upload = func(*http.Request) (*http.Response, error) {
			f.loop.do(func() {
				switch mutation {
				case "context":
					f.state.SetContextToken("chat", "new-context")
				case "window":
					_, err := f.state.RecordInboundContext("new-inbound", "chat", i64(nowMs()-500), true, "")
					mustNoErr(t, err)
				case "account":
					f.state.Account.Token = "replacement-token"
				}
			})
			return bytesResponse(200, nil, http.Header{"X-Encrypted-Param": {"synthetic-download"}}), nil
		}
		if _, err := f.sendFile(f.client(f.state, 0), wxaReplyMeta, FileReplyOptions{DeliveryKey: mutation}); err == nil || !strings.Contains(err.Error(), "reply interrupted") {
			t.Fatalf("%s: %v", mutation, err)
		}
		if len(f.sent()) != 0 || f.remaining() != 10 || f.state.ReplyDeliveries[mutation].InFlight {
			t.Fatalf("%s: sent or charged", mutation)
		}
	}
}

// from: gateway/tests/wechat/file-client.test.ts: shared admission policy can yield a file after upload without consuming quota or creating uncertainty
func TestWxaFileClientYieldsAfterUpload(t *testing.T) {
	f := wxaNewMediaFixture(t)
	c := f.client(f.state, 0)
	c.SetBeforeSendCheck(func(chat, key string) error {
		if chat != "chat" || key != "priority-file" {
			t.Errorf("check %s %s", chat, key)
		}
		return deferred(DeferBudgetExhausted, "priority reminder pending")
	})
	_, err := f.sendFile(c, wxaReplyMeta, FileReplyOptions{DeliveryKey: "priority-file"})
	if _, ok := asDeferred(err); !ok {
		t.Fatalf("%v", err)
	}
	if f.count("/c2c/upload") != 1 || len(f.sent()) != 0 || f.remaining() != 10 || f.restart().ReplyDeliveries["priority-file"].InFlight {
		t.Fatal("yield spent quota or left uncertainty")
	}
	c.SetBeforeSendCheck(func(string, string) error { return nil })
	mustNoErr(t, wxaErr(f.sendFile(c, wxaReplyMeta, FileReplyOptions{DeliveryKey: "priority-file"})))
	if len(f.sent()) != 1 {
		t.Fatal("not sent after the yield")
	}
}
