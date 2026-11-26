package wechat

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rtdState(t *testing.T, dir string, account *Account, policy PolicyOptions) *StateManager {
	t.Helper()
	s, err := NewStateManager(account, filepath.Join(dir, stateFileName), policy)
	mustNoErr(t, err)
	return s
}

func rtdAccount(token, id string) *Account { return &Account{Token: token, AccountID: id} }

var rtdFailure = errors.New("injected checkpoint failure")

func rtdFailSaves(s *StateManager) func() {
	s.failSave = func() error { return rtdFailure }
	return func() { s.failSave = nil }
}

func rtdWantErr(t *testing.T, err error, part string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), part) {
		t.Fatalf("error = %v, want %q", err, part)
	}
}

// from: gateway/tests/runtime/state.test.ts: failed processed checkpoint preserves reply progress in memory and on disk
func TestRtdStateFailedProcessedCheckpoint(t *testing.T) {
	dir, account := t.TempDir(), rtdAccount("fake-token", "test-bot")
	s := rtdState(t, dir, account, PolicyOptions{})
	key := s.ComputeDeliveryKey("test-chat", "test-message")
	s.SentReplyProgress[key] = 2
	mustNoErr(t, s.save(func() { delete(s.SentReplyProgress, key) }))
	restore := rtdFailSaves(s)
	if err := s.RecordProcessedMessageKey(key, false); !errors.Is(err, rtdFailure) {
		t.Fatalf("error = %v", err)
	}
	if s.IsMessageProcessed(key) || s.ReplySentChunks(key) != 2 {
		t.Fatal("rollback lost the in-memory state")
	}
	if r := rtdState(t, dir, account, PolicyOptions{}); r.IsMessageProcessed(key) || r.ReplySentChunks(key) != 2 {
		t.Fatal("disk changed despite the failed checkpoint")
	}
	restore()
	mustNoErr(t, s.RecordProcessedMessageKey(key, false))
	if !s.IsMessageProcessed(key) || s.ReplySentChunks(key) != 0 {
		t.Fatal("processed key not recorded")
	}
	if c := rtdState(t, dir, account, PolicyOptions{}); !c.IsMessageProcessed(key) || c.ReplySentChunks(key) != 0 {
		t.Fatal("processed key not on disk")
	}
}

// from: gateway/tests/runtime/state.test.ts: StateManager loads account readonly and writes atomic state with 0600 permissions
func TestRtdStateAccountAndAtomicState(t *testing.T) {
	dir := t.TempDir()
	account := &Account{Token: "tok123", AccountID: "bot1", BaseURL: "https://custom.example"}
	s := rtdState(t, dir, account, PolicyOptions{})
	if s.Account.Token != "tok123" || s.Account.AccountID != "bot1" || s.Account.BaseURL != "https://custom.example" || s.SyncBuf != "" {
		t.Fatalf("account = %+v, cursor %q", s.Account, s.SyncBuf)
	}
	mustNoErr(t, s.CommitCursor("buf-1"))
	st, err := os.Stat(s.StatePath)
	mustNoErr(t, err)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", st.Mode().Perm())
	}
	var raw map[string]any
	data, err := os.ReadFile(s.StatePath)
	mustNoErr(t, err)
	mustNoErr(t, json.Unmarshal(data, &raw))
	if raw["sync_buf"] != "buf-1" {
		t.Fatalf("sync_buf = %v", raw["sync_buf"])
	}
	key := s.ComputeDeliveryKey("user1", "msg1")
	if key != `["bot1","user1","msg1"]` || s.IsMessageProcessed(key) {
		t.Fatalf("key = %s", key)
	}
	mustNoErr(t, s.RecordProcessedMessageKey(key, false))
	if s2 := rtdState(t, dir, account, PolicyOptions{}); s2.SyncBuf != "buf-1" || !s2.IsMessageProcessed(key) {
		t.Fatal("reload lost the state")
	}
	// A new login token invalidates the old cursor and processed keys.
	s3 := rtdState(t, dir, rtdAccount("new-tok", "bot1"), PolicyOptions{})
	if s3.SyncBuf != "" || s3.IsMessageProcessed(key) {
		t.Fatal("token change kept the old state")
	}
}

