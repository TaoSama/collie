package wechat

import (
	"errors"
	"strings"
	"testing"
)

const rtcNow = int64(1_800_000_000_000)

func rtcBudget(patch func(*ContextBudgetState)) *ContextBudgetState {
	b := &ContextBudgetState{RefreshedAt: rtcNow, Remaining: DefaultContextPolicy.SendBudget, LastInboundKey: "inbound-key"}
	if patch != nil {
		patch(b)
	}
	return b
}

func rtcAssertDeferred(t *testing.T, state *ContextBudgetState, kind, reason string, now int64) {
	t.Helper()
	if CanSend(state, DefaultContextPolicy, kind, now) {
		t.Fatalf("CanSend(%+v, %s) = true, want deferred %s", state, kind, reason)
	}
	_, err := ConsumeSend(state, DefaultContextPolicy, kind, now)
	d, ok := asDeferred(err)
	if !ok || d.Reason != reason {
		t.Fatalf("ConsumeSend error = %v, want LocalSendDeferredError %s", err, reason)
	}
}

// from: gateway/tests/runtime/context-policy.test.ts: context policy defaults are conservative and configuration accepts the resource limits
func TestRtcContextPolicyDefaults(t *testing.T) {
	p, err := ValidateContextPolicy(PolicyOptions{})
	mustNoErr(t, err)
	if p != (ContextPolicy{ContextWindowMs: 23 * 60 * 60 * 1000, SendBudget: 10, OutboxLimit: 1000}) {
		t.Fatalf("defaults = %+v", p)
	}
	if MaxContextWindowMs != 604_800_000 || MaxSendBudget != 1000 || MaxOutboxLimit != DefaultContextPolicy.OutboxLimit {
		t.Fatal("limits changed")
	}
	for _, c := range []struct {
		in   PolicyOptions
		want ContextPolicy
	}{
		{PolicyOptions{ContextWindowMs: 1000, SendBudget: 1, OutboxLimit: 1}, ContextPolicy{1000, 1, 1}},
		{PolicyOptions{SendBudget: 2}, ContextPolicy{DefaultContextPolicy.ContextWindowMs, 2, DefaultContextPolicy.OutboxLimit}},
		{PolicyOptions{ContextWindowMs: MaxContextWindowMs, SendBudget: MaxSendBudget}, ContextPolicy{MaxContextWindowMs, MaxSendBudget, MaxOutboxLimit}},
	} {
		got, err := ValidateContextPolicy(c.in)
		mustNoErr(t, err)
		if got != c.want {
			t.Fatalf("ValidateContextPolicy(%+v) = %+v, want %+v", c.in, got, c.want)
		}
	}
	p.SendBudget = 1
	if DefaultContextPolicy.SendBudget != 10 {
		t.Fatal("a validated policy aliases the defaults")
	}
}

// from: gateway/tests/runtime/context-policy.test.ts: context policy rejects invalid numbers and settings above resource limits
func TestRtcContextPolicyRejectsInvalid(t *testing.T) {
	// Fractions, NaN and Infinity cannot be expressed in Go's integer options.
	limits := map[string]int64{"contextWindowMs": MaxContextWindowMs, "sendBudget": MaxSendBudget, "outboxLimit": MaxOutboxLimit}
	for key, limit := range limits {
		for _, v := range []int64{0, -1, limit + 1} {
			o := PolicyOptions{}
			switch key {
			case "contextWindowMs":
				o.ContextWindowMs, o.SetContextWindowMs = v, true
			case "sendBudget":
				o.SendBudget, o.SetSendBudget = v, true
			default:
				o.OutboxLimit, o.SetOutboxLimit = v, true
			}
			if _, err := ValidateContextPolicy(o); err == nil || !strings.Contains(err.Error(), "Invalid context policy "+key) {
				t.Fatalf("%s=%d: error %v", key, v, err)
			}
		}
	}
}

