package wechat

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// from: gateway/tests/runtime/delivery-state.test.ts: clock rollback and later replay never refill or extend a window granted from a skewed timestamp
func TestRtcDeliveryClockRollback(t *testing.T) {
	now := int64(1_800_000_000_000)
	clock := setNow(t, now)
	f := rtcNewState(t, PolicyOptions{})
	server := now + MaxInboundClockSkewMs
	_, err := f.state.RecordInboundContext("skewed", rtcChat, i64(server), true, "synthetic-context")
	mustNoErr(t, err)
	mustNoErr(t, f.state.ConsumeSendBudget(rtcChat, SendFinal))
	original := f.state.ContextWindows[rtcChat]
	clock.Store(now - 1)
	r := f.reload()
	if rtcRecord(t, r, "skewed", rtcChat, i64(server)) || r.ContextWindows[rtcChat] != original || r.CanSendToChat(rtcChat, SendFinal) {
		t.Fatal("rollback replay changed the window")
	}
	clock.Store(server)
	if rtcRecord(t, r, "skewed", rtcChat, i64(server)) || r.ContextWindows[rtcChat] != original {
		t.Fatal("later replay changed the window")
	}
	clock.Store(now + f.state.Policy.ContextWindowMs - 1)
	if !r.CanSendToChat(rtcChat, SendFinal) {
		t.Fatal("window ended early")
	}
	clock.Store(now + f.state.Policy.ContextWindowMs)
	if r.CanSendToChat(rtcChat, SendFinal) {
		t.Fatal("window extended")
	}
	if f.reload().ContextWindows[rtcChat].RefreshedAt != now {
		t.Fatal("refreshedAt changed")
	}
}

// from: gateway/tests/runtime/delivery-state.test.ts: baseline inbound checkpoints tokens and watermarks without granting quota or accepting stale tokens
func TestRtcDeliveryBaselineInbound(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{})
	if ok, err := f.state.RecordInboundContext("baseline", rtcChat, i64(f.createdAt), false, "synthetic-baseline-token"); err != nil || !ok {
		t.Fatalf("baseline refused: %v", err)
	}
	r := f.reload()
	if _, ok := r.ContextWindows[rtcChat]; ok || r.ContextTokens[rtcChat] != "synthetic-baseline-token" {
		t.Fatalf("baseline window %v token %s", ok, r.ContextTokens[rtcChat])
	}
	rtcSame(t, "watermark", r.InboundWatermarks[rtcChat], inboundWatermark{CreatedAtMs: f.createdAt, Keys: []string{"baseline"}})
	if ok, _ := r.RecordInboundContext("baseline", rtcChat, i64(f.createdAt), true, "synthetic-replayed-token"); ok || r.CanSendToChat(rtcChat, SendFinal) {
		t.Fatal("baseline replay granted a window")
	}
	if ok, _ := r.RecordInboundContext("active", rtcChat, i64(f.createdAt+1), true, "synthetic-active-token"); !ok {
		t.Fatal("active inbound refused")
	}
	mustNoErr(t, r.ConsumeSendBudget(rtcChat, SendFinal))
	if ok, _ := r.RecordInboundContext("older", rtcChat, i64(f.createdAt), true, "synthetic-stale-token"); ok {
		t.Fatal("older inbound accepted")
	}
	p := f.reload()
	if p.ContextTokens[rtcChat] != "synthetic-active-token" || p.AccountBudget.Remaining != f.state.Policy.SendBudget-1 {
		t.Fatalf("token %s budget %+v", p.ContextTokens[rtcChat], p.AccountBudget)
	}
}

// from: gateway/tests/runtime/delivery-state.test.ts: watermark-only inbound preserves an existing budget and token across restart
func TestRtcDeliveryWatermarkOnlyInbound(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{})
	_, err := f.state.RecordInboundContext("active", rtcChat, i64(f.createdAt), true, "synthetic-active-token")
	mustNoErr(t, err)
	mustNoErr(t, f.state.ConsumeSendBudget(rtcChat, SendProgress))
	previous := f.state.ContextWindows[rtcChat]
	if ok, _ := f.state.RecordInboundContext("no-context-token", rtcChat, i64(f.createdAt+1), false, ""); !ok {
		t.Fatal("watermark-only inbound refused")
	}
	r := f.reload()
	if r.ContextWindows[rtcChat] != previous || r.ContextTokens[rtcChat] != "synthetic-active-token" {
		t.Fatal("watermark-only inbound changed the window or token")
	}
	rtcSame(t, "watermark", r.InboundWatermarks[rtcChat], inboundWatermark{CreatedAtMs: f.createdAt + 1, Keys: []string{"no-context-token"}})
	if ok, _ := r.RecordInboundContext("no-context-token", rtcChat, i64(f.createdAt+1), true, "synthetic-replayed-token"); ok {
		t.Fatal("replay accepted")
	}
	if p := f.reload(); p.ContextWindows[rtcChat] != previous || p.ContextTokens[rtcChat] != "synthetic-active-token" {
		t.Fatal("replay changed persisted state")
	}
}

