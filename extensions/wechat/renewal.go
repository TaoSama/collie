package wechat

import (
	"context"
	"errors"
	"fmt"
)

// RenewalLeadMs: the reminder goes out in the window's last hour.
const (
	RenewalLeadMs         = 60 * 60 * 1000
	accountWindowIdentity = "account-window-v1"
)

// renewalText asks the person to write once more before the window closes.
func renewalText(sendBudget int64) string {
	return fmt.Sprintf("回复窗口即将结束。请主动发一条消息，刷新本轮 %d 条消息额度，避免定时任务结果继续等待。", sendBudget)
}

// renewalDeliveryKey is the older per-chat reminder's key, still checked so
// a reminder it already sent is not sent again.
func renewalDeliveryKey(account, chatID string, refreshedAt int64) string {
	return renewalPrefix + hashJSON([]any{account, chatID, refreshedAt})
}

// accountRenewalDeliveryKey is one reminder per account window.
func accountRenewalDeliveryKey(account string, budget ContextBudgetState) string {
	return renewalPrefix + hashJSON([]any{account, accountWindowIdentity, budget.RefreshedAt, budget.LastInboundKey})
}

// RenewalSend sends one reminder.
type RenewalSend func(ctx context.Context, chatID, text string, o ReplyOptions) (int, error)

// RenewalNotifier reminds the person, once per window, to renew it while
// results still wait. Callers give only chats with an active task, a future
// schedule or a pending result.
type RenewalNotifier struct {
	state      *StateManager
	send       RenewalSend
	now        func() int64
	active     func() bool
	ctx        context.Context
	delivering bool
}

func NewRenewalNotifier(ctx context.Context, state *StateManager, send RenewalSend, active func() bool, now func() int64) *RenewalNotifier {
	if now == nil {
		now = func() int64 { return nowMs() }
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return &RenewalNotifier{state: state, send: send, now: now, active: active, ctx: ctx}
}

func (r *RenewalNotifier) isActive() bool {
	return r.ctx.Err() == nil && (r.active == nil || r.active())
}

func (r *RenewalNotifier) eligible(chatID string) bool {
	s := r.state
	windowMs := s.Policy.ContextWindowMs
	window, hasWindow := s.ContextWindows[chatID]
	if s.AccountBudget == nil || !hasWindow {
		return false
	}
	refreshedAt := min(s.AccountBudget.RefreshedAt, window.RefreshedAt)
	now := r.now()
	if !r.isActive() || windowMs <= RenewalLeadMs || s.Policy.SendBudget <= 0 || refreshedAt < 0 ||
		now-refreshedAt < windowMs-RenewalLeadMs || now-refreshedAt >= windowMs || !s.CanSendToChat(chatID, SendFinal) {
		return false
	}
	return s.RemainingSendBudget(chatID) > 0
}

type renewalPending struct {
	account, token, lastInboundKey, deliveryKey string
	refreshedAt, windowMs, sendBudget           int64
}

func reminderUsed(d ReplyDeliveryState, ok bool) bool {
	return ok && (d.AcknowledgedChunks > 0 || d.InFlight || d.FailureReason != "" || d.RenewalRejected)
}

func (r *RenewalNotifier) pendingDelivery(chatID string) *renewalPending {
	s := r.state
	account, token := s.LoginFingerprint(), s.ContextTokens[chatID]
	if account == "" || token == "" || !r.eligible(chatID) {
		return nil
	}
	budget := s.AccountBudget.ContextBudgetState
	key := accountRenewalDeliveryKey(account, budget)
	// A reminder already submitted by the per-chat keys counts.
	for chat, w := range s.ContextWindows {
		if w.RefreshedAt != budget.RefreshedAt {
			continue
		}
		legacy, ok := s.ReplyDeliveries[renewalDeliveryKey(account, chat, budget.RefreshedAt)]
		if reminderUsed(legacy, ok) {
			return nil
		}
	}
	prior, ok := s.ReplyDeliveries[key]
	if reminderUsed(prior, ok) {
		return nil
	}
	return &renewalPending{account: account, token: token, refreshedAt: budget.RefreshedAt, lastInboundKey: budget.LastInboundKey,
		windowMs: s.Policy.ContextWindowMs, sendBudget: s.Policy.SendBudget, deliveryKey: key}
}

// ShouldYield is the transport's synchronous admission check: another send
// waits while this chat's reminder is due.
func (r *RenewalNotifier) ShouldYield(chatID, deliveryKey string) bool {
	p := r.pendingDelivery(chatID)
	return p != nil && p.deliveryKey != deliveryKey
}

// DeliverOnce sends each interested chat's due reminder.
func (r *RenewalNotifier) DeliverOnce(chats []string) (sent, skipped int, err error) {
	if r.delivering || !r.isActive() {
		return 0, 0, nil
	}
	for _, c := range chats {
		if c == "" {
			return 0, 0, errors.New("Invalid renewal chats")
		}
	}
	r.delivering = true
	defer func() { r.delivering = false }()
	var first error
	seen := map[string]bool{}
	for _, chatID := range chats {
		if seen[chatID] {
			continue
		}
		seen[chatID] = true
		if !r.isActive() {
			break
		}
		p := r.pendingDelivery(chatID)
		if p == nil {
			skipped++
			continue
		}
		s := r.state
		// Checked again by the shared send path after queueing and right before submission.
		shouldContinue := func() bool {
			return p.account == s.LoginFingerprint() && p.token == s.ContextTokens[chatID] && s.AccountBudget != nil &&
				p.refreshedAt == s.AccountBudget.RefreshedAt && p.lastInboundKey == s.AccountBudget.LastInboundKey &&
				p.windowMs == s.Policy.ContextWindowMs && p.sendBudget == s.Policy.SendBudget && r.eligible(chatID)
		}
		if !shouldContinue() {
			skipped++
			continue
		}
		n, err := r.send(r.ctx, chatID, renewalText(p.sendBudget), ReplyOptions{DeliveryKey: p.deliveryKey, Kind: SendFinal,
			ShouldContinue: shouldContinue, RenewalReminder: true})
		if err != nil {
			first = firstErr(first, err)
			continue
		}
		if n > 0 {
			sent++
		} else {
			skipped++
		}
	}
	return sent, skipped, first
}