// from: gateway/tests/runtime/state.test.ts: StateManager fails closed on corrupted credentials or corrupted state file without leaking secrets
func TestRtdStateFailsClosed(t *testing.T) {
	// The login comes from connector.secrets instead of a credentials file.
	if _, err := decodeSecrets(base64.StdEncoding.EncodeToString([]byte("{ invalid json with secret_token_xyz"))); err == nil ||
		strings.Contains(err.Error(), "secret_token_xyz") {
		t.Fatalf("corrupt secrets: %v", err)
	}
	if _, err := decodeSecrets(base64.StdEncoding.EncodeToString([]byte(`{"bot_id":"bot1","user_id":"u"}`))); err == nil {
		t.Fatal("secrets without token accepted")
	}
	dir := t.TempDir()
	sensitive := "super-secret-wechat-bot-token-99999"
	mustNoErr(t, os.WriteFile(filepath.Join(dir, stateFileName), []byte("{ broken state json with sensitive_context_token_8888"), 0o600))
	_, err := NewStateManager(rtdAccount(sensitive, "bot1"), filepath.Join(dir, stateFileName), PolicyOptions{})
	rtdWantErr(t, err, "Failed to load gateway state at ")
	rtdWantErr(t, err, "InvalidJsonSyntax")
	if strings.Contains(err.Error(), sensitive) || strings.Contains(err.Error(), "sensitive_context_token_8888") {
		t.Fatalf("error leaks secrets: %v", err)
	}
}

// from: gateway/tests/runtime/state.test.ts: reply intent and acknowledgment roll back safely when durable checkpoints fail
func TestRtdStateReplyIntentRollback(t *testing.T) {
	dir := t.TempDir()
	account := rtdAccount("synthetic", "fixture")
	s := rtdState(t, dir, account, PolicyOptions{})
	digest := strings.Repeat("a", 64)
	_, err := s.PrepareReplyDelivery("key", digest, 2)
	mustNoErr(t, err)
	restore := rtdFailSaves(s)
	if err := s.BeginReplyChunk("key", 0); !errors.Is(err, rtdFailure) || s.ReplyDeliveries["key"].InFlight {
		t.Fatalf("begin rollback: %v", err)
	}
	restore()
	mustNoErr(t, s.BeginReplyChunk("key", 0))
	restore = rtdFailSaves(s)
	if err := s.ConfirmReplyChunk("key", 0); !errors.Is(err, rtdFailure) || !s.ReplyDeliveries["key"].InFlight || s.ReplySentChunks("key") != 0 {
		t.Fatalf("confirm rollback: %v", err)
	}
	restore()
	disk := rtdState(t, dir, account, PolicyOptions{})
	if !disk.ReplyDeliveries["key"].InFlight {
		t.Fatal("intent not on disk")
	}
	_, err = disk.PrepareReplyDelivery("key", digest, 2)
	rtdWantErr(t, err, "uncertain")
}

// from: gateway/tests/runtime/state.test.ts: renewal delivery can replace only an unsent legacy checkpoint
func TestRtdStateRenewalReplacesUnsentOnly(t *testing.T) {
	dir := t.TempDir()
	account := rtdAccount("synthetic", "fixture")
	s := rtdState(t, dir, account, PolicyOptions{})
	key, oldDigest, newDigest := "renewal:legacy", strings.Repeat("a", 64), strings.Repeat("b", 64)
	_, err := s.PrepareReplyDelivery(key, oldDigest, 1)
	mustNoErr(t, err)
	replaced, err := s.PrepareRenewalReplyDelivery(key, newDigest, 1)
	mustNoErr(t, err)
	if replaced.Digest != newDigest || replaced.AcknowledgedChunks != 0 {
		t.Fatalf("replaced = %+v", replaced)
	}
	if rtdState(t, dir, account, PolicyOptions{}).ReplyDeliveries[key].Digest != newDigest {
		t.Fatal("replacement not on disk")
	}
	mustNoErr(t, s.BeginReplyChunk(key, 0))
	mustNoErr(t, s.ConfirmReplyChunk(key, 0))
	_, err = s.PrepareRenewalReplyDelivery(key, oldDigest, 1)
	rtdWantErr(t, err, "content or chunk plan changed")
	if d := s.ReplyDeliveries[key]; d.Digest != newDigest || d.AcknowledgedChunks != 1 {
		t.Fatalf("sent reminder changed: %+v", d)
	}
	uncertain := "renewal:uncertain"
	_, err = s.PrepareReplyDelivery(uncertain, oldDigest, 1)
	mustNoErr(t, err)
	mustNoErr(t, s.BeginReplyChunk(uncertain, 0))
	_, err = s.PrepareRenewalReplyDelivery(uncertain, oldDigest, 1)
	rtdWantErr(t, err, "uncertain")
	if d := s.ReplyDeliveries[uncertain]; d.Digest != oldDigest || !d.InFlight {
		t.Fatalf("uncertain reminder changed: %+v", d)
	}
	_, err = s.PrepareRenewalReplyDelivery("ordinary", newDigest, 1)
	rtdWantErr(t, err, "Invalid renewal delivery key")
}