// from: gateway/tests/runtime/delivery-state.test.ts: a saturated timestamp key set withholds refresh without blocking messages and advances on newer time
func TestRtcDeliverySaturatedWatermark(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{})
	s, full := f.state, f.state.Policy.SendBudget
	for i := 0; i < maxSameTimestampKeys; i++ {
		rtcRecord(t, s, "same-time-"+strings.Repeat("x", i%3)+string(rune('a'+i%26))+itoaRtc(i), rtcChat, i64(f.createdAt))
	}
	if n := len(s.InboundWatermarks[rtcChat].Keys); n != maxSameTimestampKeys {
		t.Fatalf("%d keys", n)
	}
	mustNoErr(t, s.ConsumeSendBudget(rtcChat, SendFinal))
	rtcRecord(t, s, "overflow", rtcChat, i64(f.createdAt))
	if s.AccountBudget.Remaining != full-1 || len(s.InboundWatermarks[rtcChat].Keys) != maxSameTimestampKeys {
		t.Fatal("saturated key set refreshed")
	}
	mustNoErr(t, s.RecordProcessedMessageKey("overflow", false))
	if !s.IsMessageProcessed("overflow") {
		t.Fatal("overflow blocked")
	}
	r := f.reload()
	rtcRecord(t, r, "overflow-after-restart", rtcChat, i64(f.createdAt))
	if r.AccountBudget.Remaining != full-1 {
		t.Fatal("restart refreshed a saturated set")
	}
	rtcRecord(t, r, "newer", rtcChat, i64(f.createdAt+1))
	if r.AccountBudget.Remaining != full {
		t.Fatal("newer time did not refresh")
	}
	rtcSame(t, "watermark", f.reload().InboundWatermarks[rtcChat], inboundWatermark{CreatedAtMs: f.createdAt + 1, Keys: []string{"newer"}})
}

func itoaRtc(i int) string { return jsonText(i) }

// from: gateway/tests/runtime/delivery-state.test.ts: outbox capacity retains existing references and permits idempotent retry at capacity
func TestRtcDeliveryOutboxCapacity(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{OutboxLimit: 1})
	s := f.state
	mustNoErr(t, s.CommitCursor("retained-cursor"))
	mustNoErr(t, s.PutOutbox(rtcReference("reply-reference", nil)))
	mustNoErr(t, s.PutOutbox(rtcReference("reply-reference", nil)))
	mustNoErr(t, s.AssertOutboxCapacity("reply-reference"))
	if err := s.PutOutbox(rtcReference("second-reference", nil)); !errors.Is(err, OutboxBackpressureError) {
		t.Fatalf("second put = %v", err)
	}
	if err := s.AssertOutboxCapacity(""); !errors.Is(err, OutboxBackpressureError) {
		t.Fatalf("capacity = %v", err)
	}
	r := f.reload()
	if r.SyncBuf != "retained-cursor" || !reflect.DeepEqual(rtcOutboxKeys(r), []string{"reply-reference"}) {
		t.Fatalf("reloaded %s %v", r.SyncBuf, rtcOutboxKeys(r))
	}
	if err := s.PutOutbox(rtcReference("reply-reference", func(i *DeliveryOutboxItem) { i.MessageID = "changed-message" })); err == nil ||
		!strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("changed identity = %v", err)
	}
	mustNoErr(t, s.RemoveOutbox("reply-reference"))
	mustNoErr(t, s.PutOutbox(rtcReference("second-reference", nil)))
	if got := rtcOutboxKeys(f.reload()); !reflect.DeepEqual(got, []string{"second-reference"}) {
		t.Fatalf("keys = %v", got)
	}
}

