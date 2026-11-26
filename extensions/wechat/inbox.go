package wechat

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"
)

const (
	InboundInboxMaxBytes     = 16 * 1024 * 1024
	InboundInboxTextMaxBytes = 256 * 1024
	inboxIDMaxBytes          = 4096
	inboxContextMaxBytes     = 16 * 1024
	inboxSchemaVersion       = 1
	inboxMaxItems            = 10_000
	lockWaitTimeout          = 10 * time.Second
	lockRetryDelay           = 10 * time.Millisecond
	lockStaleAfter           = 60 * time.Second
)

var (
	inboxMessageFields = map[string]bool{"chatId": true, "text": true, "messageId": true, "deliveryKey": true, "contextToken": true,
		"createdAtMs": true, "unsupportedKinds": true, "media": true, "quote": true, "ignoredKinds": true}
	inboxMediaFields     = map[string]bool{"kind": true, "fullUrl": true, "encryptQueryParam": true, "aesKeyHex": true, "fileName": true}
	inboxStateFields     = map[string]bool{"version": true, "fingerprint": true, "items": true}
	unsupportedKindNames = map[string]bool{KindImage: true, KindFile: true, KindVideo: true, KindVoice: true, KindUnknown: true}
	lowerAESKey          = regexp.MustCompile(`^[0-9a-f]{32}$`)

	// heldLocks are the lock files this process holds; any other record with
	// our pid is stale.
	heldLocks   = map[string]bool{}
	heldLocksMu sync.Mutex
)

// InboxConflictError: a durable message's identity or content changed.
var InboxConflictError = errors.New("Durable inbound message identity or content changed")

var (
	errInboxSchema      = errors.New("Invalid inbound inbox schema")
	errInboxString      = errors.New("Invalid inbound inbox string")
	errInboxKinds       = errors.New("Invalid inbound inbox unsupported kinds")
	errInboxMedia       = errors.New("Invalid inbound inbox media")
	errInboxTimestamp   = errors.New("Invalid inbound inbox timestamp")
	errInboxState       = errors.New("Invalid inbound inbox state")
	errInboxBudget      = errors.New("Inbound inbox exceeds byte budget")
	errInboxDuplicate   = errors.New("Duplicate inbound inbox delivery key")
	errInboxFingerprint = errors.New("Inbound inbox account fingerprint changed")
	errInboxLockTimeout = errors.New("Timed out waiting for inbound inbox lock")
	errInboxCapacity    = errors.New("Invalid inbound inbox capacity")
)

func inboxObject(raw json.RawMessage, fields map[string]bool) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, errInboxSchema
	}
	for k := range m {
		if !fields[k] {
			return nil, errInboxSchema
		}
	}
	return m, nil
}

func inboxString(s string, maxBytes int) error {
	if len(bytes.TrimSpace([]byte(s))) == 0 || len(s) > maxBytes {
		return errInboxString
	}
	return nil
}

func optionalInboxString(s string, maxChars int) error {
	if s == "" {
		return nil
	}
	if len(bytes.TrimSpace([]byte(s))) == 0 || utf16Len(s) > maxChars || controlChars.MatchString(s) {
		return errInboxMedia
	}
	return nil
}

