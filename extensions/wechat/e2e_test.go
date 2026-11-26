package wechat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

// fakeILink is iLink for the end-to-end test: one QR login, then a queue
// of polled messages and a record of everything sent.
type fakeILink struct {
	mu         sync.Mutex
	server     *httptest.Server
	queue      [][]map[string]any
	sent       []map[string]any
	scans      int
	polls      int
	expired    bool
	beforePoll func()
}

func newFakeILink(t *testing.T) *fakeILink {
	f := &fakeILink{}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeILink) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/ilink/bot/getupdates" && f.beforePoll != nil {
		f.beforePoll()
	}
	body, _ := io.ReadAll(r.Body)
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch r.URL.Path {
	case "/ilink/bot/get_bot_qrcode":
		reply(map[string]any{"qrcode": "code-1", "qrcode_img_content": f.server.URL + "/qr/code-1"})
	case "/ilink/bot/get_qrcode_status":
		f.scans++
		if f.scans == 1 {
			reply(map[string]any{"status": "scaned"})
			return
		}
		reply(map[string]any{"status": "confirmed", "bot_token": "token-1", "ilink_bot_id": "bot-1", "ilink_user_id": "owner@im.wechat",
			"baseurl": f.server.URL})
	case "/ilink/bot/getupdates":
		f.polls++
		if r.Header.Get("Authorization") != "Bearer token-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if f.expired {
			reply(map[string]any{"ret": -14, "errcode": -14})
			return
		}
		if len(f.queue) == 0 {
			reply(map[string]any{"ret": 0, "msgs": []any{}, "get_updates_buf": "cursor"})
			return
		}
		batch := f.queue[0]
		f.queue = f.queue[1:]
		reply(map[string]any{"ret": 0, "msgs": batch, "get_updates_buf": "cursor-" + time.Now().Format("150405.000")})
	case "/ilink/bot/sendmessage":
		f.sent = append(f.sent, payload)
		reply(map[string]any{"ret": 0, "message_id": len(f.sent)})
	case "/ilink/bot/getconfig":
		reply(map[string]any{"ret": 0, "typing_ticket": "ticket"})
	case "/ilink/bot/sendtyping":
		reply(map[string]any{"ret": 0})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeILink) push(msgs ...map[string]any) {
	f.mu.Lock()
	f.queue = append(f.queue, msgs)
	f.mu.Unlock()
}

func (f *fakeILink) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, p := range f.sent {
		msg := p["msg"].(map[string]any)
		item := msg["item_list"].([]any)[0].(map[string]any)
		if t, ok := item["text_item"].(map[string]any); ok {
			out = append(out, t["text"].(string))
		}
	}
	return out
}

// fakeHost is Collie: it keeps secrets, takes messages and records events.
type fakeHost struct {
	mu              sync.Mutex
	peer            *extproto.Peer
	secret          string
	received        []extproto.Message
	events          []extproto.Delivery
	changed         []string
	beforeLoad      func()
	beforeDelivery  func(json.RawMessage)
	deliveryOffline bool
}

