package wechat

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	rtdPrivate = "private-token-account-message-url"
	rtdHourMs  = int64(60 * 60 * 1000)
)

var rtdStart = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

// rtdSocketError is a connection reset whose text carries a private value.
func rtdSocketError() error {
	return &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError(rtdPrivate, syscall.ECONNRESET)}
}

func rtdRequest(ctx context.Context, identity string, fails bool) error {
	d := newRequestDiagnostics("result_get", identity)
	if !fails {
		d.success(ctx)
		return nil
	}
	return d.failure(ctx, rtdSocketError(), 1, 1, false, false)
}

// rtdCountingStore counts writes of a real store.
type rtdCountingStore struct {
	*NotificationRecoveryStore
	writes int
}

func (s *rtdCountingStore) Save(e *RecoveryEvidence) error {
	s.writes++
	return s.NotificationRecoveryStore.Save(e)
}

func (s *rtdCountingStore) Clear() error {
	s.writes++
	return s.NotificationRecoveryStore.Clear()
}

type rtdBrokenStore struct{ *NotificationRecoveryStore }

func (rtdBrokenStore) Save(*RecoveryEvidence) error {
	return &os.PathError{Op: "open", Path: rtdPrivate, Err: syscall.EACCES}
}

// rtdCycleEventFields is the field allowlist of each notification cycle event.
var rtdCycleRequestFields = []string{"stage", "operation", "phase", "requestAttempts", "requestMaxAttempts", "elapsedMs",
	"timedOut", "parentAborted", "httpStatus", "errorName", "causeCodes"}

func rtdCheckCycleEvent(t *testing.T, event map[string]any) {
	t.Helper()
	fields := map[string][]string{
		"notification_cycle_failed": append(append([]string(nil), rtdCycleRequestFields...), "errorType", "consecutiveFailures",
			"totalFailures", "lastSucceededAt", "retryScheduled", "retryDelayMs", "failedRequests", "unresolvedFailures", "recoveryEvidenceOverflow"),
		"notification_cycle_recovered":         {"totalFailures", "elapsedMs", "recoveredOperations"},
		"notification_cycle_restored":          {"unresolvedFailures", "totalFailures", "ageMs", "lastFailureAgeMs", "recoveryEvidenceOverflow"},
		"notification_cycle_restore_discarded": {"reason", "ageMs", "unresolvedFailures", "errorName", "causeCodes"},
		"notification_cycle_persist_failed":    {"errorName", "causeCodes"},
		"notification_cycle_ready":             {},
	}
	allowed, ok := fields[event["event"].(string)]
	if !ok {
		t.Fatalf("unknown cycle event %v", event["event"])
	}
	allowed = append(allowed, "event", "service", "timestamp")
	if event["service"] != "gateway" {
		t.Fatalf("service = %v", event["service"])
	}
	if ts, _ := event["timestamp"].(string); ts == "" || isoTime(mustParseISO(t, ts)) != ts {
		t.Fatalf("timestamp = %v", event["timestamp"])
	}
	for k := range event {
		if !containsString(allowed, k) {
			t.Fatalf("unexpected notification cycle field %s in %v", k, event["event"])
		}
	}
	raw, _ := json.Marshal(event)
	if strings.Contains(string(raw), rtdPrivate) {
		t.Fatalf("event exposes a private value: %s", raw)
	}
}

func mustParseISO(t *testing.T, s string) int64 {
	t.Helper()
	tm, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("timestamp %q: %v", s, err)
	}
	return tm.UnixMilli()
}

type rtdObserver struct {
	t      *testing.T
	events []map[string]any
	d      *NotificationCycleDiagnostics
	store  recoveryStore
}

func (o *rtdObserver) run(action func(context.Context) error) error {
	return o.d.Run(context.Background(), action, func() bool { return true }, func(error) bool { return true })
}

func (o *rtdObserver) names() []string {
	var out []string
	for _, e := range o.events {
		out = append(out, e["event"].(string))
	}
	return out
}

type rtdRecoveryFixture struct {
	file string
	now  int64
}

func (f *rtdRecoveryFixture) start(t *testing.T, store recoveryStore) *rtdObserver {
	if store == nil {
		store = &rtdCountingStore{NotificationRecoveryStore: NewNotificationRecoveryStore(f.file)}
	}
	o := &rtdObserver{t: t, store: store}
	o.d = NewNotificationCycleDiagnostics(1000, func(e map[string]any) {
		rtdCheckCycleEvent(t, e)
		o.events = append(o.events, e)
	}, func() int64 { return f.now }, store)
	return o
}

