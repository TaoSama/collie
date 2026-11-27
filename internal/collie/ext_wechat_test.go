package collie

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
	"github.com/TaoSama/collie/web"
)

const wechatExt = "wechat"

// WeChat is in the built-in catalog, run by collie itself, off until you
// turn it on.
func TestCatalogHasWeChat(t *testing.T) {
	cat, err := extCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var wx *ExtManifest
	for _, m := range cat {
		if m.ID == wechatExt {
			wx = m
		}
	}
	if wx == nil || wx.Runtime.Builtin != wechatExt || builtinExts[wechatExt] == nil {
		t.Fatalf("wechat is not a built-in: %+v", wx)
	}
	_, h, _ := newExtTestHub(t)
	if h.setting(wechatExt).Enabled {
		t.Fatal("wechat is on before you turn it on")
	}
}

func TestWeChatConversationDisplayName(t *testing.T) {
	sp, h, dir := newExtTestHub(t)
	cat, err := extCatalog()
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	for _, m := range cat {
		if m.ID == wechatExt {
			h.cat[m.ID] = m
		}
	}
	h.mu.Unlock()
	chat := "owner@im.wechat"
	id := convID(wechatExt, chat)
	seed := incomingMsg{Chat: chat, ChatName: chat, ChatKind: "dm", ID: "old", Owner: true, At: time.Now()}
	if err := h.incoming(h.manifest(wechatExt), seed); err != nil {
		t.Fatal(err)
	}
	other := incomingMsg{Chat: "another-chat", ChatName: "原始名称", ID: "other", At: time.Now()}
	if err := h.incoming(h.manifest("fakew"), other); err != nil {
		t.Fatal(err)
	}

	h.s.token = "test"
	h.s.web = web.FS
	mux := http.NewServeMux()
	h.s.extRoutes(mux)
	request := func(method, path, body string, out any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
		if out != nil {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatal(err)
			}
		}
	}
	convPath := "/api/inbox/conv?id=" + url.QueryEscape(id)
	checkName := func(want string) {
		t.Helper()
		check := func(c InboxConv) {
			t.Helper()
			if c.Name != want || c.Chat != chat || c.thread() != "ext:wechat:"+chat {
				t.Fatalf("want name %q and original chat/thread, got %+v", want, c)
			}
		}
		var list struct{ Convs []convView }
		request("GET", "/api/inbox", "", &list)
		names := map[string]InboxConv{}
		for _, c := range list.Convs {
			names[c.ID] = c.InboxConv
		}
		check(names[id])
		if c := names["fakew:another-chat"]; c.Name != other.ChatName {
			t.Fatalf("unexpected change to another extension: %+v", c)
		}
		var detail struct{ Conv InboxConv }
		request("GET", convPath, "", &detail)
		check(detail.Conv)
		request("POST", "/api/inbox/conv", `{"id":"`+id+`","seen":true}`, &detail)
		check(detail.Conv)
	}
	if prop, ok := h.manifest(wechatExt).Configuration.propertyOK(wechatConversationNameKey); !ok || prop.Title != "昵称" {
		t.Fatalf("want wechat conversation_name title 昵称, got %+v", prop)
	}
	checkName("微信ClawBot")
	var saved struct{ Setting ExtSetting }
	request("POST", "/api/ext/wechat", `{"config":{"conversation_name":"  我的微信助手  "}}`, &saved)
	if cfg := h.manifest(wechatExt).programConfig(saved.Setting.Config); len(cfg) != 0 {
		t.Fatalf("display name leaked into the runtime config: %v", cfg)
	}
	checkName("我的微信助手")
	if got := newExtHost(h.s, sp, dir).displayConversation(InboxConv{Ext: wechatExt}).Name; got != "我的微信助手" {
		t.Fatalf("display name after reopening the database = %q", got)
	}
	seed.ID, seed.ChatName, seed.Live = "new", "你的微信", true
	seed.Text = "你好"
	if err := h.incoming(h.manifest(wechatExt), seed); err != nil {
		t.Fatal(err)
	}
	checkName("我的微信助手")
	ts := extTriggers(sp, "ext:wechat")
	if len(ts) != 1 || ts[0].Thread != "ext:wechat:"+chat {
		t.Fatalf("new message did not use the original thread: %+v", ts)
	}
	request("POST", "/api/ext/wechat", `{"config":{"conversation_name":"  "}}`, &saved)
	checkName("微信ClawBot")
}

