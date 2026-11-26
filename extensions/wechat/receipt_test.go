package wechat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/TaoSama/collie/internal/extproto"
)

const receiptPrivateValue = "private-token-context-chat-body"

// captureLog sends trace and diagnostic lines to a buffer for one test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	traceSinkMu.Lock()
	previous := traceSink
	traceSink = &b
	traceSinkMu.Unlock()
	t.Cleanup(func() {
		traceSinkMu.Lock()
		traceSink = previous
		traceSinkMu.Unlock()
	})
	return &b
}

func logEvents(b *bytes.Buffer, event string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(b.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["event"] == event {
			out = append(out, m)
		}
	}
	return out
}

// from: gateway/tests/wechat/send-receipt.test.ts: send receipt rejects ${JSON.stringify(response)} without exposing response
func TestSendReceiptRejectsWithoutExposingResponse(t *testing.T) {
	cases := []struct {
		response  string
		errorType string
	}{
		{`{"ret":null}`, "ILinkSendInvalidRet"},
		{`{"ret":"0"}`, "ILinkSendInvalidRet"},
		{`{"ret":false}`, "ILinkSendInvalidRet"},
		{`{"ret":{}}`, "ILinkSendInvalidRet"},
		{`{"ret":"` + receiptPrivateValue + `"}`, "ILinkSendInvalidRet"},
		{`{"ret":1.5}`, "ILinkSendInvalidRet"},
		{`{"ret":-14,"errmsg":"` + receiptPrivateValue + `"}`, "ILinkSendRejectedRet"},
		{`{"errcode":1}`, "ILinkSendRejectedErrcode"},
		{`{"ret":0,"errcode":null}`, "ILinkSendInvalidErrcode"},
		{`{"ret":0,"errcode":"0"}`, "ILinkSendInvalidErrcode"},
		{`{"ret":0,"errcode":1}`, "ILinkSendRejectedErrcode"},
		{`{"ret":1,"errcode":0}`, "ILinkSendRejectedRet"},
		{`{"base_resp":{"ret":0}}`, "ILinkSendUnsupportedShape"},
		{`{"ret":0,"base_resp":{"ret":-14}}`, "ILinkSendUnsupportedShape"},
		{`{"error":"` + receiptPrivateValue + `"}`, "ILinkSendUnsupportedShape"},
		{`{"` + receiptPrivateValue + `":0}`, "ILinkSendUnsupportedShape"},
		{`{"message_id":{}}`, "ILinkSendInvalidMessageId"},
		{`{"message_id":""}`, "ILinkSendInvalidMessageId"},
		{`{"message_id":" \t"}`, "ILinkSendInvalidMessageId"},
		{`{"message_id":-1}`, "ILinkSendInvalidMessageId"},
		{`{"message_id":1.5}`, "ILinkSendInvalidMessageId"},
		{`{"errmsg":null}`, "ILinkSendInvalidErrorMessage"},
		{`{"errmsg":"` + receiptPrivateValue + `"}`, "ILinkSendUnconfirmedErrorMessage"},
		{`[]`, "ILinkInvalidResponse"},
		{`null`, "ILinkInvalidResponse"},
		{`"` + receiptPrivateValue + `"`, "ILinkInvalidResponse"},
	}
	for _, c := range cases {
		t.Run(c.response, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			h.setHooks(func(k *harnessHooks) {
				k.send = func(map[string]any) (*http.Response, error) { return rawResponse(200, c.response), nil }
			})
			err := h.ilink.SendSingleText(context.Background(), testChat, "ordinary", "")
			if err == nil || SanitizeErrorType(err) != c.errorType || strings.Contains(err.Error(), receiptPrivateValue) {
				t.Fatalf("SendSingleText error = %v (%s), want %s without the response", err, SanitizeErrorType(err), c.errorType)
			}
			log := captureLog(t)
			h.pushResult("result-1", "结果")
			summary, err := h.deliverNotificationsOnce()
			mustNoErr(t, err)
			// An explicit refusal is not an unknown send: the merged queue defers it
			// until the next message instead of marking it uncertain.
			rejected := strings.Contains(c.errorType, "Rejected")
			events := h.host.events()
			if rejected {
				if summary.Skipped != 1 || len(events) != 1 || events[0].State != extproto.DeliveryDeferred || events[0].Reason != "rejected" {
					t.Fatalf("summary = %+v, events = %+v, want one deferred as rejected", summary, events)
				}
			} else {
				if summary.Uncertain != 1 || len(events) != 1 || events[0].State != extproto.DeliveryUncertain || events[0].Ref != "result-1" {
					t.Fatalf("summary = %+v, events = %+v, want one uncertain", summary, events)
				}
				if failed := logEvents(log, "notification_send_failed"); len(failed) != 1 {
					t.Fatalf("diagnostics = %q", log.String())
				}
			}
			if strings.Contains(log.String(), receiptPrivateValue) {
				t.Fatalf("diagnostics expose the response: %q", log.String())
			}
			_, err = h.deliverNotificationsOnce()
			mustNoErr(t, err)
			if n := len(h.sent()); n != 2 {
				t.Fatalf("%d sends, want one ordinary send and one result, never a replay", n)
			}
		})
	}
}

