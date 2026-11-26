package wechat

import (
	"errors"
	"regexp"
	"strings"
)

const (
	ResultTextChars     = 3800
	ResultTextMessages  = 3
	ResultFileBytes     = 50 * 1024 * 1024
	ResultPendingLimit  = 128
	ResultOutboxLimit   = 1000
	ResultDispatchMs    = 60_000
	deliveryAttemptMs   = 30 * 60_000
	terminalResultTTLMs = 7 * millisecondsPerDay
	progressResultTTLMs = 5 * 60_000
	resultRetentionMs   = 7 * millisecondsPerDay
	downloadLinkPrefix  = "完整结果下载："
	resultArtifactName  = "result.md"
	maxResultKeyChars   = 512
	maxResultNameBytes  = 255
)

// Result plan statuses.
const (
	ResultPreparing      = "preparing"
	ResultReady          = "ready"
	ResultClaimed        = "claimed"
	ResultWaitingForUser = "waiting_for_user"
	ResultAccepted       = "accepted"
	ResultUncertain      = "uncertain"
	ResultSuperseded     = "superseded"
)

// Result modes: up to three text messages; one message and a file; or a
// summary and a download link.
const (
	ModeText       = "text"
	ModeAttachment = "attachment"
	ModeDownload   = "download"
)

var (
	uuidPattern            = regexp.MustCompile(`(?i)^[a-f\d]{8}-[a-f\d]{4}-[a-f\d]{4}-[a-f\d]{4}-[a-f\d]{12}$`)
	resultStatuses         = map[string]bool{ResultPreparing: true, ResultReady: true, ResultClaimed: true, ResultWaitingForUser: true, ResultAccepted: true, ResultUncertain: true, ResultSuperseded: true}
	resultModes            = map[string]bool{ModeText: true, ModeAttachment: true, ModeDownload: true}
	retryableDeferReasons  = map[string]bool{"not_submitted": true, "artifact_unavailable": true, "reply_pending": true, "attempt_expired": true}
	errResultSchemaInvalid = errors.New("Result delivery schema invalid")
)

// ResultArtifact is the file a result carries (attachment) or links to
// (download). Text results keep their full text here too.
type ResultArtifact struct {
	ID          string `json:"id"`
	ChatID      string `json:"chatId"`
	Name        string `json:"name"`
	Size        int    `json:"size"`
	SHA256      string `json:"sha256"`
	ExpiresAt   int64  `json:"expiresAt"`
	DownloadURL string `json:"downloadUrl,omitempty"`
}

// ResultSource is what produced a result.
type ResultSource struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// ResultBusiness lets a newer result replace an older one not yet sent.
type ResultBusiness struct {
	Category   string `json:"category,omitempty"`
	Supersedes string `json:"supersedes,omitempty"`
}

// ResultDelivery is one result's delivery plan and where it stands.
type ResultDelivery struct {
	ID                    string          `json:"id"`
	Key                   string          `json:"key"`
	ChatID                string          `json:"chatId"`
	Source                ResultSource    `json:"source"`
	Kind                  string          `json:"kind"`
	Status                string          `json:"status"`
	Mode                  string          `json:"mode"`
	Messages              []string        `json:"messages"`
	Artifact              ResultArtifact  `json:"artifact"`
	CreatedAt             int64           `json:"createdAt"`
	ExpiresAt             int64           `json:"expiresAt"`
	RetainedUntil         int64           `json:"retainedUntil"`
	Business              *ResultBusiness `json:"business,omitempty"`
	AttemptID             string          `json:"attemptId,omitempty"`
	AttemptedAt           int64           `json:"attemptedAt,omitempty"`
	AttemptExpiresAt      int64           `json:"attemptExpiresAt,omitempty"`
	DeferReason           string          `json:"deferReason,omitempty"`
	Error                 string          `json:"error,omitempty"`
	SubmittedMessages     int             `json:"submittedMessages,omitempty"`
	LastDeferredAttemptID string          `json:"lastDeferredAttemptId,omitempty"`
	LastDeferReason       string          `json:"lastDeferReason,omitempty"`
	LastDeferredAt        int64           `json:"lastDeferredAt,omitempty"`
}