// validateInboxMessage is the inbox's schema: every field known, sizes
// bounded, unsupported content textless, everything else with its full text.
func validateInboxMessage(raw json.RawMessage) (InboundMessage, error) {
	fields, err := inboxObject(raw, inboxMessageFields)
	if err != nil {
		return InboundMessage{}, err
	}
	// A field of the wrong type is reported as that field's error.
	var kinds []string
	if k, ok := fields["unsupportedKinds"]; ok && json.Unmarshal(k, &kinds) != nil {
		return InboundMessage{}, errInboxKinds
	}
	var refs []json.RawMessage
	if r, ok := fields["media"]; ok && json.Unmarshal(r, &refs) != nil {
		return InboundMessage{}, errInboxMedia
	}
	var m InboundMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return InboundMessage{}, errInboxSchema
	}
	_, hasKinds := fields["unsupportedKinds"]
	_, hasMedia := fields["media"]
	if hasKinds {
		seen := map[string]bool{}
		if len(m.UnsupportedKinds) == 0 {
			return InboundMessage{}, errInboxKinds
		}
		for _, k := range m.UnsupportedKinds {
			if !unsupportedKindNames[k] || seen[k] {
				return InboundMessage{}, errInboxKinds
			}
			seen[k] = true
		}
		if m.Text != "" || hasMedia {
			return InboundMessage{}, errInboxString
		}
	}
	if hasMedia {
		if len(m.Media) == 0 || len(m.Media) > MaxInboundMedia {
			return InboundMessage{}, errInboxMedia
		}
		for _, item := range fieldsOf(fields["media"]) {
			if _, err := inboxObject(item, inboxMediaFields); err != nil {
				return InboundMessage{}, err
			}
		}
		for _, ref := range m.Media {
			if ref.Kind != KindImage && ref.Kind != KindFile {
				return InboundMessage{}, errInboxMedia
			}
			for _, check := range []struct {
				v   string
				max int
			}{{ref.FullURL, MaxMediaParamChars}, {ref.EncryptQueryParam, MaxMediaParamChars}, {ref.AESKeyHex, 32}, {ref.FileName, MaxMediaFileNameChars}} {
				if err := optionalInboxString(check.v, check.max); err != nil {
					return InboundMessage{}, err
				}
			}
			if ref.FullURL == "" && ref.EncryptQueryParam == "" || ref.AESKeyHex != "" && !lowerAESKey.MatchString(ref.AESKeyHex) ||
				ref.Kind == KindFile && ref.AESKeyHex == "" {
				return InboundMessage{}, errInboxMedia
			}
		}
	}
	// Media may arrive alone; otherwise every message must keep its full text.
	textless := hasKinds || hasMedia && m.Text == ""
	if !textless {
		if err := inboxString(m.Text, InboundInboxTextMaxBytes); err != nil {
			return InboundMessage{}, err
		}
	}
	for _, id := range []string{m.ChatID, m.MessageID, m.DeliveryKey} {
		if err := inboxString(id, inboxIDMaxBytes); err != nil {
			return InboundMessage{}, err
		}
	}
	if _, ok := fields["contextToken"]; ok {
		if err := inboxString(m.ContextToken, inboxContextMaxBytes); err != nil {
			return InboundMessage{}, err
		}
	}
	if ts, ok := fields["createdAtMs"]; ok {
		n, safe := safeInt(json.Number(ts))
		if !safe || n < 0 {
			return InboundMessage{}, errInboxTimestamp
		}
	}
	return m, nil
}

func fieldsOf(raw json.RawMessage) []json.RawMessage {
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	return list
}

type inboxFile struct {
	fingerprint string
	items       []InboundMessage
}

// FileInboundInbox keeps every admitted message's full content on disk
// before the poll cursor moves or a running turn receives it.
type FileInboundInbox struct {
	path               string
	accountFingerprint func() string
	limit              int
	fingerprint        string
	items              []InboundMessage
}

