package wechat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	wxbDayMs        = 24 * 60 * 60 * 1000
	wxbTypingSecret = "private-ticket-context-and-token"
)

type wxbTypingCall struct {
	endpoint  string
	payload   map[string]any
	token     string
	timeoutMs int64
	ctx       context.Context
}

// wxbTyping is a TypingIndicator over a fake request. Requests that block
// release the loop, as the real client's do.
type wxbTyping struct {
	t       *testing.T
	typing  *TypingIndicator
	loop    *loop
	mu      sync.Mutex
	calls   []wxbTypingCall
	account *typingAccount
	context string
	config  map[string]any
	receipt map[string]any
	hook    func(c wxbTypingCall) (map[string]any, error)
	now     *atomic.Int64
}

func newWxbTyping(t *testing.T, timeoutMs int64) *wxbTyping {
	f := &wxbTyping{t: t, loop: &loop{}, account: &typingAccount{token: "token-a", fingerprint: "account-a"}, context: "context-a",
		config: map[string]any{"ret": 0, "typing_ticket": "ticket-a"}, receipt: map[string]any{}}
	// An own clock, not setNow: typing goroutines of other tests read the global one.
	f.now = &atomic.Int64{}
	f.now.Store(1_000)
	f.typing = newTypingIndicator(typingOptions{timeoutMs: timeoutMs, now: f.now.Load,
		request: func(ctx context.Context, endpoint string, payload map[string]any, token string, timeoutMs int64) (map[string]any, error) {
			c := wxbTypingCall{endpoint: endpoint, payload: payload, token: token, timeoutMs: timeoutMs, ctx: ctx}
			f.calls = append(f.calls, c)
			if f.hook != nil {
				return f.hook(c)
			}
			if strings.HasSuffix(endpoint, "getconfig") {
				return f.config, nil
			}
			return f.receipt, nil
		},
		account:      func() *typingAccount { return f.account },
		contextToken: func(string) string { return f.context },
	})
	return f
}

func (f *wxbTyping) send(chat string, status int) (ok bool) {
	f.loop.do(func() { ok = f.typing.send(context.Background(), chat, status) })
	return ok
}

func (f *wxbTyping) count(suffix string) (n int) {
	f.loop.do(func() {
		for _, c := range f.calls {
			if strings.HasSuffix(c.endpoint, suffix) {
				n++
			}
		}
	})
	return n
}

func (f *wxbTyping) last() (c wxbTypingCall) {
	f.loop.do(func() { c = f.calls[len(f.calls)-1] })
	return c
}

func (f *wxbTyping) total() (n int) {
	f.loop.do(func() { n = len(f.calls) })
	return n
}

// wxbBlock makes a hook wait (with the loop released) for release or its context.
func (f *wxbTyping) wxbBlock(c wxbTypingCall, gate chan struct{}) error {
	var err error
	f.loop.await(func() {
		select {
		case <-gate:
		case <-c.ctx.Done():
			err = context.Cause(c.ctx)
		}
	})
	return err
}

// from: gateway/tests/wechat/typing.test.ts: typing obtains config then sends a signal with the expected protocol
func TestWxbTypingProtocol(t *testing.T) {
	f := newWxbTyping(t, 0)
	if !f.send("chat-a", typingOn) {
		t.Fatal("send failed")
	}
	want := []string{
		`ilink/bot/getconfig {"base_info":{"channel_version":"0.3.0"},"context_token":"context-a","ilink_user_id":"chat-a"} token-a`,
		`ilink/bot/sendtyping {"base_info":{"channel_version":"0.3.0"},"ilink_user_id":"chat-a","status":1,"typing_ticket":"ticket-a"} token-a`,
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls %d", len(f.calls))
	}
	for i, c := range f.calls {
		if got := c.endpoint + " " + jsonText(c.payload) + " " + c.token; got != want[i] {
			t.Fatalf("call %d = %s", i, got)
		}
		if c.timeoutMs <= 0 || c.timeoutMs > 1000 || c.ctx == nil {
			t.Fatalf("call %d budget %d", i, c.timeoutMs)
		}
	}
}

// from: gateway/tests/wechat/typing.test.ts: tickets are reused for start and stop and isolated by user
func TestWxbTypingTicketReuse(t *testing.T) {
	f := newWxbTyping(t, 0)
	if !f.send("chat-a", typingOn) || !f.send("chat-a", typingCancel) || f.send("chat-b", typingCancel) || f.total() != 3 {
		t.Fatal("first round")
	}
	f.config = map[string]any{"ret": 0, "typing_ticket": "ticket-b"}
	if !f.send("chat-b", typingOn) || !f.send("chat-a", typingOn) {
		t.Fatal("second round")
	}
	if f.last().payload["typing_ticket"] != "ticket-a" || f.count("getconfig") != 2 {
		t.Fatalf("ticket %v, %d configs", f.last().payload["typing_ticket"], f.count("getconfig"))
	}
}