func TestExtViewsOrderBuiltinsBeforeMarket(t *testing.T) {
	_, h, _ := newExtTestHub(t)
	cat, err := extCatalog()
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.cat = map[string]*ExtManifest{}
	for _, m := range cat {
		h.cat[m.ID] = m
	}
	h.cat["aaa-market"] = &ExtManifest{ID: "aaa-market", Name: "AAA", Market: &ExtMarketRef{Repo: "owner/repo"}}
	h.cat["zzz-market"] = &ExtManifest{ID: "zzz-market", Name: "ZZZ", Market: &ExtMarketRef{Repo: "owner/repo"}}
	h.cat["000-local"] = &ExtManifest{ID: "000-local", Name: "Local", Local: true}
	h.mu.Unlock()

	views := h.views()
	got := make([]string, len(views))
	for i, v := range views {
		got[i] = v.Manifest.ID
	}
	want := []string{"usage", "larkx", "wechat", "workbench", "aaa-market", "zzz-market", "000-local"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("views order = %v, want %v", got, want)
	}
}

func TestWeChatCustomAvatar(t *testing.T) {
	sp, h, dir := newExtTestHub(t)
	cat, err := extCatalog()
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	for _, m := range cat {
		if m.ID == wechatExt {
			h.cat[m.ID] = m
		}
	}
	h.mu.Unlock()
	chat := "owner@im.wechat"
	id := convID(wechatExt, chat)
	if err := h.incoming(h.manifest(wechatExt), incomingMsg{Chat: chat, ChatName: chat, ChatKind: "dm", ID: "m1", Owner: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := h.incoming(h.manifest("fakew"), incomingMsg{Chat: "c2", ChatName: "其他", ID: "m2", At: time.Now()}); err != nil {
		t.Fatal(err)
	}

	h.s.token = "test"
	h.s.web = web.FS
	mux := http.NewServeMux()
	h.s.extRoutes(mux)
	do := func(method, path string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer test")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	checkAvatar := func(want string) {
		t.Helper()
		var extList struct{ Extensions []extView }
		w := do("GET", "/api/ext", nil)
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &extList) != nil {
			t.Fatalf("GET /api/ext: %d %s", w.Code, w.Body)
		}
		for _, v := range extList.Extensions {
			if v.Manifest.ID == wechatExt && v.AvatarURL != want {
				t.Fatalf("ext view avatar_url = %q, want %q", v.AvatarURL, want)
			}
			if v.Manifest.ID != wechatExt && v.AvatarURL != "" {
				t.Fatalf("unexpected avatar_url on %s: %q", v.Manifest.ID, v.AvatarURL)
			}
		}
		var list struct{ Convs []convView }
		w = do("GET", "/api/inbox", nil)
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil {
			t.Fatalf("GET /api/inbox: %d %s", w.Code, w.Body)
		}
		for _, c := range list.Convs {
			if c.ID == id && c.Avatar != want {
				t.Fatalf("inbox list wechat avatar = %q, want %q", c.Avatar, want)
			}
			if c.ID == "fakew:c2" && c.Avatar != "" {
				t.Fatalf("unexpected avatar on fakew conv: %q", c.Avatar)
			}
		}
		var detail struct{ Conv convView }
		w = do("GET", "/api/inbox/conv?id="+url.QueryEscape(id), nil)
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Conv.Avatar != want {
			t.Fatalf("GET /api/inbox/conv: %d %+v want %q", w.Code, detail, want)
		}
		w = do("POST", "/api/inbox/conv", []byte(`{"id":"`+id+`","seen":true}`))
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Conv.Avatar != want {
			t.Fatalf("POST /api/inbox/conv: %d %+v want %q", w.Code, detail, want)
		}
	}

	checkAvatar("")
	if w := do("GET", "/api/ext/wechat/avatar", nil); w.Code != 404 {
		t.Fatalf("unset avatar status = %d, want 404", w.Code)
	}
	if w := do("POST", "/api/ext/fakew/avatar", []byte("\x89PNG\r\n\x1a\n")); w.Code != 404 {
		t.Fatalf("non-wechat upload status = %d, want 404", w.Code)
	}
	if w := do("POST", "/api/ext/wechat/avatar", []byte("<svg></svg>")); w.Code != 400 {
		t.Fatalf("svg upload status = %d, want 400", w.Code)
	}
	if w := do("POST", "/api/ext/wechat/avatar", append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("a"), wechatAvatarMax)...)); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload status = %d, want 413", w.Code)
	}

	pngBytes := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x02\x00\x00\x00\x90wS\xde")
	var uploaded struct {
		AvatarURL string  `json:"avatar_url"`
		View      extView `json:"view"`
	}
	w := do("POST", "/api/ext/wechat/avatar", pngBytes)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &uploaded) != nil {
		t.Fatalf("POST png avatar: %d %s", w.Code, w.Body)
	}
	if !strings.HasPrefix(uploaded.AvatarURL, "/api/ext/wechat/avatar?v=") || uploaded.View.AvatarURL != uploaded.AvatarURL {
		t.Fatalf("unexpected upload response: %+v", uploaded)
	}
	checkAvatar(uploaded.AvatarURL)

	w = do("GET", uploaded.AvatarURL, nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("X-Content-Type-Options") != "nosniff" || !bytes.Equal(w.Body.Bytes(), pngBytes) {
		t.Fatalf("GET uploaded avatar: %d %v len=%d", w.Code, w.Header(), w.Body.Len())
	}

	// Survives reopening the host.
	reopened := newExtHost(h.s, sp, dir)
	if got := reopened.convAvatarPath(InboxConv{Ext: wechatExt}); got != uploaded.AvatarURL {
		t.Fatalf("reopened convAvatarPath = %q, want %q", got, uploaded.AvatarURL)
	}

	// Replacing with JPEG updates the cache-busting URL and content type.
	jpgBytes := []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00\xff\xd9")
	var replaced struct {
		AvatarURL string `json:"avatar_url"`
	}
	w = do("POST", "/api/ext/wechat/avatar", jpgBytes)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &replaced) != nil || replaced.AvatarURL == uploaded.AvatarURL {
		t.Fatalf("replace jpeg avatar: %d %+v", w.Code, replaced)
	}
	checkAvatar(replaced.AvatarURL)
	if w := do("GET", replaced.AvatarURL, nil); w.Code != 200 || w.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("GET replaced avatar: %d %s", w.Code, w.Header().Get("Content-Type"))
	}

	// Clearing restores default.
	if w := do("DELETE", "/api/ext/wechat/avatar", nil); w.Code != 200 {
		t.Fatalf("DELETE avatar: %d %s", w.Code, w.Body)
	}
	checkAvatar("")
	if w := do("GET", "/api/ext/wechat/avatar", nil); w.Code != 404 {
		t.Fatalf("cleared avatar status = %d, want 404", w.Code)
	}
}