func rtdRecoverySetup(t *testing.T) *rtdRecoveryFixture {
	return &rtdRecoveryFixture{file: filepath.Join(t.TempDir(), "state.json.notification-recovery.json"), now: rtdStart}
}

func rtdFails(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("cycle succeeded, want failure")
	}
}

func rtdEqual(t *testing.T, got, want any, what string) {
	t.Helper()
	if jsonText(got) != jsonText(want) {
		t.Fatalf("%s = %s, want %s", what, jsonText(got), jsonText(want))
	}
}

// from: gateway/tests/runtime/notification-recovery-store.test.ts: unresolved evidence survives a restart and recovers only after matching post-restart success
func TestRtdRecoveryEvidenceSurvivesRestart(t *testing.T) {
	f := rtdRecoverySetup(t)
	before := f.start(t, nil)
	rtdFails(t, before.run(func(ctx context.Context) error { return rtdRequest(ctx, "recipient-a", true) }))
	f.now += rtdHourMs
	rtdFails(t, before.run(func(ctx context.Context) error { return rtdRequest(ctx, "recipient-b", true) }))
	rtdFails(t, before.run(func(ctx context.Context) error { return rtdRequest(ctx, "recipient-a", true) }))
	st, err := os.Stat(f.file)
	mustNoErr(t, err)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", st.Mode().Perm())
	}
	raw, err := os.ReadFile(f.file)
	mustNoErr(t, err)
	if strings.Contains(string(raw), rtdPrivate) || strings.Contains(string(raw), "recipient-") {
		t.Fatalf("state file exposes private values: %s", raw)
	}
	var saved struct {
		Unresolved []struct {
			IdentityHash string   `json:"identityHash"`
			Count        int      `json:"count"`
			CauseCodes   []string `json:"causeCodes"`
		} `json:"unresolved"`
	}
	mustNoErr(t, json.Unmarshal(raw, &saved))
	if len(saved.Unresolved) != 2 || saved.Unresolved[0].Count != 2 || saved.Unresolved[1].Count != 1 {
		t.Fatalf("unresolved = %+v", saved.Unresolved)
	}
	for _, u := range saved.Unresolved {
		if !digestPattern.MatchString(u.IdentityHash) {
			t.Fatalf("identity hash %q", u.IdentityHash)
		}
	}
	rtdEqual(t, saved.Unresolved[0].CauseCodes, []string{"ECONNRESET"}, "cause codes")

	f.now += rtdHourMs
	after := f.start(t, nil)
	mustNoErr(t, after.run(func(context.Context) error { return nil }))
	rtdEqual(t, after.names(), []string{"notification_cycle_restored"}, "events")
	e := after.events[0]
	if e["unresolvedFailures"] != 2 || e["totalFailures"] != int64(3) || e["ageMs"] != 2*rtdHourMs || e["lastFailureAgeMs"] != rtdHourMs {
		t.Fatalf("restored = %v", e)
	}
	mustNoErr(t, after.run(func(ctx context.Context) error { return rtdRequest(ctx, "recipient-a", false) }))
	mustNoErr(t, after.run(func(ctx context.Context) error { return rtdRequest(ctx, "recipient-c", false) }))
	if len(after.events) != 1 {
		t.Fatal("partial or unrelated success is not recovery")
	}
	mustNoErr(t, after.run(func(ctx context.Context) error { return rtdRequest(ctx, "recipient-b", false) }))
	rtdEqual(t, after.names(), []string{"notification_cycle_restored", "notification_cycle_recovered"}, "events")
	if after.events[1]["totalFailures"] != int64(3) {
		t.Fatalf("recovered = %v", after.events[1])
	}
	rtdEqual(t, after.events[1]["recoveredOperations"], []string{"result_get"}, "recovered operations")
	if _, err := os.Stat(f.file); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("recovered evidence is removed")
	}
	if fresh := f.start(t, nil); len(fresh.events) != 0 {
		t.Fatal("fresh start emitted events")
	}
}