// from: gateway/tests/wechat/typing.test.ts: overlapping typing during ${endpoint} is coalesced and later signals can refresh
func TestWxbTypingOverlapCoalesced(t *testing.T) {
	for _, blocked := range []string{"getconfig", "sendtyping"} {
		t.Run(blocked, func(t *testing.T) {
			f := newWxbTyping(t, 1000)
			gate, began := make(chan struct{}), make(chan struct{})
			var once sync.Once
			f.hook = func(c wxbTypingCall) (map[string]any, error) {
				if strings.HasSuffix(c.endpoint, blocked) {
					once.Do(func() { close(began) })
					if err := f.wxbBlock(c, gate); err != nil {
						return nil, err
					}
				}
				if strings.HasSuffix(c.endpoint, "getconfig") {
					return f.config, nil
				}
				return f.receipt, nil
			}
			original := make(chan bool, 1)
			go func() { original <- f.send("chat", typingOn) }()
			<-began
			count := f.total()
			if f.send("chat", typingOn) || f.total() != count {
				t.Fatal("a duplicate started another request")
			}
			close(gate)
			if !<-original {
				t.Fatal("original failed")
			}
			if f.count("getconfig") != 1 || f.count("sendtyping") != 1 {
				t.Fatal("not coalesced")
			}
			f.loop.do(func() { f.hook = nil })
			if !f.send("chat", typingOn) || f.count("getconfig") != 1 {
				t.Fatal("ticket not reused")
			}
			f.now.Add(wxbDayMs)
			f.config = map[string]any{"ret": 0, "typing_ticket": "refreshed-ticket"}
			if !f.send("chat", typingOn) || f.count("getconfig") != 2 || f.last().payload["typing_ticket"] != "refreshed-ticket" {
				t.Fatal("ticket not refreshed")
			}
		})
	}
}

// from: gateway/tests/wechat/typing.test.ts: changing accounts bypasses an old in-flight ${endpoint} and preserves the new ticket
func TestWxbTypingAccountChangeBypasses(t *testing.T) {
	for _, blocked := range []string{"getconfig", "sendtyping"} {
		t.Run(blocked, func(t *testing.T) {
			f := newWxbTyping(t, 1000)
			gate, began := make(chan struct{}), make(chan struct{})
			var once sync.Once
			f.hook = func(c wxbTypingCall) (map[string]any, error) {
				if c.token == "token-a" && strings.HasSuffix(c.endpoint, blocked) {
					once.Do(func() { close(began) })
					if err := f.wxbBlock(c, gate); err != nil {
						return nil, err
					}
					return map[string]any{"ret": -1}, nil
				}
				if strings.HasSuffix(c.endpoint, "getconfig") {
					return map[string]any{"ret": 0, "typing_ticket": "ticket:" + c.token}, nil
				}
				return map[string]any{"ret": 0}, nil
			}
			old := make(chan bool, 1)
			go func() { old <- f.send("chat", typingOn) }()
			<-began
			f.loop.do(func() { f.account = &typingAccount{token: "token-b", fingerprint: "account-b"} })
			if !f.send("chat", typingOn) {
				t.Fatal("the old account suppressed the new account's signal")
			}
			if c := f.last(); c.token != "token-b" || c.payload["typing_ticket"] != "ticket:token-b" {
				t.Fatalf("last call %+v", c)
			}
			close(gate)
			if <-old {
				t.Fatal("old account send succeeded")
			}
			before := f.total()
			if !f.send("chat", typingOn) || f.total() != before+1 {
				t.Fatal("late old rejection backed off the new account")
			}
			if c := f.last(); c.payload["typing_ticket"] != "ticket:token-b" || c.token != "token-b" {
				t.Fatalf("last call %+v", c)
			}
		})
	}
}

// from: gateway/tests/wechat/typing.test.ts: ticket expires at 24 hours without refreshing its lifetime on reuse
func TestWxbTypingTicketExpiry(t *testing.T) {
	f := newWxbTyping(t, 0)
	f.send("chat", typingOn)
	f.now.Add(wxbDayMs - 1)
	if !f.send("chat", typingOn) {
		t.Fatal("ticket expired early")
	}
	f.now.Add(1)
	if f.send("chat", typingCancel) {
		t.Fatal("expired ticket used for stop")
	}
	f.config = map[string]any{"ret": 0, "typing_ticket": "new-ticket"}
	if !f.send("chat", typingOn) || f.last().payload["typing_ticket"] != "new-ticket" || f.count("getconfig") != 2 {
		t.Fatal("ticket not renewed")
	}
}

