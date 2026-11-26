package wechat

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const rtcChat = "synthetic-chat"

type rtcStateFixture struct {
	t         *testing.T
	statePath string
	account   *Account
	policy    PolicyOptions
	state     *StateManager
	createdAt int64
}

func rtcNewState(t *testing.T, policy PolicyOptions) *rtcStateFixture {
	t.Helper()
	f := &rtcStateFixture{t: t, statePath: filepath.Join(t.TempDir(), "state.json"), policy: policy,
		account: &Account{Token: "synthetic-token", AccountID: "synthetic-account"}, createdAt: nowMs() - 1000}
	f.state = f.reload()
	return f
}

func (f *rtcStateFixture) load(policy PolicyOptions) (*StateManager, error) {
	return NewStateManager(f.account, f.statePath, policy)
}

func (f *rtcStateFixture) reload() *StateManager { return f.reloadWith(f.policy) }

func (f *rtcStateFixture) reloadWith(policy PolicyOptions) *StateManager {
	f.t.Helper()
	s, err := f.load(policy)
	mustNoErr(f.t, err)
	return s
}

// rtcRecord is recordInboundContext with the TS defaults (grant, no token).
func rtcRecord(t *testing.T, s *StateManager, key, chat string, at *int64) bool {
	t.Helper()
	ok, err := s.RecordInboundContext(key, chat, at, true, "")
	mustNoErr(t, err)
	return ok
}

func rtcReference(key string, patch func(*DeliveryOutboxItem)) DeliveryOutboxItem {
	item := DeliveryOutboxItem{Kind: "reply", DeliveryKey: key, ChatID: rtcChat, MessageID: "synthetic-message-id", SplitMaxChars: 1000, Status: OutboxPending}
	if patch != nil {
		patch(&item)
	}
	return item
}

func rtcFail(s *StateManager, failure error) { s.failSave = func() error { return failure } }

func rtcBudgetOf(s *StateManager) AccountSendBudget {
	if s.AccountBudget == nil {
		return AccountSendBudget{}
	}
	return *s.AccountBudget
}

func rtcDeepCopy[T any](t *testing.T, v T) T {
	t.Helper()
	b, err := json.Marshal(v)
	mustNoErr(t, err)
	var out T
	mustNoErr(t, json.Unmarshal(b, &out))
	return out
}

func rtcSame(t *testing.T, what string, got, want any) {
	t.Helper()
	if jsonText(got) != jsonText(want) {
		t.Fatalf("%s = %s, want %s", what, jsonText(got), jsonText(want))
	}
}

func rtcEditDisk(t *testing.T, path string, edit func(map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	mustNoErr(t, err)
	var disk map[string]any
	mustNoErr(t, json.Unmarshal(raw, &disk))
	edit(disk)
	out, err := json.Marshal(disk)
	mustNoErr(t, err)
	mustNoErr(t, os.WriteFile(path, out, 0o600))
}

func rtcReason(err error) string {
	if d, ok := asDeferred(err); ok {
		return d.Reason
	}
	return ""
}

// from: gateway/tests/runtime/delivery-state.test.ts: send charges persist across restart and a failed charge rolls back both counters
func TestRtcDeliveryChargesPersist(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{})
	s := f.state
	if s.CanSendToChat(rtcChat, SendFinal) {
		t.Fatal("send allowed before any inbound")
	}
	if _, ok := asDeferred(s.ConsumeSendBudget(rtcChat, SendFinal)); !ok {
		t.Fatal("charge without window not deferred")
	}
	rtcRecord(t, s, "inbound-one", rtcChat, i64(f.createdAt))
	mustNoErr(t, s.ConsumeSendBudget(rtcChat, SendProgress))
	if b := rtcBudgetOf(f.reload()); b.Remaining != s.Policy.SendBudget-1 || b.ProgressSent != 1 {
		t.Fatalf("reloaded budget = %+v", b)
	}
	before, beforeBudget := rtcDeepCopy(t, s.ContextWindows), rtcDeepCopy(t, s.AccountBudget)
	failure := errors.New("synthetic charge checkpoint failure")
	rtcFail(s, failure)
	if err := s.ConsumeSendBudget(rtcChat, SendProgress); err != failure {
		t.Fatalf("error = %v", err)
	}
	rtcSame(t, "windows", s.ContextWindows, before)
	rtcSame(t, "reloaded windows", f.reload().ContextWindows, before)
	rtcSame(t, "budget", s.AccountBudget, beforeBudget)
	rtcSame(t, "reloaded budget", f.reload().AccountBudget, beforeBudget)
	s.failSave = nil
	mustNoErr(t, s.ConsumeSendBudget(rtcChat, SendFinal))
	if b := rtcBudgetOf(f.reload()); b.Remaining != s.Policy.SendBudget-2 || b.ProgressSent != 1 {
		t.Fatalf("final budget = %+v", b)
	}
}