func (h *fakeHost) handle(_ context.Context, method string, params json.RawMessage) (any, error) {
	if method == extproto.MSecretsLoad && h.beforeLoad != nil {
		h.beforeLoad()
	}
	if method == extproto.MChannelDelivery && h.beforeDelivery != nil {
		h.beforeDelivery(params)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch method {
	case extproto.MSecretsLoad:
		return extproto.Secrets{Data: h.secret}, nil
	case extproto.MSecretsSave:
		var s extproto.Secrets
		_ = json.Unmarshal(params, &s)
		h.secret = s.Data
		return struct{}{}, nil
	case extproto.MSecretsDelete:
		h.secret = ""
		return struct{}{}, nil
	case extproto.MChannelDelivery:
		if h.deliveryOffline {
			return nil, extproto.Errorf(extproto.ErrHubUnavailable, "test hub disconnected")
		}
		var d extproto.Delivery
		if err := json.Unmarshal(params, &d); err != nil {
			return nil, err
		}
		h.events = append(h.events, d)
		return struct{}{}, nil
	case extproto.MChannelReceive:
		var r extproto.Receive
		_ = json.Unmarshal(params, &r)
		for _, m := range h.received {
			if m.ID == r.Message.ID {
				return struct{}{}, nil // taken once
			}
		}
		h.received = append(h.received, r.Message)
		return struct{}{}, nil
	}
	return nil, extproto.Errorf(extproto.ErrUnknownMethod, "unknown %s", method)
}

func (h *fakeHost) event(name string, params json.RawMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch name {
	case extproto.EDelivery:
		var d extproto.Delivery
		_ = json.Unmarshal(params, &d)
		h.events = append(h.events, d)
	case extproto.EChanged:
		var c extproto.Changed
		_ = json.Unmarshal(params, &c)
		h.changed = append(h.changed, c.Scope)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startExtension runs serve over pipes with a fake host attached.
func startExtension(t *testing.T, f *fakeILink) (*fakeHost, func()) {
	t.Helper()
	hostToExt, extIn := io.Pipe()
	extOut, extToHost := io.Pipe()
	o := options{http: f.server.Client(), authURL: f.server.URL, trustHost: func(h string) bool { return h == "127.0.0.1" }, pollEvery: 20}
	exited := make(chan int, 1)
	go func() { exited <- serve(context.Background(), hostToExt, extToHost, o) }()
	h := &fakeHost{}
	h.peer = extproto.NewPeer(extOut, extIn, "h", h.handle, h.event)
	go func() { _ = h.peer.Serve(context.Background()) }()
	stop := func() {
		extIn.Close()
		select {
		case code := <-exited:
			if code != 0 {
				t.Errorf("serve exited %d", code)
			}
		case <-time.After(10 * time.Second):
			t.Error("serve did not exit after stdin closed")
		}
		extToHost.Close()
	}
	return h, stop
}

func call[T any](t *testing.T, h *fakeHost, method string, params any) T {
	t.Helper()
	var out T
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.peer.Call(ctx, method, params, &out); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return out
}

// from: extensions/README.md §10: host protocol end to end (login, baseline, receive, send, receipt, delivery)
func TestHostProtocolEndToEnd(t *testing.T) {
	f := newFakeILink(t)
	h, stop := startExtension(t, f)
	defer stop()
	dir := t.TempDir()
	init := call[extproto.InitializeResult](t, h, extproto.MInitialize, extproto.InitializeParams{Protocol: 1,
		Extension: extproto.ExtensionInfo{ID: "wechat", Version: "1.0.0", DataDir: dir}})
	if init.ID != "wechat" || init.Protocol != 1 {
		t.Fatalf("initialize answered %+v", init)
	}
	if st := call[extproto.Status](t, h, extproto.MStatus, extproto.Contribution{Contribution: "wechat"}); st.State != extproto.StateNotSignedIn {
		t.Fatalf("status before login = %+v", st)
	}
	step := call[extproto.LoginStep](t, h, extproto.MLoginStep, map[string]any{"contribution": "wechat"})
	if step.Kind != "qr" || !strings.HasSuffix(step.URL, "/qr/code-1") || step.Interval != qrIntervalSeconds {
		t.Fatalf("first login step = %+v", step)
	}
	step = call[extproto.LoginStep](t, h, extproto.MLoginStep, map[string]any{"contribution": "wechat", "step": step.Step})
	if step.Hint != hintScanned {
		t.Fatalf("scanned step = %+v", step)
	}
	step = call[extproto.LoginStep](t, h, extproto.MLoginStep, map[string]any{"contribution": "wechat", "step": step.Step})
	if !step.Done || step.Account != accountName {
		t.Fatalf("confirmed step = %+v", step)
	}
	h.mu.Lock()
	saved := h.secret
	h.mu.Unlock()
	if saved == "" {
		t.Fatal("login was not saved through connector.secrets.save")
	}
	// The baseline drains the empty history; then a message from the owner and one from someone else.
	eventually(t, "baseline", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.polls >= 2 })
	now := time.Now().UnixMilli()
	f.push(
		map[string]any{"message_type": 1, "from_user_id": "stranger@im.wechat", "client_id": "s-1", "create_time_ms": now, "context_token": "ctx-s",
			"item_list": []any{map[string]any{"type": 1, "text_item": map[string]any{"text": "hello"}}}},
		map[string]any{"message_type": 1, "from_user_id": "owner@im.wechat", "client_id": "m-1", "create_time_ms": now, "context_token": "ctx-1",
			"item_list": []any{map[string]any{"type": 1, "text_item": map[string]any{"text": "总结一下"}}}},
	)
	eventually(t, "receive", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.received) == 1 })
	h.mu.Lock()
	got := h.received[0]
	h.mu.Unlock()
	if got.Text != "总结一下" || got.ChatID != "owner@im.wechat" || got.Chat != accountName || got.SenderName != "你" || got.Owner == nil || !*got.Owner {
		t.Fatalf("received %+v", got)
	}
	answer := strings.Repeat("a", 3799) + "\n" + strings.Repeat("b", 10)
	res := call[extproto.SendResult](t, h, extproto.MChannelSend, extproto.Send{Contribution: "wechat", Ref: "ref-1", Chat: got.ChatID,
		Text: answer, Kind: extproto.KindAnswer, ReplyTo: got.ID})
	if res.Deferred || res.MessageID == "" {
		t.Fatalf("answer send = %+v", res)
	}
	texts := f.texts()
	if len(texts) != 2 || texts[0] != strings.Repeat("a", 3799)+"\n" || texts[1] != strings.Repeat("b", 10) {
		t.Fatalf("WeChat got %d chunks", len(texts))
	}
	eventually(t, "delivery event", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.events) == 1 && h.events[0].Ref == "ref-1" && h.events[0].State == extproto.DeliverySent
	})
	// The same ref again is answered from the first send and sends nothing.
	again := call[extproto.SendResult](t, h, extproto.MChannelSend, extproto.Send{Contribution: "wechat", Ref: "ref-1", Chat: got.ChatID,
		Text: answer, Kind: extproto.KindAnswer})
	if again.MessageID != res.MessageID || len(f.texts()) != 2 {
		t.Fatalf("repeated ref sent again: %+v, %d messages", again, len(f.texts()))
	}
	st := call[extproto.Status](t, h, extproto.MStatus, extproto.Contribution{Contribution: "wechat"})
	// No nickname from iLink: a name for people, the id on hover.
	if st.State != extproto.StateOK || st.Account != accountName || st.AccountID != "owner@im.wechat" || st.Detail["剩余额度"] == nil {
		t.Fatalf("status after send = %+v", st)
	}
	// The login expires: status turns not_signed_in and Collie hears of it.
	f.mu.Lock()
	f.expired = true
	f.mu.Unlock()
	eventually(t, "expired status", func() bool {
		st := call[extproto.Status](t, h, extproto.MStatus, extproto.Contribution{Contribution: "wechat"})
		return st.State == extproto.StateNotSignedIn && st.Error == msgSessionExpired
	})
	h.mu.Lock()
	sawStatus := containsString(h.changed, "status")
	h.mu.Unlock()
	if !sawStatus {
		t.Fatal("no connector.changed{scope:status} after the login expired")
	}
}

