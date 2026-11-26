package wechat

import (
	"errors"
	"fmt"
)

// Send kinds: a final reply, or a progress note that must leave room for one.
const (
	SendFinal    = "final"
	SendProgress = "progress"
)

// Defer reasons: why a send waits instead of going out.
const (
	DeferBudgetExhausted = "budget_exhausted"
	DeferWindowExpired   = "window_expired"
	DeferContextChanged  = "context_changed"
)

const (
	reservedFinalSends = 3
	MaxContextWindowMs = 7 * 24 * millisecondsPerHour
	MaxSendBudget      = 1000
	MaxOutboxLimit     = 1000
)

// ContextBudgetState is one reply window's allowance.
type ContextBudgetState struct {
	RefreshedAt    int64  `json:"refreshedAt"`
	Remaining      int64  `json:"remaining"`
	ProgressSent   int64  `json:"progressSent"`
	LastInboundKey string `json:"lastInboundKey"`
	GrantedBudget  *int64 `json:"grantedBudget,omitempty"`
}

// ContextPolicy is WeChat's reply window: 23 hours and 10 messages after
// each inbound message, shared by the whole account.
type ContextPolicy struct {
	ContextWindowMs int64
	SendBudget      int64
	OutboxLimit     int64
}

// DefaultContextPolicy is conservative: a little under WeChat's 24 hours.
var DefaultContextPolicy = ContextPolicy{ContextWindowMs: 23 * millisecondsPerHour, SendBudget: 10, OutboxLimit: MaxOutboxLimit}

var contextPolicyLimits = ContextPolicy{ContextWindowMs: MaxContextWindowMs, SendBudget: MaxSendBudget, OutboxLimit: MaxOutboxLimit}

// LocalSendDeferredError is a send kept back before anything went out.
type LocalSendDeferredError struct {
	Reason  string
	Message string
}

func (e *LocalSendDeferredError) Error() string { return e.Message }

func deferred(reason string, message ...string) *LocalSendDeferredError {
	m := "Local send deferred: " + reason
	if len(message) > 0 {
		m = message[0]
	}
	return &LocalSendDeferredError{Reason: reason, Message: m}
}

// asDeferred unwraps a LocalSendDeferredError.
func asDeferred(err error) (*LocalSendDeferredError, bool) {
	var d *LocalSendDeferredError
	ok := errors.As(err, &d)
	return d, ok
}

// PolicyOptions sets parts of a ContextPolicy; zero fields keep the default.
// Set* marks a field given even when it is zero, which then fails validation.
type PolicyOptions struct {
	ContextWindowMs, SendBudget, OutboxLimit          int64
	SetContextWindowMs, SetSendBudget, SetOutboxLimit bool
}

func ValidateContextPolicy(o PolicyOptions) (ContextPolicy, error) {
	p := DefaultContextPolicy
	if o.SetContextWindowMs || o.ContextWindowMs != 0 {
		p.ContextWindowMs = o.ContextWindowMs
	}
	if o.SetSendBudget || o.SendBudget != 0 {
		p.SendBudget = o.SendBudget
	}
	if o.SetOutboxLimit || o.OutboxLimit != 0 {
		p.OutboxLimit = o.OutboxLimit
	}
	for _, f := range []struct {
		name       string
		value, max int64
	}{{"contextWindowMs", p.ContextWindowMs, contextPolicyLimits.ContextWindowMs},
		{"sendBudget", p.SendBudget, contextPolicyLimits.SendBudget},
		{"outboxLimit", p.OutboxLimit, contextPolicyLimits.OutboxLimit}} {
		if f.value <= 0 || f.value > f.max {
			return ContextPolicy{}, fmt.Errorf("Invalid context policy %s: expected an integer between 1 and %d", f.name, f.max)
		}
	}
	return p, nil
}

// GetDeferReason is why a send of kind must wait now, or "".
func GetDeferReason(state *ContextBudgetState, policy ContextPolicy, kind string, now int64) string {
	if state == nil || state.RefreshedAt < 0 || now < state.RefreshedAt || policy.ContextWindowMs <= 0 ||
		now-state.RefreshedAt >= policy.ContextWindowMs {
		return DeferWindowExpired
	}
	g := state.GrantedBudget
	if EffectiveRemaining(*state, policy) <= 0 ||
		g != nil && (*g < state.Remaining || *g <= 0 || *g > MaxSendBudget) ||
		state.ProgressSent < 0 ||
		kind == SendProgress && EffectiveRemaining(*state, policy) <= min(reservedFinalSends, policy.SendBudget) {
		return DeferBudgetExhausted
	}
	return ""
}

