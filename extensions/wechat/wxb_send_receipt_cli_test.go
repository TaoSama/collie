package wechat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

var (
	wxbUUID       = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
	wxbStage      = regexp.MustCompile(`^[a-z][a-z0-9_.]*$`)
	wxbTraceID    = regexp.MustCompile(`^[a-f0-9]{64}$`)
	wxbBaseFields = map[string]bool{"schema": true, "event": true, "service": true, "instanceId": true, "traceId": true,
		"attemptId": true, "stage": true, "timestamp": true, "elapsedMs": true}
)

// wxbParseTelemetry validates a message_latency line; nil for other lines.
func wxbParseTelemetry(line string) (map[string]any, error) {
	if !strings.HasPrefix(line, "{") {
		return nil, nil
	}
	var e map[string]any
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return nil, err
	}
	if e["event"] != "message_latency" {
		return nil, nil
	}
	str := func(k string) string { s, _ := e[k].(string); return s }
	num := func(k string) (float64, bool) { f, ok := e[k].(float64); return f, ok }
	if e["schema"] != float64(1) || e["service"] != "gateway" || !wxbUUID.MatchString(str("instanceId")) || !wxbUUID.MatchString(str("attemptId")) ||
		!wxbTraceID.MatchString(str("traceId")) || !wxbStage.MatchString(str("stage")) {
		return nil, errors.New("bad telemetry identity")
	}
	if ts, err := time.Parse(time.RFC3339Nano, str("timestamp")); err != nil || isoTime(ts.UnixMilli()) != str("timestamp") {
		return nil, errors.New("bad timestamp")
	}
	if f, ok := num("elapsedMs"); !ok || f < 0 {
		return nil, errors.New("bad elapsedMs")
	}
	if _, ok := e["durationMs"]; ok {
		if f, ok := num("durationMs"); !ok || f < 0 {
			return nil, errors.New("bad durationMs")
		}
	}
	if _, ok := e["spanId"]; ok && !wxbUUID.MatchString(str("spanId")) {
		return nil, errors.New("bad spanId")
	}
	for k, v := range e {
		if !wxbBaseFields[k] && !traceFieldNames[k] {
			return nil, fmt.Errorf("unexpected telemetry field %s", k)
		}
		switch x := v.(type) {
		case nil, bool, float64:
		case string:
			if utf16Len(x) > maxTraceStringChars {
				return nil, fmt.Errorf("long field %s", k)
			}
		default:
			return nil, fmt.Errorf("field %s is not a scalar", k)
		}
	}
	return e, nil
}

// wxbDiagnosticLines are the log lines that are not telemetry.
func wxbDiagnosticLines(log string) ([]string, error) {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if line == "" {
			continue
		}
		e, err := wxbParseTelemetry(line)
		if err != nil {
			return nil, err
		}
		if e == nil {
			out = append(out, line)
		}
	}
	return out, nil
}