// from: gateway/tests/runtime/context-policy.test.ts: unknown and expired windows defer both kinds of sends including the exact expiry boundary
func TestRtcContextPolicyExpiredWindows(t *testing.T) {
	w := DefaultContextPolicy.ContextWindowMs
	for _, kind := range []string{SendFinal, SendProgress} {
		rtcAssertDeferred(t, nil, kind, DeferWindowExpired, rtcNow)
		rtcAssertDeferred(t, rtcBudget(func(b *ContextBudgetState) { b.RefreshedAt = -1 }), kind, DeferWindowExpired, rtcNow)
		rtcAssertDeferred(t, rtcBudget(func(b *ContextBudgetState) { b.RefreshedAt = rtcNow + 1 }), kind, DeferWindowExpired, rtcNow)
		rtcAssertDeferred(t, rtcBudget(nil), kind, DeferWindowExpired, rtcNow+w)
		rtcAssertDeferred(t, rtcBudget(nil), kind, DeferWindowExpired, rtcNow+w+1)
		if !CanSend(rtcBudget(nil), DefaultContextPolicy, kind, rtcNow+w-1) {
			t.Fatalf("%s: last millisecond of the window refused", kind)
		}
	}
}

// from: gateway/tests/runtime/context-policy.test.ts: independent queries share the context budget while preserving final sends
func TestRtcContextPolicySharedBudget(t *testing.T) {
	state := rtcBudget(nil)
	for count := int64(1); count <= 7; count++ {
		next, err := ConsumeSend(state, DefaultContextPolicy, SendProgress, rtcNow)
		mustNoErr(t, err)
		state = &next
		if state.ProgressSent != count || state.Remaining != DefaultContextPolicy.SendBudget-count {
			t.Fatalf("after %d progress: %+v", count, state)
		}
	}
	rtcAssertDeferred(t, state, SendProgress, DeferBudgetExhausted, rtcNow)
	for state.Remaining > 0 {
		next, err := ConsumeSend(state, DefaultContextPolicy, SendFinal, rtcNow)
		mustNoErr(t, err)
		state = &next
		if state.ProgressSent != 7 {
			t.Fatalf("final send counted as progress: %+v", state)
		}
	}
	rtcAssertDeferred(t, state, SendFinal, DeferBudgetExhausted, rtcNow)
}

// from: gateway/tests/runtime/context-policy.test.ts: progress preserves three sends for final replies even after earlier final sends
func TestRtcContextPolicyReservesFinalSends(t *testing.T) {
	next, err := ConsumeSend(rtcBudget(func(b *ContextBudgetState) { b.Remaining = 4 }), DefaultContextPolicy, SendProgress, rtcNow)
	mustNoErr(t, err)
	if next.Remaining != 3 {
		t.Fatalf("remaining = %d", next.Remaining)
	}
	rtcAssertDeferred(t, &next, SendProgress, DeferBudgetExhausted, rtcNow)
	if !CanSend(&next, DefaultContextPolicy, SendFinal, rtcNow) {
		t.Fatal("final refused with three left")
	}
	strict, _ := ValidateContextPolicy(PolicyOptions{SendBudget: 3})
	if CanSend(rtcBudget(func(b *ContextBudgetState) { b.Remaining = 3 }), strict, SendProgress, rtcNow) ||
		!CanSend(rtcBudget(func(b *ContextBudgetState) { b.Remaining = 1 }), strict, SendFinal, rtcNow) {
		t.Fatal("budget 3 reservation wrong")
	}
	single, _ := ValidateContextPolicy(PolicyOptions{SendBudget: 1})
	if CanSend(rtcBudget(func(b *ContextBudgetState) { b.Remaining = 1 }), single, SendProgress, rtcNow) ||
		!CanSend(rtcBudget(func(b *ContextBudgetState) { b.Remaining = 1 }), single, SendFinal, rtcNow) {
		t.Fatal("budget 1 reservation wrong")
	}
}

// from: gateway/tests/runtime/context-policy.test.ts: consumeSend returns a new state and a deferred attempt preserves the original
func TestRtcContextPolicyConsumeIsPure(t *testing.T) {
	original := rtcBudget(nil)
	next, err := ConsumeSend(original, DefaultContextPolicy, SendFinal, rtcNow)
	mustNoErr(t, err)
	want := *original
	want.Remaining--
	if next != want || original.Remaining != DefaultContextPolicy.SendBudget {
		t.Fatalf("next = %+v, original = %+v", next, original)
	}
	exhausted := rtcBudget(func(b *ContextBudgetState) { b.Remaining = 0 })
	rtcAssertDeferred(t, exhausted, SendFinal, DeferBudgetExhausted, rtcNow)
	if exhausted.Remaining != 0 {
		t.Fatal("deferred attempt changed the state")
	}
}