// from: gateway/tests/runtime/notification-recovery-store.test.ts: expired evidence is dropped with a log and does not block readiness
func TestRtdRecoveryExpiredEvidenceDropped(t *testing.T) {
	f := rtdRecoverySetup(t)
	before := f.start(t, nil)
	rtdFails(t, before.run(func(ctx context.Context) error { return rtdRequest(ctx, "recipient-a", true) }))
	f.now += NotificationRecoveryExpiryMs + 1
	after := f.start(t, nil)
	mustNoErr(t, after.run(func(context.Context) error { return nil }))
	rtdEqual(t, after.names(), []string{"notification_cycle_restore_discarded", "notification_cycle_ready"}, "events")
	e := after.events[0]
	if e["reason"] != "expired" || e["ageMs"] != int64(NotificationRecoveryExpiryMs+1) || e["unresolvedFailures"] != 1 {
		t.Fatalf("discarded = %v", e)
	}
	if _, err := os.Stat(f.file); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired evidence kept")
	}
}

// from: gateway/tests/runtime/notification-recovery-store.test.ts: corrupt or unsafe state is logged and replaced by fresh diagnostics without failing cycles
func TestRtdRecoveryCorruptStateReplaced(t *testing.T) {
	f := rtdRecoverySetup(t)
	valid := f.start(t, nil)
	rtdFails(t, valid.run(func(ctx context.Context) error { return rtdRequest(ctx, "recipient-a", true) }))
	raw, err := os.ReadFile(f.file)
	mustNoErr(t, err)
	patch := func(change func(map[string]any)) string {
		var m map[string]any
		mustNoErr(t, json.Unmarshal(raw, &m))
		change(m)
		return jsonText(m)
	}
	first := func(m map[string]any) map[string]any { return m["unresolved"].([]any)[0].(map[string]any) }
	corruptions := []string{"{", "[]",
		patch(func(m map[string]any) { m["version"] = 2 }),
		patch(func(m map[string]any) { first(m)["identityHash"] = rtdPrivate }),
		patch(func(m map[string]any) { first(m)["causeCodes"] = []string{rtdPrivate} }),
		patch(func(m map[string]any) { first(m)["operation"] = rtdPrivate }),
		patch(func(m map[string]any) {
			item := first(m)
			list := make([]any, MaxPendingFailures+1)
			for i := range list {
				list[i] = item
			}
			m["unresolved"] = list
		}),
	}
	for _, content := range corruptions {
		mustNoErr(t, os.WriteFile(f.file, []byte(content), 0o600))
		after := f.start(t, nil)
		mustNoErr(t, after.run(func(context.Context) error { return nil }))
		rtdEqual(t, after.names(), []string{"notification_cycle_restore_discarded", "notification_cycle_ready"}, content)
		if after.events[0]["reason"] != "invalid" {
			t.Fatalf("%s: reason = %v", content, after.events[0]["reason"])
		}
		if _, err := os.Stat(f.file); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: corrupt state kept", content)
		}
	}
	mustNoErr(t, os.Mkdir(f.file, 0o700))
	unreadable := f.start(t, nil)
	err = unreadable.run(func(ctx context.Context) error { return rtdRequest(ctx, "recipient-a", true) })
	if errorName(err) != "TypeError" {
		t.Fatalf("error = %v (%s), want the TypeError back", err, errorName(err))
	}
	rtdEqual(t, unreadable.names(), []string{"notification_cycle_restore_discarded", "notification_cycle_persist_failed",
		"notification_cycle_failed"}, "events")
	rtdFails(t, unreadable.run(func(ctx context.Context) error { return rtdRequest(ctx, "recipient-a", true) }))
	n := 0
	for _, name := range unreadable.names() {
		if name == "notification_cycle_persist_failed" {
			n++
		}
	}
	if n != 1 {
		t.Fatal("persistence failure is logged on transition only")
	}
}