// from: gateway/tests/wechat/send-receipt-cli.test.ts: CLI diagnostics exclude only validated telemetry and retain unrelated JSON and plain errors
func TestWxbDiagnosticsExcludeOnlyValidTelemetry(t *testing.T) {
	// The Go trace lines of a real send must pass the same contract.
	log := captureLog(t)
	h := newHarness(t, harnessOptions{})
	_, _, err := func() (int, string, error) {
		var n int
		var id string
		var err error
		h.do(func() {
			n, id, err = h.ilink.sendMarkdownReply(withTrace(context.Background(), createMessageTrace(testChat, "m")), testChat, "body", ReplyOptions{})
		})
		return n, id, err
	}()
	mustNoErr(t, err)
	time.Sleep(20 * time.Millisecond)
	lines, err := wxbDiagnosticLines(log.String())
	mustNoErr(t, err)
	if len(lines) != 0 || !strings.Contains(log.String(), "message_latency") {
		t.Fatalf("send traces not valid telemetry: %q", lines)
	}
	event := map[string]any{"schema": 1, "event": "message_latency", "service": "gateway", "instanceId": "00000000-0000-4000-8000-000000000001",
		"traceId": strings.Repeat("a", 64), "attemptId": "00000000-0000-4000-8000-000000000002", "stage": "message.received",
		"timestamp": "2026-10-21T00:00:00.000Z", "elapsedMs": 0}
	warning := `{"event":"notification_send_failed","errorType":"UncertainSend"}`
	plain := "[gateway] poll cycle error: ILinkHttp401"
	got, err := wxbDiagnosticLines(jsonText(event) + "\n" + warning + "\n" + plain)
	mustNoErr(t, err)
	if strings.Join(got, "|") != warning+"|"+plain {
		t.Fatalf("lines %q", got)
	}
	for _, invalid := range []map[string]any{{"schema": 2}, {"service": "controller"}, {"traceId": "invalid"}, {"elapsedMs": -1},
		{"durationMs": -1}, {"spanId": "invalid"}, {"unexpected": "private"}} {
		e := map[string]any{}
		for k, v := range event {
			e[k] = v
		}
		for k, v := range invalid {
			e[k] = v
		}
		if _, err := wxbDiagnosticLines(jsonText(e)); err == nil {
			t.Fatalf("%v accepted", invalid)
		}
	}
	if _, err := wxbDiagnosticLines(`{"event":"message_latency"`); err == nil {
		t.Fatal("truncated telemetry accepted")
	}
}

// from: gateway/tests/wechat/send-receipt-cli.test.ts: real CLI accepts ${JSON.stringify(sendBody)} and persists inbound/result dedup across restart
func TestWxbAcceptedSendsDedupAcrossRestart(t *testing.T) {
	// In process instead of the Node CLI: poll, hand over, answer, deliver a
	// result, restart on the same data and poll the same message again.
	for _, body := range []string{`{}`, `{"errmsg":""}`, `{"ret":0}`, `{"message_id":"18446744073709551615"}`} {
		t.Run(body, func(t *testing.T) {
			log := captureLog(t)
			h := newHarness(t, harnessOptions{noWindow: true})
			msg := inbound(testChat, "cli-inbound", "synthetic inbound text", nowMs()-1000)
			var mu sync.Mutex
			polls := 0
			h.setHooks(func(k *harnessHooks) {
				k.send = func(map[string]any) (*http.Response, error) { return rawResponse(200, body), nil }
				k.poll = func() (*http.Response, error) {
					mu.Lock()
					polls++
					mu.Unlock()
					// The server answers the same message on every poll.
					return jsonResponse(200, map[string]any{"ret": 0, "msgs": []any{msg}, "get_updates_buf": "cursor"}), nil
				}
			})
			h.pushResult("result-1", "synthetic result text")
			var err error
			h.do(func() { _, err = h.gateway.pollOnce(context.Background(), true) })
			mustNoErr(t, err)
			received := h.host.messages()
			if len(received) != 1 {
				t.Fatalf("received %d", len(received))
			}
			_, err = h.send(extproto.Send{Ref: "answer-1", Text: "synthetic reply text", Kind: extproto.KindAnswer, ReplyTo: received[0].ID})
			mustNoErr(t, err)
			s, err := h.deliverNotificationsOnce()
			mustNoErr(t, err)
			if s.Accepted != 1 || len(h.sent()) != 2 {
				t.Fatalf("summary %+v, %d sends", s, len(h.sent()))
			}
			h.gateway.Stop()
			reloaded, err := NewStateManager(testAccountFor(testToken), filepath.Join(h.dir, stateFileName), PolicyOptions{})
			mustNoErr(t, err)
			if !reloaded.IsMessageProcessed(reloaded.ComputeDeliveryKey(testChat, "cli-inbound")) {
				t.Fatal("inbound dedup not persisted")
			}
			l := &loop{}
			c := newTestClient(t, reloaded, l, h.http)
			var g *Gateway
			l.do(func() {
				g, err = NewGateway(GatewayOptions{State: reloaded, ILink: c, Host: h.host, DataDir: h.dir, MediaHTTP: h.http, Health: &HealthTracker{}})
			})
			mustNoErr(t, err)
			defer g.Stop()
			for i := 0; i < 3; i++ {
				l.do(func() {
					if _, err = g.pollOnce(context.Background(), true); err == nil {
						_, err = g.deliverNotificationsOnce(context.Background())
					}
				})
				mustNoErr(t, err)
			}
			if len(h.host.messages()) != 1 || len(h.sent()) != 2 {
				t.Fatalf("after restart: %d received, %d sends", len(h.host.messages()), len(h.sent()))
			}
			for _, private := range []string{testToken, testContext, "synthetic inbound text", "synthetic reply text", "synthetic result text"} {
				if strings.Contains(log.String(), private) {
					t.Fatalf("log leaked %q", private)
				}
			}
		})
	}
}

