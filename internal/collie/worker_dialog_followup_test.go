package collie

import (
	"strings"
	"testing"
)

func TestWorkerBlockedEventNamesItsNativeDecision(t *testing.T) {
	sp, _, worker, _ := decisionTestSuper(t)

	for i := range 2 {
		sp.workerSettled(t.Context(), worker, true)
		entry := sp.lastAskLocked(worker.PaneID)
		if entry == nil || len(sp.st.Entries) != 1 || len(sp.notifyBuf) != 1 {
			t.Fatalf("attempt %d: expected one dialog and one notice, got entries=%v notices=%v", i, sp.st.Entries, sp.notifyBuf)
		}
		if len(sp.st.Triggers) != i+1 {
			t.Fatalf("attempt %d: expected one worker event per blocked observation, got %v", i, sp.st.Triggers)
		}
		event := sp.st.Triggers[i]
		head, _, _ := strings.Cut(event.Text, "Screen:")
		if event.Source != "worker" || event.Ref != worker.Name || !strings.Contains(head, "decision entry "+entry.ID) {
			t.Fatalf("attempt %d: blocked event lacks the live decision %s: %+v", i, entry.ID, event)
		}
	}
}

func TestWorkerAutoTrustDoesNotCreateADecisionOrBlockedEvent(t *testing.T) {
	sp, herdr, worker, _ := decisionTestSuper(t)
	worker.Cwd = "/home/collie/work/verify"
	herdr.panes[worker.PaneID].screen = " Accessing workspace:\n\n " + worker.Cwd +
		"\n\n Quick safety check: Is this a project you created or one you trust?" +
		"\n ❯ 1. Yes, I trust this folder\n   2. No, exit\n Enter to confirm · Esc to cancel"

	sp.workerSettled(t.Context(), worker, true)

	if worker.Status != "idle" || len(sp.st.Triggers) != 0 || len(sp.st.Entries) != 0 || len(sp.notifyBuf) != 0 {
		t.Fatalf("auto-trust should stay quiet: status=%s, triggers=%v, entries=%v, notices=%v",
			worker.Status, sp.st.Triggers, sp.st.Entries, sp.notifyBuf)
	}
}
