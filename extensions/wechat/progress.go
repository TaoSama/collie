package wechat

import (
	"context"
	"fmt"
	"strings"
)

const (
	QueryProgressLimit = 3
	MaxProgressChars   = 2000
)

// Progress outcome statuses.
const (
	ProgressSent         = "sent"
	ProgressNotSent      = "not_sent"
	ProgressLimitReached = "limit_reached"
	ProgressUnavailable  = "unavailable"
)

var safeProgressReasons = map[string]bool{"budget_exhausted": true, "window_expired": true, "context_changed": true, "limit_reached": true,
	"previous_send_unknown": true, "send_unknown": true, "rejected": true, "not_submitted": true, "delivery_failed": true, "unavailable": true}

// ProgressSink sends one progress note under id; sent false with a reason
// when it did not go out.
type ProgressSink func(ctx context.Context, text, id string) (sent bool, reason string, messageID string)

// QueryProgressResult is one report's outcome.
type QueryProgressResult struct {
	Sent      bool   `json:"sent"`
	Status    string `json:"status"`
	Used      int    `json:"used"`
	Remaining int    `json:"remaining"`
	Limit     int    `json:"limit"`
	Reason    string `json:"reason,omitempty"`
	MessageID string `json:"-"`
}

// QueryProgress is one request's progress allowance, shared by every
// message steered into it. An attempted send uses its slot even when its
// receipt is unknown; a failed note is never retried and later messages
// never grant more slots.
type QueryProgress struct {
	used     int
	blocked  string
	identity string
	sink     ProgressSink
	tail     chan struct{}
	loop     *loop
}

func NewQueryProgress(l *loop, chatID, messageID string, sink ProgressSink) *QueryProgress {
	return &QueryProgress{identity: hashJSON([]string{chatID, messageID}), sink: sink, loop: l}
}

func (q *QueryProgress) Remaining() int {
	if q.blocked != "" {
		return 0
	}
	return QueryProgressLimit - q.used
}

// Report sends text after every earlier report of this request finished.
func (q *QueryProgress) Report(ctx context.Context, text string) QueryProgressResult {
	previous := q.tail
	mine := make(chan struct{})
	q.tail = mine
	defer func() {
		close(mine)
		if q.tail == mine {
			q.tail = nil
		}
	}()
	if previous != nil {
		q.loop.wait(context.Background(), previous)
	}
	return q.reportNow(ctx, text)
}

func (q *QueryProgress) reportNow(ctx context.Context, text string) QueryProgressResult {
	if strings.TrimSpace(text) == "" || utf16Len(text) > MaxProgressChars {
		return q.result(ProgressNotSent, false, "rejected", "")
	}
	if q.blocked != "" {
		return q.result(ProgressUnavailable, false, q.blocked, "")
	}
	if q.Remaining() == 0 {
		return q.result(ProgressLimitReached, false, "limit_reached", "")
	}
	q.used++
	sequence := q.used
	if q.sink == nil {
		return q.result(ProgressUnavailable, false, "unavailable", "")
	}
	sent, reason, id := q.sink(ctx, text, fmt.Sprintf("query-progress:%s:%d", q.identity, sequence))
	if !safeProgressReasons[reason] {
		reason = "delivery_failed"
	}
	if !sent && (reason == DeferBudgetExhausted || reason == DeferWindowExpired) {
		q.blocked = reason
	}
	if sent {
		return q.result(ProgressSent, true, "", id)
	}
	return q.result(ProgressNotSent, false, reason, "")
}

func (q *QueryProgress) result(status string, sent bool, reason, id string) QueryProgressResult {
	return QueryProgressResult{Sent: sent, Status: status, Used: q.used, Remaining: q.Remaining(), Limit: QueryProgressLimit, Reason: reason, MessageID: id}
}
