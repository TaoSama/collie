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

func TestRouterDocumentsInboxDraft(t *testing.T) {
	if !strings.Contains(routerDoc, "inbox-draft") || !strings.Contains(routerDoc, "draft:<id>") {
		t.Fatal("routerDoc must document inbox-draft / draft:<id> handling")
	}
}

type draftWorkerTest struct {
	sp       *Super
	h        *extHost
	herdr    *fakeHerdr
	worker   *Worker
	pane     *fakePane
	dir, log string
}

const draftWorkerLog = "sessions/2026/11/26/rollout-date-id.jsonl"

func newDraftWorkerTest(t *testing.T) *draftWorkerTest {
	t.Helper()
	sp, h, dir := newExtTestHub(t)
	s, f, pane := chatServer(t, 0, "codex")
	sp.s.herdr = s.herdr
	sp.transcripts, sp.lastScreen = map[string]*transcript{}, map[string]screenMark{}
	t.Setenv("CODEX_HOME", dir)
	w := &Worker{Name: "larkx-draft", Kind: "codex", PaneID: pane, Conv: "larkx:chat", Thread: "ext:larkx:chat", Status: "idle"}
	sp.st.Workers[w.Name] = w
	p := f.panes[pane]
	p.name, p.title, p.sim = w.Name, "Topic | me", true
	f.add("w-router", routerLabel, routerName, "codex", sp.routerCwd(""))
	conv := InboxConv{ID: w.Conv, Ext: larkxID, Chat: "chat", Worker: w.Name}
	h.store.mu.Lock()
	h.store.convs[conv.ID] = &conv
	h.store.mu.Unlock()
	log := strings.Replace(zstRollout, `"payload":{}`, `"payload":{"cwd":"/tmp"}`, 1) +
		`{"type":"turn_context","payload":{"model":"gpt-5","effort":"low"}}` + "\n"
	writeFiles(t, dir, map[string]string{
		"session_index.jsonl": `{"id":"id","thread_name":"Topic"}` + "\n",
		draftWorkerLog:        log,
	})
	return &draftWorkerTest{sp: sp, h: h, herdr: f, worker: w, pane: p, dir: dir, log: log}
}

func (f *draftWorkerTest) start(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	f.sp.s.inboxDraftStart(rec, httptest.NewRequest("POST", "/api/inbox/draft", strings.NewReader(`{"id":"larkx:chat"}`)))
	var d inboxDraft
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || d.Via != f.worker.Name || d.Status != "pending" {
		t.Fatalf("start returned %d %s", rec.Code, rec.Body)
	}
	return d.ID
}

