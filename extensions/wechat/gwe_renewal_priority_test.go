package wechat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

const gweReminder = "主动发一条消息"

// gwePriority is 22 hours and a second into the window with remaining sends
// and a pending answer "ordinary" whose text is body.
func gwePriority(t *testing.T, remaining int64, body string) *harness {
	t.Helper()
	h := newHarness(t, harnessOptions{})
	h.do(func() {
		s := h.state
		at := nowMs() - 22*gweHour - 1000
		w := s.ContextWindows[testChat]
		w.RefreshedAt = at
		s.ContextWindows[testChat] = w
		s.AccountBudget.RefreshedAt = at
		s.AccountBudget.Remaining = remaining
		mustNoErr(t, s.SaveState())
	})
	gweFreeze(t, h.gateway, "ordinary", testChat, body, 2000)
	return h
}

func gweRemaining(h *harness, s *StateManager) (n int64) {
	h.do(func() { n = s.AccountBudget.Remaining })
	return n
}

// from: gateway/tests/results/renewal-priority.test.ts: ${source} pending reply qualifies without Controller interest; reminder wins with ${remaining} sends
func TestGwePendingReplyQualifiesForReminder(t *testing.T) {
	// Only answers frozen here (the "engine" source) exist now; Controller-held replies are gone.
	for _, remaining := range []int64{1, 2} {
		t.Run(fmt.Sprint(remaining), func(t *testing.T) {
			h := gwePriority(t, remaining, "ordinary")
			_, err := h.deliverNotificationsOnce()
			mustNoErr(t, err)
			texts := h.sentTexts()
			if len(texts) != int(remaining) || !strings.Contains(texts[0], "发一条消息") || gweRemaining(h, h.state) != 0 {
				t.Fatalf("texts %q, remaining %d", texts, gweRemaining(h, h.state))
			}
			h.do(func() {
				item, pending := h.state.Outbox["ordinary"]
				if remaining == 1 {
					if !pending || item.Status != OutboxPending || len(h.host.events()) != 0 {
						t.Fatal("the answer must wait, unacknowledged")
					}
				} else if pending || texts[1] != "ordinary" {
					t.Fatal("the answer must follow the reminder")
				}
			})
		})
	}
}

// from: gateway/tests/results/renewal-priority.test.ts: expiry reminder precedes ordinary reply even with ${remaining} sends remaining
func TestGweReminderPrecedesOrdinaryReply(t *testing.T) {
	for _, remaining := range []int64{3, 8, 10} {
		h := gwePriority(t, remaining, "ordinary")
		_, err := h.deliverNotificationsOnce()
		mustNoErr(t, err)
		texts := h.sentTexts()
		if len(texts) != 2 || !strings.Contains(texts[0], gweReminder) || texts[1] != "ordinary" || gweRemaining(h, h.state) != remaining-2 {
			t.Fatalf("remaining %d: texts %q", remaining, texts)
		}
		h.do(func() {
			if _, ok := h.state.Outbox["ordinary"]; ok {
				t.Fatal("answer still in the outbox")
			}
			key := accountRenewalDeliveryKey(h.state.LoginFingerprint(), h.state.AccountBudget.ContextBudgetState)
			if h.state.ReplyDeliveries[key].AcknowledgedChunks != 1 {
				t.Fatal("reminder not acknowledged")
			}
		})
	}
}

