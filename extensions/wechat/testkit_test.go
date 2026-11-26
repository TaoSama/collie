package wechat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

// Test fixtures shared by every ported test. The fake network is an
// http.RoundTripper, the way the original tests replace fetch.

const (
	testChat    = "chat"
	testToken   = "synthetic"
	testAccount = "fixture"
	testContext = "synthetic-context"
)

// fetchFunc answers requests in place of the network.
type fetchFunc func(req *http.Request, body []byte) (*http.Response, error)

func (f fetchFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = b
	}
	resp, err := f(req, body)
	if resp != nil && resp.Request == nil {
		resp.Request = req
	}
	return resp, err
}

func fakeHTTP(f fetchFunc) *http.Client { return &http.Client{Transport: f} }

// jsonResponse is Response.json(value, {status}).
func jsonResponse(status int, v any) *http.Response {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return rawResponse(status, string(b))
}

func rawResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}

func bytesResponse(status int, body []byte, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: -1}
}

// endpoint is the last path segment of an iLink call ("sendmessage").
func endpoint(req *http.Request) string {
	p := req.URL.Path
	return p[strings.LastIndex(p, "/")+1:]
}

func decodeBody(t testing.TB, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	return m
}

// setNow fixes the clock at ms until the test ends; advance moves it.
func setNow(t testing.TB, ms int64) *atomic.Int64 {
	t.Helper()
	var now atomic.Int64
	now.Store(ms)
	previous := clockOverride.Load()
	f := func() int64 { return now.Load() }
	clockOverride.Store(&f)
	t.Cleanup(func() { clockOverride.Store(previous) })
	return &now
}

func i64(v int64) *int64 { return &v }

func testAccountFor(token string) *Account {
	return &Account{Token: token, AccountID: testAccount, UserID: testChat}
}

// newTestState is a StateManager in a temporary directory, for the bound
// account "fixture" whose owner is chat "chat".
func newTestState(t testing.TB, dir string, policy PolicyOptions) *StateManager {
	t.Helper()
	s, err := NewStateManager(testAccountFor(testToken), filepath.Join(dir, stateFileName), policy)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	return s
}

func mustNoErr(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// openWindow gives chat a context token and a fresh window, as an inbound
// message one second ago does.
func openWindow(t testing.TB, s *StateManager, chat string) {
	t.Helper()
	s.SetContextToken(chat, testContext)
	_, err := s.RecordInboundContext("fixture:inbound:"+chat, chat, i64(nowMs()-1000), true, "")
	mustNoErr(t, err)
}

func newTestClient(t testing.TB, s *StateManager, l *loop, hc *http.Client) *ILinkClient {
	t.Helper()
	c, err := NewILinkClient(s, l, ILinkOptions{HTTP: hc})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

// testHost is Collie for gateway tests.
type testHost struct {
	mu         sync.Mutex
	receive    func(extproto.Message) error
	received   []extproto.Message
	deliveries []extproto.Delivery
	changed    []string
}

func (h *testHost) Receive(_ context.Context, m extproto.Message) error {
	h.mu.Lock()
	hook := h.receive
	h.mu.Unlock()
	if hook != nil {
		if err := hook(m); err != nil {
			return err
		}
	}
	h.mu.Lock()
	h.received = append(h.received, m)
	h.mu.Unlock()
	return nil
}

func (h *testHost) Delivery(_ context.Context, d extproto.Delivery) error {
	h.mu.Lock()
	h.deliveries = append(h.deliveries, d)
	h.mu.Unlock()
	return nil
}

func (h *testHost) Changed(scope string) error {
	h.mu.Lock()
	h.changed = append(h.changed, scope)
	h.mu.Unlock()
	return nil
}

func (h *testHost) messages() []extproto.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]extproto.Message(nil), h.received...)
}

func (h *testHost) events() []extproto.Delivery {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]extproto.Delivery(nil), h.deliveries...)
}

// harness is a gateway over a fake iLink: what the original notification
// harness and gateway fixtures build. hooks change one endpoint's answer.
type harness struct {
	t       testing.TB
	dir     string
	loop    *loop
	state   *StateManager
	ilink   *ILinkClient
	gateway *Gateway
	host    *testHost
	http    *http.Client

	mu       sync.Mutex
	sends    []map[string]any // sendmessage msg objects
	typings  []map[string]any
	requests []string // endpoints, in order
	hooks    harnessHooks
}

type harnessHooks struct {
	send    func(msg map[string]any) (*http.Response, error)
	poll    func() (*http.Response, error)
	upload  func(req *http.Request, body []byte) (*http.Response, error)
	cdn     func(req *http.Request, body []byte) (*http.Response, error)
	getconf func() (*http.Response, error)
}

type harnessOptions struct {
	policy   PolicyOptions
	noBase   bool // baseline not established
	noWindow bool // chat has no window yet
	account  *Account
	splitMax int
}

