package wechat

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

// from: gateway/tests/runtime/lifecycle.test.ts: processing remains healthy through a 35s poll plus 90s model, then expires without fake successes
func TestRtdLifecycleProcessingPhaseBudget(t *testing.T) {
	// Health lives in memory and connector.status now; the phase and its
	// deadline are what the health file check read.
	now := setNow(t, 1_800_000_000_000)
	h := &HealthTracker{}
	h.StartRun()
	h.RecordPollSuccess()
	snap, _ := h.Snapshot()
	lastSuccess := snap.LastPollSucceededAt
	mustNoErr(t, h.RecordPollStart((DefaultPollTimeoutMs+PollNetworkMarginMs)*MaxPollAttempts+CheckpointMarginMs))
	now.Add(35000)
	_, err := h.RecordMessageStart(MaxProcessingPhaseMs)
	mustNoErr(t, err)
	now.Add(90000)
	snap, _ = h.Snapshot()
	deadline := mustParseISO(t, snap.Phase.DeadlineAt)
	if snap.Phase.Name != "processing" || now.Load() > deadline || snap.LastPollSucceededAt != lastSuccess {
		t.Fatalf("health = %+v", snap)
	}
	now.Add(MaxProcessingPhaseMs - 90000 + 1)
	if now.Load() <= deadline {
		t.Fatal("processing phase did not expire")
	}
	if _, err := h.RecordMessageStart(MaxProcessingPhaseMs + 1); err == nil {
		t.Fatal("a processing budget over the maximum was accepted")
	}
	h.StartRun()
	snap, _ = h.Snapshot()
	if snap.Phase != nil || snap.LastPollSucceededAt != "" || snap.ProcessedMessageCount != 0 {
		t.Fatalf("restarted health = %+v", snap)
	}
}

// rtdGateway builds a gateway over dir for account, as a process start does.
func rtdGateway(t *testing.T, dir string, account *Account, fetch fetchFunc, host Host, health *HealthTracker) (*Gateway, *StateManager) {
	t.Helper()
	s, err := NewStateManager(account, filepath.Join(dir, stateFileName), PolicyOptions{})
	mustNoErr(t, err)
	l := &loop{}
	c := newTestClient(t, s, l, fakeHTTP(fetch))
	var g *Gateway
	l.do(func() {
		g, err = NewGateway(GatewayOptions{State: s, ILink: c, Host: host, DataDir: dir, MediaHTTP: fakeHTTP(fetch), Health: health})
	})
	mustNoErr(t, err)
	t.Cleanup(g.Stop)
	return g, s
}

func rtdPoll(g *Gateway, wait bool) (s PollCycleSummary, err error) {
	g.loop.do(func() { s, err = g.pollOnce(context.Background(), wait) })
	return s, err
}

// from: gateway/tests/runtime/lifecycle.test.ts: stop completes one message and durable key, preserves cursor, then restart deduplicates and resumes
func TestRtdLifecycleStopCompletesOneMessage(t *testing.T) {
	dir := t.TempDir()
	account := &Account{Token: "synthetic", AccountID: "fixture", UserID: "user"}
	s0, err := NewStateManager(account, filepath.Join(dir, stateFileName), PolicyOptions{})
	mustNoErr(t, err)
	mustNoErr(t, s0.BeginBaseline())
	mustNoErr(t, s0.MarkBaselineEstablished())
	mustNoErr(t, s0.CommitCursor("old-cursor"))
	health := &HealthTracker{}
	health.StartRun()
	var mu sync.Mutex
	var sent []string
	created := nowMs() - 1000
	fetch := func(req *http.Request, body []byte) (*http.Response, error) {
		switch endpoint(req) {
		case "getupdates":
			return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "new-cursor",
				"msgs": []any{inbound("user", "first", "first", created), inbound("user", "second", "second", created)}}), nil
		case "getconfig":
			return jsonResponse(200, map[string]any{"ret": 0, "typing_ticket": "ticket"}), nil
		case "sendtyping":
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		}
		mu.Lock()
		sent = append(sent, decodeBody(t, body)["msg"].(map[string]any)["client_id"].(string))
		mu.Unlock()
		return jsonResponse(200, map[string]any{"ret": 0}), nil
	}
	host := &testHost{}
	g, s := rtdGateway(t, dir, account, fetch, host, health)
	host.receive = func(extproto.Message) error { g.Stop(); return nil }
	stopped, err := rtdPoll(g, true)
	mustNoErr(t, err)
	if !stopped.Interrupted || stopped.ProcessedCount != 1 || stopped.NextSyncBuf != "old-cursor" {
		t.Fatalf("summary = %+v", stopped)
	}
	if got := host.messages(); len(got) != 1 || got[0].Text != "first" {
		t.Fatalf("received %+v", got)
	}
	// The answer now comes later through channel.send: nothing was sent yet.
	if len(sent) != 0 || len(s.ProcessedMessageKeys) != 1 {
		t.Fatalf("sent %d, processed %d", len(sent), len(s.ProcessedMessageKeys))
	}
	if snap, _ := health.Snapshot(); snap.LastPollSucceededAt != "" {
		t.Fatal("an interrupted poll counted as a success")
	}
	resumedHost := &testHost{}
	resumed, resumedState := rtdGateway(t, dir, account, fetch, resumedHost, health)
	if resumedState.SyncBuf != "old-cursor" || len(resumedState.ProcessedMessageKeys) != 1 {
		t.Fatalf("checkpoint = %q, %d keys", resumedState.SyncBuf, len(resumedState.ProcessedMessageKeys))
	}
	summary, err := rtdPoll(resumed, true)
	mustNoErr(t, err)
	if summary.ProcessedCount != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if got := resumedHost.messages(); len(got) != 1 || got[0].Text != "second" {
		t.Fatalf("restart received %+v", got)
	}
	for _, m := range append(host.messages(), resumedHost.messages()...) {
		_, err := resumed.HandleSend(context.Background(), extproto.Send{Ref: "answer:" + m.ID, Chat: "user", Text: "reply", Kind: extproto.KindAnswer, ReplyTo: m.ID})
		mustNoErr(t, err)
	}
	if len(sent) != 2 || resumedState.SyncBuf != "new-cursor" {
		t.Fatalf("sent %d, cursor %q", len(sent), resumedState.SyncBuf)
	}
	if snap, _ := health.Snapshot(); snap.ProcessedMessageCount != 2 {
		t.Fatalf("processed count = %d", snap.ProcessedMessageCount)
	}
}

