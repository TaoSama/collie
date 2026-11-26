package wechat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
)

const rtcHour = int64(60 * 60 * 1000)

func rtcReloadHarness(t *testing.T, h *harness, policy PolicyOptions) *StateManager {
	t.Helper()
	s, err := NewStateManager(h.state.Account, h.state.StatePath, policy)
	mustNoErr(t, err)
	return s
}

func rtcSetWindow(s *StateManager, chat string, refreshedAt int64) {
	w := s.ContextWindows[chat]
	w.RefreshedAt = refreshedAt
	s.ContextWindows[chat] = w
}

func rtcRenewal(c *ILinkClient, s *StateManager) *RenewalNotifier {
	return NewRenewalNotifier(context.Background(), s, func(ctx context.Context, chat, text string, o ReplyOptions) (int, error) {
		n, _, err := c.sendMarkdownReply(ctx, chat, text, o)
		return n, err
	}, nil, nil)
}

func rtcDeliverRenewal(t *testing.T, l *loop, r *RenewalNotifier, chats ...string) int {
	t.Helper()
	var sent int
	var err error
	l.do(func() { sent, _, err = r.DeliverOnce(chats) })
	mustNoErr(t, err)
	return sent
}

// from: gateway/tests/runtime/account-context.test.ts: concurrent sends to different recipients share exactly ten charges and preserve each recipient token
func TestRtcAccountConcurrentSendsShareTen(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.do(func() {
		_, err := h.state.RecordInboundContext("inbound-b", "b", i64(nowMs()), true, "context-b")
		mustNoErr(t, err)
	})
	var wg sync.WaitGroup
	errs := make([]error, 24)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			chat := "b"
			if i%2 == 1 {
				chat = testChat
			}
			errs[i] = h.ilink.SendSingleText(context.Background(), chat, "message-"+jsonText(i), "")
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if _, d := asDeferred(err); !d {
			t.Fatalf("send failed with %v", err)
		}
	}
	sends := h.sent()
	if ok != 10 || len(sends) != 10 {
		t.Fatalf("%d fulfilled, %d sent", ok, len(sends))
	}
	for _, m := range sends {
		want := testContext
		if m["to_user_id"] == "b" {
			want = "context-b"
		}
		if m["context_token"] != want {
			t.Fatalf("send to %v used token %v", m["to_user_id"], m["context_token"])
		}
	}
	restarted := rtcReloadHarness(t, h, PolicyOptions{})
	if restarted.RemainingSendBudget(testChat) != 0 || restarted.RemainingSendBudget("b") != 0 {
		t.Fatal("restart refilled the shared budget")
	}
	raw, err := os.ReadFile(h.state.StatePath)
	mustNoErr(t, err)
	var disk map[string]json.RawMessage
	mustNoErr(t, json.Unmarshal(raw, &disk))
	if _, ok := disk["context_budgets"]; ok {
		t.Fatal("per-chat quota allocations written")
	}
	var budget AccountSendBudget
	var windows map[string]ChatContextWindow
	mustNoErr(t, json.Unmarshal(disk["account_send_budget"], &budget))
	mustNoErr(t, json.Unmarshal(disk["context_windows"], &windows))
	if budget.Remaining != 0 || windows["b"].LastInboundKey != "inbound-b" {
		t.Fatalf("budget %+v windows %+v", budget, windows)
	}
}

// from: gateway/tests/runtime/account-context.test.ts: a genuine inbound refreshes the shared allowance, but another recipient's older input or replay cannot refill it
func TestRtcAccountGenuineInboundRefreshes(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	s, latest := h.state, nowMs()
	h.do(func() {
		_, err := s.RecordInboundContext("new-b", "b", i64(latest), true, "context-b")
		mustNoErr(t, err)
		for i := 0; i < 10; i++ {
			chat := "b"
			if i%2 == 1 {
				chat = testChat
			}
			mustNoErr(t, s.ConsumeSendBudget(chat, SendFinal))
		}
		if ok, _ := s.RecordInboundContext("older-c", "c", i64(latest-1), true, "context-c"); !ok || s.RemainingSendBudget("c") != 0 {
			t.Fatal("older input refilled the budget")
		}
	})
	r := rtcReloadHarness(t, h, PolicyOptions{})
	if ok, _ := r.RecordInboundContext("new-b", "b", i64(latest), true, "replayed-token"); ok || r.RemainingSendBudget(testChat) != 0 {
		t.Fatal("replay refilled the budget")
	}
	_, err := r.RecordInboundContext("newest-c", "c", i64(latest+1), true, "new-context-c")
	mustNoErr(t, err)
	if r.RemainingSendBudget(testChat) != 10 || r.RemainingSendBudget("b") != 10 || r.ContextTokens["b"] != "context-b" || r.ContextTokens["c"] != "new-context-c" {
		t.Fatalf("after newest: %d %d %v", r.RemainingSendBudget(testChat), r.RemainingSendBudget("b"), r.ContextTokens)
	}
}

