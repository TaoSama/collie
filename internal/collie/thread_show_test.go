package collie

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestShowReadsTriggersAndThreadsPastTheWorkingSet(t *testing.T) {
	sp, _, call := newPendingTestEnv(t)
	sp.st.Policies = append(sp.st.Policies, Policy{Source: "webhook:ignored", Action: "ignore"})
	const thread = "github:org/repo#4"
	first := sp.Ingest("webhook:ignored", thread, "", "the very first event", "", "", "")
	for range 700 {
		sp.Ingest("webhook:ignored", thread, "", "later event", "", "", "")
	}
	sp.Ingest("webhook:ignored", "other", "", "elsewhere", "", "", "")
	sp.flush()
	if sp.st.trigger(first.ID) != nil {
		t.Fatalf("%s should have left the working set", first.ID)
	}

	rec := call("GET", "/api/super/show/"+first.ID, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "the very first event") {
		t.Fatalf("show %s = %d %q", first.ID, rec.Code, rec.Body.String())
	}

	showThread := func(query string) string {
		t.Helper()
		rec := call("GET", "/api/super/show/"+url.PathEscape(thread)+query, nil)
		if rec.Code != 200 {
			t.Fatalf("show thread%s = %d %q", query, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	out := showThread("")
	if !strings.Contains(out, "triggers: 701 in total") || strings.Count(out, "\n  #") != showThreadPage {
		t.Fatalf("thread page:\n%s", out)
	}
	// Page back to the start through the ids the listing itself hands out.
	for pages := 0; strings.Contains(out, "--before "); pages++ {
		if pages > 701/showThreadPage {
			t.Fatalf("paging never reached the first trigger:\n%s", out)
		}
		next := out[strings.Index(out, "--before ")+len("--before "):]
		out = showThread("?before=" + strings.TrimSpace(next))
	}
	if !strings.Contains(out, "#"+first.ID+" ") {
		t.Fatalf("last page lacks %s:\n%s", first.ID, out)
	}

	rec = call("GET", "/api/super/triggers", nil)
	var got struct {
		Counts map[string]int `json:"counts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Counts[thread] != 701 || got.Counts["other"] != 1 {
		t.Fatalf("counts = %v", got.Counts)
	}

	for _, id := range []string{"t999999", "no-such-thread"} {
		if rec := call("GET", "/api/super/show/"+id, nil); rec.Code != 404 || !strings.Contains(rec.Body.String(), "nothing found") {
			t.Fatalf("show %s = %d %q", id, rec.Code, rec.Body.String())
		}
	}
}

func TestShowCountsTriggersNotYetFlushed(t *testing.T) {
	sp, _, call := newPendingTestEnv(t)
	sp.st.Policies = append(sp.st.Policies, Policy{Source: "webhook:ignored", Action: "ignore"})
	sp.Ingest("webhook:ignored", "fresh", "", "only in memory", "", "", "")
	rec := call("GET", "/api/super/show/fresh", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "triggers: 1 in total") || !strings.Contains(rec.Body.String(), "only in memory") {
		t.Fatalf("show fresh = %d %q", rec.Code, rec.Body.String())
	}
}
