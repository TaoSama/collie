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

	"github.com/TaoSama/collie/internal/extproto"
)

// wxbPollClient is a client whose state has the cursor "saved-cursor".
func wxbPollClient(t *testing.T, pollTimeoutMs int64, f fetchFunc) (*ILinkClient, *StateManager, *loop) {
	s, err := NewStateManager(&Account{Token: "synthetic-token", AccountID: "fixture"}, filepath.Join(t.TempDir(), "state.json"), PolicyOptions{})
	mustNoErr(t, err)
	s.SyncBuf = "saved-cursor"
	l := &loop{}
	c, err := NewILinkClient(s, l, ILinkOptions{HTTP: fakeHTTP(f), PollTimeoutMs: pollTimeoutMs})
	mustNoErr(t, err)
	return c, s, l
}

func wxbPoll(c *ILinkClient, l *loop) (b PollBatch, err error) {
	l.do(func() { b, err = c.pollBatch(context.Background()) })
	return b, err
}

// from: gateway/tests/wechat/poll-budget.test.ts: server long-poll hints update only the next HTTP timeout and include network margin
func TestWxbPollHintUpdatesNextTimeout(t *testing.T) {
	calls := 0
	var deadlines []time.Duration
	c, _, l := wxbPollClient(t, 0, func(req *http.Request, _ []byte) (*http.Response, error) {
		calls++
		d, _ := req.Context().Deadline()
		deadlines = append(deadlines, time.Until(d))
		if calls == 1 {
			return rawResponse(200, `{"ret":0,"longpolling_timeout_ms":60000}`), nil
		}
		return rawResponse(200, `{"ret":0}`), nil
	})
	if c.PollBudgetMs() != DefaultPollTimeoutMs+PollNetworkMarginMs {
		t.Fatalf("budget %d", c.PollBudgetMs())
	}
	_, err := wxbPoll(c, l)
	mustNoErr(t, err)
	if c.PollBudgetMs() != 60000+PollNetworkMarginMs {
		t.Fatalf("budget %d", c.PollBudgetMs())
	}
	_, err = wxbPoll(c, l)
	mustNoErr(t, err)
	// The timers cannot be mocked here: the request deadlines show the budgets instead.
	within := func(d time.Duration, ms int64) bool { return d <= msDuration(ms) && d > msDuration(ms)-time.Second }
	if !within(deadlines[0], DefaultPollTimeoutMs+PollNetworkMarginMs) || !within(deadlines[1], 60000+PollNetworkMarginMs) {
		t.Fatalf("deadlines %v", deadlines)
	}
}

// from: gateway/tests/wechat/poll-budget.test.ts: invalid and unbounded server hints keep the last usable poll timeout
func TestWxbPollInvalidHints(t *testing.T) {
	hints := []string{"60000", "", "null", "0", "-1", "1.5", `"35000"`, "{}", "[]", "120001", "9007199254740991", "120000"}
	c, _, l := wxbPollClient(t, 15000, func(*http.Request, []byte) (*http.Response, error) {
		hint := hints[0]
		hints = hints[1:]
		if hint == "" {
			return rawResponse(200, `{"ret":0}`), nil
		}
		return rawResponse(200, `{"ret":0,"longpolling_timeout_ms":`+hint+`}`), nil
	})
	for len(hints) > 1 {
		_, err := wxbPoll(c, l)
		mustNoErr(t, err)
		if c.PollBudgetMs() != 60000+PollNetworkMarginMs {
			t.Fatalf("budget %d after %d hints left", c.PollBudgetMs(), len(hints))
		}
	}
	_, err := wxbPoll(c, l)
	mustNoErr(t, err)
	if c.PollBudgetMs() != MaxPollTimeoutMs+PollNetworkMarginMs {
		t.Fatalf("budget %d", c.PollBudgetMs())
	}
}