// from: gateway/tests/runtime/state.test.ts: login replacement clears durable reply delivery intents
func TestRtdStateLoginReplacementClears(t *testing.T) {
	dir := t.TempDir()
	s := rtdState(t, dir, rtdAccount("synthetic-one", "fixture"), PolicyOptions{})
	_, err := s.PrepareReplyDelivery("key", strings.Repeat("b", 64), 1)
	mustNoErr(t, err)
	mustNoErr(t, s.BeginReplyChunk("key", 0))
	changed := rtdState(t, dir, rtdAccount("synthetic-two", "fixture"), PolicyOptions{})
	if len(changed.ReplyDeliveries) != 0 || len(changed.SentReplyProgress) != 0 {
		t.Fatal("login change kept reply intents")
	}
}

// from: gateway/tests/runtime/state.test.ts: reply state capacity only evicts completed deliveries and keeps uncertain intents
func TestRtdStateCapacityEvictsCompletedOnly(t *testing.T) {
	s := rtdState(t, t.TempDir(), rtdAccount("synthetic", "fixture"), PolicyOptions{})
	for i := 0; i < MaxReplyDeliveries; i++ {
		s.ReplyDeliveries["pending-"+itoa(i)] = ReplyDeliveryState{Digest: strings.Repeat("c", 64), TotalChunks: 1, InFlight: true}
	}
	_, err := s.PrepareReplyDelivery("new", strings.Repeat("d", 64), 1)
	rtdWantErr(t, err, "capacity exhausted")
	s.ReplyDeliveries["pending-0"] = ReplyDeliveryState{Digest: strings.Repeat("c", 64), TotalChunks: 1, AcknowledgedChunks: 1}
	s.SentReplyProgress["pending-0"] = 1
	_, err = s.PrepareReplyDelivery("new", strings.Repeat("d", 64), 1)
	mustNoErr(t, err)
	if len(s.ReplyDeliveries) != MaxReplyDeliveries {
		t.Fatalf("plans = %d", len(s.ReplyDeliveries))
	}
	if _, ok := s.ReplyDeliveries["pending-0"]; ok || s.ReplySentChunks("pending-0") != 0 || !s.ReplyDeliveries["pending-1"].InFlight {
		t.Fatal("eviction removed the wrong plan")
	}
}

// from: gateway/tests/runtime/state.test.ts: unknown reply quarantine preserves evidence and fails closed if its checkpoint cannot persist
func TestRtdStateQuarantinePreservesEvidence(t *testing.T) {
	dir := t.TempDir()
	account := rtdAccount("synthetic", "fixture")
	s := rtdState(t, dir, account, PolicyOptions{})
	_, err := s.PrepareReplyDelivery("key", strings.Repeat("d", 64), 2)
	mustNoErr(t, err)
	mustNoErr(t, s.BeginReplyChunk("key", 0))
	restore := rtdFailSaves(s)
	if err := s.RecordProcessedMessageKey("key", true); err == nil || s.IsMessageProcessed("key") || !s.HasUncertainReply("key") {
		t.Fatalf("failed checkpoint changed the state: %v", err)
	}
	restore()
	mustNoErr(t, s.RecordProcessedMessageKey("key", true))
	if r := rtdState(t, dir, account, PolicyOptions{}); !r.IsMessageProcessed("key") || !r.HasUncertainReply("key") {
		t.Fatal("quarantine lost its evidence")
	}
}

