package wechat

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	MaxProcessedMessageKeys  = 1000
	MaxReplyDeliveries       = 1000
	MaxRejectedReplyAttempts = 3
	// maxReceivedIdentities bounds the content hashes kept to notice a
	// message id reused with other content.
	maxReceivedIdentities = 1000
)

// Account is the bound bot: what a confirmed QR login gives.
type Account struct {
	Token     string `json:"token"`
	AccountID string `json:"accountId"` // ilink_bot_id
	BaseURL   string `json:"baseUrl,omitempty"`
	UserID    string `json:"userId,omitempty"` // ilink_user_id: the person who scanned, the only one served
	SavedAt   string `json:"savedAt,omitempty"`
}

// ReplyDeliveryState is one reply's frozen chunk plan and how far it got.
type ReplyDeliveryState struct {
	Digest             string `json:"digest"`
	TotalChunks        int    `json:"totalChunks"`
	AcknowledgedChunks int    `json:"acknowledgedChunks"`
	InFlight           bool   `json:"inFlight"`
	FailureReason      string `json:"failureReason,omitempty"` // AttachmentExpired | AttachmentUnavailable
	RenewalRejected    bool   `json:"renewalRejected,omitempty"`
	// Explicit WeChat refusals; bounded so a persistent refusal cannot drain the budget.
	RejectedAttempts int `json:"rejectedAttempts,omitempty"`
}

const (
	FailureAttachmentExpired     = "AttachmentExpired"
	FailureAttachmentUnavailable = "AttachmentUnavailable"
)

type inboundWatermark struct {
	CreatedAtMs int64    `json:"createdAtMs"`
	Keys        []string `json:"keys"`
}

// syncState is the state file.
type syncState struct {
	LoginFingerprint      string                         `json:"login_fingerprint"`
	SyncBuf               string                         `json:"sync_buf"`
	ContextTokens         map[string]string              `json:"context_tokens"`
	ProcessedMessageKeys  []string                       `json:"processed_message_keys"`
	SentReplyProgress     map[string]int                 `json:"sent_reply_progress"`
	ReplyDeliveries       map[string]ReplyDeliveryState  `json:"reply_deliveries"`
	BaselineEstablished   bool                           `json:"baseline_established"`
	BaselineInProgress    bool                           `json:"baseline_in_progress,omitempty"`
	BaselineEstablishedAt string                         `json:"baseline_established_at,omitempty"`
	ContextBudgets        map[string]ContextBudgetState  `json:"context_budgets,omitempty"`
	ContextWindows        map[string]ChatContextWindow   `json:"context_windows"`
	AccountSendBudget     *AccountSendBudget             `json:"account_send_budget,omitempty"`
	InboundWatermarks     map[string]inboundWatermark    `json:"inbound_watermarks"`
	DeliveryOutbox        map[string]*DeliveryOutboxItem `json:"delivery_outbox"`
	ReplyOrder            []string                       `json:"reply_order,omitempty"`
	ReceivedIdentities    []receivedIdentity             `json:"received_identities,omitempty"`
}

// receivedIdentity is the content hash of a message handed to Collie.
type receivedIdentity struct {
	Key  string `json:"key"`
	Hash string `json:"hash"`
}

// StateManager is the gateway's durable state: cursor, context tokens,
// dedupe keys, reply plans, windows, budget and outbox, in one private JSON
// file. Every change is written before it counts; a failed write restores
// the previous values.
type StateManager struct {
	StatePath string
	Account   *Account
	Policy    ContextPolicy

	SyncBuf               string
	ProcessedMessageKeys  []string
	SentReplyProgress     map[string]int
	ReplyDeliveries       map[string]ReplyDeliveryState
	BaselineEstablished   bool
	BaselineInProgress    bool
	BaselineEstablishedAt string

	ContextWindows    map[string]ChatContextWindow
	AccountBudget     *AccountSendBudget
	ContextTokens     map[string]string
	InboundWatermarks map[string]inboundWatermark
	Outbox            map[string]DeliveryOutboxItem
	received          []receivedIdentity
	replyOrder        []string

	processed               map[string]bool
	replyEvidenceProtection func(string) bool
	// failSave, when set, makes the next checkpoints fail (tests).
	failSave func() error
}