func rtcOutboxKeys(s *StateManager) []string {
	var keys []string
	for k := range s.Outbox {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// from: gateway/tests/runtime/delivery-state.test.ts: outbox insertion, update and removal roll back if a durable checkpoint fails
func TestRtcDeliveryOutboxRollback(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{})
	s := f.state
	mustNoErr(t, s.SaveState())
	failure := errors.New("synthetic outbox checkpoint failure")
	rtcFail(s, failure)
	if err := s.PutOutbox(rtcReference("reply-reference", nil)); err != failure || len(s.Outbox) != 0 || len(f.reload().Outbox) != 0 {
		t.Fatalf("failed put: %v", err)
	}
	s.failSave = nil
	mustNoErr(t, s.PutOutbox(rtcReference("reply-reference", nil)))
	expected := rtcDeepCopy(t, s.Outbox)
	rtcFail(s, failure)
	if err := s.UpdateOutbox("reply-reference", func(i *DeliveryOutboxItem) { i.Status, i.AttemptID = OutboxClaimed, "attempt" }); err != failure {
		t.Fatalf("update = %v", err)
	}
	rtcSame(t, "outbox", s.Outbox, expected)
	rtcSame(t, "reloaded outbox", f.reload().Outbox, expected)
	if err := s.RemoveOutbox("reply-reference"); err != failure {
		t.Fatalf("remove = %v", err)
	}
	rtcSame(t, "outbox", s.Outbox, expected)
	rtcSame(t, "reloaded outbox", f.reload().Outbox, expected)
	s.failSave = nil
	mustNoErr(t, s.UpdateOutbox("reply-reference", func(i *DeliveryOutboxItem) { i.Status, i.DeferReason = OutboxDefer, DeferBudgetExhausted }))
	if f.reload().Outbox["reply-reference"].Status != OutboxDefer {
		t.Fatal("update not saved")
	}
	mustNoErr(t, s.RemoveOutbox("reply-reference"))
	if len(f.reload().Outbox) != 0 {
		t.Fatal("remove not saved")
	}
}

// from: gateway/tests/runtime/delivery-state.test.ts: the durable outbox stores delivery references and receipts without message bodies
func TestRtcDeliveryOutboxWithoutBodies(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{})
	mustNoErr(t, f.state.PutOutbox(rtcReference("notification-reference", func(i *DeliveryOutboxItem) {
		i.Kind, i.NotificationKind, i.ExpiresAt = "notification", "completed", nowMs()+60_000
	})))
	mustNoErr(t, f.state.UpdateOutbox("notification-reference", func(i *DeliveryOutboxItem) {
		i.Status, i.AttemptID, i.ReceiptStatus = OutboxReceipt, "attempt", "accepted"
	}))
	raw, err := os.ReadFile(f.statePath)
	mustNoErr(t, err)
	var disk struct {
		DeliveryOutbox map[string]map[string]any `json:"delivery_outbox"`
	}
	mustNoErr(t, json.Unmarshal(raw, &disk))
	stored := disk.DeliveryOutbox["notification-reference"]
	reloaded := rtcDeepCopy(t, map[string]any{"x": f.reload().Outbox["notification-reference"]})["x"]
	if !reflect.DeepEqual(stored, reloaded) {
		t.Fatalf("stored = %v, reloaded = %v", stored, reloaded)
	}
	var keys []string
	for k := range stored {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"attemptId", "chatId", "deliveryKey", "expiresAt", "kind", "messageId", "notificationKind", "receiptStatus", "source", "splitMaxChars", "status"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("stored keys = %v", keys)
	}
}

// from: gateway/tests/runtime/delivery-state.test.ts: legacy multipart flags are retired without changing durable delivery references
func TestRtcDeliveryLegacyMultipartFlag(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{})
	for _, flag := range []string{"false", "true"} {
		mustNoErr(t, f.state.PutOutbox(rtcReference("legacy-"+flag, nil)))
	}
	expected := rtcDeepCopy(t, f.state.Outbox)
	rtcEditDisk(t, f.statePath, func(d map[string]any) {
		for _, flag := range []bool{false, true} {
			d["delivery_outbox"].(map[string]any)["legacy-"+jsonText(flag)].(map[string]any)["splitMultiPart"] = flag
		}
	})
	r := f.reload()
	rtcSame(t, "outbox", r.Outbox, expected)
	mustNoErr(t, r.SaveState())
	raw, _ := os.ReadFile(f.statePath)
	var disk struct {
		DeliveryOutbox map[string]DeliveryOutboxItem `json:"delivery_outbox"`
	}
	mustNoErr(t, json.Unmarshal(raw, &disk))
	rtcSame(t, "persisted", disk.DeliveryOutbox, expected)
	if strings.Contains(string(raw), "splitMultiPart") {
		t.Fatal("retired flag written back")
	}
}