// from: gateway/tests/wechat/send-receipt.test.ts: ordinary and notification replies use the same success contract: ${JSON.stringify(response)}
func TestSendReceiptSameSuccessContract(t *testing.T) {
	for _, response := range []string{`{}`, `{"errmsg":""}`, `{"errmsg":" \t"}`, `{"message_id":"123"}`,
		`{"message_id":"18446744073709551615"}`, `{"message_id":0}`, `{"message_id":123}`, `{"message_id":9007199254740991}`,
		`{"ret":0}`, `{"errcode":0}`, `{"ret":0,"errcode":0,"errmsg":"ok","message_id":"123"}`} {
		t.Run(response, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			h.setHooks(func(k *harnessHooks) {
				k.send = func(map[string]any) (*http.Response, error) { return rawResponse(200, response), nil }
			})
			mustNoErr(t, h.ilink.SendSingleText(context.Background(), testChat, "ordinary", ""))
			h.pushResult("result-1", "结果")
			summary, err := h.deliverNotificationsOnce()
			mustNoErr(t, err)
			if summary.Accepted != 1 {
				t.Fatalf("summary = %+v, want one accepted", summary)
			}
			_, err = h.deliverNotificationsOnce()
			mustNoErr(t, err)
			if n := len(h.sent()); n != 2 {
				t.Fatalf("%d sends, want 2", n)
			}
		})
	}
}

// from: gateway/tests/wechat/send-receipt.test.ts: valid JSON objects may omit a return code and 64-bit string IDs remain lossless
func TestSendReceiptOmittedCodeAndLosslessIDs(t *testing.T) {
	mustNoErr(t, ValidateSendReceipt(map[string]any{}))
	mustNoErr(t, ValidateSendReceipt(map[string]any{"errmsg": ""}))
	response := map[string]any{"message_id": "18446744073709551615"}
	mustNoErr(t, ValidateSendReceipt(response))
	if response["message_id"] != "18446744073709551615" {
		t.Fatalf("message_id changed: %v", response["message_id"])
	}
}

// from: gateway/tests/wechat/send-receipt.test.ts: numeric message IDs require a nonnegative safe integer even with an explicit success code
func TestSendReceiptNumericMessageIDs(t *testing.T) {
	for _, id := range []string{"0", "123", "9007199254740991"} {
		mustNoErr(t, ValidateSendReceipt(map[string]any{"message_id": json.Number(id)}))
	}
	for _, id := range []string{"9007199254740992", "1.5", "-1", "1e400", "-1e400"} {
		for _, response := range []map[string]any{{"message_id": json.Number(id)}, {"ret": json.Number("0"), "message_id": json.Number(id)}} {
			var receipt *SendReceiptError
			if err := ValidateSendReceipt(response); !errors.As(err, &receipt) || receipt.Reason != ReceiptInvalidMessageID {
				t.Fatalf("message_id %s: error %v, want InvalidMessageId", id, err)
			}
		}
	}
}

