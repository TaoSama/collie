package wechat

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"syscall"
	"time"
)

// Request operations named in diagnostics.
var requestOperations = []string{"notification_interest", "result_pending", "result_get", "result_claim",
	"result_materialize", "result_receipt", "result_defer", "result_resume", "result_blocked", "artifact_download",
	"notification_pending", "notification_get", "notification_claim", "notification_receipt", "notification_defer",
	"reply_get", "image_download", "wechat_sendmessage"}

// Stages of one notification cycle.
const (
	StageInterest             = "interest"
	StageRenewal              = "renewal"
	StageReplyRecovery        = "reply_recovery"
	StageResultDelivery       = "result_delivery"
	StageNotificationDelivery = "notification_delivery"
	StageUnknown              = "unknown"
)

var (
	notificationStages = []string{StageInterest, StageRenewal, StageReplyRecovery, StageResultDelivery, StageNotificationDelivery}
	requestPhases      = []string{"fetch", "headers", "body", "decode", "validate"}
	safeCauseCodes     = map[string]bool{
		"ECONNRESET": true, "ECONNREFUSED": true, "ETIMEDOUT": true, "ENOTFOUND": true, "EAI_AGAIN": true, "EHOSTUNREACH": true,
		"ENETUNREACH": true, "EPIPE": true, "CERT_HAS_EXPIRED": true, "CERT_NOT_YET_VALID": true, "DEPTH_ZERO_SELF_SIGNED_CERT": true,
		"SELF_SIGNED_CERT_IN_CHAIN": true, "UNABLE_TO_VERIFY_LEAF_SIGNATURE": true, "UNABLE_TO_GET_ISSUER_CERT_LOCALLY": true,
		"ERR_TLS_CERT_ALTNAME_INVALID": true}
	safeErrorNames = map[string]bool{"Error": true, "TypeError": true, "SyntaxError": true, "RangeError": true,
		"AbortError": true, "TimeoutError": true, "AggregateError": true}
)

const (
	otherCode     = "OtherCode"
	otherError    = "OtherError"
	maxCauseNodes = 8
)

// RequestFailure is what is known about a failed request: never its body,
// URL or error text.
type RequestFailure struct {
	Operation          string `json:"operation"`
	Phase              string `json:"phase"`
	RequestAttempts    int    `json:"requestAttempts"`
	RequestMaxAttempts int    `json:"requestMaxAttempts"`
	ElapsedMs          int64  `json:"elapsedMs"`
	TimedOut           bool   `json:"timedOut"`
	ParentAborted      bool   `json:"parentAborted"`
	HTTPStatus         int    `json:"httpStatus,omitempty"`
}

// diagnosedError carries a request's facts beside the original error, which
// errors.As still finds.
type diagnosedError struct {
	err     error
	key     string
	request *RequestFailure
	stage   string
}

func (e *diagnosedError) Error() string { return e.err.Error() }
func (e *diagnosedError) Unwrap() error { return e.err }

// errorName is the JavaScript-style category of err.
func errorName(err error) string {
	var syntax *json.SyntaxError
	var netErr net.Error
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "TimeoutError"
	case errors.Is(err, context.Canceled), errors.Is(err, errAbort):
		return "AbortError"
	case errors.As(err, &syntax):
		return "SyntaxError"
	case errors.As(err, &dnsErr), errors.As(err, &netErr):
		return "TypeError"
	}
	return "Error"
}

// causeCodes lists the allowlisted system codes found in err's chain.
func causeCodes(err error) []string {
	var codes []string
	add := func(c string) {
		if !safeCauseCodes[c] {
			c = otherCode
		}
		if !containsString(codes, c) {
			codes = append(codes, c)
		}
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ECONNRESET:
			add("ECONNRESET")
		case syscall.ECONNREFUSED:
			add("ECONNREFUSED")
		case syscall.ETIMEDOUT:
			add("ETIMEDOUT")
		case syscall.EHOSTUNREACH:
			add("EHOSTUNREACH")
		case syscall.ENETUNREACH:
			add("ENETUNREACH")
		case syscall.EPIPE:
			add("EPIPE")
		default:
			add(otherCode)
		}
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsTemporary {
			add("EAI_AGAIN")
		} else {
			add("ENOTFOUND")
		}
	}
	var expired x509.CertificateInvalidError
	var unknown x509.UnknownAuthorityError
	var host x509.HostnameError
	switch {
	case errors.As(err, &expired) && expired.Reason == x509.Expired:
		add("CERT_HAS_EXPIRED")
	case errors.As(err, &unknown):
		add("UNABLE_TO_GET_ISSUER_CERT_LOCALLY")
	case errors.As(err, &host):
		add("ERR_TLS_CERT_ALTNAME_INVALID")
	}
	if len(codes) > maxCauseNodes {
		codes = codes[:maxCauseNodes]
	}
	return codes
}

