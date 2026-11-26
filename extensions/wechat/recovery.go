package wechat

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	NotificationRecoveryStateVersion = 1
	MaxPendingFailures               = 64
	// NotificationRecoveryExpiryMs: unresolved evidence whose latest failure
	// is older no longer describes the current network.
	NotificationRecoveryExpiryMs = 7 * millisecondsPerDay
	maxRecoveryStateBytes        = 256 * 1024
	maxRecoveryCauseCodes        = 16
	minHTTPStatus                = 100
	maxHTTPStatus                = 599
	stageKeyPrefix               = "stage:"
)

// PendingFailure is one unresolved request or stage failure.
type PendingFailure struct {
	Stage         string   `json:"stage"`
	Operation     string   `json:"operation,omitempty"`
	Phase         string   `json:"phase,omitempty"`
	HTTPStatus    int      `json:"httpStatus,omitempty"`
	ErrorName     string   `json:"errorName"`
	CauseCodes    []string `json:"causeCodes"`
	Count         int64    `json:"count"`
	FirstFailedAt int64    `json:"firstFailedAt"`
	LastFailedAt  int64    `json:"lastFailedAt"`
}

// RecoveryEvidence survives restarts so a recovery is reported once the
// failing requests really succeed again.
type RecoveryEvidence struct {
	FirstFailedAt       int64
	LastFailedAt        int64
	TotalFailures       int64
	ConsecutiveFailures int64
	LastSucceededAt     string
	EvidenceOverflow    bool
	RecoveredOperations []string
	Pending             map[string]PendingFailure
	order               []string
}

// RecoveryLoad is what a load found: absent, expired or restored.
type RecoveryLoad struct {
	Kind               string // absent | expired | restored
	AgeMs              int64
	UnresolvedFailures int
	Evidence           *RecoveryEvidence
}

var ErrInvalidRecoveryState = errors.New("Invalid notification recovery state")

func stageKey(stage string) string { return stageKeyPrefix + stage }

func member(values []string, v string) bool { return containsString(values, v) }

type persistedFailure struct {
	IdentityHash *string `json:"identityHash,omitempty"`
	PendingFailure
}

type persistedRecovery struct {
	Version                  int                `json:"version"`
	FirstFailedAt            int64              `json:"firstFailedAt"`
	LastFailedAt             int64              `json:"lastFailedAt"`
	TotalFailures            int64              `json:"totalFailures"`
	ConsecutiveFailures      int64              `json:"consecutiveFailures"`
	LastSucceededAt          *string            `json:"lastSucceededAt,omitempty"`
	RecoveryEvidenceOverflow *bool              `json:"recoveryEvidenceOverflow"`
	RecoveredOperations      []string           `json:"recoveredOperations"`
	Unresolved               []persistedFailure `json:"unresolved"`
}

func pendingFromPersisted(item persistedFailure) (string, PendingFailure, error) {
	f := item.PendingFailure
	if len(f.CauseCodes) > maxRecoveryCauseCodes || !isSafeErrorName(f.ErrorName) || !member(append(notificationStages, StageUnknown), f.Stage) {
		return "", f, ErrInvalidRecoveryState
	}
	var codes []string
	for _, c := range f.CauseCodes {
		if !isSafeCauseCode(c) {
			return "", f, ErrInvalidRecoveryState
		}
		if !containsString(codes, c) {
			codes = append(codes, c)
		}
	}
	if codes == nil {
		codes = []string{}
	}
	f.CauseCodes = codes
	if f.Operation != "" && !member(requestOperations, f.Operation) || f.Phase != "" && !member(requestPhases, f.Phase) ||
		f.HTTPStatus != 0 && (f.HTTPStatus < minHTTPStatus || f.HTTPStatus > maxHTTPStatus) ||
		f.Count < 1 || f.FirstFailedAt < 0 || f.LastFailedAt < 0 || f.FirstFailedAt > f.LastFailedAt {
		return "", f, ErrInvalidRecoveryState
	}
	if item.IdentityHash == nil {
		if f.Operation != "" {
			return "", f, ErrInvalidRecoveryState
		}
		return stageKey(f.Stage), f, nil
	}
	if f.Operation == "" || !digestPattern.MatchString(*item.IdentityHash) {
		return "", f, ErrInvalidRecoveryState
	}
	return requestKey(f.Operation, *item.IdentityHash), f, nil
}

// persistedKey is a key's identity hash: an operation with the request's
// existing hash, or none for a fixed stage marker.
func persistedKey(key string, f PendingFailure) (*string, error) {
	if key == stageKey(f.Stage) && f.Operation == "" {
		return nil, nil
	}
	var parsed []string
	if json.Unmarshal([]byte(key), &parsed) != nil || len(parsed) != 2 || parsed[0] != f.Operation || !digestPattern.MatchString(parsed[1]) {
		return nil, ErrInvalidRecoveryState
	}
	return &parsed[1], nil
}

// NotificationRecoveryStore keeps bounded recovery evidence: fixed enums,
// allowlisted codes, counts, timestamps and request identity hashes only.
type NotificationRecoveryStore struct{ Path string }

