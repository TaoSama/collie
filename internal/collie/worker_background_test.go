package collie

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTickWaitsForWorkerBackground(t *testing.T) {
	for _, resumes := range []bool{false, true} {
		name := "same herdr status"
		if resumes {
			name = "worker starts another turn"
		}
		t.Run(name, func(t *testing.T) {
			_, sp, p := simAgent(t)
			sp.st.Settings.Paused = true
			w := &Worker{Name: "worker-1", Thread: mainThread, Status: "working", PaneID: "wA:p1"}
			sp.st.Workers[w.Name] = w
			ctx := context.Background()
			p.status, p.detection = "done", "● Shell started\n❯"
			sp.tick(ctx) // herdr reports done before the footer is painted
			sp.tick(ctx) // it can still be covered on the next tick
			p.detection += "\n  ⏵⏵ bypass permissions on · 1 shell"
			sp.tick(ctx)
			sp.tick(ctx)
			if got := len(sp.st.Triggers); got != 0 {
				t.Fatalf("sent %d events while the worker was waiting", got)
			}
			withFooter := p.detection
			p.detection = "● Shell started\n❯"
			sp.tick(ctx) // one frame hides the footer
			p.detection = withFooter
			sp.tick(ctx)
			if !w.Background.clearAt.IsZero() {
				t.Fatal("the restored footer did not reset the grace period")
			}
			p.detection = "● All work done\n❯"
			sp.tick(ctx)
			sp.tick(ctx) // the footer already disappeared, but herdr still says done
			if got := len(sp.st.Triggers); got != 0 {
				t.Fatalf("sent %d events during the footer grace period", got)
			}
			if resumes {
				p.status = "working"
				sp.tick(ctx)
				p.status = "done"
				sp.tick(ctx)
				if got := len(sp.st.Triggers); got != 0 {
					t.Fatalf("sent %d events before confirming the new turn", got)
				}
			}
			w.Background.clearAt = time.Now().Add(-backgroundFooterGrace)
			sp.tick(ctx)
			sp.tick(ctx)
			got := sp.st.Triggers
			if len(got) != 1 || !strings.Contains(got[0].Text, "finished a turn") {
				t.Fatalf("want exactly one completion, got %+v", got)
			}
		})
	}
}

func TestTickReportsStalledBackgroundOnce(t *testing.T) {
	_, sp, p := simAgent(t)
	sp.st.Settings.Paused = true
	w := &Worker{Name: "worker-1", Thread: mainThread, Status: "working", PaneID: "wA:p1"}
	sp.st.Workers[w.Name] = w
	ctx := context.Background()
	p.status, p.detection = "done", "● Shell started\n❯\n  ⏵⏵ bypass permissions on · 1 shell"
	sp.tick(ctx)
	sp.tick(ctx)
	w.Background.Since = time.Now().Add(-19 * time.Minute)
	sp.tick(ctx)
	w.Background.Since = time.Now().Add(-21 * time.Minute)
	p.detection = strings.Replace(p.detection, "Shell started", "Shell wrote its first output", 1)
	sp.tick(ctx)
	if got := len(sp.st.Triggers); got != 0 {
		t.Fatalf("sent %d stall events before 20 minutes without progress", got)
	}
	w.Background.Since = time.Now().Add(-21 * time.Minute)
	sp.tick(ctx)
	sp.tick(ctx)
	got := sp.st.Triggers
	if len(got) != 1 || !strings.Contains(got[0].Text, "20 minutes") || !strings.Contains(got[0].Text, "1 shell") {
		t.Fatalf("want one stall event with the background work, got %+v", got)
	}
	reloadWorkerTestState(t, sp)
	w = sp.st.Workers[w.Name]
	sp.tick(ctx)
	if len(sp.st.Triggers) != 1 || !w.Background.Stalled {
		t.Fatal("the stall warning must not repeat after restarting")
	}
	p.detection = "● Shell finished\n❯"
	sp.tick(ctx)
	w.Background.clearAt = time.Now().Add(-backgroundFooterGrace)
	sp.tick(ctx)
	if got := sp.st.Triggers; len(got) != 2 || !strings.Contains(got[1].Text, "finished a turn") {
		t.Fatalf("want completion after the stall event, got %+v", got)
	}
}

func TestTickRestoresPendingWorkerCompletion(t *testing.T) {
	_, sp, p := simAgent(t)
	sp.st.Settings.Paused = true
	w := &Worker{Name: "worker-1", Thread: mainThread, Status: "working", PaneID: "wA:p1"}
	sp.st.Workers[w.Name] = w
	ctx := context.Background()
	p.status, p.detection = "done", "● All work done\n❯"
	sp.tick(ctx)
	reloadWorkerTestState(t, sp) // restart while an ordinary turn is in its grace period
	w = sp.st.Workers[w.Name]
	if w.Background == nil || w.Status != "done" {
		t.Fatalf("lost the pending completion on restart: %+v", w)
	}
	sp.tick(ctx)
	if w.Background.clearAt.IsZero() || len(sp.st.Triggers) != 0 {
		t.Fatal("restart must confirm a freshly observed empty footer")
	}
	w.Background.clearAt = time.Now().Add(-backgroundFooterGrace)
	sp.tick(ctx)
	reloadWorkerTestState(t, sp)
	sp.tick(ctx)
	got := sp.st.Triggers
	if sp.st.Workers[w.Name].Background != nil || len(got) != 1 || !strings.Contains(got[0].Text, "finished a turn") {
		t.Fatalf("want one durable completion and a cleared wait, got %+v", got)
	}
}

func reloadWorkerTestState(t *testing.T, sp *Super) {
	t.Helper()
	if err := sp.flushLocked(); err != nil {
		t.Fatal(err)
	}
	st, err := loadState(sp.db)
	if err != nil {
		t.Fatal(err)
	}
	sp.st = st
}

func TestTickHandlesReportOrBlockDuringWorkerWait(t *testing.T) {
	for _, name := range []string{"report", "blocked"} {
		t.Run(name, func(t *testing.T) {
			_, sp, p := simAgent(t)
			sp.st.Settings.Paused = true
			w := &Worker{Name: "worker-1", Thread: mainThread, Status: "working", PaneID: "wA:p1"}
			sp.st.Workers[w.Name] = w
			ctx := context.Background()
			p.status, p.detection = "done", "⏵⏵ bypass permissions on · 1 shell"
			sp.tick(ctx)
			sp.tick(ctx)
			want := "BLOCKED"
			if name == "report" {
				if err := sp.Report(w.Name, "The task is done"); err != nil {
					t.Fatal(err)
				}
				p.detection = "● The task is done\n❯"
				sp.tick(ctx)
				w.Background.clearAt = time.Now().Add(-backgroundFooterGrace)
				want = "report from"
			} else {
				p.status = "blocked"
			}
			sp.tick(ctx)
			got := sp.st.Triggers
			if w.Background != nil || len(got) != 1 || !strings.Contains(got[0].Text, want) {
				t.Fatalf("want only the %s event and a cleared wait, got %+v", name, got)
			}
		})
	}
}