// from: gateway/tests/results/renewal-priority.test.ts: expiry reminder precedes multipart text and unsent suffix resumes across restart and refresh
func TestGweReminderPrecedesMultipartAndSuffixResumes(t *testing.T) {
	h := gwePriority(t, 3, "abcdefgh")
	h.do(func() {
		mustNoErr(t, h.state.UpdateOutbox("ordinary", func(i *DeliveryOutboxItem) { i.SplitMaxChars = 2 }))
	})
	_, err := h.deliverNotificationsOnce()
	mustNoErr(t, err)
	texts := h.sentTexts()
	if len(texts) != 3 || !strings.Contains(texts[0], gweReminder) || texts[1] != "ab" || texts[2] != "cd" {
		t.Fatalf("texts %q", texts)
	}
	h.do(func() {
		d := h.state.ReplyDeliveries["ordinary"]
		if d.AcknowledgedChunks != 2 || d.InFlight || h.state.Outbox["ordinary"].Status != OutboxPending {
			t.Fatalf("plan %+v", d)
		}
	})
	restarted, g := gweReopen(t, h)
	_, err = gweNotify(g)
	mustNoErr(t, err)
	if n := len(h.sentTexts()); n != 3 {
		t.Fatalf("%d sends after restart, the persisted reminder must not repeat", n)
	}
	g.loop.do(func() {
		if restarted.ReplyDeliveries["ordinary"].AcknowledgedChunks != 2 || restarted.Outbox["ordinary"].Status != OutboxPending {
			t.Fatal("suffix state lost across restart")
		}
		_, err := restarted.RecordInboundContext("new-inbound", testChat, i64(nowMs()-100), true, "new-context")
		mustNoErr(t, err)
	})
	g = gweOver(t, h, restarted)
	_, err = gweNotify(g)
	mustNoErr(t, err)
	texts = h.sentTexts()
	if len(texts) != 5 || texts[3] != "ef" || texts[4] != "gh" {
		t.Fatalf("texts %q", texts)
	}
	g.loop.do(func() {
		if _, ok := restarted.Outbox["ordinary"]; ok {
			t.Fatal("answer still pending")
		}
	})
	if states := gweStates(h); len(states) != 1 || states[0] != DeliverySent {
		t.Fatalf("delivery events %v", states)
	}
}

// from: gateway/tests/results/renewal-priority.test.ts: legacy unsent renewal checkpoint upgrades without permanently blocking ordinary replies
func TestGweUnsentReminderCheckpointUpgrades(t *testing.T) {
	h := gwePriority(t, 2, "ordinary")
	var key string
	h.do(func() {
		key = accountRenewalDeliveryKey(h.state.LoginFingerprint(), h.state.AccountBudget.ContextBudgetState)
		h.state.ReplyDeliveries[key] = ReplyDeliveryState{Digest: strings.Repeat("a", 64), TotalChunks: 1}
	})
	_, err := h.ilink.SendMarkdownReply(context.Background(), testChat, "ordinary", ReplyOptions{DeliveryKey: "ordinary", MaxCharsPerChunk: 2000})
	if _, ok := asDeferred(err); !ok {
		t.Fatalf("ordinary send must yield to the due reminder, got %v", err)
	}
	_, err = h.deliverNotificationsOnce()
	mustNoErr(t, err)
	h.do(func() {
		if h.state.ReplyDeliveries[key].AcknowledgedChunks != 1 {
			t.Fatal("reminder not sent")
		}
	})
	_, err = h.ilink.SendMarkdownReply(context.Background(), testChat, "ordinary", ReplyOptions{DeliveryKey: "ordinary", MaxCharsPerChunk: 2000})
	mustNoErr(t, err)
	texts := h.sentTexts()
	if !strings.Contains(texts[0], gweReminder) || texts[1] != "ordinary" {
		t.Fatalf("texts %q", texts)
	}
}