// NewStateManager loads statePath for account (nil when not signed in).
func NewStateManager(account *Account, statePath string, policy PolicyOptions) (*StateManager, error) {
	p, err := ValidateContextPolicy(policy)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(statePath)
	if err != nil {
		return nil, err
	}
	s := &StateManager{StatePath: abs, Policy: p, replyEvidenceProtection: func(string) bool { return false }}
	if account != nil {
		a := *account
		s.Account = &a
	}
	s.reset()
	if err := s.loadState(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *StateManager) reset() {
	s.SyncBuf = ""
	s.ContextTokens = map[string]string{}
	s.ProcessedMessageKeys = nil
	s.processed = map[string]bool{}
	s.SentReplyProgress = map[string]int{}
	s.ReplyDeliveries = map[string]ReplyDeliveryState{}
	s.ContextWindows = map[string]ChatContextWindow{}
	s.AccountBudget = nil
	s.InboundWatermarks = map[string]inboundWatermark{}
	s.Outbox = map[string]DeliveryOutboxItem{}
	s.received = nil
	s.replyOrder = nil
	s.BaselineEstablished = false
	s.BaselineInProgress = false
	s.BaselineEstablishedAt = ""
}

func (s *StateManager) SetReplyEvidenceProtection(protect func(string) bool) {
	s.replyEvidenceProtection = protect
}

// LoginFingerprint identifies the bound account and token: a change of
// either one starts from an empty state.
func (s *StateManager) LoginFingerprint() string {
	if s.Account == nil {
		return ""
	}
	return hashJSON([]string{s.Account.AccountID, s.Account.Token})
}

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (s *StateManager) loadState() error {
	var st syncState
	found, err := readPrivateJSON(s.StatePath, 0, &st)
	if err == nil && found {
		err = s.restore(&st)
	}
	if err != nil {
		// Never interpolate the parse error: it can quote context tokens.
		kind := "Error"
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			kind = "InvalidJsonSyntax"
		}
		return fmt.Errorf("Failed to load gateway state at %s: %s", s.StatePath, kind)
	}
	return nil
}

func (s *StateManager) restore(st *syncState) error {
	if st.LoginFingerprint != "" && st.LoginFingerprint != s.LoginFingerprint() {
		s.reset()
		return nil
	}
	s.SyncBuf = st.SyncBuf
	for k, v := range st.ContextTokens {
		if v != "" {
			s.ContextTokens[k] = v
		}
	}
	var keys []string
	for _, k := range st.ProcessedMessageKeys {
		if k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) > MaxProcessedMessageKeys {
		keys = keys[len(keys)-MaxProcessedMessageKeys:]
	}
	s.setProcessed(keys)
	for k, v := range st.SentReplyProgress {
		if v > 0 {
			s.SentReplyProgress[k] = v
		}
	}
	s.BaselineEstablished = st.BaselineEstablished
	if err := s.restoreDeliveryState(st); err != nil {
		return err
	}
	for key, item := range st.ReplyDeliveries {
		if !digestPattern.MatchString(item.Digest) || item.TotalChunks <= 0 || item.AcknowledgedChunks < 0 ||
			item.AcknowledgedChunks > item.TotalChunks || item.InFlight && item.AcknowledgedChunks == item.TotalChunks ||
			item.FailureReason != "" && (item.FailureReason != FailureAttachmentExpired && item.FailureReason != FailureAttachmentUnavailable ||
				item.InFlight || item.AcknowledgedChunks == item.TotalChunks) ||
			item.RenewalRejected && (!strings.HasPrefix(key, "renewal:") || item.InFlight || item.AcknowledgedChunks == item.TotalChunks) ||
			item.RejectedAttempts < 0 {
			return errors.New("Invalid reply delivery state")
		}
		s.ReplyDeliveries[key] = item
	}
	if len(s.ReplyDeliveries) > MaxReplyDeliveries {
		return errors.New("Reply delivery state exceeds capacity")
	}
	s.BaselineInProgress = st.BaselineInProgress && !s.BaselineEstablished
	s.BaselineEstablishedAt = st.BaselineEstablishedAt
	s.received = st.ReceivedIdentities
	s.replyOrder = st.ReplyOrder
	return nil
}

func (s *StateManager) setProcessed(keys []string) {
	s.ProcessedMessageKeys = keys
	s.processed = make(map[string]bool, len(keys))
	for _, k := range keys {
		s.processed[k] = true
	}
}

func (s *StateManager) SaveState() error {
	if s.failSave != nil {
		if err := s.failSave(); err != nil {
			return err
		}
	}
	keys := s.ProcessedMessageKeys
	if len(keys) > MaxProcessedMessageKeys {
		keys = keys[len(keys)-MaxProcessedMessageKeys:]
	}
	s.replyOrder = s.replyKeys()
	outbox := make(map[string]*DeliveryOutboxItem, len(s.Outbox))
	for k, v := range s.Outbox {
		v := v
		outbox[k] = &v
	}
	st := syncState{
		LoginFingerprint: s.LoginFingerprint(), SyncBuf: s.SyncBuf, ContextTokens: s.ContextTokens,
		ProcessedMessageKeys: nonNil(keys), SentReplyProgress: s.SentReplyProgress, ReplyDeliveries: s.ReplyDeliveries,
		BaselineEstablished: s.BaselineEstablished, ContextWindows: s.ContextWindows, AccountSendBudget: s.AccountBudget,
		InboundWatermarks: s.InboundWatermarks, DeliveryOutbox: outbox, ReceivedIdentities: s.received, ReplyOrder: s.replyOrder,
		BaselineInProgress: s.BaselineInProgress, BaselineEstablishedAt: s.BaselineEstablishedAt,
	}
	return writePrivateJSON(s.StatePath, st)
}