// from: gateway/tests/runtime/context-policy.test.ts: invalid persisted counters fail closed without spending a send
func TestRtcContextPolicyInvalidCounters(t *testing.T) {
	// NaN and fractions cannot be held in int64 counters (decoding rejects them).
	for _, b := range []*ContextBudgetState{
		rtcBudget(func(b *ContextBudgetState) { b.Remaining = -1 }),
		rtcBudget(func(b *ContextBudgetState) { b.ProgressSent = -1 }),
	} {
		for _, kind := range []string{SendFinal, SendProgress} {
			rtcAssertDeferred(t, b, kind, DeferBudgetExhausted, rtcNow)
		}
	}
}

// from: gateway/tests/runtime/context-policy.test.ts: local send deferrals carry a stable context-changed reason for callers
func TestRtcContextPolicyDeferredError(t *testing.T) {
	var err error = deferred(DeferContextChanged)
	var d *LocalSendDeferredError
	if !errors.As(err, &d) || d.Reason != DeferContextChanged || err.Error() != "Local send deferred: context_changed" {
		t.Fatalf("error = %v", err)
	}
}

// from: gateway/tests/runtime/context-policy.test.ts: tightening a budget counts prior charges and never restores spent sends
func TestRtcContextPolicyTightening(t *testing.T) {
	tightened, _ := ValidateContextPolicy(PolicyOptions{SendBudget: 5})
	g := int64(10)
	overspent := rtcBudget(func(b *ContextBudgetState) { b.GrantedBudget, b.Remaining = &g, 3 })
	if CanSend(overspent, tightened, SendFinal, rtcNow) {
		t.Fatal("overspent window allowed")
	}
	if _, err := ConsumeSend(overspent, tightened, SendFinal, rtcNow); func() bool { d, ok := asDeferred(err); return !ok || d.Reason != DeferBudgetExhausted }() {
		t.Fatalf("error = %v", err)
	}
	state := rtcBudget(func(b *ContextBudgetState) { b.GrantedBudget, b.Remaining = &g, 8 })
	if CanSend(state, tightened, SendProgress, rtcNow) {
		t.Fatal("progress allowed inside the final reservation")
	}
	for expected := int64(2); expected >= 0; expected-- {
		next, err := ConsumeSend(state, tightened, SendFinal, rtcNow)
		mustNoErr(t, err)
		state = &next
		if state.Remaining != expected || *state.GrantedBudget != 5 {
			t.Fatalf("state = %+v (grant %d)", state, *state.GrantedBudget)
		}
	}
	if CanSend(state, tightened, SendFinal, rtcNow) {
		t.Fatal("exhausted window allowed")
	}
}

// from: gateway/tests/runtime/context-policy.test.ts: raising a budget preserves a previous window's remaining allowance
func TestRtcContextPolicyRaising(t *testing.T) {
	raised, _ := ValidateContextPolicy(PolicyOptions{SendBudget: 20})
	g := int64(10)
	state := rtcBudget(func(b *ContextBudgetState) { b.GrantedBudget, b.Remaining = &g, 2 })
	for _, want := range []int64{1, 0} {
		next, err := ConsumeSend(state, raised, SendFinal, rtcNow)
		mustNoErr(t, err)
		state = &next
		if state.Remaining != want || *state.GrantedBudget != 10 {
			t.Fatalf("state = %+v", state)
		}
	}
	if CanSend(state, raised, SendFinal, rtcNow) {
		t.Fatal("raised budget refilled an old window")
	}
}

// from: gateway/tests/runtime/context-policy.test.ts: invalid persisted initial grants fail closed without producing invalid remaining counters
func TestRtcContextPolicyInvalidGrants(t *testing.T) {
	// NaN, Infinity and 0.5 are not representable; the integer cases remain.
	for _, g := range []int64{-1, 0, 1, MaxSendBudget + 1} {
		g := g
		state := rtcBudget(func(b *ContextBudgetState) { b.GrantedBudget = &g })
		for _, kind := range []string{SendFinal, SendProgress} {
			rtcAssertDeferred(t, state, kind, DeferBudgetExhausted, rtcNow)
		}
	}
}
