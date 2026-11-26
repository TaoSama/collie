package wechat

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"strings"
)

// Health statuses.
const (
	HealthStarting   = "starting"
	HealthHealthy    = "healthy"
	HealthProcessing = "processing"
	HealthDegraded   = "degraded"
	HealthStopped    = "stopped"
)

var (
	ilinkHTTPPattern = regexp.MustCompile(`^WeChat iLink HTTP (\d{3})\b`)
	httpPattern      = regexp.MustCompile(`HTTP (\d{3})`)
)

// SanitizeErrorType turns an error into a category name. Messages can quote
// tokens or bodies, so only the category is ever shown.
func SanitizeErrorType(err error) string {
	if err == nil {
		return "UnknownError"
	}
	var receipt *SendReceiptError
	if errors.As(err, &receipt) {
		return "ILinkSend" + receipt.Reason
	}
	if errors.Is(err, ErrChatNotAllowed) {
		return "ChatNotAllowed"
	}
	if errors.Is(err, ErrSessionExpired) {
		return "SessionExpired"
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	name := errorName(err)
	switch {
	case ilinkHTTPPattern.MatchString(msg):
		return "ILinkHttp" + ilinkHTTPPattern.FindStringSubmatch(msg)[1]
	case msg == "WeChat iLink returned a non-object JSON response":
		return "ILinkInvalidResponse"
	case strings.Contains(msg, "WeChat getupdates failed"):
		return "ILinkGetUpdatesError"
	case strings.Contains(msg, "WeChat sendmessage failed"):
		return "ILinkSendMessageError"
	case name == "AbortError" || name == "TimeoutError" || strings.Contains(lower, "aborted") ||
		strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out"):
		return "TimeoutError"
	case httpPattern.MatchString(msg):
		return "Http" + httpPattern.FindStringSubmatch(msg)[1]
	case name == "SyntaxError" || strings.Contains(msg, "InvalidJson"):
		return "InvalidJsonSyntax"
	case name == "TypeError":
		return "NetworkError"
	case strings.Contains(msg, "baseline is not established"):
		return "BaselineNotEstablished"
	case strings.Contains(msg, "Baseline is already established"):
		return "BaselineAlreadyEstablished"
	case strings.Contains(msg, "Cannot establish baseline"):
		return "CannotEstablishBaseline"
	case strings.Contains(msg, "WeChat credentials"):
		return "InvalidWeChatCredentials"
	case strings.Contains(msg, "gateway state"):
		return "InvalidGatewayState"
	case strings.HasPrefix(msg, "Invalid ") || strings.Contains(msg, "must be a "):
		return "InvalidOption"
	}
	return "OperationalError"
}

// Phase is what the gateway is doing now and by when it must be done.
type Phase struct {
	Name       string `json:"name"` // polling | processing
	StartedAt  string `json:"started_at"`
	DeadlineAt string `json:"deadline_at"`
}

// GatewayHealth is the gateway's health, reported in connector.status.
type GatewayHealth struct {
	RunID                 string `json:"run_id"`
	Status                string `json:"status"`
	StartedAt             string `json:"started_at"`
	UpdatedAt             string `json:"updated_at"`
	LastPollStartedAt     string `json:"last_poll_started_at,omitempty"`
	LastPollSucceededAt   string `json:"last_poll_succeeded_at,omitempty"`
	ConsecutiveErrors     int    `json:"consecutive_errors"`
	TotalErrorCount       int    `json:"total_error_count"`
	LastErrorType         string `json:"last_error_type,omitempty"`
	LastErrorAt           string `json:"last_error_at,omitempty"`
	Phase                 *Phase `json:"phase,omitempty"`
	ProcessedMessageCount int    `json:"processed_message_count"`
}

var errInvalidPhaseBudget = errors.New("Invalid health phase budget")

// HealthTracker keeps GatewayHealth in memory; the host reads it through
// connector.status instead of a health file.
type HealthTracker struct {
	data          *GatewayHealth
	phaseRevision int
}

func (h *HealthTracker) StartRun() GatewayHealth {
	now := isoTime(nowMs())
	h.data = &GatewayHealth{RunID: "run_" + newUUID(), Status: HealthStarting, StartedAt: now, UpdatedAt: now}
	return *h.data
}

// Snapshot is the current health; ok false before StartRun.
func (h *HealthTracker) Snapshot() (GatewayHealth, bool) {
	if h == nil || h.data == nil {
		return GatewayHealth{}, false
	}
	d := *h.data
	if d.Phase != nil {
		p := *d.Phase
		d.Phase = &p
	}
	return d, true
}

func (h *HealthTracker) RecordPollStart(budgetMs int64) error {
	if h == nil || h.data == nil {
		return nil
	}
	now := isoTime(nowMs())
	h.data.LastPollStartedAt, h.data.UpdatedAt = now, now
	if err := h.setPhase("polling", budgetMs); err != nil {
		return err
	}
	return nil
}

func (h *HealthTracker) setPhase(name string, budgetMs int64) error {
	maxBudget := int64(MaxPollPhaseMs)
	if name == "processing" {
		maxBudget = MaxProcessingPhaseMs
	}
	if budgetMs <= 0 || budgetMs > maxBudget {
		return errInvalidPhaseBudget
	}
	now := nowMs()
	h.data.Phase = &Phase{Name: name, StartedAt: isoTime(now), DeadlineAt: isoTime(now + budgetMs)}
	h.phaseRevision++
	return nil
}

func (h *HealthTracker) RecordMessageStart(budgetMs int64) (int, error) {
	if h == nil || h.data == nil {
		return 0, nil
	}
	if err := h.setPhase("processing", budgetMs); err != nil {
		return 0, err
	}
	h.data.UpdatedAt = isoTime(nowMs())
	return h.phaseRevision, nil
}

func (h *HealthTracker) RecordMessageProgress() {
	if h == nil || h.data == nil {
		return
	}
	h.data.ProcessedMessageCount++
	h.data.UpdatedAt = isoTime(nowMs())
	h.data.Phase = nil
}

// RecordProcessingEnd clears the phase unless a newer one began (revision).
func (h *HealthTracker) RecordProcessingEnd(revision int) {
	if h == nil || h.data == nil || revision != 0 && revision != h.phaseRevision {
		return
	}
	h.data.Phase = nil
	h.data.UpdatedAt = isoTime(nowMs())
}

func (h *HealthTracker) RecordPollSuccess() {
	if h == nil || h.data == nil {
		return
	}
	now := isoTime(nowMs())
	h.data.LastPollSucceededAt, h.data.UpdatedAt = now, now
	h.data.ConsecutiveErrors = 0
	h.data.Status = HealthHealthy
	h.data.Phase = nil
}

func (h *HealthTracker) RecordPollError(err error) {
	if h == nil || h.data == nil {
		return
	}
	now := isoTime(nowMs())
	h.data.UpdatedAt = now
	h.data.ConsecutiveErrors++
	h.data.TotalErrorCount++
	h.data.LastErrorType = SanitizeErrorType(err)
	h.data.LastErrorAt = now
	h.data.Status = HealthDegraded
	h.data.Phase = nil
}

// errorName is the JavaScript-style category of err.
func errorName(err error) string {
	var syntax *json.SyntaxError
	var netErr net.Error
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "TimeoutError"
	case errors.Is(err, context.Canceled), errors.Is(err, errAbort):
		return "AbortError"
	case errors.As(err, &syntax):
		return "SyntaxError"
	case errors.As(err, &dnsErr), errors.As(err, &netErr):
		return "TypeError"
	}
	return "Error"
}