func NewFileInboundInbox(path string, fingerprint func() string, limit int) (*FileInboundInbox, error) {
	if limit <= 0 || limit > inboxMaxItems {
		return nil, errInboxCapacity
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	b := &FileInboundInbox{path: abs, accountFingerprint: fingerprint, limit: limit}
	if b.fingerprint, err = b.currentFingerprint(); err != nil {
		return nil, err
	}
	st, err := b.read(false)
	if err != nil || st == nil {
		return b, err
	}
	if st.fingerprint != b.fingerprint {
		return b, withFileLock(b.path, func() error {
			latest, err := b.read(false)
			if err != nil {
				return err
			}
			if latest != nil && latest.fingerprint != b.fingerprint {
				return b.save(nil, b.fingerprint)
			}
			return nil
		})
	}
	if len(st.items) > limit {
		return nil, OutboxBackpressureError
	}
	b.items = st.items
	return b, nil
}

func (b *FileInboundInbox) currentFingerprint() (string, error) {
	f := b.accountFingerprint()
	return f, inboxString(f, inboxIDMaxBytes)
}

// account follows a login change: the inbox of another account is emptied.
func (b *FileInboundInbox) account() error {
	current, err := b.currentFingerprint()
	if err != nil {
		return err
	}
	if current == b.fingerprint {
		return nil
	}
	if err := withFileLock(b.path, func() error {
		latest, err := b.read(true)
		if err != nil {
			return err
		}
		if latest == nil || latest.fingerprint != current {
			return b.save(nil, current)
		}
		return nil
	}); err != nil {
		return err
	}
	b.items, b.fingerprint = nil, current
	return nil
}

// read loads the file; nil when there is none. eisdir reports a directory
// as such instead of as an oversized file.
func (b *FileInboundInbox) read(eisdir bool) (*inboxFile, error) {
	f, err := os.Open(b.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		if eisdir {
			return nil, &fs.PathError{Op: "read", Path: "inbound inbox", Err: syscall.EISDIR}
		}
		return nil, errInboxBudget
	}
	if st.Size() > InboundInboxMaxBytes {
		return nil, errInboxBudget
	}
	raw, err := os.ReadFile(b.path)
	if err != nil {
		return nil, err
	}
	if len(raw) > InboundInboxMaxBytes {
		return nil, errInboxBudget
	}
	if !json.Valid(raw) {
		return nil, &json.SyntaxError{}
	}
	fields, err := inboxObject(raw, inboxStateFields)
	if err != nil {
		return nil, err
	}
	var version int
	var fingerprint string
	var items []json.RawMessage
	if json.Unmarshal(fields["version"], &version) != nil || version != inboxSchemaVersion ||
		json.Unmarshal(fields["items"], &items) != nil || fields["items"] == nil || len(items) > inboxMaxItems {
		return nil, errInboxState
	}
	if json.Unmarshal(fields["fingerprint"], &fingerprint) != nil || inboxString(fingerprint, inboxIDMaxBytes) != nil {
		return nil, errInboxString
	}
	out := &inboxFile{fingerprint: fingerprint}
	seen := map[string]bool{}
	for _, raw := range items {
		m, err := validateInboxMessage(raw)
		if err != nil {
			return nil, err
		}
		if seen[m.DeliveryKey] {
			return nil, errInboxDuplicate
		}
		seen[m.DeliveryKey] = true
		out.items = append(out.items, m)
	}
	return out, nil
}

func (b *FileInboundInbox) reload() error {
	if err := b.account(); err != nil {
		return err
	}
	if st, err := os.Stat(b.path); err == nil && st.IsDir() {
		return nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	st, err := b.read(false)
	if err != nil {
		return err
	}
	if st == nil || st.fingerprint != b.fingerprint {
		b.items = nil
		return nil
	}
	if len(st.items) > b.limit {
		return OutboxBackpressureError
	}
	b.items = st.items
	return nil
}

func (b *FileInboundInbox) save(items []InboundMessage, fingerprint string) error {
	if items == nil {
		items = []InboundMessage{}
	}
	state := map[string]any{"version": inboxSchemaVersion, "fingerprint": fingerprint, "items": items}
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if len(encoded)+1 > InboundInboxMaxBytes {
		return OutboxBackpressureError
	}
	return writePrivateJSON(b.path, state)
}

// List is every pending message in admission order.
func (b *FileInboundInbox) List() ([]InboundMessage, error) {
	if err := b.reload(); err != nil {
		return nil, err
	}
	return append([]InboundMessage(nil), b.items...), nil
}

func (b *FileInboundInbox) Has(key string) (bool, error) {
	if err := b.reload(); err != nil {
		return false, err
	}
	if err := inboxString(key, inboxIDMaxBytes); err != nil {
		return false, err
	}
	return b.index(b.items, key) >= 0, nil
}

func (b *FileInboundInbox) index(items []InboundMessage, key string) int {
	for i, m := range items {
		if m.DeliveryKey == key {
			return i
		}
	}
	return -1
}

// Put stores a message once; the same key with other content is a conflict.
func (b *FileInboundInbox) Put(value InboundMessage) error {
	if err := b.account(); err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	validated, err := validateInboxMessage(encoded)
	if err != nil {
		return err
	}
	return withFileLock(b.path, func() error {
		st, err := b.read(true)
		if err != nil {
			return err
		}
		if st != nil && st.fingerprint != b.fingerprint {
			return errInboxFingerprint
		}
		var current []InboundMessage
		if st != nil {
			current = st.items
		}
		if i := b.index(current, validated.DeliveryKey); i >= 0 {
			if jsonText(current[i]) != jsonText(validated) {
				return InboxConflictError
			}
			b.items = current
			return nil
		}
		if len(current) >= b.limit {
			return OutboxBackpressureError
		}
		next := append(append([]InboundMessage(nil), current...), validated)
		if err := b.save(next, b.fingerprint); err != nil {
			return err
		}
		b.items = next
		return nil
	})
}

func (b *FileInboundInbox) Remove(key string) error {
	if err := b.account(); err != nil {
		return err
	}
	if err := inboxString(key, inboxIDMaxBytes); err != nil {
		return err
	}
	return withFileLock(b.path, func() error {
		st, err := b.read(true)
		if err != nil {
			return err
		}
		if st != nil && st.fingerprint != b.fingerprint {
			return errInboxFingerprint
		}
		var current []InboundMessage
		if st != nil {
			current = st.items
		}
		i := b.index(current, key)
		if i < 0 {
			b.items = current
			return nil
		}
		next := append(append([]InboundMessage(nil), current[:i]...), current[i+1:]...)
		if err := b.save(next, b.fingerprint); err != nil {
			return err
		}
		b.items = next
		return nil
	})
}