// from: gateway/tests/runtime/delivery-state.test.ts: outbox source survives restart and legacy references default to controller
func TestRtcDeliveryOutboxSource(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{})
	for _, source := range []string{SourceController, SourceEngine, SourceConflict} {
		mustNoErr(t, f.state.PutOutbox(rtcReference("reply-"+source, func(i *DeliveryOutboxItem) { i.Source = source })))
		if got := f.reload().Outbox["reply-"+source].Source; got != source {
			t.Fatalf("source %s became %s", source, got)
		}
	}
	mustNoErr(t, f.state.PutOutbox(rtcReference("legacy-reference", nil)))
	if f.state.Outbox["legacy-reference"].Source != SourceController {
		t.Fatal("absent source is not controller")
	}
	rtcEditDisk(t, f.statePath, func(d map[string]any) {
		delete(d["delivery_outbox"].(map[string]any)["legacy-reference"].(map[string]any), "source")
	})
	r := f.reload()
	if r.Outbox["legacy-reference"].Source != SourceController {
		t.Fatal("legacy reference has no source after restart")
	}
	mustNoErr(t, r.UpdateOutbox("legacy-reference", func(i *DeliveryOutboxItem) { i.Status, i.Source = OutboxDefer, "" }))
	if f.reload().Outbox["legacy-reference"].Source != SourceController {
		t.Fatal("clearing the source changed it")
	}
}

// from: gateway/tests/runtime/delivery-state.test.ts: outbox rejects unknown source values before storing or restoring them
func TestRtcDeliveryOutboxUnknownSource(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{})
	mustNoErr(t, f.state.PutOutbox(rtcReference("reply-reference", nil)))
	expected := rtcDeepCopy(t, f.state.Outbox)
	// null and "" are how Go spells an absent source (controller); the other
	// non-strings cannot be held and only reach Go through the state file.
	for _, source := range []string{"fallback", "Controller"} {
		if err := f.state.PutOutbox(rtcReference("invalid", func(i *DeliveryOutboxItem) { i.Source = source })); err == nil ||
			!strings.Contains(err.Error(), "Invalid delivery outbox source") {
			t.Fatalf("put %s = %v", source, err)
		}
		if err := f.state.UpdateOutbox("reply-reference", func(i *DeliveryOutboxItem) { i.Source = source }); err == nil ||
			!strings.Contains(err.Error(), "Invalid delivery outbox source") {
			t.Fatalf("update %s = %v", source, err)
		}
		rtcSame(t, "outbox", f.state.Outbox, expected)
	}
	for _, source := range []any{"fallback", "Controller", 1, map[string]any{}, []any{}} {
		rtcEditDisk(t, f.statePath, func(d map[string]any) {
			d["delivery_outbox"].(map[string]any)["reply-reference"].(map[string]any)["source"] = source
		})
		if _, err := f.load(f.policy); err == nil || !strings.Contains(err.Error(), "Failed to load gateway state") {
			t.Fatalf("restore %v = %v", source, err)
		}
	}
}

// from: gateway/tests/runtime/delivery-state.test.ts: the same outbox identity cannot switch reply stores through put or update
func TestRtcDeliveryOutboxSourceIdentity(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{})
	for _, original := range []string{SourceController, SourceEngine, SourceConflict} {
		key := "reply-" + original
		mustNoErr(t, f.state.PutOutbox(rtcReference(key, func(i *DeliveryOutboxItem) { i.Source = original })))
		for _, replacement := range []string{"", SourceController, SourceEngine, SourceConflict} {
			if effective := map[bool]string{true: SourceController, false: replacement}[replacement == ""]; effective == original {
				continue
			}
			if err := f.state.PutOutbox(rtcReference(key, func(i *DeliveryOutboxItem) { i.Source = replacement })); err == nil ||
				!strings.Contains(err.Error(), "identity changed") {
				t.Fatalf("put %s over %s = %v", replacement, original, err)
			}
			if err := f.state.UpdateOutbox(key, func(i *DeliveryOutboxItem) { i.Source = replacement }); err == nil ||
				!strings.Contains(err.Error(), "identity changed") {
				t.Fatalf("update %s over %s = %v", replacement, original, err)
			}
			if f.state.Outbox[key].Source != original || f.reload().Outbox[key].Source != original {
				t.Fatal("source switched")
			}
		}
		mustNoErr(t, f.state.PutOutbox(rtcReference(key, func(i *DeliveryOutboxItem) { i.Source = original })))
		mustNoErr(t, f.state.UpdateOutbox(key, setStatus(OutboxDefer)))
		if f.reload().Outbox[key].Source != original {
			t.Fatal("update changed the source")
		}
	}
}