// from: gateway/tests/runtime/state.test.ts: permanent image failure persists, blocks replay and rolls back failed checkpoints
func TestRtdStatePermanentImageFailure(t *testing.T) {
	dir := t.TempDir()
	account := rtdAccount("synthetic", "fixture")
	s := rtdState(t, dir, account, PolicyOptions{})
	key, digest := "image-reply:key", strings.Repeat("e", 64)
	_, err := s.PrepareReplyDelivery(key, digest, 2)
	mustNoErr(t, err)
	mustNoErr(t, s.BeginReplyChunk(key, 0))
	rtdWantErr(t, s.RecordReplyFailure(key, FailureAttachmentExpired), "Invalid")
	mustNoErr(t, s.ConfirmReplyChunk(key, 0))
	restore := rtdFailSaves(s)
	if err := s.RecordReplyFailure(key, FailureAttachmentExpired); !errors.Is(err, rtdFailure) || s.HasPermanentReplyFailure(key) {
		t.Fatalf("failed checkpoint: %v", err)
	}
	restore()
	if rtdState(t, dir, account, PolicyOptions{}).HasPermanentReplyFailure(key) {
		t.Fatal("failure on disk despite the failed checkpoint")
	}
	mustNoErr(t, s.RecordReplyFailure(key, FailureAttachmentExpired))
	mustNoErr(t, s.RecordProcessedMessageKey("key", true))
	r := rtdState(t, dir, account, PolicyOptions{})
	if !r.HasPermanentReplyFailure(key) || r.ReplyDeliveries[key].AcknowledgedChunks != 1 || r.ReplyDeliveries[key].FailureReason != FailureAttachmentExpired {
		t.Fatalf("reloaded = %+v", r.ReplyDeliveries[key])
	}
	_, err = r.PrepareReplyDelivery(key, digest, 2)
	rtdWantErr(t, err, "permanently failed")
	rtdWantErr(t, r.BeginReplyChunk(key, 1), "Invalid")
	rtdWantErr(t, r.RecordReplyFailure(key, FailureAttachmentUnavailable), "Invalid")
}

// from: gateway/tests/runtime/state.test.ts: corrupt permanent failure state fails closed
func TestRtdStateCorruptPermanentFailure(t *testing.T) {
	dir := t.TempDir()
	account := rtdAccount("synthetic", "fixture")
	s := rtdState(t, dir, account, PolicyOptions{})
	for _, invalid := range []ReplyDeliveryState{
		{FailureReason: "Unrecognized"},
		{FailureReason: FailureAttachmentExpired, InFlight: true},
		{FailureReason: FailureAttachmentUnavailable, AcknowledgedChunks: 1},
	} {
		invalid.Digest, invalid.TotalChunks = strings.Repeat("f", 64), 1
		s.ReplyDeliveries["key"] = invalid
		mustNoErr(t, s.SaveState())
		_, err := NewStateManager(account, s.StatePath, PolicyOptions{})
		rtdWantErr(t, err, "Failed to load gateway state")
	}
}

// from: gateway/tests/runtime/state.test.ts: active external delivery evidence is retained until its owner releases it
func TestRtdStateExternalEvidenceRetained(t *testing.T) {
	s := rtdState(t, t.TempDir(), nil, PolicyOptions{})
	for i := 0; i < MaxReplyDeliveries; i++ {
		s.ReplyDeliveries["result:"+itoa(i)] = ReplyDeliveryState{Digest: strings.Repeat("c", 64), TotalChunks: 1, AcknowledgedChunks: 1}
	}
	s.SetReplyEvidenceProtection(func(k string) bool { return strings.HasPrefix(k, "result:") })
	_, err := s.PrepareReplyDelivery("ordinary", strings.Repeat("d", 64), 1)
	rtdWantErr(t, err, "capacity exhausted")
	s.SetReplyEvidenceProtection(func(k string) bool { return k != "result:0" })
	_, err = s.PrepareReplyDelivery("ordinary", strings.Repeat("d", 64), 1)
	mustNoErr(t, err)
	if _, ok := s.ReplyDeliveries["result:0"]; ok {
		t.Fatal("released evidence kept")
	}
	if _, ok := s.ReplyDeliveries["result:1"]; !ok {
		t.Fatal("protected evidence evicted")
	}
}

// from: gateway/tests/runtime/state.test.ts: reported remaining budget honors a lowered policy and expired window
func TestRtdStateRemainingBudgetHonorsPolicy(t *testing.T) {
	s := rtdState(t, t.TempDir(), nil, PolicyOptions{SendBudget: 4})
	now := nowMs()
	s.ContextWindows["chat"] = ChatContextWindow{RefreshedAt: now, LastInboundKey: "inbound"}
	granted := int64(10)
	s.AccountBudget = &AccountSendBudget{InboundCreatedAt: now, ContextBudgetState: ContextBudgetState{RefreshedAt: now,
		LastInboundKey: "inbound", GrantedBudget: &granted, Remaining: 7}}
	if got := s.RemainingSendBudget("chat"); got != 1 {
		t.Fatalf("remaining = %d, want 1", got)
	}
	w := s.ContextWindows["chat"]
	w.RefreshedAt -= s.Policy.ContextWindowMs
	s.ContextWindows["chat"] = w
	if got := s.RemainingSendBudget("chat"); got != 0 {
		t.Fatalf("remaining after expiry = %d", got)
	}
}