// startWeChat runs the fake extension under the id wechat, signed in.
func startWeChat(t *testing.T, h *extHost) {
	t.Helper()
	cat, err := extCatalog()
	if err != nil {
		t.Fatal(err)
	}
	m := fakeManifest(wechatExt, "receive")
	for _, real := range cat {
		if real.ID == wechatExt {
			m.Contributes.Channel = real.Contributes.Channel // how it sends: no footer
		}
	}
	h.mu.Lock()
	h.cat[wechatExt] = m
	h.mu.Unlock()
	t.Cleanup(func() { h.configure(wechatExt, func(x *ExtSetting) { x.Enabled = false }) })
	login(t, startExt(t, h, wechatExt))
}

// Asking for the session list is a request like any other: it goes to
// the Router, and Collie sends nothing by itself.
func TestWeChatSessionListGoesToRouter(t *testing.T) {
	sp, h, dir := newExtTestHub(t)
	startWeChat(t, h)
	for i, text := range []string{"查看会话列表", "有哪些空闲会话"} {
		feed(t, dir, wechatExt, fakeMsg{Chat: "u1", ChatName: "我", ID: fmt.Sprintf("w%d", i), Sender: "u1", SenderName: "我", Mine: true, Text: text})
		waitFor(t, text+" to the Router", func() bool { return len(extTriggers(sp, "ext:"+wechatExt)) == i+1 })
	}
	time.Sleep(200 * time.Millisecond)
	if sent := sentLines(dir, wechatExt); len(sent) != 0 {
		t.Fatalf("Collie answered by itself: %v", sent)
	}
}

