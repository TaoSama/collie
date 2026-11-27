package collie

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"
)

func TestInboxSearchFindsRetainedMessageBodies(t *testing.T) {
	sp, h, _ := newExtTestHub(t)
	now := time.Now()
	for _, c := range []InboxConv{
		{ID: "fake:active", Ext: "fake", On: true, LastAt: now, Preview: "最近消息"},
		{ID: "fake:closed", Ext: "fake", LastAt: now, Preview: "最近消息"},
		{ID: "fake:sent", Ext: "fake", LastAt: now, Preview: "最近消息"},
	} {
		if err := putInboxConv(sp.db, &c); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []InboxMsg{
		{ID: "m1", Conv: "fake:active", At: now.Add(-time.Hour), Text: "早前的 ÄBC 10% 灰度"},
		{ID: "m2", Conv: "fake:active", At: now, Text: "最近消息"},
		{ID: "m3", Conv: "fake:active", At: now.Add(-time.Minute), Text: "另一次 ÄBC 10% 灰度"},
		{ID: "m1", Conv: "fake:closed", At: now.Add(-time.Hour), Text: "早前的 ÄBC 10% 灰度"},
		{ID: "out:1", Conv: "fake:sent", At: now.Add(-time.Hour), Text: "回复 ÄBC 10% 灰度", Ref: "1"},
	} {
		if _, err := putInboxMsg(sp.db, m); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"  äbc 10% 灰度  ", []string{"fake:active", "fake:sent"}},
		{"早前的", []string{"fake:active"}},
		{"_%", []string{}},
		{" ", []string{}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.s.inboxSearch(w, httptest.NewRequest("GET", "/api/inbox/search?q="+url.QueryEscape(tc.query), nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body)
			}
			var got struct{ Matches []string }
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			slices.Sort(got.Matches)
			if !slices.Equal(got.Matches, tc.want) {
				t.Fatalf("matches = %v, want %v", got.Matches, tc.want)
			}
		})
	}
}