func NewNotificationRecoveryStore(path string) *NotificationRecoveryStore {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return &NotificationRecoveryStore{Path: abs}
}

// Load answers ErrInvalidRecoveryState for corrupt content and the original
// error when the file cannot be read.
func (s *NotificationRecoveryStore) Load(now int64) (RecoveryLoad, error) {
	st, err := os.Stat(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return RecoveryLoad{Kind: "absent"}, nil
	}
	if err != nil {
		return RecoveryLoad{}, err
	}
	if !st.Mode().IsRegular() || st.Size() > maxRecoveryStateBytes {
		return RecoveryLoad{}, ErrInvalidRecoveryState
	}
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return RecoveryLoad{}, err
	}
	var p persistedRecovery
	if json.Unmarshal(raw, &p) != nil || p.Version != NotificationRecoveryStateVersion || p.RecoveryEvidenceOverflow == nil ||
		p.Unresolved == nil || len(p.Unresolved) > MaxPendingFailures || p.RecoveredOperations == nil ||
		len(p.RecoveredOperations) > len(requestOperations) || p.FirstFailedAt < 0 || p.LastFailedAt < 0 ||
		p.TotalFailures < 0 || p.ConsecutiveFailures < 0 {
		return RecoveryLoad{}, ErrInvalidRecoveryState
	}
	e := &RecoveryEvidence{FirstFailedAt: p.FirstFailedAt, LastFailedAt: p.LastFailedAt, TotalFailures: p.TotalFailures,
		ConsecutiveFailures: p.ConsecutiveFailures, EvidenceOverflow: *p.RecoveryEvidenceOverflow, Pending: map[string]PendingFailure{}}
	for _, op := range p.RecoveredOperations {
		if !member(requestOperations, op) {
			return RecoveryLoad{}, ErrInvalidRecoveryState
		}
		if !containsString(e.RecoveredOperations, op) {
			e.RecoveredOperations = append(e.RecoveredOperations, op)
		}
	}
	if p.LastSucceededAt != nil {
		t, err := time.Parse(time.RFC3339Nano, *p.LastSucceededAt)
		if err != nil {
			return RecoveryLoad{}, ErrInvalidRecoveryState
		}
		e.LastSucceededAt = isoTime(t.UnixMilli())
	}
	for _, item := range p.Unresolved {
		key, f, err := pendingFromPersisted(item)
		if err != nil {
			return RecoveryLoad{}, err
		}
		if _, dup := e.Pending[key]; dup {
			return RecoveryLoad{}, ErrInvalidRecoveryState
		}
		e.Pending[key] = f
		e.order = append(e.order, key)
	}
	if e.FirstFailedAt > e.LastFailedAt || e.TotalFailures < 1 || len(e.Pending) == 0 && !e.EvidenceOverflow {
		return RecoveryLoad{}, ErrInvalidRecoveryState
	}
	age := max(0, now-e.LastFailedAt)
	if age > NotificationRecoveryExpiryMs {
		return RecoveryLoad{Kind: "expired", AgeMs: age, UnresolvedFailures: len(e.Pending)}, nil
	}
	return RecoveryLoad{Kind: "restored", Evidence: e}, nil
}

func (s *NotificationRecoveryStore) Save(e *RecoveryEvidence) error {
	overflow := e.EvidenceOverflow
	p := persistedRecovery{Version: NotificationRecoveryStateVersion, FirstFailedAt: e.FirstFailedAt, LastFailedAt: e.LastFailedAt,
		TotalFailures: e.TotalFailures, ConsecutiveFailures: e.ConsecutiveFailures, RecoveryEvidenceOverflow: &overflow,
		RecoveredOperations: nonNil(e.RecoveredOperations), Unresolved: []persistedFailure{}}
	if e.LastSucceededAt != "" {
		p.LastSucceededAt = &e.LastSucceededAt
	}
	for _, key := range e.keys() {
		f := e.Pending[key]
		hash, err := persistedKey(key, f)
		if err != nil {
			return err
		}
		p.Unresolved = append(p.Unresolved, persistedFailure{IdentityHash: hash, PendingFailure: f})
	}
	return writePrivateJSON(s.Path, p)
}

func (s *NotificationRecoveryStore) Clear() error {
	// A directory in its place is not evidence to remove: clearing fails.
	if st, err := os.Lstat(s.Path); err == nil && st.IsDir() {
		return &fs.PathError{Op: "remove", Path: "notification recovery state", Err: syscall.EISDIR}
	}
	if err := os.Remove(s.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// keys lists Pending in insertion order.
func (e *RecoveryEvidence) keys() []string {
	var out []string
	seen := map[string]bool{}
	for _, k := range e.order {
		if _, ok := e.Pending[k]; ok && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range e.Pending {
		if !seen[k] {
			out = append(out, k)
		}
	}
	return out
}

func (e *RecoveryEvidence) set(key string, f PendingFailure) {
	if _, ok := e.Pending[key]; !ok {
		e.order = append(e.order, key)
	}
	e.Pending[key] = f
}
