package wechat

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// from: gateway/tests/runtime/inbox.test.ts: missing account identity prevents old account access until the same identity returns
func TestRtcInboxMissingAccount(t *testing.T) {
	f := rtcNewInbox(t, 10)
	mustNoErr(t, f.inbox.Put(rtcInbound("retained", nil)))
	saved, err := os.ReadFile(f.path)
	mustNoErr(t, err)
	f.fingerprint = ""
	if _, err := f.inbox.List(); err == nil || !strings.Contains(err.Error(), "Invalid inbound inbox string") {
		t.Fatalf("list error = %v", err)
	}
	if now, _ := os.ReadFile(f.path); string(now) != string(saved) {
		t.Fatal("file changed without an account")
	}
	f.fingerprint = "account-one"
	if got := rtcList(t, f.inbox); !reflect.DeepEqual(got, []InboundMessage{rtcInbound("retained", nil)}) {
		t.Fatalf("list = %v", got)
	}
}

// from: gateway/tests/runtime/inbox.test.ts: input validation rejects malformed messages without modifying durable state
func TestRtcInboxInputValidation(t *testing.T) {
	f := rtcNewInbox(t, 10)
	mustNoErr(t, f.inbox.Put(rtcInbound("valid", nil)))
	invalid := []InboundMessage{{}, rtcInbound("bad", func(m *InboundMessage) { m.Text = "" }),
		rtcInbound("bad", func(m *InboundMessage) { m.ChatID = " " }), rtcInbound("bad", func(m *InboundMessage) { m.DeliveryKey = "" }),
		rtcInbound("bad", func(m *InboundMessage) { m.MessageID = strings.Repeat("m", 4097) }),
		rtcInbound("bad", func(m *InboundMessage) { m.ContextToken = strings.Repeat("c", 16*1024+1) }),
		rtcInbound("bad", func(m *InboundMessage) { m.Text = strings.Repeat("界", (InboundInboxTextMaxBytes+2)/3) }),
		rtcInbound("bad", func(m *InboundMessage) { m.CreatedAtMs = i64(-1) })}
	for _, m := range invalid {
		if err := f.inbox.Put(m); err == nil || !strings.Contains(err.Error(), "Invalid inbound inbox") {
			t.Fatalf("put %+v: error %v", m.DeliveryKey, err)
		}
	}
	// Shapes a Go value cannot take are checked on their JSON form.
	base, _ := json.Marshal(rtcInbound("bad", nil))
	raw := func(field, value string) json.RawMessage {
		var m map[string]json.RawMessage
		mustNoErr(t, json.Unmarshal(base, &m))
		m[field] = json.RawMessage(value)
		out, _ := json.Marshal(m)
		return out
	}
	for _, r := range []json.RawMessage{json.RawMessage("null"), json.RawMessage("[]"), json.RawMessage("{}"),
		raw("createdAtMs", "0.5"), raw("createdAtMs", `"123"`), raw("createdAtMs", "null"), raw("createdAtMs", "1e400"),
		raw("unknown", `"field"`)} {
		if _, err := validateInboxMessage(r); err == nil || !strings.Contains(err.Error(), "Invalid inbound inbox") {
			t.Fatalf("validate %s: error %v", r, err)
		}
	}
	if got := rtcList(t, f.reload(t)); !reflect.DeepEqual(got, []InboundMessage{rtcInbound("valid", nil)}) {
		t.Fatalf("list = %v", got)
	}
	if _, err := f.inbox.Has(""); err == nil || !strings.Contains(err.Error(), "Invalid inbound inbox string") {
		t.Fatalf("has error = %v", err)
	}
	if err := f.inbox.Remove(""); err == nil || !strings.Contains(err.Error(), "Invalid inbound inbox string") {
		t.Fatalf("remove error = %v", err)
	}
}

// from: gateway/tests/runtime/inbox.test.ts: disk corruption is never mistaken for an empty inbox, including another account's state
func TestRtcInboxDiskCorruption(t *testing.T) {
	f := rtcNewInbox(t, 10)
	first, _ := json.Marshal(rtcInbound("first", nil))
	textNumber := strings.Replace(string(first), `"text":"先帮我查一下这件事"`, `"text":12`, 1)
	for _, state := range []string{"null", "[]", "{}",
		`{"version":2,"fingerprint":"account-one","items":[` + string(first) + `]}`,
		`{"version":1,"fingerprint":"account-one","items":[` + string(first) + `],"unknown":true}`,
		`{"version":1,"fingerprint":"","items":[` + string(first) + `]}`,
		`{"version":1,"fingerprint":"account-one","items":{}}`,
		`{"version":1,"fingerprint":"account-one","items":[` + string(first) + `,` + string(first) + `]}`,
		`{"version":1,"fingerprint":"account-one","items":[` + textNumber + `]}`} {
		mustNoErr(t, os.WriteFile(f.path, []byte(state), 0o600))
		if _, err := f.open(); err == nil {
			t.Fatalf("accepted %s", state)
		}
		f.fingerprint = "other-account"
		if _, err := f.open(); err == nil {
			t.Fatalf("other account accepted %s", state)
		}
		f.fingerprint = "account-one"
	}
	mustNoErr(t, os.WriteFile(f.path, []byte("{not JSON"), 0o600))
	var syntax *json.SyntaxError
	if _, err := f.open(); !errors.As(err, &syntax) {
		t.Fatalf("broken JSON error = %v", err)
	}
}