// A file too big for the extension goes as a link the hub serves for a day,
// signed for that one file: in the text before the footer for an answer, as
// the link of a result.
func TestExtBigFileLink(t *testing.T) {
	sp, h, dir := newExtTestHub(t)
	h.s.token, h.s.publicURLs = "tok", []string{"https://hub.example"}
	startWeChat(t, h)
	feed(t, dir, wechatExt, fakeMsg{Chat: "u1", ChatName: "我", ID: "w1", Sender: "u1", SenderName: "我", Mine: true, Text: "把日志发我"})
	waitFor(t, "a trigger", func() bool { return len(extTriggers(sp, "ext:"+wechatExt)) == 1 })

	stage := func(size int64) string {
		id := strings.Repeat("a", 31) + fmt.Sprint(size%10)
		p, ok := h.stagedPath(id)
		if !ok {
			t.Fatal("bad id")
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString("head")
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
		f.Close()
		return id
	}
	big := int64(extFileInlineMost + 1)
	o, err := h.sendOutFiles("ext:wechat:u1", "日志在这", extproto.KindAnswer, runInfo{}, "",
		[]sendFile{{Name: "big.log", Mime: "text/plain", ID: stage(big)}, {Name: "a.txt", Mime: "text/plain", Data: "aGk="}})
	if err != nil {
		t.Fatal(err)
	}
	link := o.Files[0].Link
	if !strings.HasPrefix(link, "https://hub.example/dl/"+o.Ref+"/0?exp=") || o.Files[1].Link != "" {
		t.Fatalf("links: %+v", o.Files)
	}
	if !strings.Contains(o.Text, link) {
		t.Fatalf("the link is in the text:\n%s", o.Text)
	}
	var sent []map[string]any
	waitFor(t, "sent", func() bool { sent = sentLines(dir, wechatExt); return len(sent) == 1 })
	if sent[0]["files"] != float64(1) || sent[0]["link"] != "" {
		t.Fatalf("the small file goes inline, the big one only as its link: %v", sent[0])
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("ref", strings.Split(r.URL.Path, "/")[2])
		r.SetPathValue("n", strings.Split(r.URL.Path, "/")[3])
		h.s.serveDownload(w, r)
	}))
	defer srv.Close()
	get := func(u string) *http.Response {
		t.Helper()
		resp, err := http.Get(srv.URL + strings.TrimPrefix(u, "https://hub.example"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := get(link); resp.StatusCode != 200 || resp.ContentLength != big || !strings.Contains(resp.Header.Get("Content-Disposition"), "big.log") {
		t.Fatalf("the link serves the file: %d %d %v", resp.StatusCode, resp.ContentLength, resp.Header)
	}
	u, _ := url.Parse(link)
	q := u.Query()
	for name, bad := range map[string]string{
		"another file": strings.Replace(link, "/0?", "/1?", 1),
		"a later end":  strings.Replace(link, "exp="+q.Get("exp"), fmt.Sprintf("exp=%d", time.Now().Add(48*time.Hour).Unix()), 1),
		"a bad sig":    strings.Replace(link, "sig="+q.Get("sig"), "sig="+strings.Repeat("0", 64), 1),
		"another send": strings.Replace(link, o.Ref, "o0000000000000000", 1),
	} {
		if resp := get(bad); resp.StatusCode != 404 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	// Past its day, the same link is gone.
	exp := time.Now().Add(-time.Minute).Unix()
	old := fmt.Sprintf("/dl/%s/0?exp=%d&sig=%x", o.Ref, exp, h.s.downloadSig(o.Ref, 0, exp))
	if resp := get(old); resp.StatusCode != 404 {
		t.Fatalf("an expired link: %d", resp.StatusCode)
	}

	r, err := h.sendOutFiles("ext:wechat:u1", "结果太大", extproto.KindResult, runInfo{}, "", []sendFile{{Name: "dump.bin", ID: stage(big + 1)}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the result sent", func() bool { sent = sentLines(dir, wechatExt); return len(sent) == 2 })
	if sent[1]["link"] != r.Files[0].Link || sent[1]["files"] != float64(0) || strings.Contains(r.Text, "下载链接") {
		t.Fatalf("a result carries the link itself: %v\n%s", sent[1], r.Text)
	}

	h.s.publicURLs = nil
	if _, err := h.sendOutFiles("ext:wechat:u1", "x", extproto.KindAnswer, runInfo{}, "", []sendFile{{Name: "big.log", ID: stage(big + 2)}}); err == nil {
		t.Fatal("no address to link to, and yet sent")
	}
}

// The number the phone shows reaches the extension with the step it was
// asked for, through the sign-in API.
func TestExtLoginSecretReachesExtension(t *testing.T) {
	_, h, dir := newExtTestHub(t)
	startWeChat(t, h)
	body, _ := json.Marshal(map[string]string{"step": "code", "secret": "246810"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/ext/wechat/login", bytes.NewReader(body))
	r.SetPathValue("id", wechatExt)
	h.s.extLogin(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"done":true`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	b, err := os.ReadFile(filepath.Join(dir, "ext", wechatExt, "secrets.json"))
	if err != nil || !strings.Contains(string(b), "MjQ2ODEw") { // base64 of 246810, as the fake saves it
		t.Fatalf("the secret never reached the extension: %s %v", b, err)
	}
}

// `collie reply --file` uploads each file ahead, streamed, and the reply
// names it: a file too big for a JSON body still gets there.
func TestReplyFileUploadsAhead(t *testing.T) {
	sp, h, dir := newExtTestHub(t)
	h.s.token, h.s.publicURLs = "tok", []string{"https://hub.example"}
	startWeChat(t, h)
	feed(t, dir, wechatExt, fakeMsg{Chat: "u1", ChatName: "我", ID: "w1", Sender: "u1", SenderName: "我", Mine: true, Text: "发我截图"})
	waitFor(t, "a trigger", func() bool { return len(extTriggers(sp, "ext:"+wechatExt)) == 1 })
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/super/reply-files", h.s.stageReplyFile)
	mux.HandleFunc("POST /api/super/reply", h.s.superReply)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "addr"), []byte(srv.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	pic := filepath.Join(t.TempDir(), "shot.png")
	data := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("x"), 2<<20)...) // past the 1 MB JSON body
	if err := os.WriteFile(pic, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cliReply([]string{"--thread", "ext:wechat:u1", "--file", pic, "截图在这"}); err != nil {
		t.Fatal(err)
	}
	var sent []map[string]any
	waitFor(t, "sent", func() bool { sent = sentLines(dir, wechatExt); return len(sent) == 1 })
	if sent[0]["files"] != float64(1) {
		t.Fatalf("the picture went with it: %v", sent[0])
	}
	o := h.store.out(fmt.Sprint(sent[0]["ref"]))
	if o == nil || len(o.Files) != 1 || o.Files[0].Mime != "image/png" || o.Files[0].Name != "shot.png" {
		t.Fatalf("%+v", o)
	}
	if left, _ := os.ReadDir(h.stagedDir()); len(left) != 0 {
		t.Fatalf("the upload stays staged: %v", left)
	}
	if err := cliReply([]string{"--thread", "ext:wechat:u1", "--file", filepath.Join(dir, "nope"), "x"}); err == nil {
		t.Fatal("a missing file was sent")
	}
}

// WeChat answers go without the footer (Feishu keeps it), and the bound
// person's message is a request for the Router, not a candidate.
func TestWeChatNoFooterAndRequest(t *testing.T) {
	sp, h, dir := newExtTestHub(t)
	startWeChat(t, h)
	feed(t, dir, wechatExt, fakeMsg{Chat: "u1", ChatName: "我", ID: "w1", Sender: "u1", SenderName: "我", Mine: true, Text: "你好"})
	var ts []*Trigger
	waitFor(t, "a trigger", func() bool { ts = extTriggers(sp, "ext:"+wechatExt); return len(ts) == 1 })
	sp.mu.Lock()
	action := ts[0].Action
	sp.mu.Unlock()
	if action != "respond" {
		t.Fatalf("your own message is a %s", action)
	}
	o, err := h.sendOut("ext:wechat:u1", "你好呀", extproto.KindAnswer, runInfo{Agent: "Claude"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if o.Text != "你好呀" || o.Footer != "" {
		t.Fatalf("a WeChat answer carries no footer: %q %q", o.Text, o.Footer)
	}
	if f, err := h.sendOut("ext:wechat:u1", "结果", extproto.KindResult, runInfo{}, ""); err != nil || f.Text != "结果" {
		t.Fatalf("nor a result: %+v %v", f, err)
	}

	// A rule set for the source still decides.
	sp.mu.Lock()
	sp.st.Policies = []Policy{{Source: "ext:" + wechatExt, Action: "candidate"}}
	sp.mu.Unlock()
	feed(t, dir, wechatExt, fakeMsg{Chat: "u1", ChatName: "我", ID: "w2", Sender: "u1", SenderName: "我", Mine: true, Text: "再说一句"})
	waitFor(t, "a second trigger", func() bool { ts = extTriggers(sp, "ext:"+wechatExt); return len(ts) == 2 })
	sp.mu.Lock()
	action = ts[1].Action
	sp.mu.Unlock()
	if action != "candidate" {
		t.Fatalf("the rule says candidate, got %s", action)
	}
}