// from: gateway/tests/runtime/notification-recovery-store.test.ts: overflow is bounded, persisted and keeps recovery unproven after restart
func TestRtdRecoveryOverflowBounded(t *testing.T) {
	f := rtdRecoverySetup(t)
	before := f.start(t, nil)
	rtdFails(t, before.run(func(ctx context.Context) error {
		var errs []error
		for i := 0; i <= MaxPendingFailures; i++ {
			errs = append(errs, newRequestDiagnostics("result_get", "recipient-"+itoa(i)).failure(ctx, rtdSocketError(), 1, 1, false, false))
		}
		return errs[0]
	}))
	raw, err := os.ReadFile(f.file)
	mustNoErr(t, err)
	var saved struct {
		Unresolved []any `json:"unresolved"`
		Overflow   bool  `json:"recoveryEvidenceOverflow"`
	}
	mustNoErr(t, json.Unmarshal(raw, &saved))
	if len(saved.Unresolved) != MaxPendingFailures || !saved.Overflow {
		t.Fatalf("saved %d unresolved, overflow %v", len(saved.Unresolved), saved.Overflow)
	}
	after := f.start(t, nil)
	mustNoErr(t, after.run(func(ctx context.Context) error {
		for i := 0; i <= MaxPendingFailures; i++ {
			mustNoErr(t, rtdRequest(ctx, "recipient-"+itoa(i), false))
		}
		return nil
	}))
	mustNoErr(t, after.run(func(context.Context) error { return nil }))
	rtdEqual(t, after.names(), []string{"notification_cycle_restored"}, "events")
	if after.events[0]["recoveryEvidenceOverflow"] != true || after.events[0]["unresolvedFailures"] != MaxPendingFailures {
		t.Fatalf("restored = %v", after.events[0])
	}
}

func itoa(i int) string { return jsonText(i) }

// from: gateway/tests/runtime/notification-recovery-store.test.ts: steady successful cycles never write; only evidence changes do
func TestRtdRecoverySteadyCyclesNeverWrite(t *testing.T) {
	f := rtdRecoverySetup(t)
	store := &rtdCountingStore{NotificationRecoveryStore: NewNotificationRecoveryStore(f.file)}
	steady := f.start(t, store)
	ok := func(id string) {
		mustNoErr(t, steady.run(func(ctx context.Context) error { return rtdRequest(ctx, id, false) }))
	}
	for i := 0; i < 5; i++ {
		ok("recipient-a")
	}
	if store.writes != 0 {
		t.Fatalf("writes = %d", store.writes)
	}
	rtdFails(t, steady.run(func(ctx context.Context) error { return rtdRequest(ctx, "recipient-a", true) }))
	if store.writes != 1 {
		t.Fatalf("writes = %d", store.writes)
	}
	for i := 0; i < 5; i++ {
		ok("recipient-b")
	}
	if store.writes != 1 {
		t.Fatal("unrelated success leaves unresolved evidence unchanged")
	}
	ok("recipient-a")
	if store.writes != 2 {
		t.Fatal("recovery clears the file once")
	}
	for i := 0; i < 5; i++ {
		ok("recipient-a")
	}
	if store.writes != 2 {
		t.Fatalf("writes = %d", store.writes)
	}
}

// from: gateway/tests/runtime/notification-recovery-store.test.ts: a failing store never changes cycle results
func TestRtdRecoveryFailingStore(t *testing.T) {
	f := rtdRecoverySetup(t)
	broken := f.start(t, rtdBrokenStore{NewNotificationRecoveryStore(f.file)})
	original := rtdSocketError()
	if err := broken.run(func(context.Context) error { return original }); err != original {
		t.Fatalf("error = %v, want the original", err)
	}
	mustNoErr(t, broken.run(func(context.Context) error { return nil }))
	rtdEqual(t, broken.names(), []string{"notification_cycle_failed", "notification_cycle_persist_failed"}, "events")
	rtdEqual(t, broken.events[1]["causeCodes"], []string{"OtherCode"}, "cause codes")
}

// from: gateway/tests/runtime/transfer-budget.test.ts: local transfer allowance scales with trusted byte size and remains bounded
func TestRtdTransferBudget(t *testing.T) {
	const mib = 1024 * 1024
	for _, c := range []struct{ size, setup, want int64 }{
		{32 * mib, 15_000, 47_000}, {50 * mib, 30_000, 80_000}, {512 * mib, 30_000, 542_000}, {maxSafeInteger, 30_000, MaxTransferTimeoutMs},
	} {
		got, err := transferTimeoutMs(c.size, c.setup)
		mustNoErr(t, err)
		if got != c.want {
			t.Fatalf("transferTimeoutMs(%d, %d) = %d, want %d", c.size, c.setup, got, c.want)
		}
	}
	// 1.5 and Infinity cannot be an int64 byte count; zero, negative and beyond-safe sizes stay invalid.
	for _, size := range []int64{0, -1, maxSafeInteger + 1} {
		if _, err := transferTimeoutMs(size, 15_000); err == nil || !strings.Contains(err.Error(), "Invalid transfer budget") {
			t.Fatalf("size %d: err = %v", size, err)
		}
	}
}