// from: gateway/tests/runtime/account-context.test.ts: refreshing the account budget cannot revive an expired recipient token window
func TestRtcAccountRefreshCannotReviveExpiredWindow(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	s := h.state
	h.do(func() {
		rtcSetWindow(s, testChat, nowMs()-23*rtcHour)
		_, err := s.RecordInboundContext("fresh-b", "b", i64(nowMs()), true, "context-b")
		mustNoErr(t, err)
		if s.RemainingSendBudget("b") != 10 || s.CanSendToChat(testChat, SendFinal) {
			t.Fatal("expired window revived")
		}
		if rtcReason(s.ConsumeSendBudget(testChat, SendFinal)) != DeferWindowExpired {
			t.Fatal("charge not deferred as window_expired")
		}
		if s.RemainingSendBudget("b") != 10 {
			t.Fatal("refused charge spent the budget")
		}
	})
}

// from: gateway/tests/runtime/account-context.test.ts: one account reminder across interested recipients survives restart and uses the remaining final send
func TestRtcAccountOneReminderSurvivesRestart(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	s, now := h.state, nowMs()
	refreshedAt := now - 22*rtcHour
	h.do(func() {
		_, err := s.RecordInboundContext("inbound-b", "b", i64(now), true, "context-b")
		mustNoErr(t, err)
		rtcSetWindow(s, testChat, refreshedAt)
		rtcSetWindow(s, "b", refreshedAt)
		s.AccountBudget.RefreshedAt, s.AccountBudget.Remaining = refreshedAt, 2
	})
	if sent := rtcDeliverRenewal(t, h.loop, rtcRenewal(h.ilink, s), testChat, "b"); sent != 1 || len(h.sent()) != 1 {
		t.Fatalf("sent %d, %d messages", sent, len(h.sent()))
	}
	r := rtcReloadHarness(t, h, PolicyOptions{})
	l := &loop{}
	c := newTestClient(t, r, l, h.http)
	if sent := rtcDeliverRenewal(t, l, rtcRenewal(c, r), "b", testChat); sent != 0 {
		t.Fatal("reminder sent again after restart")
	}
	if r.RemainingSendBudget("b") != 1 {
		t.Fatalf("remaining = %d", r.RemainingSendBudget("b"))
	}
}

// rtcLegacyDisk replaces the account context with older per-chat ledgers.
func rtcLegacyDisk(t *testing.T, path string, budgets map[string]any, watermark map[string]any) {
	rtcEditDisk(t, path, func(d map[string]any) {
		delete(d, "context_windows")
		delete(d, "account_send_budget")
		d["context_budgets"] = budgets
		if watermark != nil {
			d["inbound_watermarks"].(map[string]any)[testChat] = watermark
		}
	})
}

// from: gateway/tests/runtime/account-context.test.ts: single legacy ledger retains spent quota and clock skew through migration and another restart
func TestRtcAccountSingleLegacyLedger(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	now := nowMs()
	rtcLegacyDisk(t, h.state.StatePath, map[string]any{testChat: map[string]any{"refreshedAt": now, "remaining": 4, "grantedBudget": 10,
		"progressSent": 2, "lastInboundKey": "old"}}, map[string]any{"createdAtMs": now + 100, "keys": []string{"old"}})
	m := rtcReloadHarness(t, h, PolicyOptions{})
	if m.RemainingSendBudget(testChat) != 4 || m.AccountBudget.InboundCreatedAt != now+100 {
		t.Fatalf("migrated %+v", m.AccountBudget)
	}
	mustNoErr(t, m.ConsumeSendBudget(testChat, SendFinal))
	if rtcReloadHarness(t, h, PolicyOptions{}).RemainingSendBudget(testChat) != 3 {
		t.Fatal("charge after migration lost")
	}
	_, err := m.RecordInboundContext("older-b", "b", i64(now+1), true, "context-b")
	mustNoErr(t, err)
	if m.RemainingSendBudget("b") != 3 {
		t.Fatalf("older inbound refilled: %d", m.RemainingSendBudget("b"))
	}
}

