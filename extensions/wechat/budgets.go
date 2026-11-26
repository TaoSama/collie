package wechat

import (
	"context"
	"errors"
	"time"
)

// Time budgets, in milliseconds like the state they are compared with.
const (
	DefaultPollTimeoutMs   = 35000
	MaxPollTimeoutMs       = 120000
	PollNetworkMarginMs    = 5000
	MaxPollAttempts        = 1
	SendRequestTimeoutMs   = 15000
	ReplyTimeoutMs         = 60000 // the complete multipart reply, every request included
	NotificationReplyMs    = 30 * 60 * 1000
	ChatAuthorizationMs    = 10000
	CheckpointMarginMs     = 5000
	MaxEngineTurnTimeoutMs = 30 * 60_000
	// The host answers in its own time: a reply is still expected this long
	// after the message was handed over.
	DefaultHostTimeoutMs  = 105000
	DefaultHostMaxRetries = 2
	MaxEngineRequestMs    = MaxEngineTurnTimeoutMs + DefaultHostTimeoutMs*(DefaultHostMaxRetries+1)
	MaxProcessingPhaseMs  = MaxEngineRequestMs + ReplyTimeoutMs + ChatAuthorizationMs + CheckpointMarginMs
	MaxPollPhaseMs        = (MaxPollTimeoutMs+PollNetworkMarginMs)*MaxPollAttempts + CheckpointMarginMs

	// Local transfer policy, independent of any WeChat size or protocol limit.
	MaxTransferTimeoutMs    = 10 * 60_000
	transferBytesPerSecond  = 1024 * 1024
	maxSafeInteger          = 1<<53 - 1
	millisecondsPerHour     = 60 * 60 * 1000
	millisecondsPerDay      = 24 * millisecondsPerHour
	maxTimestampMs          = 8_640_000_000_000_000
	defaultNotifyIntervalMs = 1000
)

var errInvalidTransferBudget = errors.New("Invalid transfer budget")

// transferTimeoutMs is one MiB/s plus a setup allowance; every retry shares
// this single bounded deadline.
func transferTimeoutMs(byteLength, setupMs int64) (int64, error) {
	if byteLength <= 0 || byteLength > maxSafeInteger || setupMs <= 0 || setupMs > MaxTransferTimeoutMs {
		return 0, errInvalidTransferBudget
	}
	ms := setupMs + (byteLength*1000+transferBytesPerSecond-1)/transferBytesPerSecond
	return min(MaxTransferTimeoutMs, ms), nil
}

func msDuration(ms int64) time.Duration { return time.Duration(ms) * time.Millisecond }

// errAbort is what a cancelled operation reports when its context carries no
// cause of its own.
var errAbort = errors.New("This operation was aborted")

// withTimeout bounds ctx and names the cause, the way AbortSignal reasons do.
func withTimeout(ctx context.Context, ms int64, cause error) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(ctx, msDuration(ms), cause)
}

// abortCause is why ctx ended.
func abortCause(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if ctx.Err() != nil {
		return errAbort
	}
	return nil
}