func (f *draftWorkerTest) finish(t *testing.T, reply string) {
	t.Helper()
	f.sp.tick(t.Context()) // observe the running turn before it finishes
	line, err := json.Marshal(map[string]any{
		"type": "response_item",
		"payload": map[string]any{"type": "message", "role": "assistant",
			"content": []map[string]string{{"text": reply}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.log += string(line) + "\n"
	writeFiles(t, f.dir, map[string]string{draftWorkerLog: f.log})
	f.pane.status = "done"
	f.sp.tick(t.Context())
}

func (f *draftWorkerTest) restoreState(t *testing.T) {
	t.Helper()
	f.sp.mu.Lock()
	defer f.sp.mu.Unlock()
	saved, err := loadState(f.sp.db)
	if err != nil {
		t.Fatal(err)
	}
	f.sp.st, f.worker = saved, saved.Workers[f.worker.Name]
	f.sp.transcripts = map[string]*transcript{}
}

func (f *draftWorkerTest) check(t *testing.T, id, status, text string, outCount int) {
	t.Helper()
	d, ok := f.h.getDraft(id)
	if (!ok && status != "") || (ok && (d.Status != status || d.Draft != text)) {
		t.Errorf("draft = %+v, want %q, %q", d, status, text)
	}
	if outs := f.h.store.outs("conv = ?", f.worker.Conv); len(outs) != outCount {
		t.Errorf("outbox entries = %d, want %d", len(outs), outCount)
	}
}

func TestInboxDraftDedicatedTurn(t *testing.T) {
	t.Run("previous turn finishes before draft is delivered", func(t *testing.T) {
		f := newDraftWorkerTest(t)
		f.pane.status, f.worker.Status = "working", "working"
		id := f.start(t)
		f.pane.status = "done"
		f.sp.tick(t.Context())
		f.check(t, id, "pending", "", 1)
		f.finish(t, "Draft for review.")
		f.check(t, id, "done", "Draft for review.", 1)
	})
	t.Run("draft finishes after timeout and restart", func(t *testing.T) {
		f := newDraftWorkerTest(t)
		id := f.start(t)
		f.sp.tick(t.Context())
		f.sp.tick(t.Context())
		f.h.draftMu.Lock()
		f.h.drafts[id].CreatedAt = time.Now().Add(-draftTimeout - time.Second)
		f.h.draftMu.Unlock()
		f.check(t, id, "error", "", 0)
		f.h.draftMu.Lock()
		clear(f.h.drafts)
		f.h.draftMu.Unlock()
		f.sp.flush()
		f.restoreState(t)
		tr := f.sp.st.trigger(f.worker.DraftTrigger)
		if tr == nil || tr.Ref != id || tr.Status != "delivered" {
			t.Fatalf("persisted draft turn = %+v, want delivered trigger for %s", tr, id)
		}
		f.finish(t, "Late draft.")
		f.check(t, id, "", "", 0)
	})
	t.Run("draft settles on cancellation", func(t *testing.T) {
		f := newDraftWorkerTest(t)
		id := f.start(t)
		f.sp.tick(t.Context())
		f.pane.status = "blocked"
		f.sp.tick(t.Context())
		f.pane.status = "idle"
		f.sp.tick(t.Context())
		f.check(t, id, "error", "", 0)
		f.restoreState(t)
		if f.worker.DraftTrigger != "" {
			t.Fatalf("cancelled draft remains attached: %s", f.worker.DraftTrigger)
		}
	})
}

func TestInboxDraftClearSurvivesRestartBeforeFlush(t *testing.T) {
	f := newDraftWorkerTest(t)
	id := f.start(t)
	f.sp.tick(t.Context())
	f.finish(t, "Draft for review.")
	f.check(t, id, "done", "Draft for review.", 0)
	f.restoreState(t) // no flush after the draft settled
	if f.worker.DraftTrigger != "" || f.worker.Status != "done" {
		t.Fatalf("restarted worker has draft %q and status %q", f.worker.DraftTrigger, f.worker.Status)
	}
	f.sp.Ingest("router", f.worker.Thread, routerName, "ordinary", "", f.worker.Name, "")
	f.sp.tick(t.Context())
	if len(f.pane.said) != 2 || !strings.Contains(f.pane.said[1], "ordinary") {
		t.Fatalf("ordinary turn not delivered after restart: %q", f.pane.said)
	}
	f.finish(t, "Ordinary reply.")
	f.check(t, id, "done", "Draft for review.", 1)
}

func TestInboxDraftDedicatedTurnsAreSeparate(t *testing.T) {
	f := newDraftWorkerTest(t)
	first, second := f.start(t), f.start(t)
	f.sp.tick(t.Context())
	if len(f.pane.said) != 1 || !strings.Contains(f.pane.said[0], first) || strings.Contains(f.pane.said[0], second) {
		t.Fatalf("first prompt contains wrong drafts: %q", f.pane.said)
	}
	f.finish(t, "First draft.")
	f.check(t, first, "done", "First draft.", 0)
	f.check(t, second, "pending", "", 0)
	if len(f.pane.said) != 2 || !strings.Contains(f.pane.said[1], second) || strings.Contains(f.pane.said[1], first) {
		t.Fatalf("second prompt contains wrong drafts: %q", f.pane.said)
	}
	f.finish(t, "Second draft.")
	f.check(t, second, "done", "Second draft.", 0)
}

func TestInboxDraftRestoredTurnIsDiscarded(t *testing.T) {
	f := newDraftWorkerTest(t)
	id := f.start(t)
	f.sp.tick(t.Context())
	delete(f.herdr.panes, f.worker.PaneID)
	f.sp.tick(t.Context())
	f.herdr.panes[f.worker.PaneID] = f.pane
	f.sp.tick(t.Context())
	f.finish(t, "Interrupted draft reply.")
	f.check(t, id, "pending", "", 0)
	if f.worker.DraftTrigger != "" || f.worker.DiscardRestoredDraft {
		t.Fatalf("restored worker still owns an interrupted draft: %+v", f.worker)
	}
}