// from: gateway/tests/runtime/account-context.test.ts: multiple legacy ledgers combine known spending conservatively without multiplying account grants
func TestRtcAccountMultipleLegacyLedgers(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	now := nowMs()
	rtcLegacyDisk(t, h.state.StatePath, map[string]any{
		testChat: map[string]any{"refreshedAt": now - 10, "remaining": 7, "grantedBudget": 10, "progressSent": 1, "lastInboundKey": "old-a"},
		"b":      map[string]any{"refreshedAt": now, "remaining": 8, "grantedBudget": 10, "progressSent": 0, "lastInboundKey": "old-b"},
	}, nil)
	m := rtcReloadHarness(t, h, PolicyOptions{})
	if m.RemainingSendBudget(testChat) != 5 || m.RemainingSendBudget("b") != 5 {
		t.Fatalf("remaining %d %d", m.RemainingSendBudget(testChat), m.RemainingSendBudget("b"))
	}
	mustNoErr(t, m.SaveState())
	if rtcReloadHarness(t, h, PolicyOptions{}).RemainingSendBudget(testChat) != 5 {
		t.Fatal("migration not saved")
	}
	_, err := m.RecordInboundContext("real-inbound", "b", i64(now+1), true, "context-b")
	mustNoErr(t, err)
	if m.RemainingSendBudget(testChat) != 10 {
		t.Fatalf("real inbound gave %d", m.RemainingSendBudget(testChat))
	}
}

// from: gateway/tests/runtime/account-context.test.ts: legacy migration honors a lower configured allowance and never tops up a higher configured allowance
func TestRtcAccountLegacyMigrationAllowance(t *testing.T) {
	for _, c := range [][4]int64{{10, 8, 5, 3}, {5, 3, 10, 3}} {
		h := newHarness(t, harnessOptions{})
		rtcLegacyDisk(t, h.state.StatePath, map[string]any{testChat: map[string]any{"refreshedAt": nowMs(), "remaining": c[1],
			"grantedBudget": c[0], "progressSent": 0, "lastInboundKey": "old"}}, nil)
		if got := rtcReloadHarness(t, h, PolicyOptions{SendBudget: c[2]}).RemainingSendBudget(testChat); got != c[3] {
			t.Fatalf("%v: remaining %d", c, got)
		}
	}
}

// from: gateway/tests/runtime/account-context.test.ts: text submission cannot yield between its quota charge and starting the network request
func TestRtcAccountTextChargeDoesNotYield(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	s := h.state
	var epoch string
	h.do(func() { epoch = s.AccountBudget.LastInboundKey })
	key := "synchronous-text-intent"
	peerDone := make(chan struct{})
	triggered := false
	h.do(func() {
		s.failSave = func() error {
			if s.ReplyDeliveries[key].InFlight && !triggered {
				triggered = true
				// A ready peer inbound waits for the loop, as the microtask waited in the original.
				go func() {
					h.loop.do(func() { _, _ = s.RecordInboundContext("peer-after-intent", "peer", i64(nowMs()), true, "peer-token") })
					close(peerDone)
				}()
			}
			return nil
		}
	})
	_, err := h.ilink.SendMarkdownReply(context.Background(), testChat, "result from the charged generation", ReplyOptions{DeliveryKey: key,
		ShouldContinue: func() bool { return s.AccountBudget.LastInboundKey == epoch }})
	mustNoErr(t, err)
	<-peerDone
	// The transport runs with the loop released, so the request itself is
	// checked: it was built from the charged generation's chat context.
	sends := h.sent()
	if len(sends) != 1 || sends[0]["context_token"] != testContext {
		t.Fatalf("sends = %v", sends)
	}
	h.do(func() {
		s.failSave = nil
		if s.ReplyDeliveries[key].AcknowledgedChunks != 1 || s.AccountBudget.LastInboundKey != "peer-after-intent" || s.RemainingSendBudget("peer") != 10 {
			t.Fatalf("delivery %+v epoch %s", s.ReplyDeliveries[key], s.AccountBudget.LastInboundKey)
		}
	})
}