// from: gateway/tests/wechat/typing.test.ts: changing either fingerprint or token clears all cached users
func TestWxbTypingAccountChangeClearsCache(t *testing.T) {
	f := newWxbTyping(t, 0)
	f.send("chat", typingOn)
	for _, a := range []*typingAccount{{token: "token-a", fingerprint: "account-b"}, {token: "token-b", fingerprint: "account-b"}} {
		f.account = a
		if f.send("chat", typingCancel) || !f.send("chat", typingOn) {
			t.Fatalf("account %+v", a)
		}
	}
	if f.count("getconfig") != 3 {
		t.Fatalf("configs %d", f.count("getconfig"))
	}
	f.account = nil
	if f.send("chat", typingOn) {
		t.Fatal("sent without an account")
	}
	f.account = &typingAccount{token: "token-a", fingerprint: "account-a"}
	if f.send("chat", typingCancel) {
		t.Fatal("cache survived the account change")
	}
}

// from: gateway/tests/wechat/typing.test.ts: account changes during config cannot publish an old account ticket
func TestWxbTypingAccountChangeDuringConfig(t *testing.T) {
	f := newWxbTyping(t, 0)
	f.hook = func(wxbTypingCall) (map[string]any, error) {
		f.account = &typingAccount{token: "new-token", fingerprint: "new-account"}
		return map[string]any{"ret": 0, "typing_ticket": wxbTypingSecret}, nil
	}
	if f.send("chat", typingOn) || f.total() != 1 || f.send("chat", typingCancel) {
		t.Fatal("old account ticket published")
	}
}

// from: gateway/tests/wechat/typing.test.ts: invalid config ${Object.keys(config).join(",")} is not cached and backs off
func TestWxbTypingInvalidConfig(t *testing.T) {
	for i, config := range []map[string]any{{}, {"ret": 1, "typing_ticket": wxbTypingSecret}, {"ret": "0", "typing_ticket": wxbTypingSecret},
		{"ret": 0, "errcode": 1, "typing_ticket": wxbTypingSecret}, {"ret": 0}, {"ret": 0, "typing_ticket": "  "}, {"ret": 0, "typing_ticket": 123}} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			f := newWxbTyping(t, 0)
			f.config = config
			if f.send("chat", typingOn) || f.send("chat", typingOn) || f.total() != 1 {
				t.Fatal("invalid config cached or not backed off")
			}
			f.now.Add(2000)
			f.config = map[string]any{"ret": 0, "typing_ticket": "recovered"}
			if !f.send("chat", typingOn) || f.total() != 3 {
				t.Fatal("not recovered")
			}
		})
	}
}

// from: gateway/tests/wechat/typing.test.ts: rejected send receipt ${JSON.stringify(receipt)} invalidates its ticket
func TestWxbTypingRejectedReceipt(t *testing.T) {
	for i, receipt := range []map[string]any{{"ret": -1}, {"errcode": -14}, {"ret": 0, "errcode": 1}, {"ret": "0"}, {"errcode": nil}} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			f := newWxbTyping(t, 0)
			f.receipt = receipt
			if f.send("chat", typingOn) {
				t.Fatal("rejected receipt accepted")
			}
			f.now.Add(1999)
			if f.send("chat", typingOn) || f.total() != 2 {
				t.Fatal("not backed off")
			}
			f.now.Add(1)
			if f.send("chat", typingCancel) {
				t.Fatal("ticket not invalidated")
			}
			f.receipt = map[string]any{"ret": 0, "errcode": 0}
			if !f.send("chat", typingOn) || f.total() != 4 {
				t.Fatal("not recovered")
			}
		})
	}
}

// from: gateway/tests/wechat/typing.test.ts: transport exceptions and sensitive receipts return false without diagnostics
func TestWxbTypingFailuresSilent(t *testing.T) {
	log := captureLog(t)
	f := newWxbTyping(t, 0)
	f.hook = func(wxbTypingCall) (map[string]any, error) { return nil, errors.New(wxbTypingSecret) }
	if f.send("chat", typingOn) {
		t.Fatal("exception accepted")
	}
	f.now.Add(2000)
	f.hook = nil
	f.receipt = map[string]any{"ret": 1, "errmsg": wxbTypingSecret}
	if f.send("chat", typingOn) || log.Len() != 0 {
		t.Fatalf("sensitive receipt accepted or logged: %q", log.String())
	}
}

