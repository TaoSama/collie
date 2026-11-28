package collie

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TaoSama/collie/internal/extproto"
)

func testLarkInboxActions(t *testing.T, s *Server) {
	t.Helper()
	const group = "larkx:c_group"
	const topics = "larkx:c_topics"
	waitFor(t, "topic fixture in the inbox", func() bool {
		_, ok := s.ext.store.conv(topics)
		return ok
	})
	type detail struct {
		Msgs []InboxMsg
		Lark *extproto.LarkInbox
	}
	inbox := func(id string) detail {
		t.Helper()
		w := httptest.NewRecorder()
		s.inboxConv(w, httptest.NewRequest("GET", "/api/inbox/conv?id="+id, nil))
		var result detail
		if err := json.Unmarshal(w.Body.Bytes(), &result); w.Code != 200 || err != nil {
			t.Fatalf("detail HTTP %d: %s (%v)", w.Code, w.Body, err)
		}
		return result
	}
	post := func(handler http.HandlerFunc, path, body string, code int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
		if w.Code != code {
			t.Fatalf("POST %s HTTP %d, want %d: %s", path, w.Code, code, w.Body)
		}
		return w
	}
	action := func(body string, code int) *httptest.ResponseRecorder {
		return post(s.inboxLarkAction, "/api/inbox/lark-action", body, code)
	}
	reply := func(body string) string {
		t.Helper()
		w := post(s.inboxReply, "/api/inbox/reply", body, 200)
		var result struct {
			State string
			Ref   string
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.State != extproto.DeliverySent || result.Ref == "" {
			t.Fatalf("reply not sent: %s (%v)", w.Body, err)
		}
		return "out:" + result.Ref
	}
	find := func(d detail, id string) InboxMsg {
		t.Helper()
		for _, m := range d.Msgs {
			if m.ID == id {
				return m
			}
		}
		t.Fatalf("missing message %q", id)
		return InboxMsg{}
	}

	g := inbox(group)
	if g.Lark == nil || g.Lark.TopicMode || len(g.Lark.Emoji) == 0 {
		t.Fatalf("ordinary group metadata: %+v", g.Lark)
	}
	if before := find(g, "9001").Reactions; len(before) < 1 || before[0].Count != 2 || before[0].Mine {
		t.Fatalf("original reactions: %+v", before)
	}
	action(`{"id":"larkx:c_group","action":"reaction","message_id":"9001","key":"THUMBSUP","active":true}`, 200)
	action(`{"id":"larkx:c_group","action":"reaction","message_id":"9001","key":"THUMBSUP","active":true}`, 200)
	if after := find(inbox(group), "9001").Reactions[0]; after.Count != 3 || !after.Mine {
		t.Fatalf("adding the reaction twice: %+v", after)
	}
	action(`{"id":"larkx:c_group","action":"reaction","message_id":"9001","key":"THUMBSUP","active":false}`, 200)
	if after := find(inbox(group), "9001").Reactions[0]; after.Count != 2 || after.Mine {
		t.Fatalf("removing the reaction: %+v", after)
	}
	action(`{"id":"larkx:c_topics","action":"reaction","message_id":"9001","key":"THUMBSUP","active":true}`, 502)
	action(`{"id":"larkx:c_group","action":"reaction","message_id":"9001","key":"THUMBSUP"}`, 400)
	if q := inbox(topics); q.Lark == nil || !q.Lark.TopicMode || find(q, "9101").ThreadID != "9101" {
		t.Fatalf("topic group metadata: %+v", q)
	}

	action(`{"id":"larkx:c_topics","action":"reaction","message_id":"9101","key":"THUMBSUP","active":true}`, 200)
	action(`{"id":"larkx:c_topics","action":"subscribe","thread_id":"9101","message_id":"9101","active":true}`, 200)
	root := find(inbox(topics), "9101")
	if root.Subscribed == nil || !*root.Subscribed || root.Reactions[0].Count != 4 {
		t.Fatalf("topic state: %+v", root)
	}
	action(`{"id":"larkx:c_topics","action":"subscribe","thread_id":"9101","message_id":"9101","active":false}`, 200)
	if subscribed := find(inbox(topics), "9101").Subscribed; subscribed == nil || *subscribed {
		t.Fatalf("unsubscribe state: %v", subscribed)
	}
	reply(`{"id":"larkx:c_topics","reply_mode":"comment","reply_to":"9101","text":"评论测试"}`)
	thread := action(`{"id":"larkx:c_topics","action":"topic","thread_id":"9101","message_id":"9101"}`, 200)
	if !strings.Contains(thread.Body.String(), "评论测试") {
		t.Fatal("the comment did not appear in the topic")
	}
	newRoot := reply(`{"id":"larkx:c_topics","reply_mode":"publish","text":"发布话题测试"}`)
	if published := find(inbox(topics), newRoot); published.ThreadID == "" || published.SourceID == "" || published.ThreadID != published.SourceID {
		t.Fatalf("local outbox post and topic root did not merge: %+v", published)
	}
	reply(`{"id":"larkx:c_group","reply_mode":"quote","reply_to":"9001","text":"引用回复测试"}`)
	reply(`{"id":"larkx:c_group","reply_mode":"create_topic","reply_to":"9001","text":"创建话题测试"}`)
	if created := find(inbox(group), "9001"); created.ThreadID != "9001" || created.ReplyCount != 1 {
		t.Fatalf("the topic was not linked to its original message: %+v", created)
	}
	action(`{"id":"larkx:c_topics","action":"forward","thread_id":"9101","message_id":"9101","target":"larkx:c_group"}`, 200)
	action(`{"id":"larkx:c_group","action":"forward","message_id":"9001","target":"larkx:c_topics"}`, 200)
}