// EffectiveRemaining honors a policy lowered since the window opened.
func EffectiveRemaining(state ContextBudgetState, policy ContextPolicy) int64 {
	var spent int64
	if state.GrantedBudget != nil {
		spent = max(0, *state.GrantedBudget-state.Remaining)
	}
	return min(state.Remaining, max(0, policy.SendBudget-spent))
}

// ConsumeSend returns the state after one send; the original is left as is.
func ConsumeSend(state *ContextBudgetState, policy ContextPolicy, kind string, now int64) (ContextBudgetState, error) {
	reason := GetDeferReason(state, policy, kind, now)
	if reason != "" || state == nil {
		if reason == "" {
			reason = DeferWindowExpired
		}
		return ContextBudgetState{}, deferred(reason)
	}
	next := *state
	next.Remaining = EffectiveRemaining(*state, policy) - 1
	if state.GrantedBudget != nil {
		g := min(*state.GrantedBudget, policy.SendBudget)
		next.GrantedBudget = &g
	}
	if kind == SendProgress {
		next.ProgressSent++
	}
	return next, nil
}

// ChatContextWindow is when a chat last opened its reply window.
type ChatContextWindow struct {
	RefreshedAt    int64  `json:"refreshedAt"`
	LastInboundKey string `json:"lastInboundKey"`
}

// AccountSendBudget is the account's shared allowance. InboundCreatedAt is
// platform time, before small positive clock skew was clamped.
type AccountSendBudget struct {
	ContextBudgetState
	InboundCreatedAt int64 `json:"inboundCreatedAt"`
}

func validWindow(w *ChatContextWindow) bool { return w != nil && w.RefreshedAt >= 0 }

func validBudget(b *ContextBudgetState) bool {
	return b != nil && b.RefreshedAt >= 0 && b.Remaining >= 0 && b.ProgressSent >= 0 &&
		(b.GrantedBudget == nil || *b.GrantedBudget >= b.Remaining && *b.GrantedBudget > 0 && *b.GrantedBudget <= MaxSendBudget)
}

// restoreAccountContext reads the windows and the shared budget, converting
// the older per-chat ledgers when that is all a state file has.
func restoreAccountContext(st *syncState, policy ContextPolicy) (map[string]ChatContextWindow, *AccountSendBudget, error) {
	for _, b := range st.ContextBudgets {
		b := b
		if !validBudget(&b) {
			return nil, nil, errors.New("Invalid legacy context budget")
		}
	}
	if st.ContextWindows != nil || st.AccountSendBudget != nil {
		if st.ContextWindows == nil {
			return nil, nil, errors.New("Invalid chat context windows")
		}
		for _, w := range st.ContextWindows {
			w := w
			if !validWindow(&w) {
				return nil, nil, errors.New("Invalid chat context window")
			}
		}
		b := st.AccountSendBudget
		if b != nil && (!validBudget(&b.ContextBudgetState) || b.InboundCreatedAt < b.RefreshedAt) {
			return nil, nil, errors.New("Invalid account send budget")
		}
		return st.ContextWindows, b, nil
	}
	windows := map[string]ChatContextWindow{}
	var latest *ContextBudgetState
	for chat, b := range st.ContextBudgets {
		b := b
		windows[chat] = ChatContextWindow{RefreshedAt: b.RefreshedAt, LastInboundKey: b.LastInboundKey}
		if latest == nil || b.RefreshedAt > latest.RefreshedAt {
			latest = &b
		}
	}
	if latest == nil {
		return windows, nil, nil
	}
	// Old ledgers have no send timestamps. Count every known spend in overlapping windows
	// conservatively instead of creating an extra ten sends for each former chat ledger.
	granted := func(b ContextBudgetState) int64 {
		if b.GrantedBudget != nil {
			return *b.GrantedBudget
		}
		return policy.SendBudget
	}
	grant := min(granted(*latest), policy.SendBudget)
	var spent, progress int64
	for _, b := range st.ContextBudgets {
		if latest.RefreshedAt-b.RefreshedAt < policy.ContextWindowMs {
			spent = min(grant, spent+max(0, granted(b)-b.Remaining))
			progress = min(maxSafeInteger, progress+b.ProgressSent)
		}
	}
	inboundCreatedAt := latest.RefreshedAt
	for chat, w := range windows {
		at := w.RefreshedAt
		if wm, ok := st.InboundWatermarks[chat]; ok && containsString(wm.Keys, w.LastInboundKey) {
			at = wm.CreatedAtMs
		}
		inboundCreatedAt = max(inboundCreatedAt, at)
	}
	budget := &AccountSendBudget{ContextBudgetState: *latest, InboundCreatedAt: inboundCreatedAt}
	budget.GrantedBudget = &grant
	budget.Remaining = max(0, min(EffectiveRemaining(*latest, policy), grant-spent))
	budget.ProgressSent = progress
	return windows, budget, nil
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