// from: gateway/tests/runtime/delivery-state.test.ts: engine acknowledgement marker persists and legacy references remain unacknowledged
func TestRtcDeliveryEngineAckPersists(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{})
	mustNoErr(t, f.state.PutOutbox(rtcReference("legacy", nil)))
	if f.reload().Outbox["legacy"].EngineAcked {
		t.Fatal("legacy reference acknowledged")
	}
	mustNoErr(t, f.state.PutOutbox(rtcReference("engine", func(i *DeliveryOutboxItem) { i.Source, i.Status = SourceEngine, OutboxExpired })))
	mustNoErr(t, f.state.UpdateOutbox("engine", func(i *DeliveryOutboxItem) { i.EngineAcked = true }))
	if !f.reload().Outbox["engine"].EngineAcked {
		t.Fatal("marker not saved")
	}
	// An explicit false and an absent marker are the same unacknowledged value in Go.
	mustNoErr(t, f.state.PutOutbox(rtcReference("false-controller", func(i *DeliveryOutboxItem) { i.EngineAcked = false })))
	if f.reload().Outbox["false-controller"].EngineAcked {
		t.Fatal("false marker became true")
	}
}

// from: gateway/tests/runtime/delivery-state.test.ts: engine acknowledgement rejects malformed markers and acknowledgement of non-engine sources
func TestRtcDeliveryEngineAckValidation(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{})
	mustNoErr(t, f.state.PutOutbox(rtcReference("engine", func(i *DeliveryOutboxItem) { i.Source = SourceEngine })))
	mustNoErr(t, f.state.PutOutbox(rtcReference("controller", nil)))
	mustNoErr(t, f.state.PutOutbox(rtcReference("conflict", func(i *DeliveryOutboxItem) { i.Source = SourceConflict })))
	expected := rtcDeepCopy(t, f.state.Outbox)
	for _, source := range []string{"", SourceController, SourceConflict} {
		if err := f.state.PutOutbox(rtcReference("invalid", func(i *DeliveryOutboxItem) { i.Source, i.EngineAcked = source, true })); err == nil ||
			!strings.Contains(err.Error(), "Invalid delivery outbox engine acknowledgement") {
			t.Fatalf("put %q = %v", source, err)
		}
	}
	for _, key := range []string{"controller", "conflict"} {
		if err := f.state.UpdateOutbox(key, func(i *DeliveryOutboxItem) { i.EngineAcked = true }); err == nil ||
			!strings.Contains(err.Error(), "Invalid delivery outbox engine acknowledgement") {
			t.Fatalf("update %s = %v", key, err)
		}
	}
	rtcSame(t, "outbox", f.state.Outbox, expected)
	raw, _ := os.ReadFile(f.statePath)
	// Malformed markers ("true", 1, {}) fail decoding; null reads as unacknowledged in Go.
	for _, c := range []struct {
		key    string
		marker any
	}{{"engine", "true"}, {"engine", 1}, {"controller", true}, {"conflict", true}} {
		mustNoErr(t, os.WriteFile(f.statePath, raw, 0o600))
		rtcEditDisk(t, f.statePath, func(d map[string]any) {
			d["delivery_outbox"].(map[string]any)[c.key].(map[string]any)["engineAcked"] = c.marker
		})
		if _, err := f.load(f.policy); err == nil || !strings.Contains(err.Error(), "Failed to load gateway state") {
			t.Fatalf("restore %s=%v: %v", c.key, c.marker, err)
		}
	}
}

// from: gateway/tests/runtime/delivery-state.test.ts: a failed engine acknowledgement checkpoint rolls back the in-memory marker
func TestRtcDeliveryEngineAckRollback(t *testing.T) {
	f := rtcNewState(t, PolicyOptions{})
	mustNoErr(t, f.state.PutOutbox(rtcReference("engine", func(i *DeliveryOutboxItem) { i.Source, i.Status = SourceEngine, OutboxUncertain })))
	failure := errors.New("synthetic acknowledgement checkpoint failure")
	rtcFail(f.state, failure)
	if err := f.state.UpdateOutbox("engine", func(i *DeliveryOutboxItem) { i.EngineAcked = true }); err != failure {
		t.Fatalf("error = %v", err)
	}
	if f.state.Outbox["engine"].EngineAcked || f.reload().Outbox["engine"].EngineAcked {
		t.Fatal("marker kept after a failed checkpoint")
	}
}