func validText(s string, max int) bool { return strings.TrimSpace(s) != "" && utf16Len(s) <= max }

func validTime(t int64) bool { return t >= 0 && t <= maxTimestampMs }

// ValidateResultDelivery checks a plan's shape. A materialized download
// plan carries its link: summary and link messages.
func ValidateResultDelivery(d ResultDelivery, materialized bool) error {
	bad := errResultSchemaInvalid
	if !uuidPattern.MatchString(d.ID) || !validText(d.Key, maxResultKeyChars) || !validText(d.ChatID, maxResultKeyChars) ||
		!validText(d.Source.Type, maxResultKeyChars) || !validText(d.Source.ID, maxResultKeyChars) || !validText(d.Kind, maxResultKeyChars) ||
		!resultStatuses[d.Status] || !resultModes[d.Mode] {
		return bad
	}
	limit := 2
	switch d.Mode {
	case ModeText:
		limit = ResultTextMessages
	case ModeAttachment:
		limit = 1
	}
	if len(d.Messages) > limit || d.Status != ResultPreparing && len(d.Messages) == 0 ||
		materialized && d.Mode == ModeDownload && len(d.Messages) != 2 {
		return bad
	}
	for _, m := range d.Messages {
		if m == "" || utf16Len(m) > ResultTextChars {
			return bad
		}
	}
	a := d.Artifact
	if !uuidPattern.MatchString(a.ID) || a.ChatID != d.ChatID || !validText(a.Name, maxResultNameBytes) ||
		strings.ContainsAny(a.Name, "\r\n\x00/\\") || a.Name == "." || a.Name == ".." || a.Size < 1 || !sha256Pattern.MatchString(a.SHA256) ||
		materialized && d.Mode == ModeDownload && a.DownloadURL == "" {
		return bad
	}
	if !validTime(d.CreatedAt) || !validTime(d.ExpiresAt) || !validTime(d.RetainedUntil) || !validTime(a.ExpiresAt) ||
		d.ExpiresAt <= d.CreatedAt || d.RetainedUntil < d.ExpiresAt || a.ExpiresAt != d.RetainedUntil {
		return bad
	}
	if d.AttemptID != "" || d.AttemptedAt != 0 || d.AttemptExpiresAt != 0 {
		if !uuidPattern.MatchString(d.AttemptID) || d.AttemptedAt < d.CreatedAt || d.AttemptExpiresAt <= d.AttemptedAt {
			return bad
		}
	}
	attempted := d.Status == ResultClaimed || d.Status == ResultAccepted || d.Status == ResultUncertain
	if attempted != (d.AttemptID != "") {
		return bad
	}
	if d.LastDeferredAttemptID != "" && (!uuidPattern.MatchString(d.LastDeferredAttemptID) || !validText(d.LastDeferReason, 128)) ||
		d.SubmittedMessages < 0 {
		return bad
	}
	return nil
}

// resultIdentity is a plan's immutable identity; recovery verifies it.
func resultIdentity(d ResultDelivery) string {
	messages := d.Messages
	if d.Mode == ModeDownload && len(messages) > 1 {
		messages = messages[:1]
	}
	if messages == nil {
		messages = []string{}
	}
	var category, supersedes any
	if d.Business != nil {
		if d.Business.Category != "" {
			category = d.Business.Category
		}
		if d.Business.Supersedes != "" {
			supersedes = d.Business.Supersedes
		}
	}
	return hashJSON([]any{d.ID, d.Key, d.ChatID, d.Source.Type, d.Source.ID, d.Kind, d.Mode, messages, d.Artifact.ID, d.Artifact.Name,
		d.Artifact.Size, d.Artifact.SHA256, d.CreatedAt, d.ExpiresAt, d.RetainedUntil, category, supersedes})
}

