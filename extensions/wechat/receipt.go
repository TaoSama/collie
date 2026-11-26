package wechat

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"
)

// SendReceiptError is a sendmessage answer that does not prove acceptance.
// Its text never carries response values: codes and messages are untrusted.
type SendReceiptError struct{ Reason string }

func (e *SendReceiptError) Error() string { return "WeChat sendmessage failed: " + e.Reason }

// Receipt failure reasons.
const (
	ReceiptInvalidRet              = "InvalidRet"
	ReceiptRejectedRet             = "RejectedRet"
	ReceiptInvalidErrcode          = "InvalidErrcode"
	ReceiptRejectedErrcode         = "RejectedErrcode"
	ReceiptUnsupportedShape        = "UnsupportedShape"
	ReceiptInvalidMessageID        = "InvalidMessageId"
	ReceiptInvalidErrorMessage     = "InvalidErrorMessage"
	ReceiptUnconfirmedErrorMessage = "UnconfirmedErrorMessage"
	ReceiptEmptyResponse           = "EmptyResponse"
)

var (
	sendResponseFields = map[string]bool{"ret": true, "errcode": true, "message_id": true, "errmsg": true}
	losslessIDFields   = map[string]bool{"message_id": true, "msg_id": true, "svr_id": true}
	decimalID          = regexp.MustCompile(`^(?:0|[1-9][0-9]*)$`)
)

// errNotJSON is the parse failure; like JSON.parse's it says nothing of the text.
var errNotJSON = &json.SyntaxError{}

// ParseILinkJSON decodes an iLink answer. Numbers stay json.Number, and
// uint64 identifiers become their exact decimal text before any precision is
// lost.
func ParseILinkJSON(raw string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, errNotJSON
	}
	var extra any
	if dec.Decode(&extra) == nil {
		return nil, errNotJSON
	}
	return reviveIDs("", v)
}

func reviveIDs(key string, v any) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		for k, item := range x {
			r, err := reviveIDs(k, item)
			if err != nil {
				return nil, err
			}
			x[k] = r
		}
	case []any:
		for i, item := range x {
			r, err := reviveIDs("", item)
			if err != nil {
				return nil, err
			}
			x[i] = r
		}
	case json.Number:
		if losslessIDFields[key] {
			if decimalID.MatchString(string(x)) {
				return string(x), nil
			}
			if _, ok := safeInt(x); !ok {
				return nil, &SendReceiptError{ReceiptInvalidMessageID}
			}
		}
	}
	return v, nil
}

// safeInt is Number.isSafeInteger for a JSON number.
func safeInt(v any) (int64, bool) {
	var f float64
	switch n := v.(type) {
	case json.Number:
		parsed, err := n.Float64()
		if err != nil {
			return 0, false
		}
		f = parsed
	case float64:
		f = n
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || math.Abs(f) > maxSafeInteger {
		return 0, false
	}
	return int64(f), true
}

// isJSONNumber is typeof value === "number".
func isJSONNumber(v any) bool {
	switch v.(type) {
	case json.Number, float64, int, int64:
		return true
	}
	return false
}

// IsRejectedSendReceipt reports whether WeChat explicitly refused a message,
// as opposed to an ambiguous or malformed receipt.
func IsRejectedSendReceipt(err error) bool {
	var e *SendReceiptError
	return errors.As(err, &e) && (e.Reason == ReceiptRejectedRet || e.Reason == ReceiptRejectedErrcode)
}

// ValidateSendReceipt accepts an HTTP-success JSON object, which may omit its
// zero code. Acceptance is not delivery proof.
func ValidateSendReceipt(response map[string]any) error {
	for _, field := range []string{"ret", "errcode"} {
		value, ok := response[field]
		if !ok {
			continue
		}
		n, safe := safeInt(value)
		if !isJSONNumber(value) || !safe {
			if field == "ret" {
				return &SendReceiptError{ReceiptInvalidRet}
			}
			return &SendReceiptError{ReceiptInvalidErrcode}
		}
		if n != 0 {
			if field == "ret" {
				return &SendReceiptError{ReceiptRejectedRet}
			}
			return &SendReceiptError{ReceiptRejectedErrcode}
		}
	}
	// Unknown envelopes are not evidence of success and must not hide nested errors.
	for field := range response {
		if !sendResponseFields[field] {
			return &SendReceiptError{ReceiptUnsupportedShape}
		}
	}
	if id, ok := response["message_id"]; ok {
		valid := false
		switch x := id.(type) {
		case string:
			valid = strings.TrimSpace(x) != ""
		default:
			n, safe := safeInt(x)
			valid = isJSONNumber(x) && safe && n >= 0
		}
		if !valid {
			return &SendReceiptError{ReceiptInvalidMessageID}
		}
	}
	if msg, ok := response["errmsg"]; ok {
		s, isString := msg.(string)
		if !isString {
			return &SendReceiptError{ReceiptInvalidErrorMessage}
		}
		if strings.TrimSpace(s) != "" && !isZero(response["ret"]) && !isZero(response["errcode"]) {
			return &SendReceiptError{ReceiptUnconfirmedErrorMessage}
		}
	}
	return nil
}

func isZero(v any) bool {
	n, ok := safeInt(v)
	return ok && isJSONNumber(v) && n == 0
}

// receiptMessageID is the id a valid receipt names, as text.
func receiptMessageID(response map[string]any) string {
	switch x := response["message_id"].(type) {
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return x.String()
	}
	return ""
}