// from: gateway/tests/results/renewal-priority.test.ts: queued ordinary callers yield under the shared lock while concurrent notification passes send one reminder
func TestGweQueuedOrdinaryYieldsConcurrentPassesOneReminder(t *testing.T) {
	h := gwePriority(t, 2, "ordinary")
	// The ordinary caller is admitted first and yields; then two passes race.
	_, err := h.ilink.SendMarkdownReply(context.Background(), testChat, "ordinary", ReplyOptions{DeliveryKey: "ordinary", MaxCharsPerChunk: 2000})
	if _, ok := asDeferred(err); !ok {
		t.Fatalf("ordinary send = %v, want deferred", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.deliverNotificationsOnce(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	texts := h.sentTexts()
	if len(texts) != 2 || !strings.Contains(texts[0], "发一条消息") || texts[1] != "ordinary" {
		t.Fatalf("texts %q", texts)
	}
	h.do(func() {
		if h.state.ReplyDeliveries["ordinary"].InFlight {
			t.Fatal("ordinary in flight")
		}
	})
}

// from: gateway/tests/results/renewal-priority.test.ts: unknown reminder never repeats or blocks a pending ordinary reply after restart
func TestGweUnknownReminderNeverRepeats(t *testing.T) {
	h := gwePriority(t, 2, "ordinary")
	calls := 0
	h.setHooks(func(k *harnessHooks) {
		k.send = func(map[string]any) (*http.Response, error) {
			if calls++; calls == 1 {
				return nil, errors.New("renewal response lost")
			}
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		}
	})
	_, err := h.deliverNotificationsOnce()
	if err == nil || !strings.Contains(err.Error(), "renewal response lost") {
		t.Fatalf("error = %v", err)
	}
	texts := h.sentTexts()
	if len(texts) != 2 || texts[1] != "ordinary" {
		t.Fatalf("texts %q", texts)
	}
	h.do(func() {
		key := accountRenewalDeliveryKey(h.state.LoginFingerprint(), h.state.AccountBudget.ContextBudgetState)
		if !h.state.ReplyDeliveries[key].InFlight {
			t.Fatal("unknown reminder not kept in flight")
		}
	})
	_, g := gweReopen(t, h)
	_, err = gweNotify(g)
	mustNoErr(t, err)
	if n := len(h.sentTexts()); n != 2 {
		t.Fatalf("%d sends after restart", n)
	}
}

// from: gateway/tests/results/renewal-priority.test.ts: an explicitly rejected reminder releases priority once per account window: ${JSON.stringify(response)}
func TestGweRejectedReminderReleasesPriority(t *testing.T) {
	for _, response := range []string{`{"ret":-14}`, `{"ret":0,"errcode":1}`} {
		t.Run(response, func(t *testing.T) {
			h := gwePriority(t, 10, "ordinary")
			h.do(func() {
				h.state.SetContextToken("peer", "peer-context")
				h.state.ContextWindows["peer"] = h.state.ContextWindows[testChat]
				mustNoErr(t, h.state.SaveState())
			})
			h.gateway.SetInterest([]string{testChat, "peer"})
			calls := 0
			reply := func() *http.Response {
				if calls++; calls == 1 {
					return rawResponse(200, response)
				}
				return jsonResponse(200, map[string]any{"ret": 0})
			}
			h.setHooks(func(k *harnessHooks) { k.send = func(map[string]any) (*http.Response, error) { return reply(), nil } })
			_, err := h.deliverNotificationsOnce()
			var receipt *SendReceiptError
			if !errors.As(err, &receipt) {
				t.Fatalf("error = %v, want a receipt rejection", err)
			}
			texts := h.sentTexts()
			if len(texts) != 2 || !strings.Contains(texts[0], gweReminder) || texts[1] != "ordinary" || gweRemaining(h, h.state) != 8 {
				t.Fatalf("texts %q", texts)
			}
			var key string
			h.do(func() {
				key = accountRenewalDeliveryKey(h.state.LoginFingerprint(), h.state.AccountBudget.ContextBudgetState)
				d := h.state.ReplyDeliveries[key]
				if _, ok := h.state.Outbox["ordinary"]; ok || !d.RenewalRejected || d.InFlight || d.AcknowledgedChunks != 0 {
					t.Fatalf("reminder evidence %+v", d)
				}
			})
			restarted, _ := gweReopen(t, h)
			h.setHooks(func(k *harnessHooks) {
				k.send = func(map[string]any) (*http.Response, error) { return rawResponse(200, response), nil }
			})
			for pass := 0; pass < 5; pass++ {
				g := gweOver(t, h, restarted)
				g.SetInterest([]string{testChat, "peer"})
				_, err := gweNotify(g)
				mustNoErr(t, err)
			}
			if n := len(h.sentTexts()); n != 2 || gweRemaining(h, restarted) != 8 || !restarted.HasPermanentReplyFailure(key) {
				t.Fatalf("rejected reminder retried: %d sends", n)
			}
			g := gweOver(t, h, restarted)
			gweFreeze(t, g, "next-ordinary", testChat, "ordinary", 2000)
			g.loop.do(func() {
				_, err := restarted.RecordInboundContext("new-window", testChat, i64(nowMs()-1000), true, "new-context")
				mustNoErr(t, err)
				at := nowMs() - 22*gweHour - 1000
				restarted.AccountBudget.RefreshedAt = at
				w := restarted.ContextWindows[testChat]
				w.RefreshedAt = at
				restarted.ContextWindows[testChat] = w
			})
			h.setHooks(func(k *harnessHooks) {
				k.send = func(map[string]any) (*http.Response, error) { return jsonResponse(200, map[string]any{"ret": 0}), nil }
			})
			_, err = gweNotify(g)
			mustNoErr(t, err)
			texts = h.sentTexts()
			if len(texts) != 4 || !strings.Contains(texts[2], gweReminder) || texts[3] != "ordinary" {
				t.Fatalf("new window texts %q", texts)
			}
			g.loop.do(func() {
				if accountRenewalDeliveryKey(restarted.LoginFingerprint(), restarted.AccountBudget.ContextBudgetState) == key {
					t.Fatal("the new window must use a new reminder key")
				}
			})
		})
	}
}

// from: gateway/tests/results/renewal-priority.test.ts: an ordinary explicitly rejected reply still retries its original checkpoint
func TestGweRejectedOrdinaryReplyRetries(t *testing.T) {
	h := gwePriority(t, 10, "ordinary")
	h.do(func() {
		at := nowMs() - 1000
		h.state.AccountBudget.RefreshedAt = at
		w := h.state.ContextWindows[testChat]
		w.RefreshedAt = at
		h.state.ContextWindows[testChat] = w
	})
	calls := 0
	h.setHooks(func(k *harnessHooks) {
		k.send = func(map[string]any) (*http.Response, error) {
			if calls++; calls == 1 {
				return jsonResponse(200, map[string]any{"ret": -14}), nil
			}
			return jsonResponse(200, map[string]any{"ret": 0}), nil
		}
	})
	_, err := h.deliverNotificationsOnce()
	var receipt *SendReceiptError
	if !errors.As(err, &receipt) {
		t.Fatalf("error = %v", err)
	}
	h.do(func() {
		d := h.state.ReplyDeliveries["ordinary"]
		if d.RenewalRejected || d.InFlight || h.state.Outbox["ordinary"].Status != OutboxPending {
			t.Fatalf("plan %+v", d)
		}
	})
	_, err = h.deliverNotificationsOnce()
	mustNoErr(t, err)
	texts := h.sentTexts()
	if len(texts) != 2 || texts[0] != "ordinary" || texts[1] != "ordinary" || gweRemaining(h, h.state) != 8 {
		t.Fatalf("texts %q", texts)
	}
	h.do(func() {
		if h.state.ReplyDeliveries["ordinary"].AcknowledgedChunks != 1 {
			t.Fatal("retry not acknowledged")
		}
	})
}

// from: gateway/tests/results/renewal-priority.test.ts: local pending reply still receives reminder when Controller interest lookup is unavailable
func TestGweLocalPendingReplyReminderWithoutInterest(t *testing.T) {
	// Collie pushes interest instead of being asked, so there is no failing lookup:
	// without any interest the local pending answer still earns the reminder.
	h := gwePriority(t, 1, "ordinary")
	_, err := h.deliverNotificationsOnce()
	mustNoErr(t, err)
	texts := h.sentTexts()
	if len(texts) != 1 || !strings.Contains(texts[0], "发一条消息") {
		t.Fatalf("texts %q", texts)
	}
	h.do(func() {
		if h.state.Outbox["ordinary"].Status != OutboxPending {
			t.Fatal("answer must still wait")
		}
	})
}

// from: gateway/tests/results/renewal-priority.test.ts: a local ${evidence} reply awaiting bookkeeping does not qualify for a reminder
func TestGweReplyAwaitingBookkeepingNoReminder(t *testing.T) {
	for _, evidence := range []string{"unknown", "permanent-image-failure", "accepted"} {
		t.Run(evidence, func(t *testing.T) {
			h := gwePriority(t, 2, "ordinary")
			key := "ordinary"
			if evidence == "permanent-image-failure" {
				key = imageReplyPrefix + "ordinary"
			}
			h.do(func() {
				_, err := h.state.PrepareReplyDelivery(key, strings.Repeat("a", 64), 1)
				mustNoErr(t, err)
				if evidence == "permanent-image-failure" {
					mustNoErr(t, h.state.RecordReplyFailure(key, FailureAttachmentUnavailable))
				} else {
					mustNoErr(t, h.state.BeginReplyChunk(key, 0))
					if evidence == "accepted" {
						mustNoErr(t, h.state.ConfirmReplyChunk(key, 0))
					}
				}
			})
			restarted, g := gweReopen(t, h)
			_, err := gweNotify(g)
			mustNoErr(t, err)
			want := DeliveryUncertain
			if evidence == "accepted" {
				want = DeliverySent
			}
			if len(h.sent()) != 0 || gweRemaining(h, restarted) != 2 {
				t.Fatalf("%d sends", len(h.sent()))
			}
			if states := gweStates(h); len(states) != 1 || states[0] != want {
				t.Fatalf("events %v, want [%s]", states, want)
			}
		})
	}
}

// from: gateway/tests/results/renewal-priority.test.ts: a local stale ${terminal} result reference does not qualify without Controller interest
func TestGweStaleResultReferenceNoReminder(t *testing.T) {
	for _, terminal := range []string{"expired", "uncertain", "superseded"} {
		t.Run(terminal, func(t *testing.T) {
			h := gwePriority(t, 2, "ordinary")
			h.do(func() {
				mustNoErr(t, h.state.RemoveOutbox("ordinary"))
				now := nowMs()
				item := ResultDelivery{ID: newUUID(), Key: "stale", ChatID: testChat, Source: ResultSource{"plan", "old-plan"}, Kind: "completed",
					Status: terminal, Mode: ModeText, Messages: []string{"old result"}, CreatedAt: now - gweHour, ExpiresAt: now + gweHour,
					RetainedUntil: now + gweHour, Artifact: ResultArtifact{ID: newUUID(), ChatID: testChat, Name: "result.md", Size: 10,
						SHA256: strings.Repeat("a", 64), ExpiresAt: now + gweHour}}
				switch terminal {
				case "expired":
					item.Status, item.ExpiresAt = ResultReady, now-1000
					item.CreatedAt = now - gweHour
				case "uncertain":
					item.AttemptID, item.AttemptedAt, item.AttemptExpiresAt = newUUID(), now-2000, now-1000
				}
				h.gateway.results.plans[item.ID] = &storedResult{ResultDelivery: item}
				mustNoErr(t, h.gateway.results.save())
				mustNoErr(t, h.gateway.resultOutbox.Put(ResultReference{ID: item.ID, ChatID: testChat, Identity: resultIdentity(item), Status: RefPending}))
			})
			_, err := h.deliverNotificationsOnce()
			mustNoErr(t, err)
			if len(h.sent()) != 0 || gweRemaining(h, h.state) != 2 {
				t.Fatalf("%d sends", len(h.sent()))
			}
			h.do(func() {
				refs, err := h.gateway.resultOutbox.References()
				mustNoErr(t, err)
				if len(refs) != 0 {
					t.Fatalf("stale reference kept: %+v", refs)
				}
			})
		})
	}
}
