package wechat

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gweGateway builds a gateway over state, its own loop and a test host.
func gweGateway(t *testing.T, s *StateManager, hc *http.Client, dir string) (*Gateway, *loop, *testHost) {
	t.Helper()
	l := &loop{}
	c := newTestClient(t, s, l, hc)
	host := &testHost{}
	var g *Gateway
	var err error
	l.do(func() {
		g, err = NewGateway(GatewayOptions{State: s, ILink: c, Host: host, DataDir: dir, MediaHTTP: hc, Health: &HealthTracker{}})
	})
	mustNoErr(t, err)
	t.Cleanup(g.Stop)
	return g, l, host
}

// from: gateway/tests/gateway/baseline.test.ts: WeChatGateway refuses pollOnce/run without baseline established
func TestGweRefusesPollWithoutBaseline(t *testing.T) {
	dir := t.TempDir()
	s := newTestState(t, dir, PolicyOptions{})
	g, l, _ := gweGateway(t, s, fakeHTTP(func(*http.Request, []byte) (*http.Response, error) {
		t.Fatal("no request without a baseline")
		return nil, nil
	}), dir)
	var err error
	l.do(func() { _, err = g.pollOnce(context.Background(), true) })
	if err == nil || !strings.Contains(err.Error(), "baseline is not established") {
		t.Fatalf("pollOnce error = %v", err)
	}
	if err := g.RunLoop(10, nil, nil); err == nil || !strings.Contains(err.Error(), "baseline is not established") {
		t.Fatalf("RunLoop error = %v", err)
	}
}

// from: gateway/tests/gateway/baseline.test.ts: WeChatGateway establishBaseline drains history to empty batch without forwarding or replying
func TestGweBaselineDrainsHistory(t *testing.T) {
	createdAt := nowMs() - 1000
	dir := t.TempDir()
	s := newTestState(t, dir, PolicyOptions{})
	polls, sends := 0, 0
	hc := fakeHTTP(func(req *http.Request, _ []byte) (*http.Response, error) {
		switch endpoint(req) {
		case "getupdates":
			polls++
			switch polls {
			case 1:
				return jsonResponse(200, map[string]any{"ret": 0, "errcode": 0, "get_updates_buf": "cursor-page-1", "msgs": []any{
					map[string]any{"message_type": 1, "create_time_ms": createdAt, "from_user_id": "u-history", "client_id": "hist-msg-1",
						"context_token": "ctx-token-1", "item_list": []any{map[string]any{"type": 1, "text_item": map[string]any{"text": "old question"}}}}}}), nil
			case 2:
				// A non-text event filters to nothing but the raw page is not empty: the drain goes on.
				return jsonResponse(200, map[string]any{"ret": 0, "errcode": 0, "get_updates_buf": "cursor-page-2",
					"msgs": []any{map[string]any{"message_type": 99, "from_user_id": "u-sys"}}}), nil
			}
			return jsonResponse(200, map[string]any{"ret": 0, "errcode": 0, "get_updates_buf": "cursor-empty-final", "msgs": []any{}}), nil
		case "sendmessage":
			sends++
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		}
		t.Fatalf("unexpected request %s", req.URL)
		return nil, nil
	})
	g, l, host := gweGateway(t, s, hc, dir)
	var summary BaselineSummary
	var err error
	l.do(func() { summary, err = g.establishBaseline(context.Background(), 5) })
	mustNoErr(t, err)
	if summary.BatchesDrained != 3 || summary.RawMessagesSkipped != 2 || summary.FinalSyncBuf != "cursor-empty-final" {
		t.Fatalf("summary = %+v", summary)
	}
	if s.SyncBuf != "cursor-empty-final" || !s.BaselineEstablished || s.BaselineInProgress || s.BaselineEstablishedAt == "" {
		t.Fatalf("state after baseline: %q %v %v %q", s.SyncBuf, s.BaselineEstablished, s.BaselineInProgress, s.BaselineEstablishedAt)
	}
	if len(host.messages()) != 0 || sends != 0 {
		t.Fatal("baseline must never hand messages over or reply")
	}
	if s.ContextTokens["u-history"] != "ctx-token-1" || s.CanSendToChat("u-history", SendFinal) || len(s.Outbox) != 0 {
		t.Fatal("baseline history keeps the token but grants no budget and no outbox")
	}
	if !s.IsMessageProcessed(s.ComputeDeliveryKey("u-history", "hist-msg-1")) {
		t.Fatal("history key not recorded")
	}
	l.do(func() { _, err = g.establishBaseline(context.Background(), 5) })
	if err == nil || !strings.Contains(err.Error(), "Baseline is already established") {
		t.Fatalf("duplicate baseline error = %v", err)
	}
}

// from: gateway/tests/gateway/baseline.test.ts: Baseline resume upon interruption and refuse baseline when normal processed keys exist
func TestGweBaselineResumeAndRefuseDirty(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, stateFileName)
	mustNoErr(t, os.WriteFile(statePath, []byte(`{"login_fingerprint":"","sync_buf":"interrupted-cursor","context_tokens":{},
		"processed_message_keys":["bot-acc:\"chat-1\":\"msg-1\""],"baseline_in_progress":true,"baseline_established":false}`), 0o600))
	s, err := NewStateManager(testAccountFor(testToken), statePath, PolicyOptions{})
	mustNoErr(t, err)
	if !s.BaselineInProgress || s.BaselineEstablished || s.SyncBuf != "interrupted-cursor" {
		t.Fatalf("interrupted state not restored")
	}
	drained := false
	hc := fakeHTTP(func(*http.Request, []byte) (*http.Response, error) {
		drained = true
		return jsonResponse(200, map[string]any{"ret": 0, "errcode": 0, "get_updates_buf": "final-resumed-buf", "msgs": []any{}}), nil
	})
	g, l, _ := gweGateway(t, s, hc, dir)
	var summary BaselineSummary
	l.do(func() { summary, err = g.establishBaseline(context.Background(), 5) })
	mustNoErr(t, err)
	if !drained || summary.FinalSyncBuf != "final-resumed-buf" || !s.BaselineEstablished || s.BaselineInProgress {
		t.Fatalf("resume summary %+v", summary)
	}
	dirtyDir := t.TempDir()
	dirtyPath := filepath.Join(dirtyDir, stateFileName)
	mustNoErr(t, os.WriteFile(dirtyPath, []byte(`{"login_fingerprint":"","sync_buf":"active-cursor","context_tokens":{},
		"processed_message_keys":["key1"],"baseline_in_progress":false,"baseline_established":false}`), 0o600))
	dirty, err := NewStateManager(testAccountFor(testToken), dirtyPath, PolicyOptions{})
	mustNoErr(t, err)
	dg, dl, _ := gweGateway(t, dirty, hc, dirtyDir)
	dl.do(func() { _, err = dg.establishBaseline(context.Background(), 5) })
	if err == nil || !strings.Contains(err.Error(), "state already contains active sync cursor or processed message records") {
		t.Fatalf("dirty baseline error = %v", err)
	}
}
