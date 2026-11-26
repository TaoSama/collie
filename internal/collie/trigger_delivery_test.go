package collie

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebhookCommitsBeforeAcknowledging(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "accepted"
		if fail {
			name = "write failure"
		}
		t.Run(name, func(t *testing.T) {
			sp := newRouterTestSuper(t, newFakeHerdr(t))
			if fail {
				if _, err := sp.db.Exec(`CREATE TRIGGER reject_insert BEFORE INSERT ON triggers
					BEGIN SELECT RAISE(FAIL, 'cannot save trigger'); END`); err != nil {
					t.Fatal(err)
				}
			}
			req := httptest.NewRequest(http.MethodPost, "/api/hook/sample?key="+sp.s.hookKey(), strings.NewReader(`{"text":"please check","id":"1"}`))
			req.SetPathValue("source", "sample")
			rec := httptest.NewRecorder()
			sp.s.webhook(rec, req)
			restarted, err := loadState(sp.db)
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				if rec.Code != http.StatusServiceUnavailable || len(sp.st.Triggers) != 0 || len(restarted.Triggers) != 0 {
					t.Fatalf("failed write returned %d with %d in memory and %d on disk", rec.Code, len(sp.st.Triggers), len(restarted.Triggers))
				}
			} else if rec.Code != http.StatusAccepted || len(restarted.Triggers) != 1 || restarted.Threads["sample:sample"] == nil || restarted.Seq != 1 {
				t.Fatalf("ACK returned %d. Restart restored %d triggers, threads %v, seq %d", rec.Code, len(restarted.Triggers), restarted.Threads, restarted.Seq)
			}
		})
	}
}

func TestDeliveryCommitsBeforeNextFlush(t *testing.T) {
	_, sp, p := simAgent(t)
	tr := sp.Ingest("router", mainThread, routerName, "keep track of this", "", "worker-1", "")
	tr.Targets = append(tr.Targets, "worker-2")
	sp.flush()
	sp.deliver(context.Background(), "worker-1", []*Trigger{tr})
	restarted, err := loadState(sp.db)
	if err != nil {
		t.Fatal(err)
	}
	if saved := restarted.trigger(tr.ID); saved.Status != "queued" || len(saved.Targets) != 1 || saved.Targets[0] != "worker-2" || len(p.said) != 1 {
		t.Fatalf("agent got %d prompts, but on restart %s is %s → %v", len(p.said), saved.ID, saved.Status, saved.Targets)
	}
}

func TestDeliveryRecoversWithoutResending(t *testing.T) {
	for _, c := range []struct {
		name          string
		visible, logs bool
	}{{"visible in terminal", true, false}, {"in the agent's log", false, true}, {"receipt uncertain", false, false}} {
		visible := c.visible || c.logs
		t.Run(c.name, func(t *testing.T) {
			f, sp, p := simAgent(t)
			if c.logs {
				withLog(t, sp, p)
			}
			sp.st.Workers["worker-1"] = &Worker{Name: "worker-1", Kind: "claude", Thread: mainThread, Status: "idle", PaneID: "wA:p1"}
			tr := sp.Ingest("router", mainThread, routerName, "keep track of this", "", "worker-1", "")
			sp.flush()
			// The prompt reaches the pane, but the Hub loses its final write before restarting.
			if _, err := sp.db.Exec(`CREATE TRIGGER reject_outcome BEFORE UPDATE ON triggers
				WHEN NEW.status = 'delivered' BEGIN SELECT RAISE(FAIL, 'cannot save receipt'); END`); err != nil {
				t.Fatal(err)
			}
			sp.deliver(context.Background(), "worker-1", []*Trigger{tr})
			if _, err := sp.db.Exec(`DROP TRIGGER reject_outcome`); err != nil {
				t.Fatal(err)
			}
			if len(p.said) != 1 {
				t.Fatalf("agent got %d prompts before restart", len(p.said))
			}
			if !c.visible {
				p.said = nil // the earlier prompt has scrolled off the terminal
			}
			var err error
			sp.st, err = loadState(sp.db)
			if err != nil {
				t.Fatal(err)
			}
			sp.dirty = false
			sp.tick(context.Background())
			restarted, err := loadState(sp.db)
			if err != nil {
				t.Fatal(err)
			}
			saved := restarted.trigger(tr.ID)
			want := "delivered"
			if !visible {
				want = "failed"
				if events := chatEvents(sp); !strings.Contains(events, "Hub 重启或写入中断后无法确认") {
					t.Errorf("missing uncertain receipt notice:\n%s", events)
				}
			}
			if prompts := f.count("agent.prompt"); prompts != 1 || saved.Status != want || len(saved.Targets) != 0 {
				t.Errorf("agent got %d submissions, saved %s → %v, want one submission and %s", prompts, saved.Status, saved.Targets, want)
			}
		})
	}
}

func TestTriggerRequestsCommitBeforeAcknowledging(t *testing.T) {
	for _, route := range []string{"say", "send", "report"} {
		for _, fail := range []bool{false, true} {
			name := route
			if fail {
				name += "/write failure"
			}
			t.Run(name, func(t *testing.T) {
				sp := newRouterTestSuper(t, newFakeHerdr(t))
				worker := &Worker{Name: "worker-1", Thread: mainThread}
				sp.st.Workers[worker.Name] = worker
				sp.st.Settings.Paused = true
				sp.changed()
				sp.flush()
				if fail {
					if _, err := sp.db.Exec(`CREATE TRIGGER reject_insert BEFORE INSERT ON triggers
						BEGIN SELECT RAISE(FAIL, 'cannot save trigger'); END`); err != nil {
						t.Fatal(err)
					}
				}
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/api/super/"+route,
					strings.NewReader(`{"text":"please check","to":"worker-1","worker":"worker-1"}`))
				switch route {
				case "say":
					sp.s.superSay(rec, req)
				case "send":
					sp.s.superSend(rec, req)
				case "report":
					sp.s.superReport(rec, req)
				}
				restarted, err := loadState(sp.db)
				if err != nil {
					t.Fatal(err)
				}
				if fail {
					if rec.Code != http.StatusServiceUnavailable || len(sp.st.Triggers) != 0 || len(sp.st.Entries) != 0 || worker.LastReport != "" {
						t.Fatalf("failed write returned %d, memory has %d triggers, %d entries and report %q", rec.Code, len(sp.st.Triggers), len(sp.st.Entries), worker.LastReport)
					}
					return
				}
				if rec.Code < 200 || rec.Code >= 300 || len(restarted.Triggers) != 1 {
					t.Fatalf("ACK returned %d, restart restored %d triggers", rec.Code, len(restarted.Triggers))
				}
				if route != "send" && len(restarted.Entries) != 1 {
					t.Errorf("restart restored %d conversation entries", len(restarted.Entries))
				}
				if route == "report" && restarted.Workers[worker.Name].LastReport != "please check" {
					t.Errorf("report not restored: %+v", restarted.Workers[worker.Name])
				}
			})
		}
	}
}
