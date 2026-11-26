package wechat

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/TaoSama/collie/internal/extproto"
)

// The legacy notification outbox is merged into the result queue: these
// tests push results as Collie does (channel.send kind result) and run the
// notification cycle.

// gwfPlant queues a result for chat, bypassing the active check (tests that
// break a prerequisite first queue, then break it). Answers the plan id.
func gwfPlant(t *testing.T, h *harness, ref, chat, text string) string {
	t.Helper()
	var id string
	h.do(func() {
		_, err := h.gateway.sendResult(extproto.Send{Ref: ref, Chat: chat, Text: text, Kind: extproto.KindResult})
		mustNoErr(t, err)
		id = h.gateway.results.byKey(ref).ID
	})
	return id
}

func gwfPlan(h *harness, id string) (p ResultDelivery) {
	h.do(func() {
		if s, ok := h.gateway.results.plans[id]; ok {
			p = s.ResultDelivery
		}
	})
	return p
}

// gwfClaimed leaves a result claimed by an attempt whose send never began:
// what a lost claim answer or a crash right after the claim leaves.
func gwfClaimed(t *testing.T, h *harness, id string) string {
	t.Helper()
	attempt := newUUID()
	h.do(func() {
		g := h.gateway
		item := g.results.plans[id].ResultDelivery
		mustNoErr(t, g.resultOutbox.Put(ResultReference{ID: id, ChatID: item.ChatID, Identity: resultIdentity(item), Status: RefPending}))
		mustNoErr(t, g.resultOutbox.Update(id, func(r *ResultReference) { r.Status, r.AttemptID = RefClaiming, attempt }))
		_, err := g.results.claim(id, attempt, nowMs())
		mustNoErr(t, err)
		mustNoErr(t, g.resultOutbox.Update(id, func(r *ResultReference) { r.Status = RefClaimed }))
	})
	return attempt
}

// gwfExpire makes plan id's business deadline past, keeping its reference's
// identity in step. The clock is not moved: typing signals still read it
// after a test ends.
func gwfExpire(h *harness, id string) {
	g := h.gateway
	p := &g.results.plans[id].ResultDelivery
	p.CreatedAt, p.ExpiresAt = nowMs()-2000, nowMs()-1000
	identity := resultIdentity(*p)
	if r, ok := g.resultOutbox.items[id]; ok {
		r.Identity = identity
		g.resultOutbox.items[id] = r
	}
}

// gwfAge moves chat's last dispatch back one interval.
func gwfAge(t *testing.T, h *harness, chat string) {
	h.do(func() {
		last, err := h.gateway.resultOutbox.LastDispatchAt(chat)
		mustNoErr(t, err)
		mustNoErr(t, h.gateway.resultOutbox.RecordDispatch(chat, last-ResultDispatchMs))
	})
}

// gwfRestart is the same data directory, network and host in a new gateway.
func gwfRestart(t *testing.T, h *harness) *harness {
	t.Helper()
	r := &harness{t: t, dir: h.dir, loop: &loop{}, host: h.host, http: h.http}
	s, err := NewStateManager(testAccountFor(testToken), filepath.Join(h.dir, stateFileName), PolicyOptions{})
	mustNoErr(t, err)
	r.state = s
	r.ilink = newTestClient(t, s, r.loop, h.http)
	r.do(func() {
		r.gateway, err = NewGateway(GatewayOptions{State: s, ILink: r.ilink, Host: r.host, DataDir: h.dir, MediaHTTP: h.http, Health: &HealthTracker{}})
	})
	mustNoErr(t, err)
	t.Cleanup(func() { r.gateway.Stop() })
	return r
}

func gwfSummary(t *testing.T, h *harness) ResultDispatchSummary {
	t.Helper()
	s, err := h.deliverNotificationsOnce()
	mustNoErr(t, err)
	return s
}

func gwfClientID(s *StateManager, chat, key string, index int) string {
	return clientIDPrefix + hashJSON([]any{s.LoginFingerprint(), chat, key, index})[:deterministicIDHexChars]
}

func gwfEventStates(h *harness) []string {
	var out []string
	for _, e := range h.host.events() {
		out = append(out, e.State+":"+e.Reason)
	}
	return out
}

func gwfSendError(err error) func(*harnessHooks) {
	return func(k *harnessHooks) { k.send = func(map[string]any) (*http.Response, error) { return nil, err } }
}

func gwfSendRaw(body string) func(*harnessHooks) {
	return func(k *harnessHooks) {
		k.send = func(map[string]any) (*http.Response, error) { return rawResponse(200, body), nil }
	}
}

// gwfFailCheckpoint fails every write of files named name until the test
// ends or the returned stop is called.
func gwfFailCheckpoint(t *testing.T, name string) (stop func()) {
	t.Helper()
	var mu sync.Mutex
	on := true
	checkpointStep = func(step, path string) error {
		mu.Lock()
		defer mu.Unlock()
		if on && step == "rename" && filepath.Base(path) == name {
			return errors.New("receipt unavailable")
		}
		return nil
	}
	t.Cleanup(func() { checkpointStep = nil })
	return func() { mu.Lock(); on = false; mu.Unlock() }
}