// from: gateway/tests/wechat/send-receipt.test.ts: wire parser preserves 64-bit identifiers and never rewrites numeric-looking message bodies
func TestParseILinkJSONPreservesIdentifiers(t *testing.T) {
	raw := `{"message_id":18446744073709551615,"msg_id":9223372036854775807,"svr_id":9007199254740993,"text":"message_id:18446744073709551615 🚀","count":123}`
	parsed, err := ParseILinkJSON(raw)
	mustNoErr(t, err)
	m := parsed.(map[string]any)
	want := map[string]any{"message_id": "18446744073709551615", "msg_id": "9223372036854775807", "svr_id": "9007199254740993",
		"text": "message_id:18446744073709551615 🚀", "count": json.Number("123")}
	if fmt.Sprint(m) != fmt.Sprint(want) {
		t.Fatalf("parsed = %v, want %v", m, want)
	}
	mustNoErr(t, ValidateSendReceipt(map[string]any{"message_id": m["message_id"]}))
	escaped, err := ParseILinkJSON(`{"message_id":18446744073709551615}`)
	mustNoErr(t, err)
	if escaped.(map[string]any)["message_id"] != "18446744073709551615" {
		t.Fatalf("escaped key: %v", escaped)
	}
	quoted, err := ParseILinkJSON(`{"message_id":"18446744073709551615","text":"\"message_id\":18446744073709551615"}`)
	mustNoErr(t, err)
	if q := quoted.(map[string]any); q["message_id"] != "18446744073709551615" || q["text"] != `"message_id":18446744073709551615` {
		t.Fatalf("quoted: %v", q)
	}
}

// from: gateway/tests/wechat/send-receipt.test.ts: pre-existing uncertain notification is unchanged when the provider now returns an accepted shape
func TestSendReceiptUncertainResultUnchanged(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.pushResult("result-1", "结果")
	var id string
	h.do(func() {
		for pid, p := range h.gateway.results.plans {
			id = pid
			p.Status, p.AttemptID, p.AttemptedAt, p.AttemptExpiresAt = ResultUncertain, newUUID(), nowMs(), nowMs()+60000
		}
		mustNoErr(t, h.gateway.results.save())
	})
	var before ResultDelivery
	h.do(func() { before = h.gateway.results.plans[id].ResultDelivery })
	_, err := h.deliverNotificationsOnce()
	mustNoErr(t, err)
	var after ResultDelivery
	h.do(func() { after = h.gateway.results.plans[id].ResultDelivery })
	if jsonText(before) != jsonText(after) || len(h.sent()) != 0 {
		t.Fatalf("uncertain result changed or was sent: %d sends", len(h.sent()))
	}
}

// from: gateway/tests/wechat/send-receipt.test.ts: HTTP ${status} is attributed to the service that returned it
func TestSendReceiptHTTPStatusAttribution(t *testing.T) {
	for _, status := range []int{401, 403, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s := newTestState(t, t.TempDir(), PolicyOptions{})
			openWindow(t, s, testChat)
			c := newTestClient(t, s, &loop{}, fakeHTTP(func(*http.Request, []byte) (*http.Response, error) {
				return rawResponse(status, receiptPrivateValue), nil
			}))
			err := c.SendSingleText(context.Background(), testChat, "ordinary", "")
			if got := SanitizeErrorType(err); got != fmt.Sprintf("ILinkHttp%d", status) {
				t.Fatalf("type = %s", got)
			}
			if got := SanitizeErrorType(fmt.Errorf("Unrelated HTTP %d", status)); got != fmt.Sprintf("Http%d", status) {
				t.Fatalf("unrelated type = %s", got)
			}
		})
	}
}