// from: gateway/tests/runtime/delivery-state.test.ts: restarting with a lower budget accounts for all sends spent under the old policy
func TestRtcDeliveryLowerBudgetRestart(t *testing.T) {
	five := PolicyOptions{SendBudget: 5}
	for _, spent := range []int64{7, 2} {
		f := rtcNewState(t, PolicyOptions{SendBudget: 10})
		rtcRecord(t, f.state, "first", rtcChat, i64(f.createdAt))
		if g := f.reload().AccountBudget.GrantedBudget; g == nil || *g != 10 {
			t.Fatalf("grant = %v", g)
		}
		for i := int64(0); i < spent; i++ {
			mustNoErr(t, f.state.ConsumeSendBudget(rtcChat, SendFinal))
		}
		restarted := f.reloadWith(five)
		allowed := max(0, 5-spent)
		for count := int64(0); count < allowed; count++ {
			if !restarted.CanSendToChat(rtcChat, SendFinal) {
				t.Fatalf("spent %d: send %d refused", spent, count)
			}
			mustNoErr(t, restarted.ConsumeSendBudget(rtcChat, SendFinal))
			if b := restarted.AccountBudget; b.Remaining != allowed-count-1 || *b.GrantedBudget != 5 {
				t.Fatalf("budget = %+v", b)
			}
			restarted = f.reloadWith(five)
		}
		if restarted.CanSendToChat(rtcChat, SendFinal) || rtcReason(restarted.ConsumeSendBudget(rtcChat, SendFinal)) != DeferBudgetExhausted {
			t.Fatalf("spent %d: budget not exhausted", spent)
		}
		if f.reloadWith(five).CanSendToChat(rtcChat, SendFinal) {
			t.Fatal("reload refilled the budget")
		}
	}
}

// from: gateway/tests/runtime/delivery-state.test.ts: restarting with a higher budget cannot refill an old window but a new inbound grants the new budget
func TestRtcDeliveryHigherBudgetRestart(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{SendBudget: 5})
	ten := PolicyOptions{SendBudget: 10}
	rtcRecord(t, f.state, "first", rtcChat, i64(f.createdAt))
	mustNoErr(t, f.state.ConsumeSendBudget(rtcChat, SendFinal))
	mustNoErr(t, f.state.ConsumeSendBudget(rtcChat, SendFinal))
	restarted := f.reloadWith(ten)
	if b := restarted.AccountBudget; b.Remaining != 3 || *b.GrantedBudget != 5 {
		t.Fatalf("budget = %+v", b)
	}
	for i := 0; i < 3; i++ {
		mustNoErr(t, restarted.ConsumeSendBudget(rtcChat, SendFinal))
		if *restarted.AccountBudget.GrantedBudget != 5 {
			t.Fatal("grant changed")
		}
		restarted = f.reloadWith(ten)
	}
	if restarted.CanSendToChat(rtcChat, SendFinal) {
		t.Fatal("old window refilled")
	}
	if !rtcRecord(t, restarted, "new-window", rtcChat, i64(f.createdAt+1)) {
		t.Fatal("new inbound refused")
	}
	if b := f.reloadWith(ten).AccountBudget; b.Remaining != 10 || *b.GrantedBudget != 10 {
		t.Fatalf("new budget = %+v", b)
	}
}

// from: gateway/tests/runtime/delivery-state.test.ts: invalid persisted initial grants are rejected before restart can authorize sends
func TestRtcDeliveryInvalidGrants(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{})
	rtcRecord(t, f.state, "first", rtcChat, i64(f.createdAt))
	// A JSON null reads as an absent grant in Go (the legacy shape, which
	// never refills); every other invalid grant fails the load.
	for _, invalid := range []any{-1, 0, 0.5, 1, 1001, "10"} {
		rtcEditDisk(t, f.statePath, func(d map[string]any) { d["account_send_budget"].(map[string]any)["grantedBudget"] = invalid })
		if _, err := f.load(f.policy); err == nil || !strings.Contains(err.Error(), "Failed to load gateway state") {
			t.Fatalf("grant %v: error %v", invalid, err)
		}
	}
}

