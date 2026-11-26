package wechat

import (
	"context"
	"errors"
)

const maxReportedFailures = 8

// recoveryStore is where diagnostics keep their evidence across restarts.
type recoveryStore interface {
	Load(now int64) (RecoveryLoad, error)
	Save(e *RecoveryEvidence) error
	Clear() error
}

// NotificationCycleDiagnostics reports the notification cycle's failures and
// its recovery, independent of the inbound poll's health. A skipped request
// is not recovery evidence: only its success clears it.
type NotificationCycleDiagnostics struct {
	retryDelayMs int64
	sink         func(map[string]any)
	now          func() int64
	store        recoveryStore

	ready, restored bool
	evidence        RecoveryEvidence
	failing         bool // FirstFailedAt is set
	recovered       []string
	dirty           bool
	persistFailing  bool
}

func NewNotificationCycleDiagnostics(retryDelayMs int64, sink func(map[string]any), now func() int64, store recoveryStore) *NotificationCycleDiagnostics {
	if sink == nil {
		sink = func(e map[string]any) { writeLogLine(e, "[gateway] diagnostics sink failed\n") }
	}
	if now == nil {
		now = nowMs
	}
	return &NotificationCycleDiagnostics{retryDelayMs: retryDelayMs, sink: sink, now: now, store: store,
		evidence: RecoveryEvidence{Pending: map[string]PendingFailure{}}}
}

// Run runs one cycle, observing its requests.
func (d *NotificationCycleDiagnostics) Run(ctx context.Context, action func(context.Context) error, active func() bool, reportable func(error) bool) error {
	if !d.restored {
		d.restore()
	}
	o := newObservation()
	if err := action(observeRequests(ctx, o)); err != nil {
		if active() && reportable(err) {
			d.failed(err, o)
		}
		return err
	}
	if active() {
		d.succeeded(o)
	}
	return nil
}

func (d *NotificationCycleDiagnostics) restore() {
	d.restored = true
	if d.store == nil {
		return
	}
	loaded, err := d.store.Load(d.now())
	if err != nil {
		reason := "unreadable"
		if errors.Is(err, ErrInvalidRecoveryState) {
			reason = "invalid"
		}
		name, codes := safeErrorFacts(err)
		d.discard(reason, map[string]any{"errorName": name, "causeCodes": codes})
		return
	}
	switch loaded.Kind {
	case "absent":
		return
	case "expired":
		d.discard("expired", map[string]any{"ageMs": loaded.AgeMs, "unresolvedFailures": loaded.UnresolvedFailures})
		return
	}
	e := loaded.Evidence
	d.evidence = *e
	d.failing = true
	d.recovered = append([]string(nil), e.RecoveredOperations...)
	now := d.now()
	d.emit("notification_cycle_restored", map[string]any{"unresolvedFailures": len(e.Pending), "totalFailures": e.TotalFailures,
		"ageMs": max(0, now-e.FirstFailedAt), "lastFailureAgeMs": max(0, now-e.LastFailedAt),
		"recoveryEvidenceOverflow": e.EvidenceOverflow})
}

func (d *NotificationCycleDiagnostics) discard(reason string, fields map[string]any) {
	fields["reason"] = reason
	d.emit("notification_cycle_restore_discarded", fields)
	if err := d.store.Clear(); err != nil {
		d.persistFailed(err)
	}
}

func (d *NotificationCycleDiagnostics) persist() {
	if d.store == nil || !d.dirty {
		return
	}
	var err error
	if !d.failing {
		err = d.store.Clear()
	} else {
		e := d.evidence
		e.RecoveredOperations = d.recovered
		err = d.store.Save(&e)
	}
	if err != nil {
		// Stay dirty so the next completed cycle retries; log only the move into failure.
		d.persistFailed(err)
		return
	}
	d.dirty, d.persistFailing = false, false
}

func (d *NotificationCycleDiagnostics) persistFailed(err error) {
	if !d.persistFailing {
		name, codes := safeErrorFacts(err)
		d.emit("notification_cycle_persist_failed", map[string]any{"errorName": name, "causeCodes": codes})
	}
	d.persistFailing = true
}

func (d *NotificationCycleDiagnostics) clearCompleted(o *requestObservation) {
	o.mu.Lock()
	successes := make([]string, 0, len(o.successes))
	for k := range o.successes {
		successes = append(successes, k)
	}
	o.mu.Unlock()
	for _, key := range successes {
		f, ok := d.evidence.Pending[key]
		if !ok {
			continue
		}
		if f.Operation != "" && !containsString(d.recovered, f.Operation) {
			d.recovered = append(d.recovered, f.Operation)
		}
		delete(d.evidence.Pending, key)
		d.dirty = true
	}
}