func nonNil(keys []string) []string {
	if keys == nil {
		return []string{}
	}
	return keys
}

// save runs a change and writes it; when the write fails, undo puts the
// previous values back.
func (s *StateManager) save(undo func()) error {
	if err := s.SaveState(); err != nil {
		undo()
		return err
	}
	return nil
}

func (s *StateManager) IsReadyForActivePolling() bool {
	return s.BaselineEstablished && !s.BaselineInProgress
}

func (s *StateManager) AssertCanEstablishBaseline() error {
	if s.Account == nil || s.Account.Token == "" {
		return errors.New("Cannot establish baseline: WeChat credentials missing or invalid")
	}
	if s.BaselineEstablished {
		return errors.New("Baseline is already established; refusing duplicate baseline initialization")
	}
	if len(s.SentReplyProgress) > 0 || len(s.ReplyDeliveries) > 0 {
		return errors.New("Cannot establish baseline: active in-progress reply chunks exist in state")
	}
	if !s.BaselineInProgress && (s.SyncBuf != "" || len(s.ProcessedMessageKeys) > 0) {
		return errors.New("Cannot establish baseline: state already contains active sync cursor or processed message records without baseline_in_progress")
	}
	return nil
}

func (s *StateManager) BeginBaseline() error {
	if err := s.AssertCanEstablishBaseline(); err != nil {
		return err
	}
	prev := s.BaselineInProgress
	s.BaselineInProgress = true
	return s.save(func() { s.BaselineInProgress = prev })
}

func (s *StateManager) MarkBaselineEstablished() error {
	prevEst, prevIn, prevAt := s.BaselineEstablished, s.BaselineInProgress, s.BaselineEstablishedAt
	s.BaselineEstablished, s.BaselineInProgress, s.BaselineEstablishedAt = true, false, isoTime(nowMs())
	return s.save(func() { s.BaselineEstablished, s.BaselineInProgress, s.BaselineEstablishedAt = prevEst, prevIn, prevAt })
}

// ComputeDeliveryKey is the message's identity across restarts: account,
// chat and the platform's id.
func (s *StateManager) ComputeDeliveryKey(chatID, clientID string) string {
	if clientID == "" {
		return ""
	}
	accountID := ""
	if s.Account != nil {
		accountID = s.Account.AccountID
	}
	return jsonText([]string{accountID, chatID, clientID})
}

func (s *StateManager) IsMessageProcessed(key string) bool { return key != "" && s.processed[key] }

// RecordProcessedMessageKey marks a message done. preserveReplyEvidence
// keeps its reply plan, which a pending or uncertain reply still needs.
func (s *StateManager) RecordProcessedMessageKey(key string, preserveReplyEvidence bool) error {
	if key == "" {
		return nil
	}
	previous := append([]string(nil), s.ProcessedMessageKeys...)
	progress, hadProgress := s.SentReplyProgress[key]
	delivery, hadDelivery := s.ReplyDeliveries[key]
	if !s.processed[key] {
		keys := append(s.ProcessedMessageKeys, key)
		if len(keys) > MaxProcessedMessageKeys {
			keys = append([]string(nil), keys[len(keys)-MaxProcessedMessageKeys:]...)
		}
		s.setProcessed(keys)
	}
	if !preserveReplyEvidence {
		delete(s.SentReplyProgress, key)
		delete(s.ReplyDeliveries, key)
	}
	return s.save(func() {
		s.setProcessed(previous)
		if hadProgress {
			s.SentReplyProgress[key] = progress
		}
		if hadDelivery {
			s.ReplyDeliveries[key] = delivery
		}
	})
}

func (s *StateManager) SetContextToken(chatID, token string) {
	if chatID == "" || token == "" {
		return
	}
	s.ContextTokens[chatID] = token
}

// CommitCursor stores the poll cursor; called only after its batch is done.
func (s *StateManager) CommitCursor(cursor string) error {
	previous := s.SyncBuf
	s.SyncBuf = cursor
	return s.save(func() { s.SyncBuf = previous })
}

// receivedHash is the content hash recorded when key was handed to Collie.
func (s *StateManager) receivedHash(key string) (string, bool) {
	for _, r := range s.received {
		if r.Key == key {
			return r.Hash, true
		}
	}
	return "", false
}

// recordReceived remembers what key carried, so the same id with other
// content is refused instead of silently merged.
func (s *StateManager) recordReceived(key, hash string) error {
	if _, ok := s.receivedHash(key); ok {
		return nil
	}
	previous := s.received
	next := append(append([]receivedIdentity(nil), s.received...), receivedIdentity{Key: key, Hash: hash})
	if len(next) > maxReceivedIdentities {
		next = next[len(next)-maxReceivedIdentities:]
	}
	s.received = next
	return s.save(func() { s.received = previous })
}
