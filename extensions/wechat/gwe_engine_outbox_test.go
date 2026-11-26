package wechat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TaoSama/collie/internal/extproto"
)

const (
	gweMessage    = "synthetic-message"
	gweFinalText  = "SDK 最终原文：保留换行\n与标点。"
	gweToolReply  = "Controller 原始会话标题：保留换行\n与标点。"
	gweRequestTxt = "请记住本轮结果，等待继续"
)

// gweMailbox makes polls answer the request once while open.
func gweMailbox(h *harness, open *bool) {
	h.setHooks(func(k *harnessHooks) {
		k.poll = func() (*http.Response, error) {
			msgs := []any{}
			if *open {
				m := inbound(testChat, gweMessage, gweRequestTxt, nowMs()-1000)
				msgs = append(msgs, m)
			}
			return jsonResponse(200, map[string]any{"ret": 0, "msgs": msgs, "get_updates_buf": "cursor-after-message"}), nil
		}
	})
}

func gweReopenPolicy(t *testing.T, h *harness, policy PolicyOptions, split int) (*StateManager, *Gateway) {
	t.Helper()
	s, err := NewStateManager(testAccountFor(testToken), filepath.Join(h.dir, stateFileName), policy)
	mustNoErr(t, err)
	l := &loop{}
	c := newTestClient(t, s, l, h.http)
	var g *Gateway
	l.do(func() {
		g, err = NewGateway(GatewayOptions{State: s, ILink: c, Host: h.host, DataDir: h.dir, MediaHTTP: h.http, Health: &HealthTracker{}, SplitMaxChars: split})
	})
	mustNoErr(t, err)
	t.Cleanup(g.Stop)
	return s, g
}

func gwePoll(t *testing.T, g *Gateway) (PollCycleSummary, error) {
	t.Helper()
	var s PollCycleSummary
	var err error
	g.loop.do(func() { s, err = g.pollOnce(context.Background(), true) })
	return s, err
}

func gweSentEvents(h *harness, ref string) int {
	n := 0
	for _, e := range h.host.events() {
		if e.Ref == ref && e.State == DeliverySent {
			n++
		}
	}
	return n
}

// from: gateway/tests/gateway/engine-outbox.test.ts: refreshed budget drains the old engine reply before admission and ${remainsFull ? "retains cursor after one bounded retry" : "admits the new request directly"}
func TestGweRefreshedBudgetDrainsOldReplyFirst(t *testing.T) {
	for _, remainsFull := range []bool{false, true} {
		t.Run(fmt.Sprint(remainsFull), func(t *testing.T) {
			h := newHarness(t, harnessOptions{policy: PolicyOptions{SendBudget: 4}, noWindow: true, splitMax: 100})
			open := true
			gweMailbox(h, &open)
			h.do(func() {
				s := h.state
				mustNoErr(t, s.CommitCursor("cursor-before-backpressure"))
				_, err := s.RecordInboundContext("old-window", testChat, i64(nowMs()-2000), true, "old-context")
				mustNoErr(t, err)
				for i := 0; i < 4; i++ {
					mustNoErr(t, s.ConsumeSendBudget(testChat, SendFinal))
				}
			})
			gweFreeze(t, h.gateway, "old-0", testChat, "old reply", 100)
			h.do(func() {
				for i := 1; i < 128; i++ {
					chat := fmt.Sprintf("unfunded-chat-%d", i)
					mustNoErr(t, h.state.PutOutbox(DeliveryOutboxItem{Kind: "reply", Source: SourceEngine, ChatID: chat,
						MessageID: fmt.Sprintf("old-%d", i), DeliveryKey: fmt.Sprintf("old-%d", i), SplitMaxChars: 100, Status: OutboxPending}))
				}
			})
			var ops []string
			h.host.receive = func(m extproto.Message) error {
				ops = append(ops, "receive")
				// The old answer went out and was acknowledged before the new request is handed over.
				if gweSentEvents(h, "old-0") != 1 {
					t.Error("old reply not acknowledged before admission")
				}
				if remainsFull {
					return extproto.Errorf(extproto.ErrHubUnavailable, "Collie unavailable")
				}
				return nil
			}
			key := h.state.ComputeDeliveryKey(testChat, gweMessage)
			_, err := gwePoll(t, h.gateway)
			if remainsFull {
				// Collie being unavailable is the failure here; one attempt, the cursor stays.
				if !extproto.IsCode(err, extproto.ErrHubUnavailable) || h.state.SyncBuf != "cursor-before-backpressure" || h.state.IsMessageProcessed(key) {
					t.Fatalf("err %v cursor %q", err, h.state.SyncBuf)
				}
				if texts := h.sentTexts(); len(texts) != 1 || texts[0] != "old reply" {
					t.Fatalf("texts %q", texts)
				}
			} else {
				mustNoErr(t, err)
				if h.state.SyncBuf != "cursor-after-message" || !h.state.IsMessageProcessed(key) {
					t.Fatalf("cursor %q", h.state.SyncBuf)
				}
				_, err := h.send(extproto.Send{Ref: "new", Text: "new reply", Kind: extproto.KindAnswer, ReplyTo: key})
				mustNoErr(t, err)
				if texts := h.sentTexts(); len(texts) != 2 || texts[0] != "old reply" || texts[1] != "new reply" {
					t.Fatalf("texts %q", texts)
				}
			}
			if len(ops) != 1 {
				t.Fatalf("ops %v", ops)
			}
			h.do(func() {
				if _, ok := h.state.Outbox["old-0"]; ok || len(h.state.Outbox) != 127 {
					t.Fatalf("outbox %d", len(h.state.Outbox))
				}
			})
		})
	}
}