func TestHostProtocolSendDuringRestore(t *testing.T) {
	f := newFakeILink(t)
	load, baseline := make(chan struct{}), make(chan struct{})
	f.beforePoll = func() { <-baseline }
	h, stop := startExtension(t, f)
	releaseLoad := sync.OnceFunc(func() { close(load) })
	releaseBaseline := sync.OnceFunc(func() { close(baseline) })
	defer stop()
	defer releaseLoad()
	defer releaseBaseline()
	h.beforeLoad = func() { <-load }
	raw, err := json.Marshal(secrets{Credentials: Credentials{
		Token: "token-1", BotID: "bot-1", UserID: "owner@im.wechat", BaseURL: f.server.URL,
	}})
	mustNoErr(t, err)
	h.secret = base64.StdEncoding.EncodeToString(raw)
	call[extproto.InitializeResult](t, h, extproto.MInitialize, extproto.InitializeParams{Protocol: 1,
		Extension: extproto.ExtensionInfo{ID: "wechat", DataDir: t.TempDir()}})
	send := extproto.Send{Ref: "replayed", Chat: "owner@im.wechat", Text: "hello", Kind: extproto.KindAnswer}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	checkError := func(phase, code string) {
		t.Helper()
		if err := h.peer.Call(ctx, extproto.MChannelSend, send, nil); !extproto.IsCode(err, code) {
			t.Errorf("%s: send error = %v, want %s", phase, err, code)
		}
	}
	checkError("restoring secrets", extproto.ErrNotReady)
	releaseLoad()
	eventually(t, "baseline in progress", func() bool {
		st := call[extproto.Status](t, h, extproto.MStatus, nil)
		return st.Detail["基线"] == "建立中"
	})
	checkError("establishing baseline", extproto.ErrNotReady)
	releaseBaseline()
	eventually(t, "baseline ready", func() bool {
		st := call[extproto.Status](t, h, extproto.MStatus, nil)
		return st.State == extproto.StateOK && st.Detail["基线"] == nil
	})
	eventually(t, "outbox wake", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return containsString(h.changed, "outbox")
	})
	if got := call[extproto.SendResult](t, h, extproto.MChannelSend, send); !got.Deferred {
		t.Fatalf("after restore = %+v, want deferred until owner opens the reply window", got)
	}
	call[struct{}](t, h, extproto.MLogout, nil)
	checkError("after logout", extproto.ErrNotSignedIn)
}

