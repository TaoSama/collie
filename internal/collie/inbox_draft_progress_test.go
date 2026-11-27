package collie

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInboxDraftPollReportsItsTriggerStatus(t *testing.T) {
	sp, h, _ := newExtTestHub(t)
	c := InboxConv{ID: "fake:progress", Ext: "fake", Chat: "progress", LastAt: time.Now()}
	h.store.mu.Lock()
	h.store.convs[c.ID] = &c
	h.store.putConvLocked(&c)
	h.store.mu.Unlock()

	readDraft := func(rec *httptest.ResponseRecorder, want string) inboxDraft {
		t.Helper()
		var draft inboxDraft
		if err := json.Unmarshal(rec.Body.Bytes(), &draft); err != nil {
			t.Fatal(err)
		}
		if rec.Code != 200 || draft.DeliveryStatus != want {
			t.Fatalf("draft response %d: %s; want delivery status %q", rec.Code, rec.Body, want)
		}
		return draft
	}
	start := httptest.NewRecorder()
	sp.s.inboxDraftStart(start, httptest.NewRequest("POST", "/api/inbox/draft", strings.NewReader(`{"id":"fake:progress"}`)))
	draft := readDraft(start, "queued")
	poll := func(want string) {
		t.Helper()
		rec := httptest.NewRecorder()
		sp.s.inboxDraftGet(rec, httptest.NewRequest("GET", "/api/inbox/draft?id="+draft.ID, nil))
		readDraft(rec, want)
	}

	sp.mu.Lock()
	triggers := sp.st.Triggers
	sp.finishDeliveryLocked(routerName, triggers, false, "")
	// Delivered events may age out of the in-memory window before polling.
	sp.st.Triggers = nil
	sp.mu.Unlock()
	poll("delivered")

	if !h.completeDraft(draft.ID, "收到。", "") {
		t.Fatal("could not complete draft")
	}
	poll("")
}