func newHarness(t testing.TB, o harnessOptions) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir(), loop: &loop{}, host: &testHost{}}
	account := o.account
	if account == nil {
		account = testAccountFor(testToken)
	}
	s, err := NewStateManager(account, filepath.Join(h.dir, stateFileName), o.policy)
	mustNoErr(t, err)
	h.state = s
	if !o.noBase {
		mustNoErr(t, s.MarkBaselineEstablished())
	}
	if !o.noWindow {
		openWindow(t, s, testChat)
	}
	h.http = fakeHTTP(h.fetch)
	h.ilink = newTestClient(t, s, h.loop, h.http)
	h.loop.do(func() {
		h.gateway, err = NewGateway(GatewayOptions{State: s, ILink: h.ilink, Host: h.host, DataDir: h.dir, MediaHTTP: h.http,
			SplitMaxChars: o.splitMax, Health: &HealthTracker{}})
	})
	mustNoErr(t, err)
	t.Cleanup(func() { h.gateway.Stop() })
	return h
}

func (h *harness) fetch(req *http.Request, body []byte) (*http.Response, error) {
	name := endpoint(req)
	h.mu.Lock()
	h.requests = append(h.requests, name)
	hooks := h.hooks
	h.mu.Unlock()
	switch {
	case req.URL.Host == cdnUploadHost && strings.HasPrefix(req.URL.Path, "/c2c/"):
		if hooks.cdn != nil {
			return hooks.cdn(req, body)
		}
		if req.Method == http.MethodPost {
			return bytesResponse(200, nil, http.Header{"X-Encrypted-Param": {"download-param"}}), nil
		}
		return bytesResponse(404, nil, nil), nil
	case name == "getupdates":
		if hooks.poll != nil {
			return hooks.poll()
		}
		return jsonResponse(200, map[string]any{"ret": 0, "msgs": []any{}, "get_updates_buf": "new-cursor"}), nil
	case name == "getconfig":
		if hooks.getconf != nil {
			return hooks.getconf()
		}
		return jsonResponse(200, map[string]any{"ret": 0, "typing_ticket": "synthetic-ticket"}), nil
	case name == "sendtyping":
		h.mu.Lock()
		h.typings = append(h.typings, decodeBody(h.t, body))
		h.mu.Unlock()
		return jsonResponse(200, map[string]any{"ret": 0}), nil
	case name == "getuploadurl":
		if hooks.upload != nil {
			return hooks.upload(req, body)
		}
		return jsonResponse(200, map[string]any{"ret": 0, "upload_param": "upload-param"}), nil
	case name == "sendmessage":
		msg, _ := decodeBody(h.t, body)["msg"].(map[string]any)
		h.mu.Lock()
		h.sends = append(h.sends, msg)
		h.mu.Unlock()
		if hooks.send != nil {
			return hooks.send(msg)
		}
		return jsonResponse(200, map[string]any{"ret": 0, "errcode": 0}), nil
	}
	return jsonResponse(404, map[string]any{}), nil
}

func (h *harness) setHooks(f func(*harnessHooks)) {
	h.mu.Lock()
	f(&h.hooks)
	h.mu.Unlock()
}

func (h *harness) sent() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any(nil), h.sends...)
}

// sentTexts are the texts of every sendmessage, in order.
func (h *harness) sentTexts() []string {
	var out []string
	for _, msg := range h.sent() {
		items, _ := msg["item_list"].([]any)
		if len(items) == 0 {
			continue
		}
		if ti, ok := items[0].(map[string]any)["text_item"].(map[string]any); ok {
			out = append(out, ti["text"].(string))
		}
	}
	return out
}

// do runs f on the gateway's loop.
func (h *harness) do(f func()) { h.loop.do(f) }

// deliverNotificationsOnce runs one notification cycle.
func (h *harness) deliverNotificationsOnce() (ResultDispatchSummary, error) {
	var s ResultDispatchSummary
	var err error
	h.do(func() { s, err = h.gateway.deliverNotificationsOnce(context.Background()) })
	return s, err
}

// send is channel.send.
func (h *harness) send(p extproto.Send) (extproto.SendResult, error) {
	if p.Chat == "" {
		p.Chat = testChat
	}
	return h.gateway.HandleSend(context.Background(), p)
}

// pushResult queues a result as Collie does for a timer.
func (h *harness) pushResult(ref, text string) {
	h.t.Helper()
	res, err := h.send(extproto.Send{Ref: ref, Text: text, Kind: extproto.KindResult})
	mustNoErr(h.t, err)
	if !res.Deferred {
		h.t.Fatalf("result was not queued: %+v", res)
	}
}

// inbound is one polled message of chat with text.
func inbound(chat, clientID, text string, createdAt int64) map[string]any {
	return map[string]any{"message_type": 1, "from_user_id": chat, "client_id": clientID, "create_time_ms": createdAt,
		"context_token": testContext, "item_list": []any{map[string]any{"type": 1, "text_item": map[string]any{"text": text}}}}
}

func mustParseISO(t *testing.T, s string) int64 {
	t.Helper()
	tm, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("timestamp %q: %v", s, err)
	}
	return tm.UnixMilli()
}

func rtdEqual(t *testing.T, got, want any, what string) {
	t.Helper()
	if jsonText(got) != jsonText(want) {
		t.Fatalf("%s = %s, want %s", what, jsonText(got), jsonText(want))
	}
}

func itoa(i int) string { return jsonText(i) }
