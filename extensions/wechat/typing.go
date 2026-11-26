package wechat

import (
	"context"
	"errors"
	"strings"
	"time"
)

const (
	maxTypingTimeoutMs   = 1_000
	typingTicketTTLMs    = 24 * 60 * 60 * 1_000
	typingFailureBackoff = 2_000
	maxTypingCacheSize   = 1_000
	typingOn             = 1
	typingCancel         = 2
)

type typingAccount struct{ token, fingerprint string }

type typingEntry struct {
	ticket    string
	expiresAt int64
	retryAt   int64
}

type typingOptions struct {
	request      func(ctx context.Context, endpoint string, payload map[string]any, token string, timeoutMs int64) (map[string]any, error)
	account      func() *typingAccount
	contextToken func(chatID string) string
	timeoutMs    int64
	now          func() int64 // the clock; nowMs when nil
}

// TypingIndicator sends best-effort typing signals: failures only answer
// false and never expose credentials. Tickets are cached per chat for a day
// and forgotten when the account changes.
type TypingIndicator struct {
	opt        typingOptions
	timeoutMs  int64
	entries    map[string]typingEntry
	order      []string
	inFlight   map[string]bool
	account    *typingAccount
	generation int
}

func newTypingIndicator(o typingOptions) *TypingIndicator {
	timeout := o.timeoutMs
	if timeout <= 0 {
		timeout = maxTypingTimeoutMs
	}
	if o.now == nil {
		o.now = func() int64 { return nowMs() }
	}
	return &TypingIndicator{opt: o, timeoutMs: min(timeout, maxTypingTimeoutMs), entries: map[string]typingEntry{}, inFlight: map[string]bool{}}
}

func typingAccepted(r map[string]any) bool {
	for _, f := range []string{"ret", "errcode"} {
		if v, ok := r[f]; ok && !isZero(v) {
			return false
		}
	}
	return r != nil
}

var errTypingBudget = errors.New("Typing budget expired")

// send runs with the loop held; it is bounded by its own timeout.
func (t *TypingIndicator) send(parent context.Context, chatID string, status int) bool {
	account := t.refreshAccount()
	generation := t.generation
	if account == nil || account.token == "" || chatID == "" || status != typingOn && status != typingCancel {
		return false
	}
	if cached, ok := t.entries[chatID]; ok && cached.retryAt > t.opt.now() {
		return false
	}
	key := jsonText([]any{generation, chatID, status})
	if t.inFlight[key] {
		return false
	}
	t.inFlight[key] = true
	defer delete(t.inFlight, key)
	ctx, cancel := context.WithTimeout(parent, msDuration(t.timeoutMs))
	defer cancel()
	deadline := time.Now().Add(msDuration(t.timeoutMs))
	ok, err := t.perform(ctx, chatID, status, account, generation, deadline)
	if err != nil || ctx.Err() != nil {
		// Provider errors can carry tokens or bodies; false handles them.
		t.fail(chatID, generation)
		return false
	}
	return ok
}

func (t *TypingIndicator) perform(ctx context.Context, chatID string, status int, account *typingAccount, generation int, deadline time.Time) (bool, error) {
	entry, ok := t.entries[chatID]
	if !ok || entry.ticket == "" || entry.expiresAt <= t.opt.now() {
		t.remove(chatID)
		if status == typingCancel {
			return false, nil
		}
		contextToken := t.opt.contextToken(chatID)
		if contextToken == "" {
			return false, nil
		}
		remaining, err := remainingUntil(deadline)
		if err != nil {
			return false, err
		}
		config, err := t.opt.request(ctx, endpointGetConfig, map[string]any{"ilink_user_id": chatID, "context_token": contextToken,
			"base_info": map[string]any{"channel_version": ChannelVersion}}, account.token, remaining)
		if err != nil {
			return false, err
		}
		if !t.current(generation, ctx) {
			return false, nil
		}
		ticket, _ := config["typing_ticket"].(string)
		if !typingAccepted(config) || !isZero(config["ret"]) || strings.TrimSpace(ticket) == "" {
			t.fail(chatID, generation)
			return false, nil
		}
		entry = typingEntry{ticket: ticket, expiresAt: t.opt.now() + typingTicketTTLMs}
		t.put(chatID, entry)
	}
	if !t.current(generation, ctx) {
		return false, nil
	}
	t.put(chatID, entry)
	remaining, err := remainingUntil(deadline)
	if err != nil {
		return false, err
	}
	receipt, err := t.opt.request(ctx, endpointSendTyping, map[string]any{"ilink_user_id": chatID, "typing_ticket": entry.ticket,
		"status": status, "base_info": map[string]any{"channel_version": ChannelVersion}}, account.token, remaining)
	if err != nil {
		return false, err
	}
	if !t.current(generation, ctx) {
		return false, nil
	}
	if !typingAccepted(receipt) {
		t.fail(chatID, generation)
		return false, nil
	}
	return true, nil
}

func remainingUntil(deadline time.Time) (int64, error) {
	ms := time.Until(deadline).Milliseconds()
	if ms <= 0 {
		return 0, errTypingBudget
	}
	return ms, nil
}

func (t *TypingIndicator) refreshAccount() *typingAccount {
	a := t.opt.account()
	changed := (a == nil) != (t.account == nil) || a != nil && (a.token != t.account.token || a.fingerprint != t.account.fingerprint)
	if changed {
		t.entries, t.order = map[string]typingEntry{}, nil
		t.generation++
		t.account = nil
		if a != nil {
			copy := *a
			t.account = &copy
		}
	}
	return a
}

func (t *TypingIndicator) current(generation int, ctx context.Context) bool {
	t.refreshAccount()
	return ctx.Err() == nil && generation == t.generation
}

func (t *TypingIndicator) fail(chatID string, generation int) {
	if generation != t.generation {
		return
	}
	t.put(chatID, typingEntry{retryAt: t.opt.now() + typingFailureBackoff})
}

func (t *TypingIndicator) remove(chatID string) {
	if _, ok := t.entries[chatID]; !ok {
		return
	}
	delete(t.entries, chatID)
	for i, k := range t.order {
		if k == chatID {
			t.order = append(t.order[:i:i], t.order[i+1:]...)
			break
		}
	}
}

// put refreshes chatID's place as most recent, dropping the oldest beyond
// the cache size.
func (t *TypingIndicator) put(chatID string, e typingEntry) {
	t.remove(chatID)
	t.entries[chatID] = e
	t.order = append(t.order, chatID)
	if len(t.entries) > maxTypingCacheSize {
		oldest := t.order[0]
		t.order = t.order[1:]
		delete(t.entries, oldest)
	}
}