// safeErrorFacts never serializes messages, stacks, hostnames or arbitrary
// codes.
func safeErrorFacts(err error) (string, []string) {
	name := errorName(err)
	if !safeErrorNames[name] {
		name = otherError
	}
	codes := causeCodes(err)
	if codes == nil {
		codes = []string{}
	}
	return name, codes
}

func isSafeCauseCode(c string) bool     { return c == otherCode || safeCauseCodes[c] }
func isSafeErrorName(n string) bool     { return n == otherError || safeErrorNames[n] }
func requestKey(op, hash string) string { return jsonText([]string{op, hash}) }

// notificationFailureFacts is one failure as diagnostics keep it.
type notificationFailureFacts struct {
	Stage      string
	Key        string
	Request    *RequestFailure
	ErrorName  string
	CauseCodes []string
}

// requestObservation collects one notification cycle's request outcomes.
type requestObservation struct {
	mu        sync.Mutex
	successes map[string]bool
	failures  map[string]notificationFailureFacts
	order     []string
}

type observationKey struct{}

func newObservation() *requestObservation {
	return &requestObservation{successes: map[string]bool{}, failures: map[string]notificationFailureFacts{}}
}

func observeRequests(ctx context.Context, o *requestObservation) context.Context {
	return context.WithValue(ctx, observationKey{}, o)
}

func observation(ctx context.Context) *requestObservation {
	o, _ := ctx.Value(observationKey{}).(*requestObservation)
	return o
}

func (o *requestObservation) setFailure(key string, f notificationFailureFacts) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.failures[key]; !ok {
		o.order = append(o.order, key)
	}
	o.failures[key] = f
}

func (o *requestObservation) list() []notificationFailureFacts {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []notificationFailureFacts
	for _, k := range o.order {
		if f, ok := o.failures[k]; ok {
			out = append(out, f)
		}
	}
	return out
}

// rememberNotificationStage tags err with the cycle stage it failed in.
func rememberNotificationStage(ctx context.Context, err error, stage string) error {
	if err == nil {
		return nil
	}
	d := &diagnosedError{err: err, stage: stage}
	var inner *diagnosedError
	if errors.As(err, &inner) {
		d.key, d.request = inner.key, inner.request
	}
	if o := observation(ctx); o != nil {
		f := notificationFailure(d)
		key := f.Key
		if key == "" {
			key = stageKey(stage)
		}
		o.setFailure(key, f)
	}
	return d
}

func notificationFailure(err error) notificationFailureFacts {
	f := notificationFailureFacts{Stage: StageUnknown}
	var d *diagnosedError
	if errors.As(err, &d) {
		if d.stage != "" {
			f.Stage = d.stage
		}
		f.Key, f.Request = d.key, d.request
	}
	f.ErrorName, f.CauseCodes = safeErrorFacts(err)
	return f
}

// RequestDiagnostics follows one request's phases. Its key may hash a
// request identity but is never emitted.
type RequestDiagnostics struct {
	Phase      string
	HTTPStatus int
	operation  string
	started    time.Time
	key        string
}

func newRequestDiagnostics(operation, identity string) *RequestDiagnostics {
	if identity == "" {
		identity = operation
	}
	return &RequestDiagnostics{Phase: "fetch", operation: operation, started: time.Now(),
		key: requestKey(operation, sha256Hex(identity))}
}

func (d *RequestDiagnostics) success(ctx context.Context) {
	if o := observation(ctx); o != nil {
		o.mu.Lock()
		o.successes[d.key] = true
		delete(o.failures, d.key)
		o.mu.Unlock()
	}
}

// failure attaches the facts to err and returns it wrapped.
func (d *RequestDiagnostics) failure(ctx context.Context, err error, attempts, maxAttempts int, timedOut, parentAborted bool) error {
	if err == nil {
		return nil
	}
	req := &RequestFailure{Operation: d.operation, Phase: d.Phase, RequestAttempts: attempts, RequestMaxAttempts: maxAttempts,
		ElapsedMs: time.Since(d.started).Milliseconds(), TimedOut: timedOut, ParentAborted: parentAborted, HTTPStatus: d.HTTPStatus}
	wrapped := &diagnosedError{err: err, key: d.key, request: req}
	if o := observation(ctx); o != nil {
		o.mu.Lock()
		delete(o.successes, d.key)
		o.mu.Unlock()
		o.setFailure(d.key, notificationFailure(wrapped))
	}
	return wrapped
}