// from: gateway/tests/wechat/send-receipt-cli.test.ts: real CLI records ${failure.errorType} as ${rejected ? "waiting for user" : "permanent uncertainty"} without diagnostic leaks
func TestWxbFailedResultSendRecorded(t *testing.T) {
	const upstream = "synthetic private upstream errmsg"
	for _, c := range []struct {
		body, errorType string
		status          int
	}{
		{`{"message_id":1.5}`, "ILinkSendInvalidMessageId", 200},
		{`{"ret":7,"errmsg":"` + upstream + `"}`, "ILinkSendRejectedRet", 200},
		{`{"errcode":-1,"errmsg":"` + upstream + `"}`, "ILinkSendRejectedErrcode", 200},
		{`{"ret":"0"}`, "ILinkSendInvalidRet", 200},
		{`{"errcode":null}`, "ILinkSendInvalidErrcode", 200},
		{`{"base_resp":{"ret":0}}`, "ILinkSendUnsupportedShape", 200},
		{`{"errmsg":"` + upstream + `"}`, "ILinkHttp401", 401},
		{`{"errmsg":"` + upstream + `"}`, "ILinkHttp403", 403},
	} {
		t.Run(c.errorType, func(t *testing.T) {
			rejected := strings.Contains(c.errorType, "Rejected")
			log := captureLog(t)
			h := newHarness(t, harnessOptions{})
			h.setHooks(func(k *harnessHooks) {
				k.send = func(map[string]any) (*http.Response, error) { return rawResponse(c.status, c.body), nil }
			})
			h.pushResult("result-1", "synthetic private notification text")
			_, err := h.deliverNotificationsOnce()
			mustNoErr(t, err)
			var plan ResultDelivery
			var evidence ReplyDeliveryState
			h.do(func() {
				for _, p := range h.gateway.results.plans {
					plan = p.ResultDelivery
				}
				evidence = h.state.ReplyDeliveries["result:"+plan.ID+":step:0"]
			})
			want := ResultUncertain
			if rejected {
				want = ResultWaitingForUser
			}
			if plan.Status != want || evidence.AcknowledgedChunks != 0 || evidence.InFlight == rejected {
				t.Fatalf("status %s evidence %+v", plan.Status, evidence)
			}
			failed := logEvents(log, "notification_send_failed")
			if rejected {
				if len(failed) != 0 || plan.LastDeferReason != "rejected" {
					t.Fatalf("rejected: diagnostics %v, defer %q", failed, plan.LastDeferReason)
				}
			} else if len(failed) != 1 || failed[0]["notificationId"] != plan.ID || failed[0]["attemptId"] != plan.AttemptID ||
				// The merged queue may name the send's category (as the notification outbox did) or UncertainSend.
				failed[0]["errorType"] != "UncertainSend" && failed[0]["errorType"] != c.errorType {
				t.Fatalf("diagnostics %v", failed)
			}
			for i := 0; i < 3; i++ {
				_, err = h.deliverNotificationsOnce()
				mustNoErr(t, err)
			}
			if len(h.sent()) != 1 || strings.Contains(log.String(), upstream) || strings.Contains(log.String(), "synthetic private notification text") {
				t.Fatalf("%d sends or diagnostic leak", len(h.sent()))
			}
		})
	}
}