// from: gateway/tests/runtime/inbox.test.ts: disk size is checked before reading and only ENOENT means an empty inbox
func TestRtcInboxDiskSize(t *testing.T) {
	f := rtcNewInbox(t, 10)
	if got := rtcList(t, f.reload(t)); len(got) != 0 {
		t.Fatalf("new inbox = %v", got)
	}
	mustNoErr(t, os.WriteFile(f.path, []byte("{}"), 0o600))
	mustNoErr(t, os.Truncate(f.path, InboundInboxMaxBytes+1))
	if _, err := f.open(); err == nil || !strings.Contains(err.Error(), "exceeds byte budget") {
		t.Fatalf("oversized error = %v", err)
	}
	mustNoErr(t, os.Remove(f.path))
	mustNoErr(t, os.Mkdir(f.path, 0o700))
	if _, err := f.open(); err == nil || !strings.Contains(err.Error(), "exceeds byte budget") {
		t.Fatalf("directory error = %v", err)
	}
}

// from: gateway/tests/runtime/inbox.test.ts: combined message body size causes backpressure before the durable file exceeds its budget
func TestRtcInboxCombinedSize(t *testing.T) {
	f := rtcNewInbox(t, 1000)
	text := strings.Repeat("x", InboundInboxTextMaxBytes)
	accepted := InboundInboxMaxBytes/InboundInboxTextMaxBytes - 1
	items := make([]InboundMessage, accepted)
	for i := range items {
		items[i] = rtcInbound("body-"+strconv.Itoa(i), func(m *InboundMessage) { m.Text = text })
	}
	raw, err := json.Marshal(map[string]any{"version": 1, "fingerprint": f.fingerprint, "items": items})
	mustNoErr(t, err)
	mustNoErr(t, os.WriteFile(f.path, raw, 0o600))
	inbox := f.reload(t)
	next := "body-" + strconv.Itoa(accepted)
	if err := inbox.Put(rtcInbound(next, func(m *InboundMessage) { m.Text = text })); !errors.Is(err, OutboxBackpressureError) {
		t.Fatalf("put error = %v", err)
	}
	st, err := os.Stat(f.path)
	mustNoErr(t, err)
	if st.Size() > InboundInboxMaxBytes {
		t.Fatalf("file grew to %d", st.Size())
	}
	if got := rtcList(t, f.reload(t)); len(got) != accepted {
		t.Fatalf("%d items", len(got))
	}
	if has, err := inbox.Has(next); err != nil || has {
		t.Fatalf("has = %v %v", has, err)
	}
}