// claimDelivery starts an attempt; nil when the plan cannot be claimed.
func claimDelivery(p ResultDelivery, attemptID string, now int64) *ResultDelivery {
	if p.AttemptID == attemptID && (p.Status == ResultClaimed || p.Status == ResultAccepted || p.Status == ResultUncertain) {
		if p.Status == ResultClaimed && p.AttemptExpiresAt <= now {
			p.Status = ResultUncertain
		}
		return &p
	}
	if p.Status != ResultReady || p.ExpiresAt <= now || p.RetainedUntil <= now || p.CreatedAt > now {
		return nil
	}
	p.Status, p.AttemptID, p.AttemptedAt, p.AttemptExpiresAt = ResultClaimed, attemptID, now, now+deliveryAttemptMs
	return &p
}

func receiptDelivery(p ResultDelivery, status, attemptID string) *ResultDelivery {
	if p.Status != ResultClaimed && p.Status != ResultAccepted && p.Status != ResultUncertain || p.AttemptID != attemptID {
		return nil
	}
	if p.Status != ResultAccepted {
		p.Status = status
	}
	return &p
}

// deferDelivery ends an attempt without a send: retryable reasons go back to
// ready, the others wait for the person's next message.
func deferDelivery(p ResultDelivery, attemptID, reason string, now int64, submitted int) *ResultDelivery {
	if p.LastDeferredAttemptID == attemptID {
		if p.LastDeferReason == reason {
			return &p
		}
		return nil
	}
	if p.Status != ResultClaimed || p.AttemptID != attemptID || p.AttemptExpiresAt <= now {
		return nil
	}
	p.AttemptID, p.AttemptedAt, p.AttemptExpiresAt = "", 0, 0
	p.Status = ResultWaitingForUser
	if retryableDeferReasons[reason] {
		p.Status = ResultReady
	}
	p.DeferReason = reason
	p.SubmittedMessages = max(p.SubmittedMessages, submitted)
	p.LastDeferredAttemptID, p.LastDeferReason, p.LastDeferredAt = attemptID, reason, now
	return &p
}

func canSupersede(older, newer ResultDelivery) bool {
	if older.ChatID != newer.ChatID || older.ID == newer.ID || older.CreatedAt > newer.CreatedAt ||
		older.Status != ResultReady && older.Status != ResultPreparing && older.Status != ResultWaitingForUser || older.SubmittedMessages > 0 {
		return false
	}
	sameSourceProgress := older.Kind == KindProgressResult && older.Source == newer.Source
	return sameSourceProgress || older.Business != nil && older.Business.Supersedes != "" && newer.Business != nil &&
		older.Business.Supersedes == newer.Business.Supersedes
}

// KindProgressResult is a milestone result: it uses the progress budget.
const KindProgressResult = "progress"

// splitResultMessages cuts at exact UTF-16 budgets, keeping every character
// and never dividing an emoji.
func splitResultMessages(text string, maxChars int) []string {
	u := toUTF16(text)
	var chunks []string
	for offset := 0; offset < len(u); {
		end := min(len(u), offset+maxChars)
		if end < len(u) && isHighSurrogate(u[end-1]) && isLowSurrogate(u[end]) {
			end--
		}
		chunks = append(chunks, fromUTF16(u[offset:end]))
		offset = end
	}
	return chunks
}

// selectResultDeliveries keeps one important, recent result per chat per
// pass; terminal results outrank milestones.
func selectResultDeliveries(items []ResultDelivery) []ResultDelivery {
	ordered := append([]ResultDelivery(nil), items...)
	sortStable(ordered, func(a, b ResultDelivery) bool {
		ap, bp := a.Kind == KindProgressResult, b.Kind == KindProgressResult
		if ap != bp {
			return !ap
		}
		if a.CreatedAt != b.CreatedAt {
			return a.CreatedAt > b.CreatedAt
		}
		return a.ID < b.ID
	})
	seen := map[string]bool{}
	var out []ResultDelivery
	for _, item := range ordered {
		if !seen[item.ChatID] {
			seen[item.ChatID] = true
			out = append(out, item)
		}
	}
	return out
}