// from: gateway/tests/wechat/poll-budget.test.ts: expired-account ${codeField}=-14 never clears the cursor or retries
func TestWxbPollExpiredAccount(t *testing.T) {
	for _, field := range []string{"ret", "errcode"} {
		t.Run(field, func(t *testing.T) {
			calls := 0
			c, s, l := wxbPollClient(t, 0, func(_ *http.Request, body []byte) (*http.Response, error) {
				calls++
				if decodeBody(t, body)["get_updates_buf"] != "saved-cursor" {
					t.Error("cursor not sent")
				}
				return jsonResponse(200, map[string]any{field: -14, "errmsg": "session timeout", "longpolling_timeout_ms": MaxPollTimeoutMs}), nil
			})
			_, err := wxbPoll(c, l)
			// errcode -14 is the expired login (added): it is reported as such
			// instead of a generic poll failure.
			if field == "errcode" {
				if !errors.Is(err, ErrSessionExpired) {
					t.Fatalf("err = %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "WeChat getupdates failed") {
				t.Fatalf("err = %v", err)
			}
			if calls != 1 || s.SyncBuf != "saved-cursor" || c.PollBudgetMs() != DefaultPollTimeoutMs+PollNetworkMarginMs {
				t.Fatalf("calls %d cursor %q budget %d", calls, s.SyncBuf, c.PollBudgetMs())
			}
		})
	}
}

// from: gateway/tests/wechat/poll-budget.test.ts: poll carries timestamps without treating malformed values as fresh context
func TestWxbPollTimestamps(t *testing.T) {
	values := []any{nil, 1_800_000_000_000, "1800000000001", 0, -1, "", "NaN", "1.5", map[string]any{}, int64(maxSafeInteger) + 1}
	var msgs []any
	for i, v := range values {
		m := map[string]any{"message_type": 1, "from_user_id": "chat", "client_id": string(rune('0' + i)), "context_token": "new-context",
			"item_list": []any{map[string]any{"type": 1, "text_item": map[string]any{"text": "body"}}}}
		if v != nil {
			m["create_time_ms"] = v
		}
		msgs = append(msgs, m)
	}
	c, s, l := wxbPollClient(t, 0, func(*http.Request, []byte) (*http.Response, error) {
		return jsonResponse(200, map[string]any{"ret": 0, "msgs": msgs}), nil
	})
	b, err := wxbPoll(c, l)
	mustNoErr(t, err)
	if len(b.Messages) != len(values) || b.Messages[0].CreatedAtMs != nil {
		t.Fatalf("messages %d", len(b.Messages))
	}
	for i, want := range []int64{1_800_000_000_000, 1_800_000_000_001, 0} {
		if got := b.Messages[i+1].CreatedAtMs; got == nil || *got != want {
			t.Fatalf("message %d: %v", i+1, got)
		}
	}
	for _, m := range b.Messages[4:] {
		if m.CreatedAtMs != nil {
			t.Fatalf("malformed timestamp kept: %d", *m.CreatedAtMs)
		}
	}
	for _, m := range b.Messages {
		if m.ContextToken != "new-context" {
			t.Fatal("context token lost")
		}
	}
	if len(s.ContextTokens) != 0 {
		t.Fatal("polling mutated context before authorization and durable dedup")
	}
}

// from: gateway/tests/wechat/poll-budget.test.ts: health accepts the maximum hinted poll plus network and checkpoint margins
func TestWxbHealthMaxPollBudget(t *testing.T) {
	// The health file and its checker became connector.status; the phase
	// budget and deadline they enforced are checked on the tracker.
	h := &HealthTracker{}
	h.StartRun()
	budget := int64(MaxPollTimeoutMs + PollNetworkMarginMs + CheckpointMarginMs)
	mustNoErr(t, h.RecordPollStart(budget))
	snap, _ := h.Snapshot()
	start, _ := time.Parse(time.RFC3339Nano, snap.Phase.StartedAt)
	deadline, _ := time.Parse(time.RFC3339Nano, snap.Phase.DeadlineAt)
	if deadline.Sub(start) != msDuration(budget) {
		t.Fatalf("deadline %v after start", deadline.Sub(start))
	}
	if err := h.RecordPollStart(budget + 1); !errors.Is(err, errInvalidPhaseBudget) {
		t.Fatalf("over-budget phase accepted: %v", err)
	}
}

// from: gateway/tests/wechat/poll-budget.test.ts: empty HTTP 200 send body remains explicitly unknown
func TestWxbEmptySendBodyUnknown(t *testing.T) {
	for _, raw := range []string{"", " \n\t "} {
		s := newTestState(t, t.TempDir(), PolicyOptions{})
		openWindow(t, s, testChat)
		c := newTestClient(t, s, &loop{}, fakeHTTP(func(req *http.Request, _ []byte) (*http.Response, error) {
			if endpoint(req) != "sendmessage" {
				return jsonResponse(200, map[string]any{"ret": 0, "typing_ticket": "t"}), nil
			}
			return rawResponse(200, raw), nil
		}))
		var receipt *SendReceiptError
		if err := c.SendSingleText(context.Background(), testChat, "body", ""); !errors.As(err, &receipt) || receipt.Reason != ReceiptEmptyResponse {
			t.Fatalf("%q: %v", raw, err)
		}
	}
}

// from: gateway/tests/wechat/poll-budget.test.ts: inbound identity preserves client IDs and distinguishes adjacent lossless uint64 server IDs
func TestWxbPollLosslessServerIDs(t *testing.T) {
	ids := []string{"18446744073709551614", "18446744073709551615"}
	var parts []string
	for _, id := range ids {
		parts = append(parts, `{"message_type":1,"message_id":`+id+`,"from_user_id":"chat","item_list":[{"type":1,"text_item":{"text":"body"}}]}`)
	}
	parts = append(parts, `{"message_type":1,"message_id":18446744073709551615,"client_id":"existing-client","from_user_id":"chat","item_list":[{"type":1,"text_item":{"text":"body"}}]}`)
	c, _, l := wxbPollClient(t, 0, func(*http.Request, []byte) (*http.Response, error) {
		return rawResponse(200, `{"ret":0,"msgs":[`+strings.Join(parts, ",")+`]}`), nil
	})
	b, err := wxbPoll(c, l)
	mustNoErr(t, err)
	got := []string{}
	keys := map[string]bool{}
	for _, m := range b.Messages {
		got = append(got, m.MessageID)
		keys[m.DeliveryKey] = true
	}
	if strings.Join(got, ",") != "server:"+ids[0]+",server:"+ids[1]+",existing-client" || len(keys) != 3 {
		t.Fatalf("ids %v keys %d", got, len(keys))
	}
}

// from: gateway/tests/wechat/poll-budget.test.ts: server-only inbound replay across cursor, index and restart never forwards or replenishes budget twice
func TestWxbServerIDReplayAcrossRestart(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	var cursors []string
	msg := map[string]any{"message_type": 1, "message_id": "18446744073709551615", "from_user_id": testChat,
		"create_time_ms": nowMs() - 1000, "context_token": testContext,
		"item_list": []any{map[string]any{"type": 1, "text_item": map[string]any{"text": "same original text"}}}}
	h := newHarness(t, harnessOptions{noWindow: true})
	poll := func(body []byte) *http.Response {
		mu.Lock()
		defer mu.Unlock()
		cursors = append(cursors, decodeBody(t, body)["get_updates_buf"].(string))
		polls++
		msgs := []any{msg}
		if polls > 1 {
			msgs = []any{map[string]any{"message_type": 2}, msg}
		}
		return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "cursor-" + string(rune('0'+polls)), "msgs": msgs})
	}
	h.gateway.Stop()
	hc := fakeHTTP(func(req *http.Request, body []byte) (*http.Response, error) {
		if endpoint(req) == "getupdates" {
			return poll(body), nil
		}
		return h.fetch(req, body)
	})
	wxbGateway := func(s *StateManager) (*Gateway, *loop) {
		l := &loop{}
		c := newTestClient(t, s, l, hc)
		var g *Gateway
		var err error
		l.do(func() {
			g, err = NewGateway(GatewayOptions{State: s, ILink: c, Host: h.host, DataDir: h.dir, MediaHTTP: hc, Health: &HealthTracker{}})
		})
		mustNoErr(t, err)
		t.Cleanup(g.Stop)
		return g, l
	}
	g1, l1 := wxbGateway(h.state)
	l1.do(func() { mustNoErr(t, h.state.CommitCursor("cursor-initial")) })
	var out PollCycleSummary
	var err error
	l1.do(func() { out, err = g1.pollOnce(context.Background(), true) })
	mustNoErr(t, err)
	received := h.host.messages()
	if out.ProcessedCount != 1 || len(received) != 1 || !strings.HasSuffix(received[0].ID, `"server:18446744073709551615"]`) {
		t.Fatalf("processed %d, received %v", out.ProcessedCount, received)
	}
	// Collie answers; that is the one send.
	_, err = g1.HandleSend(context.Background(), extproto.Send{Ref: "answer-1", Chat: testChat, Text: "reply", Kind: extproto.KindAnswer, ReplyTo: received[0].ID})
	mustNoErr(t, err)
	l1.do(func() {
		if h.state.AccountBudget.Remaining != 9 {
			t.Errorf("remaining %d", h.state.AccountBudget.Remaining)
		}
		mustNoErr(t, h.state.ConsumeSendBudget(testChat, SendFinal))
	})
	g1.Stop()
	reloaded, err := NewStateManager(testAccountFor(testToken), filepath.Join(h.dir, stateFileName), PolicyOptions{})
	mustNoErr(t, err)
	g, l := wxbGateway(reloaded)
	l.do(func() { out, err = g.pollOnce(context.Background(), true) })
	mustNoErr(t, err)
	if out.PolledCount != 0 || strings.Join(cursors, ",") != "cursor-initial,cursor-1" || len(h.host.messages()) != 1 || len(h.sentTexts()) != 1 {
		t.Fatalf("polled %d cursors %v received %d sends %d", out.PolledCount, cursors, len(h.host.messages()), len(h.sentTexts()))
	}
	l.do(func() {
		if reloaded.AccountBudget.Remaining != 8 || len(reloaded.InboundWatermarks[testChat].Keys) != 1 || reloaded.SyncBuf != "cursor-2" {
			t.Errorf("remaining %d watermark %v cursor %q", reloaded.AccountBudget.Remaining, reloaded.InboundWatermarks[testChat], reloaded.SyncBuf)
		}
	})
}
