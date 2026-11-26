package wechat

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

type rtcInboxFixture struct {
	dir, path   string
	fingerprint string
	limit       int
	inbox       *FileInboundInbox
}

func rtcNewInbox(t *testing.T, limit int) *rtcInboxFixture {
	t.Helper()
	f := &rtcInboxFixture{dir: t.TempDir(), fingerprint: "account-one", limit: limit}
	f.path = filepath.Join(f.dir, "inbound.json")
	f.inbox = f.reload(t)
	return f
}

func (f *rtcInboxFixture) open() (*FileInboundInbox, error) {
	return NewFileInboundInbox(f.path, func() string { return f.fingerprint }, f.limit)
}

func (f *rtcInboxFixture) reload(t *testing.T) *FileInboundInbox {
	t.Helper()
	b, err := f.open()
	mustNoErr(t, err)
	return b
}

func rtcInbound(key string, patch func(*InboundMessage)) InboundMessage {
	m := InboundMessage{ChatID: "chat-one", Text: "先帮我查一下这件事", MessageID: "message-" + key, DeliveryKey: key,
		ContextToken: "synthetic-context", CreatedAtMs: i64(1_800_000_000_000)}
	if patch != nil {
		patch(&m)
	}
	return m
}

func rtcList(t *testing.T, b *FileInboundInbox) []InboundMessage {
	t.Helper()
	list, err := b.List()
	mustNoErr(t, err)
	return list
}

func rtcKeys(list []InboundMessage) []string {
	var keys []string
	for _, m := range list {
		keys = append(keys, m.DeliveryKey)
	}
	return keys
}

func rtcIsDir(err error) bool { return errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.EPERM) }

func rtcReadDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	mustNoErr(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// from: gateway/tests/runtime/inbox.test.ts: inbound messages recover exact text, metadata and poll insertion order across restarts
func TestRtcInboxRecoversOrder(t *testing.T) {
	f := rtcNewInbox(t, 10)
	messages := []InboundMessage{rtcInbound("10", nil), rtcInbound("2", func(m *InboundMessage) { m.Text = "补充：请保留\n原始换行与 emoji 🦊" }), rtcInbound("1", nil)}
	for _, m := range messages {
		mustNoErr(t, f.inbox.Put(m))
	}
	if got := rtcList(t, f.reload(t)); !reflect.DeepEqual(got, messages) {
		t.Fatalf("list = %+v", got)
	}
	st, err := os.Stat(f.path)
	mustNoErr(t, err)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode())
	}
	restarted := f.reload(t)
	mustNoErr(t, restarted.Remove("2"))
	mustNoErr(t, restarted.Put(rtcInbound("2", func(m *InboundMessage) { m.Text = "新的入站内容" })))
	if got := rtcKeys(rtcList(t, f.reload(t))); !reflect.DeepEqual(got, []string{"10", "1", "2"}) {
		t.Fatalf("order = %v", got)
	}
}

// from: gateway/tests/runtime/inbox.test.ts: separate gateway inbox instances merge durable writes instead of overwriting each other
func TestRtcInboxInstancesMerge(t *testing.T) {
	f := rtcNewInbox(t, 10)
	first, second := f.reload(t), f.reload(t)
	mustNoErr(t, first.Put(rtcInbound("first", nil)))
	mustNoErr(t, second.Put(rtcInbound("second", nil)))
	if got := rtcKeys(rtcList(t, f.reload(t))); !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("keys = %v", got)
	}
	mustNoErr(t, first.Remove("first"))
	mustNoErr(t, second.Remove("second"))
	if got := rtcList(t, f.reload(t)); len(got) != 0 {
		t.Fatalf("left = %v", got)
	}
}

// from: gateway/tests/runtime/inbox.test.ts: retained messages accept exact retries at capacity and reject changed content or metadata
func TestRtcInboxRetriesAndConflicts(t *testing.T) {
	f := rtcNewInbox(t, 1)
	original := rtcInbound("same-key", nil)
	mustNoErr(t, f.inbox.Put(original))
	mustNoErr(t, f.inbox.Put(original))
	for _, patch := range []func(*InboundMessage){
		func(m *InboundMessage) { m.Text = "篡改后的内容" }, func(m *InboundMessage) { m.ChatID = "other-chat" },
		func(m *InboundMessage) { m.MessageID = "other-message" }, func(m *InboundMessage) { m.ContextToken = "changed-context" },
		func(m *InboundMessage) { m.CreatedAtMs = i64(*m.CreatedAtMs + 1) }, func(m *InboundMessage) { m.ContextToken = "" },
	} {
		if err := f.inbox.Put(rtcInbound("same-key", patch)); !errors.Is(err, InboxConflictError) {
			t.Fatalf("changed put error = %v", err)
		}
	}
	if err := f.inbox.Put(rtcInbound("overflow", nil)); !errors.Is(err, OutboxBackpressureError) {
		t.Fatalf("overflow error = %v", err)
	}
	if got := rtcList(t, f.reload(t)); !reflect.DeepEqual(got, []InboundMessage{original}) {
		t.Fatalf("list = %+v", got)
	}
	mustNoErr(t, f.inbox.Remove("same-key"))
	mustNoErr(t, f.inbox.Put(rtcInbound("overflow", nil)))
	if has, err := f.reload(t).Has("overflow"); err != nil || !has {
		t.Fatalf("has = %v %v", has, err)
	}
}

