package wechat

import (
	"context"
	"errors"
	"time"
)

const (
	DefaultPollIntervalMs = 1000
	pollBackoffMinMs      = 1000
	pollBackoffMaxMs      = 60_000
)

// pollBackoffMs is the wait after consecutive failed polls: 1 s doubling to
// 60 s; a success starts over.
func pollBackoffMs(failures int) int64 {
	ms := int64(pollBackoffMinMs)
	for i := 1; i < failures && ms < pollBackoffMaxMs; i++ {
		ms *= 2
	}
	return min(ms, pollBackoffMaxMs)
}

// RunLoop polls until stopped, with the notification cycle beside it. A
// failed poll goes to onError and waits its backoff; an expired login ends
// the loop with ErrSessionExpired.
func (g *Gateway) RunLoop(pollIntervalMs int64, onError, onNotificationError func(error)) (err error) {
	g.loop.do(func() { err = g.runLoop(pollIntervalMs, onError, onNotificationError) })
	return err
}

func (g *Gateway) runLoop(pollIntervalMs int64, onError, onNotificationError func(error)) error {
	if !g.state.IsReadyForActivePolling() {
		return errors.New("Gateway cannot start poll loop: baseline is not established. Sign in again to establish it.")
	}
	g.onInboundError = onError
	g.runner.Schedule()
	var notificationFailure error
	notifyDone := make(chan struct{})
	go g.loop.do(func() {
		defer close(notifyDone)
		for !g.stopped {
			if err := g.notifyOnce(onError, onNotificationError); err != nil {
				notificationFailure = err
				g.stop()
				return
			}
			if g.stopped {
				return
			}
			g.waitNotification(pollIntervalMs)
		}
	})
	var loopErr error
	failures := 0
	for !g.stopped {
		_, err := g.pollOnce(g.ctx, false)
		wait := pollIntervalMs
		if err == nil {
			failures = 0
			g.runner.Schedule()
		} else {
			if g.stopped {
				break
			}
			if errors.Is(err, ErrSessionExpired) {
				loopErr = err
				break
			}
			failures++
			wait = pollBackoffMs(failures)
			if onError == nil {
				loopErr = err
				break
			}
			onError(err)
		}
		if g.stopped {
			break
		}
		if wait > 0 {
			g.loop.sleep(g.ctx, msDuration(wait))
		}
	}
	g.stop()
	g.loop.wait(context.Background(), notifyDone)
	g.runner.Stop()
	if loopErr != nil {
		return loopErr
	}
	if notificationFailure != nil {
		return notificationFailure
	}
	return g.inboundErr
}

// notifyOnce runs one notification cycle; an error back means the loop
// must stop (a failing error callback).
func (g *Gateway) notifyOnce(onError, onNotificationError func(error)) error {
	var err error
	if g.diagnostics != nil {
		err = g.diagnostics.Run(g.ctx, func(ctx context.Context) error {
			_, err := g.deliverNotificationsOnce(ctx)
			return err
		}, func() bool { return !g.stopped && g.ctx.Err() == nil }, func(error) bool { return true })
	} else {
		_, err = g.deliverNotificationsOnce(g.ctx)
	}
	if err == nil || g.stopped || g.ctx.Err() != nil {
		return nil
	}
	switch {
	case onNotificationError != nil:
		onNotificationError(err)
	case onError != nil:
		onError(err)
	default:
		writeLogLine(map[string]any{"event": "notification_cycle_error", "errorType": SanitizeErrorType(err)}, "")
	}
	return nil
}

// waitNotification sleeps one interval, cut short by a new result or answer.
func (g *Gateway) waitNotification(intervalMs int64) {
	t := time.NewTimer(msDuration(max(minNotificationIntervalMs, intervalMs)))
	defer t.Stop()
	g.loop.await(func() {
		select {
		case <-t.C:
		case <-g.kick:
		case <-g.ctx.Done():
		}
	})
}
