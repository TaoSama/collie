package wechat

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/TaoSama/collie/internal/extproto"
)

// gwgReopen is a restart: a new state, client and gateway over the same
// directory, network and host.
func gwgReopen(t *testing.T, h *harness, policy PolicyOptions, splitMax int) *harness {
	t.Helper()
	s, err := NewStateManager(h.state.Account, filepath.Join(h.dir, stateFileName), policy)
	mustNoErr(t, err)
	r := &harness{t: t, dir: h.dir, loop: &loop{}, host: h.host, http: h.http, state: s}
	r.ilink = newTestClient(t, s, r.loop, h.http)
	r.loop.do(func() {
		r.gateway, err = NewGateway(GatewayOptions{State: s, ILink: r.ilink, Host: h.host, DataDir: h.dir, MediaHTTP: h.http,
			SplitMaxChars: splitMax, Health: &HealthTracker{}})
	})
	mustNoErr(t, err)
	t.Cleanup(func() { r.gateway.Stop() })
	return r
}

// gwgAnswerWith makes the host answer each message it receives, the way the
// original Controller returned a reply: the answer's ref is the message id,
// so its outbox key is the message's delivery key. An empty answer sends
// nothing.
func gwgAnswerWith(h *harness, reply func(m extproto.Message) string) {
	h.host.mu.Lock()
	h.host.receive = func(m extproto.Message) error {
		text := reply(m)
		if text == "" {
			return nil
		}
		_, _ = h.gateway.HandleSend(context.Background(), extproto.Send{Ref: m.ID, Chat: m.ChatID, Text: text,
			Kind: extproto.KindAnswer, ReplyTo: m.ID})
		return nil
	}
	h.host.mu.Unlock()
}

// gwgPages answers getupdates with the next page, then empty pages.
type gwgPages struct {
	mu    sync.Mutex
	pages [][]any
	n     int
}

func (p *gwgPages) set(pages ...[]any) {
	p.mu.Lock()
	p.pages = pages
	p.mu.Unlock()
}

func (p *gwgPages) poll() (*http.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	var msgs []any
	if len(p.pages) > 0 {
		msgs, p.pages = p.pages[0], p.pages[1:]
	}
	if msgs == nil {
		msgs = []any{}
	}
	return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "cursor-" + strconv.Itoa(p.n), "msgs": msgs}), nil
}

func gwgPoll(t *testing.T, h *harness) PollCycleSummary {
	t.Helper()
	s, err := gwgPollErr(h)
	mustNoErr(t, err)
	return s
}

func gwgPollErr(h *harness) (PollCycleSummary, error) {
	var s PollCycleSummary
	var err error
	h.do(func() { s, err = h.gateway.pollOnce(context.Background(), true) })
	return s, err
}

func gwgFlush(h *harness, chat string) error {
	var err error
	h.do(func() { err = h.gateway.flushRepliesOnce(context.Background(), chat, chat) })
	return err
}

func gwgReceivedChats(h *harness) []string {
	var out []string
	for _, m := range h.host.messages() {
		out = append(out, m.ChatID)
	}
	return out
}

func gwgEqual(t *testing.T, what string, got, want any) {
	t.Helper()
	if jsonText(got) != jsonText(want) {
		t.Fatalf("%s = %s, want %s", what, jsonText(got), jsonText(want))
	}
}

// gwgCustom is a harness whose network is fetch instead of the harness's
// fake; sends and typings are then whatever fetch records.
func gwgCustom(t *testing.T, o harnessOptions, fetch fetchFunc) *harness {
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
	h.http = fakeHTTP(fetch)
	h.ilink = newTestClient(t, s, h.loop, h.http)
	h.loop.do(func() {
		h.gateway, err = NewGateway(GatewayOptions{State: s, ILink: h.ilink, Host: h.host, DataDir: h.dir, MediaHTTP: h.http,
			SplitMaxChars: o.splitMax, Health: &HealthTracker{}, Diagnostics: o.diagnosis})
	})
	mustNoErr(t, err)
	t.Cleanup(func() { h.gateway.Stop() })
	return h
}

// gwgText is a sendmessage's text, "" for media.
func gwgText(msg map[string]any) string {
	items, _ := msg["item_list"].([]any)
	if len(items) == 0 {
		return ""
	}
	if ti, ok := items[0].(map[string]any)["text_item"].(map[string]any); ok {
		return ti["text"].(string)
	}
	return ""
}

// gwgPutEngine is an outbox reference to a frozen answer of Collie, keyed by
// the message it answers.
func gwgPutEngine(t *testing.T, h *harness, chat, messageID, status, text string) string {
	t.Helper()
	var key string
	h.do(func() {
		key = h.state.ComputeDeliveryKey(chat, messageID)
		if text != "" {
			_, err := h.gateway.answers.put(FrozenReply{Ref: key, ChatID: chat, Kind: extproto.KindAnswer, Text: text,
				Identity: hashJSON([]any{chat, text}), CreatedAt: nowMs()}, nil, nil)
			mustNoErr(t, err)
		}
		item := DeliveryOutboxItem{Kind: "reply", Source: SourceEngine, ChatID: chat, MessageID: messageID, DeliveryKey: key,
			SplitMaxChars: 1000, Status: status}
		if status == OutboxReceipt {
			item.ReceiptStatus = "accepted"
		}
		mustNoErr(t, h.state.PutOutbox(item))
	})
	return key
}

// gwgFlakyHost fails the first deliveries reports it is told to.
type gwgFlakyHost struct {
	*testHost
	mu    sync.Mutex
	fail  func(extproto.Delivery) error
	calls int
}

func (f *gwgFlakyHost) Delivery(d extproto.Delivery) error {
	f.mu.Lock()
	f.calls++
	hook := f.fail
	f.mu.Unlock()
	if hook != nil {
		if err := hook(d); err != nil {
			return err
		}
	}
	return f.testHost.Delivery(d)
}

func (f *gwgFlakyHost) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// gwgFlaky puts a gwgFlakyHost in front of h's gateway.
func gwgFlaky(h *harness) *gwgFlakyHost {
	f := &gwgFlakyHost{testHost: h.host}
	h.do(func() { h.gateway.host = f })
	return f
}

// gwgTraceEvents are the captured message_latency events.
func gwgTraceEvents(b interface{ String() string }) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(b.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["event"] == "message_latency" {
			out = append(out, m)
		}
	}
	return out
}

func gwgStage(events []map[string]any, stage string) map[string]any {
	for _, e := range events {
		if e["stage"] == stage {
			return e
		}
	}
	return nil
}

func gwgStageIndex(events []map[string]any, stage string) int {
	for i, e := range events {
		if e["stage"] == stage {
			return i
		}
	}
	return -1
}