// from: gateway/tests/runtime/delivery-state.test.ts: failed inbound checkpoints roll back budgets, tokens and timestamp watermarks
func TestRtcDeliveryInboundRollback(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{})
	s := f.state
	failure := errors.New("synthetic inbound checkpoint failure")
	rtcFail(s, failure)
	if _, err := s.RecordInboundContext("first", rtcChat, i64(f.createdAt), true, "synthetic-first-context"); err != failure {
		t.Fatalf("error = %v", err)
	}
	if len(s.ContextWindows) != 0 || s.AccountBudget != nil || len(s.InboundWatermarks) != 0 || len(s.ContextTokens) != 0 {
		t.Fatal("failed inbound left state behind")
	}
	s.failSave = nil
	_, err := s.RecordInboundContext("first", rtcChat, i64(f.createdAt), true, "synthetic-first-context")
	mustNoErr(t, err)
	mustNoErr(t, s.ConsumeSendBudget(rtcChat, SendFinal))
	windows, budget := rtcDeepCopy(t, s.ContextWindows), rtcDeepCopy(t, s.AccountBudget)
	watermarks, tokens := rtcDeepCopy(t, s.InboundWatermarks), rtcDeepCopy(t, s.ContextTokens)
	rtcFail(s, failure)
	if _, err := s.RecordInboundContext("second", rtcChat, i64(f.createdAt+1), true, "synthetic-second-context"); err != failure {
		t.Fatalf("error = %v", err)
	}
	r := f.reload()
	rtcSame(t, "windows", s.ContextWindows, windows)
	rtcSame(t, "watermarks", s.InboundWatermarks, watermarks)
	rtcSame(t, "tokens", s.ContextTokens, tokens)
	rtcSame(t, "budget", s.AccountBudget, budget)
	rtcSame(t, "reloaded windows", r.ContextWindows, windows)
	rtcSame(t, "reloaded budget", r.AccountBudget, budget)
	rtcSame(t, "reloaded watermarks", r.InboundWatermarks, watermarks)
	rtcSame(t, "reloaded tokens", r.ContextTokens, tokens)
	s.failSave = nil
	rtcRecord(t, s, "second", rtcChat, i64(f.createdAt+1))
	if f.reload().ContextWindows[rtcChat].LastInboundKey != "second" {
		t.Fatal("second inbound not saved")
	}
}

// from: gateway/tests/runtime/delivery-state.test.ts: changing the account identity or token clears all delivery state
func TestRtcDeliveryAccountChangeClears(t *testing.T) {
	for _, account := range []Account{{Token: "different-synthetic-token", AccountID: "synthetic-account"},
		{Token: "synthetic-token", AccountID: "different-synthetic-account"}} {
		f := rtcNewState(t, PolicyOptions{})
		_, err := f.state.RecordInboundContext("inbound-one", rtcChat, i64(f.createdAt), true, "synthetic-context")
		mustNoErr(t, err)
		mustNoErr(t, f.state.ConsumeSendBudget(rtcChat, SendFinal))
		mustNoErr(t, f.state.PutOutbox(rtcReference("reply-reference", nil)))
		f.account = &account
		changed := f.reload()
		if len(changed.ContextWindows) != 0 || changed.AccountBudget != nil || len(changed.InboundWatermarks) != 0 ||
			len(changed.ContextTokens) != 0 || len(changed.Outbox) != 0 || changed.CanSendToChat(rtcChat, SendFinal) {
			t.Fatalf("state kept for %+v", account)
		}
		mustNoErr(t, changed.SaveState())
		if len(f.reload().ContextWindows) != 0 {
			t.Fatal("windows came back")
		}
	}
}

// from: gateway/tests/runtime/delivery-state.test.ts: only timestamped new inbound messages refresh budget across restart
func TestRtcDeliveryOnlyNewInboundRefreshes(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{})
	s, full := f.state, f.state.Policy.SendBudget
	rtcRecord(t, s, "first", rtcChat, i64(f.createdAt))
	mustNoErr(t, s.ConsumeSendBudget(rtcChat, SendProgress))
	rtcRecord(t, s, "first", rtcChat, i64(f.createdAt))
	if s.AccountBudget.Remaining != full-1 {
		t.Fatal("replay refilled")
	}
	r := f.reload()
	rtcRecord(t, r, "first", rtcChat, i64(f.createdAt))
	if r.AccountBudget.Remaining != full-1 {
		t.Fatal("replay after restart refilled")
	}
	rtcRecord(t, r, "same-time-new-key", rtcChat, i64(f.createdAt))
	if r.AccountBudget.Remaining != full || r.AccountBudget.ProgressSent != 0 {
		t.Fatalf("new key at same time: %+v", r.AccountBudget)
	}
	mustNoErr(t, r.ConsumeSendBudget(rtcChat, SendFinal))
	rtcRecord(t, r, "later", rtcChat, i64(f.createdAt+1))
	if r.AccountBudget.Remaining != full {
		t.Fatal("later inbound did not refill")
	}
	rtcSame(t, "watermark", f.reload().InboundWatermarks[rtcChat], inboundWatermark{CreatedAtMs: f.createdAt + 1, Keys: []string{"later"}})
}