// from: gateway/tests/gateway/engine-outbox.test.ts: ${tool ? "tool-backed" : "pure chat"} engine outbox resumes the frozen reply after budget exhaustion and process restart
func TestGweFrozenReplyResumesAfterRestart(t *testing.T) {
	// Tool-backed or not is the engine's affair; the frozen answer is the same either way.
	for _, text := range []string{gweFinalText, gweToolReply} {
		h := newHarness(t, harnessOptions{policy: PolicyOptions{SendBudget: 1}, splitMax: 3})
		open := true
		gweMailbox(h, &open)
		_, err := gwePoll(t, h.gateway)
		mustNoErr(t, err)
		msgs := h.host.messages()
		if len(msgs) != 1 {
			t.Fatalf("%d hand-overs", len(msgs))
		}
		res, err := h.send(extproto.Send{Ref: "answer", Text: text, Kind: extproto.KindAnswer, ReplyTo: msgs[0].ID})
		mustNoErr(t, err)
		if !res.Deferred || h.sentTexts()[0] != string([]rune(text)[:3]) || len(h.sentTexts()) != 1 {
			t.Fatalf("res %+v texts %q", res, h.sentTexts())
		}
		h.do(func() {
			item := h.state.Outbox["answer"]
			if item.Source != SourceEngine || item.Status != OutboxPending || h.state.ReplyDeliveries["answer"].AcknowledgedChunks != 1 {
				t.Fatalf("outbox %+v", item)
			}
		})
		raw, err := os.ReadFile(filepath.Join(h.dir, stateFileName))
		mustNoErr(t, err)
		if strings.Contains(string(raw), text) {
			t.Fatal("the state file holds the reply body")
		}
		open = false
		restarted, g := gweReopenPolicy(t, h, PolicyOptions{SendBudget: 100}, 1)
		g.loop.do(func() {
			fresh, err := restarted.RecordInboundContext("synthetic-budget-refresh", testChat, i64(nowMs()), true, "fresh-context")
			mustNoErr(t, err)
			if !fresh {
				t.Fatal("refresh not recorded")
			}
			mustNoErr(t, g.flushRepliesOnce(context.Background(), "", ""))
			if _, ok := restarted.Outbox["answer"]; ok {
				t.Fatal("answer still pending")
			}
		})
		if got := strings.Join(h.sentTexts(), ""); got != text {
			t.Fatalf("sent %q, want the frozen reply with its original chunk size", got)
		}
		if gweSentEvents(h, "answer") != 1 {
			t.Fatal("no sent event")
		}
		_, g = gweReopenPolicy(t, h, PolicyOptions{SendBudget: 100}, 0)
		_, err = gwePoll(t, g)
		mustNoErr(t, err)
		if strings.Join(h.sentTexts(), "") != text || len(h.host.messages()) != 1 {
			t.Fatal("restart repeated the request or the reply")
		}
	}
}

// gweFlakyHost fails delivery reports while fail says so.
type gweFlakyHost struct {
	*testHost
	fail func() bool
}

func (h *gweFlakyHost) Delivery(d extproto.Delivery) error {
	if h.fail() {
		return errors.New("synthetic ack persistence failure")
	}
	return h.testHost.Delivery(d)
}