// from: gateway/tests/runtime/inbox.test.ts: inbox callers cannot mutate persisted input through put or list object references
func TestRtcInboxCopies(t *testing.T) {
	f := rtcNewInbox(t, 10)
	item := rtcInbound("first", nil)
	expected := item
	mustNoErr(t, f.inbox.Put(item))
	item.Text = "input mutated"
	rtcList(t, f.inbox)[0].Text = "output mutated"
	if got := rtcList(t, f.inbox); !reflect.DeepEqual(got, []InboundMessage{expected}) {
		t.Fatalf("list = %+v", got)
	}
	if got := rtcList(t, f.reload(t)); !reflect.DeepEqual(got, []InboundMessage{expected}) {
		t.Fatalf("reloaded = %+v", got)
	}
}

// from: gateway/tests/runtime/inbox.test.ts: failed checkpoints keep memory and insertion order unchanged and clean up temporary files
func TestRtcInboxFailedCheckpoints(t *testing.T) {
	f := rtcNewInbox(t, 10)
	mustNoErr(t, f.inbox.Put(rtcInbound("first", nil)))
	mustNoErr(t, f.inbox.Put(rtcInbound("second", nil)))
	saved, err := os.ReadFile(f.path)
	mustNoErr(t, err)
	before := rtcList(t, f.inbox)
	mustNoErr(t, os.Remove(f.path))
	mustNoErr(t, os.Mkdir(f.path, 0o700))
	if err := f.inbox.Put(rtcInbound("third", nil)); !rtcIsDir(err) {
		t.Fatalf("put error = %v", err)
	}
	if got := rtcList(t, f.inbox); !reflect.DeepEqual(got, before) {
		t.Fatalf("after put: %v", got)
	}
	if err := f.inbox.Remove("first"); !rtcIsDir(err) {
		t.Fatalf("remove error = %v", err)
	}
	if got := rtcList(t, f.inbox); !reflect.DeepEqual(got, before) {
		t.Fatalf("after remove: %v", got)
	}
	if names := rtcReadDir(t, f.dir); !reflect.DeepEqual(names, []string{"inbound.json"}) {
		t.Fatalf("dir = %v", names)
	}
	mustNoErr(t, os.Remove(f.path))
	mustNoErr(t, os.WriteFile(f.path, saved, 0o600))
	if got := rtcList(t, f.reload(t)); !reflect.DeepEqual(got, before) {
		t.Fatalf("restored = %v", got)
	}
	mustNoErr(t, f.inbox.Remove("first"))
	if got := rtcList(t, f.reload(t)); !reflect.DeepEqual(got, []InboundMessage{rtcInbound("second", nil)}) {
		t.Fatalf("final = %v", got)
	}
}

// from: gateway/tests/runtime/inbox.test.ts: a changed account resets pending messages durably before allowing any access
func TestRtcInboxAccountReset(t *testing.T) {
	f := rtcNewInbox(t, 10)
	mustNoErr(t, f.inbox.Put(rtcInbound("old-account", nil)))
	f.fingerprint = "account-two"
	if has, err := f.inbox.Has("old-account"); err != nil || has {
		t.Fatalf("old message visible: %v %v", has, err)
	}
	if got := rtcList(t, f.reload(t)); len(got) != 0 {
		t.Fatalf("reloaded = %v", got)
	}
	raw, err := os.ReadFile(f.path)
	mustNoErr(t, err)
	var disk struct{ Fingerprint string }
	mustNoErr(t, json.Unmarshal(raw, &disk))
	if disk.Fingerprint != "account-two" {
		t.Fatalf("disk fingerprint = %s", disk.Fingerprint)
	}
	mustNoErr(t, f.inbox.Put(rtcInbound("new-account", nil)))
	f.fingerprint = "account-three"
	if got := rtcList(t, f.reload(t)); len(got) != 0 {
		t.Fatalf("account three sees %v", got)
	}
	f.fingerprint = "account-two"
	if got := rtcList(t, f.reload(t)); len(got) != 0 {
		t.Fatalf("account two sees %v", got)
	}
	if names := rtcReadDir(t, f.dir); !reflect.DeepEqual(names, []string{"inbound.json"}) {
		t.Fatalf("dir = %v", names)
	}
}

// from: gateway/tests/runtime/inbox.test.ts: failed account reset does not expose old messages or switch the in-memory account
func TestRtcInboxFailedAccountReset(t *testing.T) {
	f := rtcNewInbox(t, 10)
	mustNoErr(t, f.inbox.Put(rtcInbound("old-account", nil)))
	original, err := os.ReadFile(f.path)
	mustNoErr(t, err)
	mustNoErr(t, os.Remove(f.path))
	mustNoErr(t, os.Mkdir(f.path, 0o700))
	f.fingerprint = "account-two"
	if _, err := f.inbox.List(); !rtcIsDir(err) {
		t.Fatalf("list error = %v", err)
	}
	if _, err := f.inbox.Has("old-account"); !rtcIsDir(err) {
		t.Fatalf("has error = %v", err)
	}
	mustNoErr(t, os.Remove(f.path))
	mustNoErr(t, os.WriteFile(f.path, original, 0o600))
	if got := rtcList(t, f.inbox); len(got) != 0 {
		t.Fatalf("old messages exposed: %v", got)
	}
	mustNoErr(t, f.inbox.Put(rtcInbound("new-account", nil)))
	if got := rtcList(t, f.inbox); !reflect.DeepEqual(got, []InboundMessage{rtcInbound("new-account", nil)}) {
		t.Fatalf("list = %v", got)
	}
}