// from: gateway/tests/runtime/delivery-state.test.ts: missing, invalid, future and older inbound timestamps never refresh quota or block business
func TestRtcDeliveryBadTimestamps(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{})
	s := f.state
	rtcRecord(t, s, "missing-first", rtcChat, nil)
	if s.CanSendToChat(rtcChat, SendFinal) {
		t.Fatal("missing timestamp opened a window")
	}
	rtcRecord(t, s, "valid", rtcChat, i64(f.createdAt))
	mustNoErr(t, s.ConsumeSendBudget(rtcChat, SendFinal))
	expected := s.ContextWindows[rtcChat]
	// NaN, Infinity and fractional timestamps cannot reach an int64.
	for _, c := range []struct {
		key string
		at  *int64
	}{{"missing", nil}, {"negative", i64(-1)}, {"future", i64(nowMs() + 60_000)}, {"older", i64(f.createdAt - 1)}} {
		rtcRecord(t, s, c.key, rtcChat, c.at)
		if s.ContextWindows[rtcChat] != expected {
			t.Fatalf("%s changed the window", c.key)
		}
		mustNoErr(t, s.RecordProcessedMessageKey(c.key, false))
		if !s.IsMessageProcessed(c.key) {
			t.Fatalf("%s not processed", c.key)
		}
	}
	r := f.reload()
	if r.ContextWindows[rtcChat] != expected {
		t.Fatal("reloaded window changed")
	}
	rtcSame(t, "watermark", r.InboundWatermarks[rtcChat], inboundWatermark{CreatedAtMs: f.createdAt, Keys: []string{"valid"}})
}

// from: gateway/tests/runtime/delivery-state.test.ts: per-recipient timestamp watermarks cannot refill the account budget with an older message in another chat
func TestRtcDeliveryOlderMessageOtherChat(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{})
	full := f.state.Policy.SendBudget
	rtcRecord(t, f.state, "first", rtcChat, i64(f.createdAt))
	mustNoErr(t, f.state.ConsumeSendBudget(rtcChat, SendFinal))
	r := f.reload()
	rtcRecord(t, r, "first", rtcChat, i64(f.createdAt))
	rtcRecord(t, r, "previous-window", rtcChat, i64(f.createdAt-1))
	if r.AccountBudget.Remaining != full-1 {
		t.Fatal("older message refilled")
	}
	rtcSame(t, "watermark", r.InboundWatermarks[rtcChat], inboundWatermark{CreatedAtMs: f.createdAt, Keys: []string{"first"}})
	rtcRecord(t, r, "new-chat", "second-chat", i64(f.createdAt-1))
	if r.AccountBudget.Remaining != full-1 {
		t.Fatal("older message in another chat refilled the account")
	}
}

// from: gateway/tests/runtime/delivery-state.test.ts: small server clock skew grants a usable window while preserving its original timestamp for replay detection
func TestRtcDeliveryClockSkew(t *testing.T) {
	now := int64(1_800_000_000_000)
	setNow(t, now)
	for _, skew := range []int64{1, MaxInboundClockSkewMs} {
		f := rtcNewState(t, PolicyOptions{})
		s, server := f.state, now+skew
		if ok, err := s.RecordInboundContext("skewed", rtcChat, i64(server), true, "synthetic-context"); err != nil || !ok {
			t.Fatalf("skew %d refused: %v", skew, err)
		}
		if !s.CanSendToChat(rtcChat, SendFinal) || s.ContextWindows[rtcChat].RefreshedAt != now {
			t.Fatalf("window = %+v", s.ContextWindows[rtcChat])
		}
		rtcSame(t, "watermark", f.reload().InboundWatermarks[rtcChat], inboundWatermark{CreatedAtMs: server, Keys: []string{"skewed"}})
		mustNoErr(t, s.ConsumeSendBudget(rtcChat, SendFinal))
		r := f.reload()
		if ok, _ := r.RecordInboundContext("skewed", rtcChat, i64(server), true, "replayed-context"); ok {
			t.Fatal("replay accepted")
		}
		if r.AccountBudget.Remaining != s.Policy.SendBudget-1 || r.ContextTokens[rtcChat] != "synthetic-context" {
			t.Fatalf("replay changed budget or token: %+v %s", r.AccountBudget, r.ContextTokens[rtcChat])
		}
		if rtcRecord(t, r, "beyond-tolerance", rtcChat, i64(now+MaxInboundClockSkewMs+1)) || r.AccountBudget.Remaining != s.Policy.SendBudget-1 {
			t.Fatal("timestamp beyond tolerance accepted")
		}
	}
}
