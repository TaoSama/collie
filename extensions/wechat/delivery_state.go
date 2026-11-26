package wechat

import (
	"errors"
	"regexp"
)

const (
	maxSameTimestampKeys = 256
	// MaxInboundClockSkewMs is how far ahead of this computer a message's
	// timestamp may be and still open a window.
	MaxInboundClockSkewMs = 30_000
)

// Outbox sources: whose reply a reference points at.
const (
	SourceController = "controller" // legacy references without a source
	SourceEngine     = "engine"     // Collie's answer, frozen in the reply store
	// Fixed notices older state may still hold; no longer sent.
	SourceConflict         = "conflict"
	SourceUnsupportedMedia = "unsupported-media"
)

// Outbox statuses.
const (
	OutboxPending   = "pending"
	OutboxClaiming  = "claiming"
	OutboxClaimed   = "claimed"
	OutboxDefer     = "defer"
	OutboxReceipt   = "receipt"
	OutboxUncertain = "uncertain"
	OutboxExpired   = "expired"
)

var (
	outboxStatuses       = map[string]bool{OutboxPending: true, OutboxClaiming: true, OutboxClaimed: true, OutboxDefer: true, OutboxReceipt: true, OutboxUncertain: true, OutboxExpired: true}
	outboxSources        = map[string]bool{SourceController: true, SourceEngine: true, SourceConflict: true, SourceUnsupportedMedia: true}
	notificationIdentity = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// DeliveryOutboxItem is a small reference to a reply still to deliver; the
// reply itself stays in the store it came from.
type DeliveryOutboxItem struct {
	Kind          string `json:"kind"` // reply | notification
	Source        string `json:"source,omitempty"`
	EngineAcked   bool   `json:"engineAcked,omitempty"` // Collie was told the outcome
	DeliveryKey   string `json:"deliveryKey"`
	ChatID        string `json:"chatId"`
	MessageID     string `json:"messageId"`
	SplitMaxChars int    `json:"splitMaxChars"`
	Status        string `json:"status"`
	AttemptID     string `json:"attemptId,omitempty"`
	DeferReason   string `json:"deferReason,omitempty"`
	ReceiptStatus string `json:"receiptStatus,omitempty"` // accepted | uncertain
	ExpiresAt     int64  `json:"expiresAt,omitempty"`

	ReceiptMessageID string `json:"receiptMessageId,omitempty"`
	// NotificationKind and NotificationIdentity keep a pushed result's
	// immutable identity for recovery.
	NotificationKind     string `json:"notificationKind,omitempty"`
	NotificationIdentity string `json:"notificationIdentity,omitempty"`
}

// OutboxBackpressureError keeps the inbound cursor while the outbox is full.
var OutboxBackpressureError = errors.New("Gateway durable outbox capacity exhausted; inbound cursor retained")

func outboxSource(source string) (string, error) {
	if source == "" {
		return SourceController, nil
	}
	if !outboxSources[source] {
		return "", errors.New("Invalid delivery outbox source")
	}
	return source, nil
}

func validateEngineAcknowledgement(item DeliveryOutboxItem) error {
	source, err := outboxSource(item.Source)
	if err != nil {
		return err
	}
	if item.EngineAcked && source != SourceEngine {
		return errors.New("Invalid delivery outbox engine acknowledgement")
	}
	return nil
}

func (s *StateManager) restoreDeliveryState(st *syncState) error {
	windows, budget, err := restoreAccountContext(st, s.Policy)
	if err != nil {
		return err
	}
	for _, w := range st.InboundWatermarks {
		if w.CreatedAtMs < 0 || len(w.Keys) > maxSameTimestampKeys || containsString(w.Keys, "") {
			return errors.New("Invalid inbound watermark")
		}
	}
	outbox := map[string]DeliveryOutboxItem{}
	for key, item := range st.DeliveryOutbox {
		if item == nil || item.DeliveryKey != key || item.Kind != "reply" && item.Kind != "notification" ||
			item.ChatID == "" || item.MessageID == "" || item.SplitMaxChars <= 0 || !outboxStatuses[item.Status] ||
			item.NotificationIdentity != "" && (item.Kind != "notification" || !notificationIdentity.MatchString(item.NotificationIdentity)) {
			return errors.New("Invalid delivery outbox reference")
		}
		source, err := outboxSource(item.Source)
		if err != nil {
			return err
		}
		item.Source = source
		if err := validateEngineAcknowledgement(*item); err != nil {
			return err
		}
		outbox[key] = *item
	}
	if windows == nil {
		windows = map[string]ChatContextWindow{}
	}
	s.ContextWindows, s.AccountBudget, s.Outbox = windows, budget, outbox
	if st.InboundWatermarks != nil {
		s.InboundWatermarks = st.InboundWatermarks
	}
	return nil
}

// RecordInboundContext notes a polled message. A new, timestamped message
// opens its chat's window and, when it is the account's newest, refills the
// shared budget. False means it changed nothing: missing or old timestamps,
// replays and timestamps far in the future never refresh anything.
func (s *StateManager) RecordInboundContext(key, chatID string, createdAtMs *int64, grantBudget bool, contextToken string) (bool, error) {
	now := nowMs()
	watermark, hadWatermark := s.InboundWatermarks[chatID]
	if key == "" || chatID == "" || createdAtMs == nil || *createdAtMs < 0 || *createdAtMs > maxSafeInteger ||
		*createdAtMs-now > MaxInboundClockSkewMs || hadWatermark && (*createdAtMs < watermark.CreatedAtMs ||
		*createdAtMs == watermark.CreatedAtMs && (containsString(watermark.Keys, key) || len(watermark.Keys) >= maxSameTimestampKeys)) {
		return false, nil
	}
	created := *createdAtMs
	previous, hadWindow := s.ContextWindows[chatID]
	previousToken, hadToken := s.ContextTokens[chatID]
	previousBudget := s.AccountBudget
	keys := []string{key}
	if hadWatermark && watermark.CreatedAtMs == created {
		keys = append(append([]string(nil), watermark.Keys...), key)
	}
	s.InboundWatermarks[chatID] = inboundWatermark{CreatedAtMs: created, Keys: keys}
	if grantBudget {
		refreshedAt := min(created, now)
		s.ContextWindows[chatID] = ChatContextWindow{RefreshedAt: refreshedAt, LastInboundKey: key}
		if previousBudget == nil || created >= previousBudget.InboundCreatedAt {
			grant := s.Policy.SendBudget
			s.AccountBudget = &AccountSendBudget{InboundCreatedAt: created, ContextBudgetState: ContextBudgetState{
				RefreshedAt: refreshedAt, Remaining: s.Policy.SendBudget, GrantedBudget: &grant, LastInboundKey: key}}
		}
	}
	if contextToken != "" {
		s.ContextTokens[chatID] = contextToken
	}
	err := s.save(func() {
		if hadWindow {
			s.ContextWindows[chatID] = previous
		} else {
			delete(s.ContextWindows, chatID)
		}
		s.AccountBudget = previousBudget
		if hadWatermark {
			s.InboundWatermarks[chatID] = watermark
		} else {
			delete(s.InboundWatermarks, chatID)
		}
		if hadToken {
			s.ContextTokens[chatID] = previousToken
		} else {
			delete(s.ContextTokens, chatID)
		}
	})
	return err == nil, err
}

func (s *StateManager) CanSendToChat(chatID, kind string) bool {
	return s.SendDeferReason(chatID, kind) == ""
}

// SendDeferReason checks both the chat's window and the account's budget.
func (s *StateManager) SendDeferReason(chatID, kind string) string {
	window, ok := s.ContextWindows[chatID]
	if !ok || s.AccountBudget == nil {
		return DeferWindowExpired
	}
	now := nowMs()
	chat := s.AccountBudget.ContextBudgetState
	chat.RefreshedAt = window.RefreshedAt
	if r := GetDeferReason(&chat, s.Policy, kind, now); r != "" {
		return r
	}
	return GetDeferReason(&s.AccountBudget.ContextBudgetState, s.Policy, kind, now)
}

// RemainingSendBudget honors the current policy and the chat's window.
func (s *StateManager) RemainingSendBudget(chatID string) int64 {
	if s.AccountBudget == nil || !s.CanSendToChat(chatID, SendFinal) {
		return 0
	}
	return EffectiveRemaining(s.AccountBudget.ContextBudgetState, s.Policy)
}

// ConsumeSendBudget charges one send or answers why it must wait.
func (s *StateManager) ConsumeSendBudget(chatID, kind string) error {
	if reason := s.SendDeferReason(chatID, kind); reason != "" {
		return deferred(reason)
	}
	previous := s.AccountBudget
	next, err := ConsumeSend(&previous.ContextBudgetState, s.Policy, kind, nowMs())
	if err != nil {
		return err
	}
	s.AccountBudget = &AccountSendBudget{ContextBudgetState: next, InboundCreatedAt: previous.InboundCreatedAt}
	return s.save(func() { s.AccountBudget = previous })
}

func (s *StateManager) AssertOutboxCapacity(key string) error {
	if _, ok := s.Outbox[key]; (key == "" || !ok) && int64(len(s.Outbox)) >= s.Policy.OutboxLimit {
		return OutboxBackpressureError
	}
	return nil
}

func validateNotificationIdentity(item DeliveryOutboxItem) error {
	if item.NotificationIdentity != "" && (item.Kind != "notification" || !notificationIdentity.MatchString(item.NotificationIdentity)) {
		return errors.New("Invalid notification outbox identity")
	}
	return nil
}

// PutOutbox stores a reference once; the same key again must name the same
// reply.
func (s *StateManager) PutOutbox(item DeliveryOutboxItem) error {
	source, err := outboxSource(item.Source)
	if err != nil {
		return err
	}
	if err := validateNotificationIdentity(item); err != nil {
		return err
	}
	if err := validateEngineAcknowledgement(item); err != nil {
		return err
	}
	if previous, ok := s.Outbox[item.DeliveryKey]; ok {
		if previous.ChatID != item.ChatID || previous.MessageID != item.MessageID || previous.Kind != item.Kind || previous.Source != source {
			return errors.New("Outbox reference identity changed")
		}
		return nil
	}
	if err := s.AssertOutboxCapacity(item.DeliveryKey); err != nil {
		return err
	}
	item.Source = source
	s.Outbox[item.DeliveryKey] = item
	return s.save(func() { delete(s.Outbox, item.DeliveryKey) })
}

// UpdateOutbox changes a reference's progress; its identity cannot change.
func (s *StateManager) UpdateOutbox(key string, patch func(*DeliveryOutboxItem)) error {
	previous, ok := s.Outbox[key]
	if !ok {
		return errors.New("Missing durable outbox reference")
	}
	updated := previous
	patch(&updated)
	if updated.DeliveryKey != key || updated.ChatID != previous.ChatID || updated.MessageID != previous.MessageID ||
		updated.Kind != previous.Kind {
		return errors.New("Outbox reference identity changed")
	}
	if updated.Source != previous.Source {
		if source, err := outboxSource(updated.Source); err != nil {
			return err
		} else if source != previous.Source {
			return errors.New("Outbox reference identity changed")
		}
		updated.Source = previous.Source
	}
	if err := validateNotificationIdentity(updated); err != nil {
		return err
	}
	if err := validateEngineAcknowledgement(updated); err != nil {
		return err
	}
	s.Outbox[key] = updated
	return s.save(func() { s.Outbox[key] = previous })
}

func (s *StateManager) RemoveOutbox(key string) error {
	previous, ok := s.Outbox[key]
	if !ok {
		return nil
	}
	delete(s.Outbox, key)
	return s.save(func() { s.Outbox[key] = previous })
}

// setStatus is the common outbox patch.
func setStatus(status string) func(*DeliveryOutboxItem) {
	return func(i *DeliveryOutboxItem) { i.Status = status }
}
