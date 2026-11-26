package collie

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestPaneDecisionSignatureIncludesOptionDescription(t *testing.T) {
	before := Approval{Title: "Approve", Choices: []Choice{{Label: "Yes", Desc: "Write to report A"}}}
	after := Approval{Title: "Approve", Choices: []Choice{{Label: "Yes", Desc: "Write to report B"}}}
	if askSig(before) == askSig(after) {
		t.Fatal("changing a displayed option description must invalidate the approval")
	}
}

func TestPaneDecisionRequiresMatchingSignature(t *testing.T) {
	sp, f, w, _ := decisionTestSuper(t)
	sig := askSig(parseApproval(testDialog))
	f.panes[w.PaneID].screen = strings.Replace(testDialog, "git push origin main", "git status", 1)
	for _, sig := range []string{"", sig} {
		code, _ := decide(t, sp, map[string]any{"pane": w.PaneID, "choice": "Yes", "sig": sig})
		if code != http.StatusConflict {
			t.Errorf("signature %q: status %d; want 409", sig, code)
		}
	}
	if writes := f.count("pane.send_keys"); writes != 0 {
		t.Errorf("sent keys for a stale or unsigned decision: %d", writes)
	}
}

func TestPaneDecisionReadsMovedCursorAndRejectsConcurrentRepeat(t *testing.T) {
	sp, f, w, _ := decisionTestSuper(t)
	sig := askSig(parseApproval(testDialog))
	f.panes[w.PaneID].screen = strings.ReplaceAll(strings.ReplaceAll(testDialog, "❯ 1.", "  1."), "  2.", "❯ 2.")
	body := map[string]any{"pane": w.PaneID, "choice": "Yes", "sig": sig}
	start, codes := make(chan struct{}), make(chan int, 2)
	for range 2 {
		go func() {
			<-start
			code, _ := decide(t, sp, body)
			codes <- code
		}()
	}
	close(start)
	got := []int{<-codes, <-codes}
	slices.Sort(got)
	if !slices.Equal(got, []int{http.StatusOK, http.StatusConflict}) {
		t.Errorf("statuses = %v; want one success and one conflict", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Equal(f.panes[w.PaneID].sentKeys, []string{"up", "enter"}) {
		t.Errorf("keys = %v; want exactly one selection from the moved cursor", f.panes[w.PaneID].sentKeys)
	}
}

func TestPaneDecisionExecutesAtNode(t *testing.T) {
	sp, f, w, _ := decisionTestSuper(t)
	node := &Server{herdr: sp.s.herdr, node: true}
	hub, _ := remoteNode(t, f, node.NodeRoutes(), false)
	sp.s.herdr = hub
	body := map[string]any{"pane": w.PaneID + "@mac", "choice": "Yes", "sig": askSig(parseApproval(testDialog))}
	if code, out := decide(t, sp, body); code != http.StatusOK || out["picked"] != "Yes" {
		t.Fatalf("node decision = %d %v", code, out)
	}
	if writes := f.count("pane.send_keys"); writes != 1 {
		t.Errorf("node writes = %d; want 1", writes)
	}
}

func TestPaneDecisionOnOlderNodeUsesSignedReadback(t *testing.T) {
	sp, f, w, _ := decisionTestSuper(t)
	hub, _ := remoteNode(t, f, http.NewServeMux(), true)
	sp.s.herdr = hub
	body := map[string]any{"pane": w.PaneID + "@mac", "choice": "Yes", "sig": askSig(parseApproval(testDialog))}
	if code, out := decide(t, sp, body); code != http.StatusOK || out["picked"] != "Yes" {
		t.Fatalf("old node decision = %d %v", code, out)
	}
	if code, _ := decide(t, sp, body); code != http.StatusConflict {
		t.Errorf("repeated choice on old node: %d", code)
	}
	if writes := f.count("pane.send_keys"); writes != 1 {
		t.Errorf("node writes = %d; want 1", writes)
	}
}

func TestPaneDecisionDoesNotRetryOnUncertainNodeAnswer(t *testing.T) {
	sp, f, w, _ := decisionTestSuper(t)
	api := http.HandlerFunc(func(resp http.ResponseWriter, r *http.Request) {
		resp.WriteHeader(http.StatusBadGateway)
	})
	hub, _ := remoteNode(t, f, api, true)
	sp.s.herdr = hub
	body := map[string]any{"pane": w.PaneID + "@mac", "choice": "Yes", "sig": askSig(parseApproval(testDialog))}
	if code, _ := decide(t, sp, body); code != http.StatusBadGateway {
		t.Errorf("uncertain node answer status = %d; want 502", code)
	}
	if writes := f.count("pane.send_keys"); writes != 0 {
		t.Errorf("retried an uncertain node answer: %d writes", writes)
	}
}