// from: gateway/tests/wechat/typing.test.ts: missing account or context performs no requests
func TestWxbTypingMissingAccountOrContext(t *testing.T) {
	f := newWxbTyping(t, 0)
	f.account = nil
	if f.send("chat", typingOn) {
		t.Fatal("sent without account")
	}
	f.account = &typingAccount{token: "token", fingerprint: "account"}
	f.context = ""
	if f.send("chat", typingOn) || f.total() != 0 {
		t.Fatal("requests without context")
	}
}

// from: gateway/tests/wechat/typing.test.ts: overall timeout bounds an abort-ignoring ${blockedEndpoint} and discards late completion
func TestWxbTypingOverallTimeout(t *testing.T) {
	// Go requests end with their context, so the blocked fake waits on it;
	// a late completion therefore cannot reach the indicator at all.
	for _, blocked := range []string{"getconfig", "sendtyping"} {
		t.Run(blocked, func(t *testing.T) {
			f := newWxbTyping(t, 20)
			never := make(chan struct{})
			f.hook = func(c wxbTypingCall) (map[string]any, error) {
				if strings.HasSuffix(c.endpoint, blocked) {
					if err := f.wxbBlock(c, never); err != nil {
						return nil, err
					}
				}
				return map[string]any{"ret": 0, "typing_ticket": "ticket"}, nil
			}
			started := time.Now()
			if f.send("chat", typingOn) || time.Since(started) > 500*time.Millisecond {
				t.Fatal("not bounded")
			}
			if f.last().ctx.Err() == nil {
				t.Fatal("blocked request not aborted")
			}
			count := f.total()
			if f.send("chat", typingOn) || f.total() != count {
				t.Fatal("not backed off")
			}
			f.now.Add(2000)
			if f.send("chat", typingCancel) {
				t.Fatal("stop without a ticket")
			}
			f.hook = nil
			f.config = map[string]any{"ret": 0, "typing_ticket": "ticket"}
			if !f.send("chat", typingOn) || f.count("getconfig") != 2 {
				t.Fatalf("not recovered: %d configs", f.count("getconfig"))
			}
		})
	}
}

// from: gateway/tests/wechat/typing.test.ts: timeout covers the combined config and send budget
func TestWxbTypingCombinedBudget(t *testing.T) {
	f := newWxbTyping(t, 30)
	f.hook = func(c wxbTypingCall) (map[string]any, error) {
		if strings.HasSuffix(c.endpoint, "getconfig") {
			f.loop.await(func() { time.Sleep(15 * time.Millisecond) })
			return map[string]any{"ret": 0, "typing_ticket": "ticket"}, nil
		}
		if c.timeoutMs > 20 {
			t.Errorf("send budget %d", c.timeoutMs)
		}
		return nil, f.wxbBlock(c, make(chan struct{}))
	}
	started := time.Now()
	if f.send("chat", typingOn) || time.Since(started) > 500*time.Millisecond {
		t.Fatal("combined budget not enforced")
	}
}

// from: gateway/tests/wechat/typing.test.ts: a late rejection after timeout cannot invalidate a recovered ticket
func TestWxbTypingLateRejection(t *testing.T) {
	f := newWxbTyping(t, 20)
	f.hook = func(c wxbTypingCall) (map[string]any, error) { return nil, f.wxbBlock(c, make(chan struct{})) }
	if f.send("chat", typingOn) {
		t.Fatal("timed out send succeeded")
	}
	f.now.Add(2000)
	f.hook = nil
	if !f.send("chat", typingOn) || !f.send("chat", typingCancel) || f.count("getconfig") != 2 {
		t.Fatal("recovered ticket lost")
	}
}

// from: gateway/tests/wechat/typing.test.ts: custom budgets cannot exceed 1000 ms
func TestWxbTypingBudgetCap(t *testing.T) {
	f := newWxbTyping(t, 5000)
	if !f.send("chat", typingOn) {
		t.Fatal("send failed")
	}
	for _, c := range f.calls {
		if c.timeoutMs > 1000 {
			t.Fatalf("budget %d", c.timeoutMs)
		}
	}
}

// from: gateway/tests/wechat/typing.test.ts: cache evicts the least recently used user beyond 1000 entries
func TestWxbTypingLRU(t *testing.T) {
	f := newWxbTyping(t, 0)
	for i := 0; i < 1000; i++ {
		if !f.send(fmt.Sprintf("chat-%d", i), typingOn) {
			t.Fatal(i)
		}
	}
	if !f.send("chat-0", typingCancel) || !f.send("chat-new", typingOn) {
		t.Fatal("refresh or insert")
	}
	before := f.total()
	if f.send("chat-1", typingCancel) || f.total() != before || !f.send("chat-0", typingCancel) {
		t.Fatal("wrong entry evicted")
	}
}
