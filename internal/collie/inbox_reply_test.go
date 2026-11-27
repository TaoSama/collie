package collie

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInboxDirectReplyFakeFeishuAndWeChat(t *testing.T) {
	sp, h, dir := newExtTestHub(t)
	h.cat["fakew"].Contributes.Channel.NoFooter = true

	// Enable and sign in both fake Feishu (self) and fake WeChat (owner/bot).
	for _, id := range []string{"fake", "fakew"} {
		if _, err := h.configure(id, func(x *ExtSetting) { x.Enabled = true }); err != nil {
			t.Fatal(err)
		}
		waitFor(t, id+" running", func() bool { r := h.run(id); return r != nil && r.running() })
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/ext/"+id+"/login", strings.NewReader(`{"step":"s1"}`))
		req.SetPathValue("id", id)
		sp.s.extLogin(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s login: %d %s", id, rec.Code, rec.Body)
		}
	}

	// Seed one closed Feishu group (not auto-handled) and one WeChat owner chat.
	now := time.Now().UTC().Format(time.RFC3339)
	writeFakeIn := func(extID string, m fakeMsg) {
		p := filepath.Join(dir, "ext", extID, "data", "in.jsonl")
		b, _ := json.Marshal(m)
		if err := os.WriteFile(p, append(b, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFakeIn("fake", fakeMsg{Chat: "g1", ChatName: "发布值班群", ID: "m1", Sender: "u1", SenderName: "赵六", Text: "今晚 20:00 帮忙盯下监控", At: now})
	writeFakeIn("fakew", fakeMsg{Chat: "wx1", ChatName: "微信ClawBot", ID: "w1", Sender: "me", SenderName: "我", Text: "部署好了吗", Mine: true, At: now})

	waitFor(t, "fake chats arrive", func() bool {
		_, ok1 := h.store.conv("fake:g1")
		_, ok2 := h.store.conv("fakew:wx1")
		return ok1 && ok2
	})

	// GET /api/inbox reports honest send_as / no_footer capabilities.
	listRec := httptest.NewRecorder()
	sp.s.inboxList(listRec, httptest.NewRequest("GET", "/api/inbox", nil))
	var listRes struct {
		Convs []convView `json:"convs"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listRes); err != nil {
		t.Fatal(err)
	}
	byID := map[string]convView{}
	for _, c := range listRes.Convs {
		byID[c.ID] = c
	}
	if byID["fake:g1"].SendAs != "self" || byID["fake:g1"].SendMedia != "none" || byID["fake:g1"].NoFooter {
		t.Fatalf("fake Feishu send caps = %+v, want send_as=self send_media=none no_footer=false", byID["fake:g1"])
	}
	if byID["fakew:wx1"].SendAs != "bot" || byID["fakew:wx1"].SendMedia != "all" || !byID["fakew:wx1"].NoFooter {
		t.Fatalf("fake WeChat send caps = %+v, want send_as=bot send_media=all no_footer=true", byID["fakew:wx1"])
	}

	// 1) Direct human reply to closed Feishu chat: sent without AI footer, marked Human=true.
	postReply := func(payload string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		sp.s.inboxReply(rec, httptest.NewRequest("POST", "/api/inbox/reply", strings.NewReader(payload)))
		return rec
	}
	rec := postReply(`{"id":"fake:g1","text":"收到，今晚 20:00 我在线盯监控。","ai_footer":false}`)
	if rec.Code != 200 {
		t.Fatalf("direct reply failed: %d %s", rec.Code, rec.Body)
	}
	sentBytes, err := os.ReadFile(filepath.Join(dir, "ext", "fake", "data", "sent.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sentBytes), "收到，今晚 20:00 我在线盯监控。") {
		t.Fatalf("sent.jsonl missing direct reply: %s", sentBytes)
	}
	if strings.Contains(string(sentBytes), "AI 代答") {
		t.Fatalf("direct human reply must not include AI footer: %s", sentBytes)
	}
	convRec := httptest.NewRecorder()
	sp.s.inboxConv(convRec, httptest.NewRequest("GET", "/api/inbox/conv?id=fake:g1", nil))
	var detail struct {
		Msgs []InboxMsg `json:"msgs"`
	}
	if err := json.Unmarshal(convRec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Msgs) != 1 || !detail.Msgs[0].Human || !detail.Msgs[0].Mine || detail.Msgs[0].Ref == "" {
		t.Fatalf("closed chat should surface direct human outbox message with Human=true: %+v", detail.Msgs)
	}

	// 2) Confirmed AI draft reply with footer enabled: includes AI footer and Human=false.
	rec = postReply(`{"id":"fake:g1","text":"好的，我已经打开大屏监控了。","ai_footer":true}`)
	if rec.Code != 200 {
		t.Fatalf("AI-confirmed reply failed: %d %s", rec.Code, rec.Body)
	}
	sentBytes, _ = os.ReadFile(filepath.Join(dir, "ext", "fake", "data", "sent.jsonl"))
	if !strings.Contains(string(sentBytes), "AI 代答") {
		t.Fatalf("AI-confirmed reply with ai_footer=true should carry footer: %s", sentBytes)
	}
	convRec = httptest.NewRecorder()
	sp.s.inboxConv(convRec, httptest.NewRequest("GET", "/api/inbox/conv?id=fake:g1", nil))
	detail.Msgs = nil
	if err := json.Unmarshal(convRec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Msgs) != 2 || detail.Msgs[1].Human {
		t.Fatalf("AI-confirmed reply should have Human=false: %+v", detail.Msgs)
	}

	// 3) WeChat bot reply: NoFooter suppresses footer even if ai_footer=true, and Human=false (bot identity).
	rec = postReply(`{"id":"fakew:wx1","text":"已全部部署完成。","ai_footer":true}`)
	if rec.Code != 200 {
		t.Fatalf("WeChat reply failed: %d %s", rec.Code, rec.Body)
	}
	wxSent, err := os.ReadFile(filepath.Join(dir, "ext", "fakew", "data", "sent.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wxSent), "已全部部署完成。") || strings.Contains(string(wxSent), "AI 代答") {
		t.Fatalf("WeChat reply unexpected payload: %s", wxSent)
	}
	wxMsgs := h.store.msgs("fakew:wx1", time.Time{})
	last := wxMsgs[len(wxMsgs)-1]
	if last.Human {
		t.Fatalf("WeChat bot reply must not be marked Human=true: %+v", last)
	}

	// 4) Attachments: WeChat delivers uploaded image + file via channel.send; text-only fake Feishu refuses honestly; path traversal rejected.
	upDir := filepath.Join(dir, "uploads", "20261127")
	if err := os.MkdirAll(upDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pngPath := filepath.Join(upDir, "a1b2c3-shot.png")
	pdfPath := filepath.Join(upDir, "d4e5f6-report.pdf")
	if err := os.WriteFile(pngPath, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pdfPath, []byte("%PDF-1.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mediaReq, _ := json.Marshal(map[string]any{
		"id":   "fakew:wx1",
		"text": "附上截图和报告",
		"files": []map[string]string{
			{"path": pngPath, "name": "shot.png", "mime": "image/png"},
			{"path": pdfPath, "name": "report.pdf", "mime": "application/pdf"},
		},
	})
	rec = postReply(string(mediaReq))
	if rec.Code != 200 {
		t.Fatalf("WeChat media reply failed: %d %s", rec.Code, rec.Body)
	}
	wxSent, _ = os.ReadFile(filepath.Join(dir, "ext", "fakew", "data", "sent.jsonl"))
	if !strings.Contains(string(wxSent), `"files":2`) {
		t.Fatalf("fakew channel.send should receive 2 attachments: %s", wxSent)
	}
	wxMsgs = h.store.msgs("fakew:wx1", time.Time{})
	last = wxMsgs[len(wxMsgs)-1]
	if !strings.Contains(last.Text, "[图片]") || !strings.Contains(last.Text, "[文件] report.pdf") {
		t.Fatalf("WeChat media reply message text missing attachment summary: %q", last.Text)
	}

	// Text-only extension refuses attachments honestly without creating a failed message.
	feishuMediaReq, _ := json.Marshal(map[string]any{
		"id":    "fake:g1",
		"text":  "看图",
		"files": []map[string]string{{"path": pngPath, "name": "shot.png", "mime": "image/png"}},
	})
	rec = postReply(string(feishuMediaReq))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "只发文字") {
		t.Fatalf("text-only extension should reject media with 400: %d %s", rec.Code, rec.Body)
	}

	// Path outside hub uploads/ is rejected.
	outside := filepath.Join(dir, "secret.txt")
	os.WriteFile(outside, []byte("nope"), 0o600)
	badPathReq, _ := json.Marshal(map[string]any{
		"id":    "fakew:wx1",
		"files": []map[string]string{{"path": outside, "name": "secret.txt", "mime": "text/plain"}},
	})
	rec = postReply(string(badPathReq))
	if rec.Code != 400 {
		t.Fatalf("upload path outside uploads/ should be rejected with 400: %d %s", rec.Code, rec.Body)
	}

	// 5) WeChat rejects AI draft endpoint (WeChat only offers direct bot send & send to Collie).
	draftRec := httptest.NewRecorder()
	sp.s.inboxDraftStart(draftRec, httptest.NewRequest("POST", "/api/inbox/draft", strings.NewReader(`{"id":"fakew:wx1","hint":"帮回一下"}`)))
	if draftRec.Code != 400 || !strings.Contains(draftRec.Body.String(), "不支持 AI 帮回") {
		t.Fatalf("WeChat inboxDraftStart should return 400: %d %s", draftRec.Code, draftRec.Body)
	}
}

func TestInboxDraftViaRouterNeverAutoSends(t *testing.T) {
	sp, h, dir := newExtTestHub(t)

	// Seed a Feishu conversation with a recent message.
	now := time.Now()
	c := InboxConv{ID: "fake:g2", Ext: "fake", Chat: "g2", Name: "发布值班群", Kind: "group", LastAt: now, On: true, Handler: handlerCollie}
	h.store.mu.Lock()
	h.store.convs[c.ID] = &c
	h.store.putConvLocked(&c)
	h.store.mu.Unlock()
	h.store.addMsg(InboxMsg{ID: "m1", Conv: c.ID, At: now.Add(-2 * time.Minute), SenderName: "赵六", Text: "@你 今晚 20:00 灰度帮忙盯下？"})

	// Request AI draft (refining with shorter tone + hub-uploaded image context).
	upDir := filepath.Join(dir, "uploads", "20261127")
	if err := os.MkdirAll(upDir, 0o700); err != nil {
		t.Fatal(err)
	}
	imgPath := filepath.Join(upDir, "ref.png")
	if err := os.WriteFile(imgPath, []byte{0x89, 'P', 'N', 'G'}, 0o600); err != nil {
		t.Fatal(err)
	}
	reqBytes, _ := json.Marshal(map[string]any{
		"id":            "fake:g2",
		"hint":          "说我会在线盯监控",
		"tone":          "shorter",
		"current_draft": "好的，今晚八点我会全程关注灰度情况并随时同步。",
		"files":         []string{imgPath},
	})
	rec := httptest.NewRecorder()
	sp.s.inboxDraftStart(rec, httptest.NewRequest("POST", "/api/inbox/draft", bytes.NewReader(reqBytes)))
	if rec.Code != 200 {
		t.Fatalf("POST /api/inbox/draft: %d %s", rec.Code, rec.Body)
	}
	var started inboxDraft
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.ID == "" || started.Status != "pending" || started.Via != "Collie" {
		t.Fatalf("unexpected draft start: %+v", started)
	}

	// Verify trigger targets Collie Router with Action=respond and instructs draft-only reply.
	sp.mu.Lock()
	var trig *Trigger
	for _, tr := range sp.st.Triggers {
		if tr.Source == inboxDraftSource && tr.Ref == started.ID {
			trig = tr
			break
		}
	}
	entriesBefore := len(sp.st.Entries)
	sp.mu.Unlock()
	if trig == nil {
		t.Fatalf("no inbox-draft trigger created")
	}
	if trig.Action != "respond" || trig.Thread != draftThreadPrefix+started.ID {
		t.Fatalf("unexpected trigger routing: %+v", trig)
	}
	if !strings.Contains(trig.Text, "更简短") || !strings.Contains(trig.Text, "今晚 20:00 灰度") || !strings.Contains(trig.Text, "collie reply --thread "+draftThreadPrefix+started.ID) || !strings.Contains(trig.Text, "ref.png") {
		t.Fatalf("draft prompt missing context, attachment, or reply command:\n%s", trig.Text)
	}
	// Verify fromHuman prioritizes inbox-draft triggers.
	if got := fromHuman([]*Trigger{trig}); len(got) != 1 {
		t.Fatalf("fromHuman should include inbox-draft triggers")
	}

	// Simulate Router answering via `collie reply --thread draft:<id>`.
	replyBody, _ := json.Marshal(map[string]any{
		"thread": draftThreadPrefix + started.ID,
		"text":   "收到，今晚 20:00 我在线盯监控，有问题随时喊我。",
	})
	replyRec := httptest.NewRecorder()
	sp.s.superReply(replyRec, httptest.NewRequest("POST", "/api/super/reply", bytes.NewReader(replyBody)))
	if replyRec.Code != 200 {
		t.Fatalf("superReply for draft: %d %s", replyRec.Code, replyRec.Body)
	}

	// Poll GET /api/inbox/draft?id=... -> done with draft text.
	pollRec := httptest.NewRecorder()
	sp.s.inboxDraftGet(pollRec, httptest.NewRequest("GET", "/api/inbox/draft?id="+started.ID, nil))
	if pollRec.Code != 200 {
		t.Fatalf("GET /api/inbox/draft: %d %s", pollRec.Code, pollRec.Body)
	}
	var done inboxDraft
	if err := json.Unmarshal(pollRec.Body.Bytes(), &done); err != nil {
		t.Fatal(err)
	}
	if done.Status != "done" || done.Draft != "收到，今晚 20:00 我在线盯监控，有问题随时喊我。" {
		t.Fatalf("unexpected completed draft: %+v", done)
	}

	// Crucially: nothing was sent to the extension and nothing was appended to chat:main!
	if _, err := os.Stat(filepath.Join(dir, "ext", "fake", "data", "sent.jsonl")); err == nil {
		t.Fatalf("drafting must never auto-send to extension")
	}
	sp.mu.Lock()
	entriesAfter := len(sp.st.Entries)
	sp.mu.Unlock()
	if entriesAfter != entriesBefore {
		t.Fatalf("draft reply must not pollute chat:main entries (%d -> %d)", entriesBefore, entriesAfter)
	}
}

func TestInboxDraftDedicatedWorkerHelper(t *testing.T) {
	_, h, _ := newExtTestHub(t)
	d, err := h.newDraft("fake:g1", "lark-group", "lark-group")
	if err != nil {
		t.Fatal(err)
	}
	got := h.activeWorkerDraft("lark-group")
	if got == nil || got.ID != d.ID {
		t.Fatalf("activeWorkerDraft = %+v, want %s", got, d.ID)
	}
	if !h.completeDraft(d.ID, "草稿内容", "") {
		t.Fatal("completeDraft returned false")
	}
	if h.activeWorkerDraft("lark-group") != nil {
		t.Fatal("completed draft should no longer be active")
	}
	if !strings.Contains(routerDoc, "inbox-draft") || !strings.Contains(routerDoc, "draft:<id>") {
		t.Fatal("routerDoc must document inbox-draft / draft:<id> handling")
	}
}