// from: gateway/tests/runtime/account-context.test.ts: a peer inbound during image upload defers the image without charging the new account generation
func TestRtcAccountPeerInboundDuringImageUpload(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	s := h.state
	png, _ := hex.DecodeString("89504e470d0a1a0a00000000")
	sum := sha256.Sum256(png)
	image := ImageAttachment{ID: "epoch-image", Mime: "image/png", Size: len(png), SHA256: hex.EncodeToString(sum[:])}
	uploaded, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	gate := true
	var mu sync.Mutex
	var sends []map[string]any
	client := newTestClient(t, s, h.loop, fakeHTTP(func(req *http.Request, body []byte) (*http.Response, error) {
		switch {
		case req.URL.Path == "/c2c/upload":
			mu.Lock()
			wait := gate
			mu.Unlock()
			if wait {
				once.Do(func() { close(uploaded) })
				<-release
			}
			return bytesResponse(200, nil, http.Header{"X-Encrypted-Param": {"synthetic-download"}}), nil
		case strings.HasSuffix(req.URL.Path, "getuploadurl"):
			return jsonResponse(200, map[string]any{"ret": 0, "upload_param": "synthetic-upload"}), nil
		case strings.HasSuffix(req.URL.Path, "getconfig"):
			return jsonResponse(200, map[string]any{"ret": 0, "typing_ticket": "synthetic-ticket"}), nil
		case strings.HasSuffix(req.URL.Path, "sendtyping"):
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		}
		mu.Lock()
		sends = append(sends, decodeBody(t, body)["msg"].(map[string]any))
		mu.Unlock()
		return jsonResponse(200, map[string]any{"ret": 0, "errcode": 0}), nil
	}))
	send := func() (int, error) {
		var n int
		var err error
		h.do(func() {
			n, err = client.sendImageReply(context.Background(), testChat, []ImageAttachment{image}, ImageReplyOptions{DeliveryKey: "peer-epoch-image",
				GetImageBytes: func(context.Context, ImageAttachment) ([]byte, error) { return png, nil }})
		})
		return n, err
	}
	result := make(chan error, 1)
	go func() { _, err := send(); result <- err }()
	<-uploaded
	h.do(func() {
		_, err := s.RecordInboundContext("peer-during-upload", "peer", i64(nowMs()), true, "peer-token")
		mustNoErr(t, err)
		if s.ContextTokens[testChat] != testContext {
			t.Fatal("chat token changed")
		}
	})
	mu.Lock()
	gate = false
	mu.Unlock()
	close(release)
	if err := <-result; err == nil || !strings.Contains(err.Error(), "image reply interrupted before") {
		t.Fatalf("error = %v", err)
	}
	h.do(func() {
		d := s.ReplyDeliveries["peer-epoch-image"]
		if len(sends) != 0 || s.AccountBudget.Remaining != 10 || d.InFlight || d.AcknowledgedChunks != 0 {
			t.Fatalf("sends %d budget %d delivery %+v", len(sends), s.AccountBudget.Remaining, d)
		}
	})
	n, err := send()
	mustNoErr(t, err)
	h.do(func() {
		if n != 1 || len(sends) != 1 || s.AccountBudget.Remaining != 9 {
			t.Fatalf("n %d sends %d remaining %d", n, len(sends), s.AccountBudget.Remaining)
		}
	})
}

// from: gateway/tests/runtime/account-context.test.ts: an interested recipient nearing 23 hours is reminded even after another recipient refreshes the account
func TestRtcAccountReminderAfterPeerRefresh(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	s, now := h.state, nowMs()
	recipientRefreshedAt := now - 22*rtcHour
	h.do(func() {
		rtcSetWindow(s, testChat, recipientRefreshedAt)
		_, err := s.RecordInboundContext("peer-fresh-account", "peer", i64(now), true, "peer-token")
		mustNoErr(t, err)
		s.AccountBudget.Remaining = 8
	})
	r := rtcRenewal(h.ilink, s)
	if sent := rtcDeliverRenewal(t, h.loop, r, testChat, "peer"); sent != 1 {
		t.Fatalf("sent %d", sent)
	}
	sends := h.sent()
	if len(sends) != 1 || sends[0]["to_user_id"] != testChat || sends[0]["context_token"] != testContext {
		t.Fatalf("sends = %v", sends)
	}
	h.do(func() {
		if s.ContextWindows[testChat].RefreshedAt != recipientRefreshedAt || s.AccountBudget.RefreshedAt != now || s.AccountBudget.Remaining != 7 {
			t.Fatalf("window %+v budget %+v", s.ContextWindows[testChat], s.AccountBudget)
		}
	})
	if sent := rtcDeliverRenewal(t, h.loop, r, testChat, "peer"); sent != 0 {
		t.Fatal("reminder sent twice")
	}
}