// from: gateway/tests/gateway/engine-outbox.test.ts: ${fault} failure after final receipt is recovered without repeating model or WeChat send
func TestGweFailureAfterFinalReceiptRecovered(t *testing.T) {
	for _, fault := range []string{"ack-save", "outbox-remove"} {
		t.Run(fault, func(t *testing.T) {
			h := newHarness(t, harnessOptions{policy: PolicyOptions{SendBudget: 100}, splitMax: 100})
			open := true
			gweMailbox(h, &open)
			injected := false
			if fault == "ack-save" {
				h.do(func() {
					h.gateway.host = &gweFlakyHost{h.host, func() bool {
						if injected {
							return false
						}
						injected = true
						return true
					}}
				})
			} else {
				seen := false
				h.state.failSave = func() error {
					_, present := h.state.Outbox["answer"]
					seen = seen || present
					if seen && !present && !injected {
						injected = true
						return errors.New("synthetic outbox cleanup failure")
					}
					return nil
				}
			}
			_, err := gwePoll(t, h.gateway)
			mustNoErr(t, err)
			_, err = h.send(extproto.Send{Ref: "answer", Text: gweToolReply, Kind: extproto.KindAnswer, ReplyTo: h.host.messages()[0].ID})
			mustNoErr(t, err)
			if !injected || len(h.sentTexts()) != 1 || h.sentTexts()[0] != gweToolReply {
				t.Fatalf("injected %v texts %q", injected, h.sentTexts())
			}
			h.do(func() {
				item := h.state.Outbox["answer"]
				d := h.state.ReplyDeliveries["answer"]
				if item.Status != OutboxReceipt || item.ReceiptStatus != "accepted" || d.AcknowledgedChunks != d.TotalChunks || d.InFlight {
					t.Fatalf("outbox %+v plan %+v", item, d)
				}
			})
			h.state.failSave = nil
			open = false
			restarted, g := gweReopenPolicy(t, h, PolicyOptions{SendBudget: 100}, 100)
			_, err = gwePoll(t, g)
			mustNoErr(t, err)
			g.loop.do(func() {
				if _, ok := restarted.Outbox["answer"]; ok {
					t.Fatal("receipt not cleaned up")
				}
			})
			if gweSentEvents(h, "answer") != 1 || len(h.sentTexts()) != 1 || len(h.host.messages()) != 1 {
				t.Fatalf("events %v, %d sends", h.host.events(), len(h.sentTexts()))
			}
		})
	}
}

// from: gateway/tests/gateway/engine-outbox.test.ts: accepted empty reply receipt survives journal pruning and restart without retrieval or sends
func TestGweAcceptedReceiptWithoutBodyRecovered(t *testing.T) {
	// An accepted receipt whose frozen answer is already gone: cleanup needs no body and sends nothing.
	h := newHarness(t, harnessOptions{})
	h.do(func() {
		mustNoErr(t, h.state.PutOutbox(DeliveryOutboxItem{Kind: "reply", Source: SourceEngine, ChatID: testChat, MessageID: gweMessage,
			DeliveryKey: "answer", SplitMaxChars: 100, Status: OutboxPending}))
		mustNoErr(t, h.state.UpdateOutbox("answer", func(i *DeliveryOutboxItem) { i.Status, i.ReceiptStatus = OutboxReceipt, "accepted" }))
	})
	restarted, g := gweReopen(t, h)
	_, err := gwePoll(t, g)
	mustNoErr(t, err)
	g.loop.do(func() {
		if _, ok := restarted.Outbox["answer"]; ok || restarted.SyncBuf != "new-cursor" {
			t.Fatal("receipt not cleaned up")
		}
	})
	if len(h.sent()) != 0 || gweSentEvents(h, "answer") != 1 {
		t.Fatalf("%d sends, events %v", len(h.sent()), h.host.events())
	}
}

// from: gateway/tests/gateway/engine-outbox.test.ts: ${status} engine receipt persists acknowledgement once and never repeats journal I/O after restart
func TestGweTerminalReceiptAcknowledgedOnce(t *testing.T) {
	for _, status := range []string{OutboxExpired, OutboxUncertain} {
		t.Run(status, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			h.do(func() {
				mustNoErr(t, h.state.PutOutbox(DeliveryOutboxItem{Kind: "reply", Source: SourceEngine, ChatID: testChat, MessageID: gweMessage,
					DeliveryKey: "answer", SplitMaxChars: 3, Status: status}))
				mustNoErr(t, h.gateway.flushRepliesOnce(context.Background(), "", ""))
				mustNoErr(t, h.gateway.flushRepliesOnce(context.Background(), "", ""))
			})
			if n := len(h.host.events()); n != 1 || h.host.events()[0].State != status {
				t.Fatalf("events %v", h.host.events())
			}
			restarted, g := gweReopen(t, h)
			g.loop.do(func() {
				if !restarted.Outbox["answer"].EngineAcked {
					t.Fatal("acknowledgement not persisted")
				}
				mustNoErr(t, g.flushRepliesOnce(context.Background(), "", ""))
			})
			if len(h.host.events()) != 1 || len(h.sent()) != 0 {
				t.Fatalf("repeated: %v", h.host.events())
			}
		})
	}
}