func (d *NotificationCycleDiagnostics) emit(event string, fields map[string]any) {
	out := map[string]any{"event": event, "service": "gateway", "timestamp": isoTime(d.now())}
	for k, v := range fields {
		out[k] = v
	}
	d.sink(out)
}

func (d *NotificationCycleDiagnostics) rememberFailures(o *requestObservation) {
	now := d.now()
	o.mu.Lock()
	keys := append([]string(nil), o.order...)
	o.mu.Unlock()
	for _, key := range keys {
		o.mu.Lock()
		f, ok := o.failures[key]
		o.mu.Unlock()
		if !ok {
			continue
		}
		previous, had := d.evidence.Pending[key]
		if had || len(d.evidence.Pending) < MaxPendingFailures {
			d.evidence.set(key, pendingFailure(f, now, previous, had))
		} else {
			d.evidence.EvidenceOverflow = true
		}
		d.dirty = true
	}
}

func (d *NotificationCycleDiagnostics) failed(err error, o *requestObservation) {
	d.clearCompleted(o)
	f := notificationFailure(err)
	key := f.Key
	if key == "" {
		key = stageKey(f.Stage)
	}
	o.setFailure(key, f)
	d.rememberFailures(o)
	now := d.now()
	if !d.failing {
		d.evidence.FirstFailedAt, d.failing = now, true
	}
	d.evidence.LastFailedAt = now
	d.evidence.ConsecutiveFailures++
	d.evidence.TotalFailures++
	d.dirty = true
	var failed []map[string]any
	for i, item := range o.list() {
		if i >= maxReportedFailures {
			break
		}
		failed = append(failed, failureFields(item))
	}
	fields := failureFields(f)
	delete(fields, "stage")
	var lastOK any
	if d.evidence.LastSucceededAt != "" {
		lastOK = d.evidence.LastSucceededAt
	}
	for k, v := range map[string]any{"stage": f.Stage, "errorType": SanitizeErrorType(err), "consecutiveFailures": d.evidence.ConsecutiveFailures,
		"totalFailures": d.evidence.TotalFailures, "lastSucceededAt": lastOK, "retryScheduled": true, "retryDelayMs": d.retryDelayMs,
		"failedRequests": failed, "unresolvedFailures": len(d.evidence.Pending), "recoveryEvidenceOverflow": d.evidence.EvidenceOverflow} {
		fields[k] = v
	}
	d.emit("notification_cycle_failed", fields)
	d.persist()
}

func failureFields(f notificationFailureFacts) map[string]any {
	out := map[string]any{"stage": f.Stage, "errorName": f.ErrorName, "causeCodes": f.CauseCodes}
	if r := f.Request; r != nil {
		out["operation"], out["phase"], out["requestAttempts"], out["requestMaxAttempts"] = r.Operation, r.Phase, r.RequestAttempts, r.RequestMaxAttempts
		out["elapsedMs"], out["timedOut"], out["parentAborted"] = r.ElapsedMs, r.TimedOut, r.ParentAborted
		if r.HTTPStatus != 0 {
			out["httpStatus"] = r.HTTPStatus
		}
	}
	return out
}

func (d *NotificationCycleDiagnostics) succeeded(o *requestObservation) {
	d.clearCompleted(o)
	d.rememberFailures(o)
	if len(d.evidence.Pending) > 0 || d.evidence.EvidenceOverflow {
		d.persist()
		return
	}
	// A stage without matching request evidence stays unresolved, even if a later cycle skips it.
	if d.failing {
		d.emit("notification_cycle_recovered", map[string]any{"totalFailures": d.evidence.TotalFailures,
			"elapsedMs": d.now() - d.evidence.FirstFailedAt, "recoveredOperations": nonNil(d.recovered)})
		d.recovered = nil
		d.failing = false
		d.evidence.FirstFailedAt, d.evidence.LastFailedAt = 0, 0
		d.dirty = true
	} else if !d.ready {
		d.emit("notification_cycle_ready", map[string]any{})
	}
	d.evidence.LastSucceededAt = isoTime(d.now())
	d.evidence.ConsecutiveFailures = 0
	d.ready = true
	d.persist()
}

// pendingFailure keeps only fixed facts; counts span cycles and restarts.
func pendingFailure(f notificationFailureFacts, now int64, previous PendingFailure, had bool) PendingFailure {
	p := PendingFailure{Stage: f.Stage, ErrorName: f.ErrorName, CauseCodes: f.CauseCodes, Count: 1, FirstFailedAt: now, LastFailedAt: now}
	if f.Request != nil {
		p.Operation, p.Phase, p.HTTPStatus = f.Request.Operation, f.Request.Phase, f.Request.HTTPStatus
	}
	if had {
		p.Count = previous.Count + 1
		p.FirstFailedAt = previous.FirstFailedAt
	}
	return p
}
