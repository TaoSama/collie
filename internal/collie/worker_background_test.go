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
			p.detection = "● All work done\n❯"
			if resumes {
				p.status = "working"
				sp.tick(ctx)
				p.status = "done"
				sp.tick(ctx)
				if got := len(sp.st.Triggers); got != 0 {
					t.Fatalf("sent %d events before confirming the new turn", got)
				}
				w.background.since = time.Now().Add(-backgroundFooterGrace)
			}
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
	w.background.since = time.Now().Add(-19 * time.Minute)
	sp.tick(ctx)
	w.background.since = time.Now().Add(-21 * time.Minute)
	p.detection = strings.Replace(p.detection, "Shell started", "Shell wrote its first output", 1)
	sp.tick(ctx)
	if got := len(sp.st.Triggers); got != 0 {
		t.Fatalf("sent %d stall events before 20 minutes without progress", got)
	}
	w.background.since = time.Now().Add(-21 * time.Minute)
	sp.tick(ctx)
	sp.tick(ctx)
	got := sp.st.Triggers
	if len(got) != 1 || !strings.Contains(got[0].Text, "20 minutes") || !strings.Contains(got[0].Text, "1 shell") {
		t.Fatalf("want one stall event with the background work, got %+v", got)
	}
	p.detection = "● Shell finished\n❯"
	sp.tick(ctx)
	if got := sp.st.Triggers; len(got) != 2 || !strings.Contains(got[1].Text, "finished a turn") {
		t.Fatalf("want completion after the stall event, got %+v", got)
	}
}