// from: gateway/tests/gateway/notification-delivery.test.ts: terminal notification is one plain-text send with deterministic evidence ID and no state reset
func TestGwfTerminalResultOneSend(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	mustNoErr(t, h.state.CommitCursor("original-cursor"))
	mustNoErr(t, h.state.RecordProcessedMessageKey("old-key", false))
	fingerprint := h.state.LoginFingerprint()
	text := "计划已收到精确完成回执。"
	id := gwfPlant(t, h, "result-1", testChat, text)
	if s := gwfSummary(t, h); s != (ResultDispatchSummary{Accepted: 1}) {
		t.Fatalf("summary = %+v", s)
	}
	sends := h.sent()
	key := resultStepKey(gwfPlan(h, id), 0)
	if len(sends) != 1 || sends[0]["client_id"] != gwfClientID(h.state, testChat, key, 0) || sends[0]["to_user_id"] != testChat ||
		sends[0]["context_token"] != testContext || jsonText(sends[0]["item_list"]) != jsonText([]any{map[string]any{"type": 1, "text_item": map[string]any{"text": text}}}) {
		t.Fatalf("sends = %v", sends)
	}
	var cursor string
	var processed bool
	h.do(func() { cursor, processed = h.state.SyncBuf, h.state.IsMessageProcessed("old-key") })
	if h.state.LoginFingerprint() != fingerprint || cursor != "original-cursor" || !processed {
		t.Fatal("result delivery reset unrelated state")
	}
	reloaded, err := NewStateManager(testAccountFor(testToken), filepath.Join(h.dir, stateFileName), PolicyOptions{})
	mustNoErr(t, err)
	if reloaded.ReplyDeliveries[key].AcknowledgedChunks != 1 {
		t.Fatalf("durable evidence = %+v", reloaded.ReplyDeliveries[key])
	}
	if gwfPlan(h, id).Status != ResultAccepted || fmt.Sprint(gwfEventStates(h)) != "[sent:]" {
		t.Fatalf("plan %s, events %v", gwfPlan(h, id).Status, gwfEventStates(h))
	}
	if s := gwfSummary(t, h); s != (ResultDispatchSummary{}) || len(h.sent()) != 1 {
		t.Fatalf("second cycle %+v, %d sends", s, len(h.sent()))
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: ${source} result Markdown is sent verbatim
// from: gateway/tests/gateway/notification-delivery.test.ts: goal ${kind} is delivered unchanged to synthetic iLink exactly once
func TestGwfResultSentVerbatimOnce(t *testing.T) {
	for _, text := range []string{
		"## 执行结果\n\n**完成**：修复确认流程。\n- 已通过 `pnpm test`\n- [查看变更](https://example.com/pull/42)",
		"持续目标：模型根据精确完成记录判断条件已满足；这不是独立业务验收。可查询目标进展核对。",
		"持续目标：已停止自动推进，需要处理。请查询目标进展核对当前状态；这不表示原会话任务停止。",
		"持续目标：授权已到期，已停止后续派发。请查询目标进展核对当前状态；这不表示原会话任务停止。",
	} {
		h := newHarness(t, harnessOptions{})
		id := gwfPlant(t, h, "result-1", testChat, text)
		if s := gwfSummary(t, h); s != (ResultDispatchSummary{Accepted: 1}) {
			t.Fatalf("summary = %+v", s)
		}
		if got := h.sentTexts(); len(got) != 1 || got[0] != text {
			t.Fatalf("sent %q", got)
		}
		if h.sent()[0]["client_id"] != gwfClientID(h.state, testChat, resultStepKey(gwfPlan(h, id), 0), 0) {
			t.Fatal("client id is not the step's deterministic id")
		}
		gwfSummary(t, h)
		if len(h.sent()) != 1 {
			t.Fatal("sent twice")
		}
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: long notification preserves all Markdown and Unicode across bounded chunks
func TestGwfLongResultPreservesText(t *testing.T) {
	h := newHarness(t, harnessOptions{policy: PolicyOptions{SendBudget: 100}})
	// Results are cut into at most three messages of ResultTextChars, not the reply split size.
	text := "# 执行结果\n\n- **完成**：" + strings.Repeat("验证通过🚀", 1200)
	id := gwfPlant(t, h, "result-1", testChat, text)
	if s := gwfSummary(t, h); s.Accepted != 1 {
		t.Fatalf("summary = %+v", s)
	}
	texts, plan := h.sentTexts(), gwfPlan(h, id)
	if len(texts) < 2 {
		t.Fatalf("%d messages", len(texts))
	}
	for i, chunk := range texts {
		if utf16Len(chunk) > ResultTextChars || strings.ContainsRune(chunk, '�') {
			t.Fatalf("chunk %d is %d units or split a character", i, utf16Len(chunk))
		}
		if h.sent()[i]["client_id"] != gwfClientID(h.state, testChat, resultStepKey(plan, i), 0) {
			t.Fatalf("chunk %d client id", i)
		}
	}
	if strings.Join(texts, "") != text {
		t.Fatal("chunks do not join to the original text")
	}
	count := len(h.sent())
	gwfSummary(t, h)
	if len(h.sent()) != count {
		t.Fatal("resent after completion")
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: failed Markdown notification is permanently uncertain without another send after restart
// from: gateway/tests/gateway/notification-delivery.test.ts: uncertain goal notification remains permanent across gateway restart
func TestGwfUncertainResultPermanentAcrossRestart(t *testing.T) {
	for name, hook := range map[string]func(*harnessHooks){
		"network": gwfSendError(errors.New("response lost")), "invalid receipt": gwfSendRaw(`{"ret":null}`)} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			h.setHooks(hook)
			id := gwfPlant(t, h, "result-1", testChat, "# 执行结果\n\n- **完成**："+strings.Repeat("验证通过🚀", 200))
			if s := gwfSummary(t, h); s != (ResultDispatchSummary{Uncertain: 1}) {
				t.Fatalf("summary = %+v", s)
			}
			r := gwfRestart(t, h)
			gwfSummary(t, r)
			if gwfPlan(r, id).Status != ResultUncertain || len(h.sent()) != 1 {
				t.Fatalf("status %s, %d sends", gwfPlan(r, id).Status, len(h.sent()))
			}
		})
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: claim cannot change ${sourceType} source type with the same ID, kind and text
func TestGwfRecoveryRefusesChangedSource(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	id := gwfPlant(t, h, "result-1", testChat, "结果")
	gwfClaimed(t, h, id)
	// The stored plan changes identity under its durable reference.
	h.do(func() { h.gateway.results.plans[id].Source.Type = "goal" })
	for i := 0; i < 2; i++ {
		if _, err := h.deliverNotificationsOnce(); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
			t.Fatalf("cycle %d error = %v", i, err)
		}
	}
	if len(h.sent()) != 0 || gwfPlan(h, id).Status != ResultClaimed {
		t.Fatalf("%d sends, status %s", len(h.sent()), gwfPlan(h, id).Status)
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: missing prerequisites, expired budgets and stop perform no list/claim/send
func TestGwfMissingPrerequisitesSendNothing(t *testing.T) {
	for _, mode := range []string{"baseline", "context", "account", "budget", "window", "stop"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			id := gwfPlant(t, h, "result-1", testChat, "结果")
			h.do(func() {
				s := h.state
				switch mode {
				case "baseline":
					s.BaselineEstablished = false
				case "context":
					s.ContextTokens = map[string]string{}
				case "account":
					s.Account = nil
				case "budget":
					s.ContextWindows = map[string]ChatContextWindow{}
				case "window":
					w := s.ContextWindows[testChat]
					w.RefreshedAt = nowMs() - s.Policy.ContextWindowMs
					s.ContextWindows[testChat] = w
				case "stop":
					h.gateway.stop()
				}
			})
			if s := gwfSummary(t, h); s != (ResultDispatchSummary{}) {
				t.Fatalf("summary = %+v", s)
			}
			if len(h.sent()) != 0 {
				t.Fatal("sent without its prerequisites")
			}
			if mode != "account" && gwfPlan(h, id).Status == ResultClaimed {
				t.Fatal("claimed without its prerequisites")
			}
		})
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: notifications deliver by default without a feature flag
func TestGwfResultsDeliverByDefault(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.pushResult("result-1", "结果")
	if s := gwfSummary(t, h); s.Accepted != 1 || len(h.sent()) != 1 {
		t.Fatalf("summary %+v, %d sends", s, len(h.sent()))
	}
}

// from: gateway/tests/gateway/notification-delivery.test.ts: missing-context older records do not block reachable chat notifications
func TestGwfUnreachableChatDoesNotBlock(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	var stuck []string
	for i := 1; i <= 4; i++ {
		stuck = append(stuck, gwfPlant(t, h, fmt.Sprintf("old-%d", i), "unreachable", "旧结果"))
	}
	gwfPlant(t, h, "result-5", testChat, "结果")
	if s := gwfSummary(t, h); s.Accepted != 1 {
		t.Fatalf("summary = %+v", s)
	}
	for _, send := range h.sent() {
		if send["to_user_id"] != testChat {
			t.Fatalf("sent to %v", send["to_user_id"])
		}
	}
	for _, id := range stuck {
		if st := gwfPlan(h, id).Status; st != ResultReady && st != ResultWaitingForUser {
			t.Fatalf("unreachable result became %s", st)
		}
	}
}