// from: gateway/tests/runtime/lifecycle.test.ts: stop during getupdates starts no Controller message and does not checkpoint an unprocessed page
func TestRtdLifecycleStopDuringPoll(t *testing.T) {
	dir := t.TempDir()
	account := &Account{Token: "synthetic", AccountID: "fixture", UserID: "user"}
	s0, err := NewStateManager(account, filepath.Join(dir, stateFileName), PolicyOptions{})
	mustNoErr(t, err)
	mustNoErr(t, s0.BeginBaseline())
	mustNoErr(t, s0.MarkBaselineEstablished())
	mustNoErr(t, s0.CommitCursor("old-cursor"))
	host := &testHost{}
	var g *Gateway
	g, s := rtdGateway(t, dir, account, func(*http.Request, []byte) (*http.Response, error) {
		g.Stop()
		return jsonResponse(200, map[string]any{"get_updates_buf": "unprocessed-cursor", "msgs": []any{
			map[string]any{"message_type": 1, "from_user_id": "user", "client_id": "id", "item_list": []any{
				map[string]any{"type": 1, "text_item": map[string]any{"text": "message"}}}}}}), nil
	}, host, &HealthTracker{})
	summary, err := rtdPoll(g, true)
	mustNoErr(t, err)
	if !summary.Interrupted || len(host.messages()) != 0 || s.SyncBuf != "old-cursor" {
		t.Fatalf("summary %+v, received %d, cursor %q", summary, len(host.messages()), s.SyncBuf)
	}
}

// from: gateway/tests/runtime/lifecycle.test.ts: multipart sending has a finite 60s budget and persists only completed chunks
func TestRtdLifecycleMultipartBudget(t *testing.T) {
	// The 60 s budget and 14 s requests are scaled down to 900 ms and 200 ms.
	s := newTestState(t, t.TempDir(), PolicyOptions{})
	s.SetContextToken("user", testContext)
	_, err := s.RecordInboundContext("new-inbound", "user", i64(nowMs()-1000), true, "")
	mustNoErr(t, err)
	var mu sync.Mutex
	requests := 0
	c := newTestClient(t, s, &loop{}, fakeHTTP(func(req *http.Request, _ []byte) (*http.Response, error) {
		switch endpoint(req) {
		case "getconfig":
			return jsonResponse(200, map[string]any{"ret": 0, "typing_ticket": "ticket"}), nil
		case "sendtyping":
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		}
		mu.Lock()
		requests++
		mu.Unlock()
		select {
		case <-time.After(200 * time.Millisecond):
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}))
	_, err = c.SendMarkdownReply(context.Background(), "user", "abcdefghij", ReplyOptions{MaxCharsPerChunk: 1, DeliveryKey: "delivery", TimeoutMs: 900})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	// 900 ms fits four 200 ms sends and cuts the fifth (five requests, four
	// chunks); slow machines (-race under load) fit fewer. What must hold:
	// the budget ends the reply early, and only the finished sends count.
	if chunks := s.ReplySentChunks("delivery"); requests < 2 || requests > 5 || chunks != requests-1 || s.IsMessageProcessed("delivery") {
		t.Fatalf("requests %d, chunks %d", requests, chunks)
	}
}