// from: gateway/tests/runtime/state.test.ts: renewal rejection is one durable transition that blocks replay and rolls back failed checkpoints
func TestRtdStateRenewalRejection(t *testing.T) {
	dir := t.TempDir()
	account := rtdAccount("synthetic", "fixture")
	s := rtdState(t, dir, account, PolicyOptions{})
	key, digest := "renewal:rejected", strings.Repeat("a", 64)
	_, err := s.PrepareRenewalReplyDelivery(key, digest, 1)
	mustNoErr(t, err)
	mustNoErr(t, s.BeginReplyChunk(key, 0))
	original := s.ReplyDeliveries[key]
	restore := rtdFailSaves(s)
	if err := s.RejectReplyChunk(key, 0, true); !errors.Is(err, rtdFailure) || s.ReplyDeliveries[key] != original {
		t.Fatalf("failed rejection changed the state: %v", err)
	}
	restore()
	if rtdState(t, dir, account, PolicyOptions{}).ReplyDeliveries[key] != original {
		t.Fatal("disk changed despite the failed checkpoint")
	}
	mustNoErr(t, s.RejectReplyChunk(key, 0, true))
	r := rtdState(t, dir, account, PolicyOptions{})
	want := original
	want.InFlight, want.RenewalRejected = false, true
	if r.ReplyDeliveries[key] != want || !r.HasPermanentReplyFailure(key) {
		t.Fatalf("reloaded = %+v", r.ReplyDeliveries[key])
	}
	_, err = r.PrepareReplyDelivery(key, digest, 1)
	rtdWantErr(t, err, "permanently failed")
	_, err = r.PrepareRenewalReplyDelivery(key, digest, 1)
	rtdWantErr(t, err, "permanently failed")
	_, err = r.PrepareRenewalReplyDelivery(key, strings.Repeat("b", 64), 1)
	rtdWantErr(t, err, "content or chunk plan changed")
	rtdWantErr(t, r.BeginReplyChunk(key, 0), "Invalid")
	rtdWantErr(t, r.RecordReplyFailure(key, FailureAttachmentUnavailable), "Invalid")
	if !r.ReplyDeliveries[key].RenewalRejected {
		t.Fatal("rejection marker lost")
	}
	_, err = s.PrepareReplyDelivery("ordinary", digest, 1)
	mustNoErr(t, err)
	mustNoErr(t, s.BeginReplyChunk("ordinary", 0))
	rtdWantErr(t, s.RejectReplyChunk("ordinary", 0, true), "Invalid renewal delivery key")
	if !s.ReplyDeliveries["ordinary"].InFlight {
		t.Fatal("refused marker changed the plan")
	}
	mustNoErr(t, s.RejectReplyChunk("ordinary", 0, false))
	d, err := s.PrepareReplyDelivery("ordinary", digest, 1)
	mustNoErr(t, err)
	if d.RenewalRejected {
		t.Fatal("ordinary rejection marked as renewal")
	}
}

// from: gateway/tests/runtime/state.test.ts: invalid renewal rejection markers fail closed without changing attachment failure compatibility
func TestRtdStateInvalidRenewalMarkers(t *testing.T) {
	dir := t.TempDir()
	account := rtdAccount("synthetic", "fixture")
	s := rtdState(t, dir, account, PolicyOptions{})
	for _, c := range []struct {
		key   string
		patch map[string]any
	}{
		{"ordinary", map[string]any{"renewalRejected": true}},
		{"renewal:invalid", map[string]any{"renewalRejected": "true"}},
		{"renewal:invalid", map[string]any{"renewalRejected": true, "inFlight": true}},
		{"renewal:invalid", map[string]any{"renewalRejected": true, "acknowledgedChunks": 1}},
	} {
		item := map[string]any{"digest": strings.Repeat("f", 64), "totalChunks": 1, "acknowledgedChunks": 0, "inFlight": false}
		for k, v := range c.patch {
			item[k] = v
		}
		s.ReplyDeliveries = map[string]ReplyDeliveryState{}
		mustNoErr(t, s.SaveState())
		data, err := os.ReadFile(s.StatePath)
		mustNoErr(t, err)
		var raw map[string]any
		mustNoErr(t, json.Unmarshal(data, &raw))
		raw["reply_deliveries"] = map[string]any{c.key: item}
		mustNoErr(t, os.WriteFile(s.StatePath, []byte(jsonText(raw)), 0o600))
		_, err = NewStateManager(account, s.StatePath, PolicyOptions{})
		rtdWantErr(t, err, "Failed to load gateway state")
	}
	// The original also refused renewalRejected:false; a false flag decodes
	// as absent here and is accepted as an ordinary unsent plan.
}
