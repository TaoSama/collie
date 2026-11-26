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
)

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

// startWeChat runs the fake extension under the id wechat, signed in.
func startWeChat(t *testing.T, h *extHost) {
	t.Helper()
	h.mu.Lock()
	h.cat[wechatExt] = fakeManifest(wechatExt, "receive")
	h.mu.Unlock()
	t.Cleanup(func() { h.configure(wechatExt, func(x *ExtSetting) { x.Enabled = false }) })
	login(t, startExt(t, h, wechatExt))
}

// The session list asked for on its own is answered by Collie at once,
// as the answer to that message; anything more goes to the Router.
func TestWeChatDirectoryQuickPath(t *testing.T) {
	sp, h, dir := newExtTestHub(t)
	now := time.Now()
	sp.mu.Lock()
	sp.st.Workers["fix-login"] = &Worker{Name: "fix-login", Kind: "claude", Status: "working", Cwd: "/w/collie", Task: "修登录\n细节", CreatedAt: now}
	sp.st.Workers["docs"] = &Worker{Name: "docs", Kind: "codex", Status: "idle", Cwd: "/w/collie", Task: "写文档", CreatedAt: now.Add(-time.Hour)}
	sp.st.Workers["old"] = &Worker{Name: "old", Kind: "codex", Status: "gone", Ended: true, Cwd: "/w/x", Task: "旧的", CreatedAt: now}
	sp.mu.Unlock()
	startWeChat(t, h)
	feed(t, dir, wechatExt, fakeMsg{Chat: "u1", ChatName: "我", ID: "w1", Sender: "u1", SenderName: "我", Mine: true, Text: "查看会话列表"})
	var sent []map[string]any
	waitFor(t, "the list sent", func() bool { sent = sentLines(dir, wechatExt); return len(sent) == 1 })
	text := fmt.Sprint(sent[0]["text"])
	for _, want := range []string{"collie（", "1. [忙] fix-login：修登录", "2. [闲] docs：写文档", "Collie · 会话列表"} {
		if !strings.Contains(text, want) {
			t.Errorf("the list lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "old") || sent[0]["kind"] != extproto.KindAnswer {
		t.Errorf("an ended worker listed, or not an answer: %v", sent[0])
	}
	if o := h.store.out(fmt.Sprint(sent[0]["ref"])); o == nil || o.ReplyTo != "w1" {
		t.Fatalf("the list answers w1: %+v", o)
	}
	if n := len(extTriggers(sp, "ext:"+wechatExt)); n != 0 {
		t.Fatalf("the list went to the Router too: %d triggers", n)
	}

	feed(t, dir, wechatExt, fakeMsg{Chat: "u1", ChatName: "我", ID: "w2", Sender: "u1", SenderName: "我", Mine: true, Text: "有哪些空闲会话"})
	waitFor(t, "the idle list", func() bool { sent = sentLines(dir, wechatExt); return len(sent) == 2 })
	if text := fmt.Sprint(sent[1]["text"]); strings.Contains(text, "fix-login") || !strings.Contains(text, "docs") {
		t.Fatalf("idle only:\n%s", text)
	}

	feed(t, dir, wechatExt, fakeMsg{Chat: "u1", ChatName: "我", ID: "w3", Sender: "u1", SenderName: "我", Mine: true, Text: "列出会话并发送"})
	waitFor(t, "a request to the Router", func() bool { return len(extTriggers(sp, "ext:"+wechatExt)) == 1 })
	if len(sentLines(dir, wechatExt)) != 2 {
		t.Fatal("a compound request was answered as the list")
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
	if i, j := strings.Index(o.Text, link), strings.Index(o.Text, o.Footer); i < 0 || j < i {
		t.Fatalf("the link goes before the footer:\n%s", o.Text)
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
