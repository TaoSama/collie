package wechat

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/TaoSama/collie/internal/extproto"
)

// gweResultPlan queues a result and claims it, as the dispatcher does.
func gweResultPlan(t *testing.T, h *harness) (ResultDelivery, string) {
	t.Helper()
	res, err := h.send(extproto.Send{Ref: "task", Text: "完整成果🚀\n", Kind: extproto.KindResult})
	if err != nil || !res.Deferred {
		t.Fatalf("result %+v %v", res, err)
	}
	attempt := newUUID()
	var plan ResultDelivery
	h.do(func() {
		for id := range h.gateway.results.plans {
			plan, err = h.gateway.results.claim(id, attempt, nowMs())
		}
	})
	mustNoErr(t, err)
	return plan, attempt
}

// from: gateway/tests/results/result-client.test.ts: materialization authenticates only configured Controller route and checks claim identity
func TestGweResultMaterializeChecksClaim(t *testing.T) {
	// The route and bearer belonged to the Controller client; the claim identity check stays.
	h := newHarness(t, harnessOptions{})
	plan, attempt := gweResultPlan(t, h)
	var err error
	var got ResultDelivery
	h.do(func() { got, err = localResults{h.gateway}.materializeDelivery(plan.ID, attempt) })
	mustNoErr(t, err)
	if got.Artifact.SHA256 != plan.Artifact.SHA256 {
		t.Fatal("materialized another artifact")
	}
	h.do(func() { _, err = localResults{h.gateway}.materializeDelivery(plan.ID, newUUID()) })
	if err == nil || !strings.Contains(err.Error(), "attempt mismatch") {
		t.Fatalf("other attempt: %v", err)
	}
}

// from: gateway/tests/results/result-client.test.ts: a cross-artifact URL cannot fetch a different file under the same origin
func TestGweResultCrossArtifactRefused(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	plan, _ := gweResultPlan(t, h)
	other := plan.Artifact
	other.ID = newUUID()
	var err error
	h.do(func() { _, err = h.gateway.downloadArtifact(context.Background(), other) })
	if err == nil {
		t.Fatal("read an artifact under another id")
	}
}

// from: gateway/tests/results/result-client.test.ts: download verifies immutable size, SHA256, MIME, and application file limit
// from: gateway/tests/results/result-client.test.ts: chunked download stops at expected bytes even without Content-Length
// from: gateway/tests/results/result-client.test.ts: short multi-chunk artifact never exposes unfilled preallocated bytes
func TestGweResultArtifactIntegrity(t *testing.T) {
	// MIME and transfer limits were the HTTP transfer's; the stored file's size and hash are checked.
	original := []byte("完整成果🚀\n")
	for name, content := range map[string][]byte{"corrupt": []byte("corrupt"), "zeroed": make([]byte, len(original)),
		"longer": append(append([]byte(nil), original...), 0), "shorter": original[:len(original)-1]} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			plan, _ := gweResultPlan(t, h)
			var data []byte
			var err error
			h.do(func() { data, err = h.gateway.downloadArtifact(context.Background(), plan.Artifact) })
			if err != nil || string(data) != string(original) {
				t.Fatalf("intact artifact: %q %v", data, err)
			}
			h.do(func() { err = os.WriteFile(h.gateway.results.artifactPath(plan.Artifact.ID), content, 0o600) })
			mustNoErr(t, err)
			h.do(func() { _, err = h.gateway.downloadArtifact(context.Background(), plan.Artifact) })
			if err == nil || !strings.Contains(err.Error(), "integrity mismatch") {
				t.Fatalf("changed artifact: %v", err)
			}
		})
	}
}

// from: gateway/tests/results/result-client.test.ts: pending list refuses results belonging to another chat
func TestGweResultPendingListOwnChatOnly(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.pushResult("task", "结果")
	h.do(func() {
		other, err := h.gateway.results.listPending([]string{"other"}, nowMs())
		mustNoErr(t, err)
		mine, err := h.gateway.results.listPending([]string{testChat}, nowMs())
		mustNoErr(t, err)
		if len(other) != 0 || len(mine) != 1 || mine[0].ChatID != testChat {
			t.Fatalf("other %d, mine %+v", len(other), mine)
		}
	})
}

// from: gateway/tests/results/result-client.test.ts: strict DTO rejects unsafe filenames, cross-chat artifacts and oversized text
func TestGweResultStrictSchema(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	plan, _ := gweResultPlan(t, h)
	mustNoErr(t, ValidateResultDelivery(plan, false))
	bad := plan
	bad.Artifact.Name = "../secret"
	if ValidateResultDelivery(bad, false) == nil {
		t.Fatal("path in file name accepted")
	}
	bad = plan
	bad.Artifact.ChatID = "other"
	if ValidateResultDelivery(bad, false) == nil {
		t.Fatal("cross-chat artifact accepted")
	}
	bad = plan
	bad.Messages = []string{strings.Repeat("a", 3801)}
	if err := ValidateResultDelivery(bad, false); err == nil || !strings.Contains(err.Error(), "schema invalid") {
		t.Fatalf("oversized text: %v", err)
	}
}

// from: gateway/tests/results/result-client.test.ts: notification interest includes all allowed chats while validating the interested subset
func TestGweNotificationInterest(t *testing.T) {
	// Collie pushes interest; chats with results held here join the dispatch list.
	h := newHarness(t, harnessOptions{})
	h.pushResult("task", "结果")
	h.gateway.SetInterest([]string{"no-context"})
	h.do(func() {
		in := h.gateway.interest
		if len(in.interested) != 1 || in.interested[0] != "no-context" || !containsString(in.chatIDs, "no-context") || !containsString(in.chatIDs, testChat) {
			t.Fatalf("interest %+v", in)
		}
	})
}