// from: gateway/tests/runtime/inbox.test.ts: invalid capacities and reduced restart capacity reject overflow without rewriting messages
func TestRtcInboxCapacity(t *testing.T) {
	f := rtcNewInbox(t, 10)
	// Fractional, NaN and infinite limits cannot be expressed as int.
	for _, limit := range []int{0, -1, 10_001} {
		if _, err := NewFileInboundInbox(f.path, func() string { return f.fingerprint }, limit); err == nil ||
			!strings.Contains(err.Error(), "Invalid inbound inbox capacity") {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
	mustNoErr(t, f.inbox.Put(rtcInbound("first", nil)))
	mustNoErr(t, f.inbox.Put(rtcInbound("second", nil)))
	before, _ := os.ReadFile(f.path)
	if _, err := NewFileInboundInbox(f.path, func() string { return f.fingerprint }, 1); !errors.Is(err, OutboxBackpressureError) {
		t.Fatalf("reduced capacity error = %v", err)
	}
	if after, _ := os.ReadFile(f.path); string(after) != string(before) {
		t.Fatal("reduced capacity rewrote the file")
	}
}

// from: gateway/tests/runtime/inbox.test.ts: unsupported content is durable with empty text only when its kinds are present
func TestRtcInboxUnsupportedContent(t *testing.T) {
	f := rtcNewInbox(t, 10)
	media := rtcInbound("media", func(m *InboundMessage) { m.Text, m.UnsupportedKinds = "", []string{KindImage, KindVoice} })
	mustNoErr(t, f.inbox.Put(media))
	if got := rtcList(t, f.reload(t)); !reflect.DeepEqual(got, []InboundMessage{media}) {
		t.Fatalf("list = %+v", got)
	}
	if err := f.inbox.Put(rtcInbound("empty", func(m *InboundMessage) { m.Text = "" })); err == nil || !strings.Contains(err.Error(), "Invalid inbound inbox string") {
		t.Fatalf("empty text error = %v", err)
	}
	if err := f.inbox.Put(rtcInbound("text-with-kinds", func(m *InboundMessage) { m.UnsupportedKinds = []string{KindFile} })); err == nil ||
		!strings.Contains(err.Error(), "Invalid inbound inbox string") {
		t.Fatalf("text with kinds error = %v", err)
	}
	for _, kinds := range [][]string{{"image", "image"}, {"sticker"}} {
		if err := f.inbox.Put(rtcInbound("bad-kinds", func(m *InboundMessage) { m.Text, m.UnsupportedKinds = "", kinds })); err == nil ||
			!strings.Contains(err.Error(), "unsupported kinds") {
			t.Fatalf("kinds %v error = %v", kinds, err)
		}
	}
	// An empty list is omitted by a Go value; its JSON form is checked directly.
	if _, err := validateInboxMessage(json.RawMessage(`{"chatId":"c","text":"","messageId":"m","deliveryKey":"k","unsupportedKinds":[]}`)); err == nil ||
		!strings.Contains(err.Error(), "unsupported kinds") {
		t.Fatalf("empty kinds error = %v", err)
	}
	tampered, _ := json.Marshal(rtcInbound("tampered", func(m *InboundMessage) { m.Text = "" }))
	state := `{"version":1,"fingerprint":"account-one","items":[` + strings.TrimSuffix(string(tampered), "}") + `,"unsupportedKinds":"image"}]}`
	mustNoErr(t, os.WriteFile(f.path, []byte(state), 0o600))
	if _, err := f.open(); err == nil || !strings.Contains(err.Error(), "unsupported kinds") {
		t.Fatalf("tampered error = %v", err)
	}
}

// from: gateway/tests/runtime/inbox.test.ts: a lock left with this process PID by a previous instance is reclaimed without waiting
func TestRtcInboxReclaimsOwnPIDLock(t *testing.T) {
	f := rtcNewInbox(t, 10)
	lockPath := f.path + ".lock"
	for _, mtime := range []time.Time{time.Now().Add(-time.Hour), time.Now()} {
		rec, _ := json.Marshal(map[string]int64{"pid": int64(os.Getpid()), "createdAt": mtime.UnixMilli()})
		mustNoErr(t, os.WriteFile(lockPath, rec, 0o600))
		mustNoErr(t, os.Chtimes(lockPath, mtime, mtime))
		started := time.Now()
		mustNoErr(t, f.inbox.Put(rtcInbound("after-"+strconv.Itoa(int(mtime.UnixMilli())), nil)))
		if time.Since(started) >= time.Second {
			t.Fatal("reclaim waited for the lock timeout")
		}
		if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
			t.Fatalf("lock left behind: %v", err)
		}
	}
	if got := rtcList(t, f.reload(t)); len(got) != 2 {
		t.Fatalf("%d items", len(got))
	}
}

// from: gateway/tests/runtime/inbox.test.ts: an expired, partially written lock is still reclaimed
func TestRtcInboxReclaimsExpiredPartialLock(t *testing.T) {
	f := rtcNewInbox(t, 10)
	lockPath := f.path + ".lock"
	mustNoErr(t, os.WriteFile(lockPath, nil, 0o600))
	old := time.Now().Add(-time.Hour)
	mustNoErr(t, os.Chtimes(lockPath, old, old))
	mustNoErr(t, f.inbox.Put(rtcInbound("after-expired-partial", nil)))
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock left behind: %v", err)
	}
}

// from: gateway/tests/runtime/inbox.test.ts: media references survive restart, alone or with text, and malformed references are refused
func TestRtcInboxMediaReferences(t *testing.T) {
	f := rtcNewInbox(t, 10)
	key := "2b7e151628aed2a6abf7158809cf4f3c"
	messages := []InboundMessage{
		rtcInbound("m1", func(m *InboundMessage) {
			m.Text, m.Media = "", []InboundMediaRef{{Kind: KindImage, EncryptQueryParam: "param", AESKeyHex: key}}
		}),
		rtcInbound("m2", func(m *InboundMessage) {
			m.Media = []InboundMediaRef{{Kind: KindFile, FullURL: "https://novac2c.cdn.weixin.qq.com/x", AESKeyHex: key, FileName: "a.pdf"}}
		}),
	}
	for _, m := range messages {
		mustNoErr(t, f.inbox.Put(m))
	}
	if got := rtcList(t, f.reload(t)); !reflect.DeepEqual(got, messages) {
		t.Fatalf("list = %+v", got)
	}
	data, _ := os.ReadFile(f.path)
	mustNoErr(t, os.WriteFile(f.path, []byte(strings.Replace(string(data), key, "not-hex", 1)), 0o600))
	if _, err := f.open(); err == nil || !strings.Contains(err.Error(), "Invalid inbound inbox") {
		t.Fatalf("corrupt media error = %v", err)
	}
}