func TestHostProtocolDeliveryAfterReconnect(t *testing.T) {
	f := newFakeILink(t)
	h, stop := startExtension(t, f)
	defer stop()
	h.deliveryOffline = true
	call[extproto.InitializeResult](t, h, extproto.MInitialize, extproto.InitializeParams{Protocol: 1,
		Extension: extproto.ExtensionInfo{ID: "wechat", DataDir: t.TempDir()}})
	step := call[extproto.LoginStep](t, h, extproto.MLoginStep, nil)
	call[extproto.LoginStep](t, h, extproto.MLoginStep, map[string]string{"step": step.Step})
	call[extproto.LoginStep](t, h, extproto.MLoginStep, map[string]string{"step": step.Step})
	eventually(t, "baseline", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.polls >= 2 })
	f.push(map[string]any{"message_type": 1, "from_user_id": "owner@im.wechat", "client_id": "m-1",
		"create_time_ms": nowMs(), "context_token": "ctx-1",
		"item_list": []any{map[string]any{"type": 1, "text_item": map[string]any{"text": "hello"}}}})
	eventually(t, "receive", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.received) == 1 })
	req := extproto.Send{Contribution: "wechat", Ref: "offline", Chat: "owner@im.wechat", Text: "only once", Kind: extproto.KindAnswer}
	res := call[extproto.SendResult](t, h, extproto.MChannelSend, req)
	h.mu.Lock()
	reports := len(h.events)
	h.deliveryOffline = false
	h.mu.Unlock()
	if !res.Deferred || reports != 0 {
		t.Errorf("while disconnected: send = %+v, deliveries = %d", res, reports)
	}
	eventually(t, "acknowledged delivery", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.events) == 1 && h.events[0].Ref == req.Ref &&
			h.events[0].State == extproto.DeliverySent && h.events[0].MessageID != ""
	})
	if got := f.texts(); len(got) != 1 || got[0] != req.Text {
		t.Fatalf("WeChat sends = %v", got)
	}
}

func TestHostProtocolLogoutWithDeliveryOutstanding(t *testing.T) {
	for _, kind := range []string{extproto.KindAnswer, extproto.KindResult} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeILink(t)
			h, stop := startExtension(t, f)
			defer stop()
			entered, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			var once sync.Once
			h.beforeDelivery = func(raw json.RawMessage) {
				var d extproto.Delivery
				if err := json.Unmarshal(raw, &d); err == nil && d.Ref == "outstanding" {
					once.Do(func() { close(entered) })
					<-release
				}
			}
			raw, err := json.Marshal(secrets{Credentials: Credentials{
				Token: "token-1", BotID: "bot-1", UserID: "owner@im.wechat", BaseURL: f.server.URL,
			}})
			mustNoErr(t, err)
			h.secret = base64.StdEncoding.EncodeToString(raw)
			call[extproto.InitializeResult](t, h, extproto.MInitialize, extproto.InitializeParams{Protocol: 1,
				Extension: extproto.ExtensionInfo{ID: "wechat", DataDir: t.TempDir()}})
			eventually(t, "baseline", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.polls >= 2 })
			f.push(map[string]any{"message_type": 1, "from_user_id": "owner@im.wechat", "client_id": "m-1",
				"create_time_ms": nowMs(), "context_token": "ctx-1",
				"item_list": []any{map[string]any{"type": 1, "text_item": map[string]any{"text": "hello"}}}})
			eventually(t, "receive", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.received) == 1 })
			if kind == extproto.KindResult {
				call[extproto.SendResult](t, h, extproto.MChannelSend, extproto.Send{
					Ref: "complete-request", Chat: "owner@im.wechat", Text: "answer", Kind: extproto.KindAnswer,
				})
			}

			sent := make(chan error, 1)
			go func() {
				sent <- h.peer.Call(t.Context(), extproto.MChannelSend, extproto.Send{
					Ref: "outstanding", Chat: "owner@im.wechat", Text: "answer", Kind: kind,
				}, nil)
			}()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("delivery RPC did not start")
			}
			for _, method := range []string{extproto.MStatus, extproto.MLogout} {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				err := h.peer.Call(ctx, method, nil, nil)
				cancel()
				if err != nil {
					t.Fatalf("%s while receipt is waiting: %v", method, err)
				}
			}
			select {
			case err := <-sent:
				mustNoErr(t, err)
			case <-time.After(time.Second):
				t.Fatal("logout did not cancel the delivery wait")
			}
		})
	}
}
