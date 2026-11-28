package collie

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

func TestInboxListFillsLinkTitleFromConversationHistory(t *testing.T) {
	sp, h, _ := newExtTestHub(t)
	m := fakeManifest(larkxID, "receive")
	m.Contributes.Connector.Capabilities = append(m.Contributes.Connector.Capabilities, capInboxRecent)
	hubConn, extConn := net.Pipe()
	defer hubConn.Close()
	defer extConn.Close()

	const chat, cold, warm = "123", "[文档] 飞书文档", "[文档] 发布计划"
	at := time.Now().Truncate(time.Second)
	msg := extproto.Message{ID: "1", ChatID: chat, At: at.Format(time.RFC3339), SenderName: "小林",
		Text: "https://example.feishu.cn/docx/sample", Preview: cold, View: &extproto.MessageView{Kind: "post"}}
	var resolved atomic.Bool
	peer := extproto.NewPeer(hubConn, hubConn, "h", nil, nil)
	remote := extproto.NewPeer(extConn, extConn, "p", func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method != extproto.MInboxRecent {
			t.Errorf("unexpected request %s", method)
		}
		latest := msg
		if resolved.Load() {
			latest.Preview, latest.PreviewFallback = warm, cold
		}
		return extproto.InboxRecentResult{Messages: []extproto.Message{latest}}, nil
	}, nil)
	go peer.Serve(t.Context())
	go remote.Serve(t.Context())
	h.mu.Lock()
	h.cat[larkxID] = m
	h.runs[larkxID] = &extRun{h: h, man: m, peer: peer, proc: extProcState{State: extRunning}}
	h.mu.Unlock()

	first, _, err := h.fileIncoming(m, incomingMsg{Chat: chat, ID: msg.ID, At: at, Live: true,
		Text: msg.Text, SenderName: msg.SenderName, Preview: msg.Preview, View: msg.View})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/inbox", sp.s.inboxList)
	mux.HandleFunc("GET /api/inbox/conv", sp.s.inboxConv)
	server := httptest.NewServer(mux)
	defer server.Close()
	get := func(path string, dst any) {
		t.Helper()
		resp, err := server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %s", path, resp.Status)
		}
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			t.Fatal(err)
		}
	}
	assertList := func(preview string, lastAt time.Time, unread int) {
		t.Helper()
		var list struct{ Convs []convView }
		get("/api/inbox", &list)
		if len(list.Convs) != 1 {
			t.Fatalf("inbox list = %+v", list.Convs)
		}
		conv := list.Convs[0]
		if conv.Preview != "小林: "+preview || !conv.LastAt.Equal(lastAt) || conv.Unread != unread {
			t.Fatalf("list preview=%q, at=%v, unread=%d", conv.Preview, conv.LastAt, conv.Unread)
		}
	}
	readDetail := func() convView {
		t.Helper()
		var detail struct{ Conv convView }
		get("/api/inbox/conv?id="+first.ID, &detail)
		return detail.Conv
	}

	assertList(cold, at, first.Unread)
	readDetail()
	assertList(cold, at, first.Unread)
	resolved.Store(true)
	if got := readDetail().Preview; got != "小林: "+warm {
		t.Fatalf("detail preview = %q, want resolved title", got)
	}
	assertList(warm, at, first.Unread)

	resolved.Store(false)
	readDetail()
	assertList(warm, at, first.Unread)

	newer, _, err := h.fileIncoming(m, incomingMsg{Chat: chat, ID: "2", At: at.Add(time.Second), Live: true,
		Text: "最新消息", SenderName: msg.SenderName})
	if err != nil {
		t.Fatal(err)
	}
	resolved.Store(true)
	readDetail()
	assertList("最新消息", newer.LastAt, newer.Unread)
}

func TestInboxHistoryShowsRecalledMessage(t *testing.T) {
	sp, h, _ := newExtTestHub(t)
	m := fakeManifest(larkxID, "receive")
	at := time.Now().Truncate(time.Second)
	c, _, err := h.fileIncoming(m, incomingMsg{Chat: "123", ID: "1", At: at, Live: true,
		Text: "原消息", SenderName: "小林"})
	if err != nil {
		t.Fatal(err)
	}
	const recalled = "撤回了一条消息"
	source := []extproto.Message{{ID: "1", ChatID: c.Chat, At: at.Format(time.RFC3339),
		Text: recalled, Preview: recalled, SenderName: "小林", View: &extproto.MessageView{Kind: "recalled"}}}
	sent := []InboxMsg{{ID: "local", Ref: "sent", At: at, Text: "原消息"}}
	outs := map[string]*OutboxItem{"sent": {MessageID: "1"}}
	if got := mergeInboxRecent(c, sent, source, outs, ""); len(got) != 1 || got[0].Text != recalled {
		t.Fatalf("detail messages = %+v", got)
	}
	sp.s.repairInboxPreview(&c, source)
	if got, _ := h.store.conv(c.ID); got.Preview != "小林: "+recalled || got.Unread != c.Unread || !got.LastAt.Equal(at) {
		t.Fatalf("list preview=%q, at=%v, unread=%d", got.Preview, got.LastAt, got.Unread)
	}
}
