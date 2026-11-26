package wechat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// wxaStateWith is a StateManager for account whose state file is in dir.
func wxaStateWith(t *testing.T, dir string, account *Account) *StateManager {
	t.Helper()
	s, err := NewStateManager(account, filepath.Join(dir, stateFileName), PolicyOptions{})
	mustNoErr(t, err)
	return s
}

func wxaPoll(t *testing.T, c *ILinkClient) PollBatch {
	t.Helper()
	var b PollBatch
	var err error
	c.loop.do(func() { b, err = c.pollBatch(context.Background()) })
	mustNoErr(t, err)
	return b
}

// from: gateway/tests/wechat/ilink-url.test.ts: iLink rejects unsafe account and explicit URLs before any credential-bearing request
func TestWxaILinkRejectsUnsafeURLs(t *testing.T) {
	urls := []string{"http://remote.example", "http://127.0.0.1", "ftp://remote.example", "not-a-url",
		"https://user:private-value@example.com", "https://example.com?token=private-value",
		"https://example.com#private-value", "https://exam\nple.com", "https://example.com\\path"}
	requests := 0
	hc := fakeHTTP(func(*http.Request, []byte) (*http.Response, error) {
		requests++
		return nil, errors.New("unexpected network call")
	})
	for _, u := range urls {
		for _, explicit := range []bool{false, true} {
			account := &Account{Token: "fixture-token", AccountID: "fixture"}
			o := ILinkOptions{HTTP: hc}
			if explicit {
				o.ConfiguredBaseURL = u
			} else {
				account.BaseURL = u
			}
			s := wxaStateWith(t, t.TempDir(), account)
			_, err := NewILinkClient(s, &loop{}, o)
			if err == nil || strings.Contains(err.Error(), "private-value") || !strings.Contains(err.Error(), "iLink base URL") {
				t.Fatalf("%q explicit=%v: error %v", u, explicit, err)
			}
		}
	}
	if requests != 0 {
		t.Fatalf("%d requests", requests)
	}
}

// from: gateway/tests/wechat/ilink-url.test.ts: iLink preserves HTTPS regional/path endpoints and configured override, and requests redirect rejection
func TestWxaILinkPreservesEndpoints(t *testing.T) {
	c := newTestClient(t, wxaStateWith(t, t.TempDir(), &Account{Token: "fixture-token", AccountID: "fixture"}), &loop{}, nil)
	if c.BaseURL != DefaultILinkBaseURL {
		t.Fatalf("default base %q", c.BaseURL)
	}
	c = newTestClient(t, wxaStateWith(t, t.TempDir(), &Account{Token: "fixture-token", AccountID: "fixture", BaseURL: "https://region.example/"}), &loop{}, nil)
	if c.BaseURL != "https://region.example" {
		t.Fatalf("regional base %q", c.BaseURL)
	}
	calls := 0
	s := wxaStateWith(t, t.TempDir(), &Account{Token: "fixture-token", AccountID: "fixture", BaseURL: "https://old.example"})
	c, err := NewILinkClient(s, &loop{}, ILinkOptions{ConfiguredBaseURL: "https://region.example/proxy/",
		HTTP: fakeHTTP(func(req *http.Request, _ []byte) (*http.Response, error) {
			calls++
			if req.URL.String() != "https://region.example/proxy/ilink/bot/getupdates" || req.Header.Get("Authorization") != "Bearer fixture-token" {
				t.Errorf("request %s %q", req.URL, req.Header.Get("Authorization"))
			}
			return rawResponse(200, `{"ret":0,"msgs":[],"get_updates_buf":"next"}`), nil
		})})
	mustNoErr(t, err)
	// fetch's redirect:"error" is the client refusing to follow any redirect.
	if c.http.CheckRedirect == nil || c.http.CheckRedirect(nil, nil) == nil {
		t.Fatal("redirects are followed")
	}
	if b := wxaPoll(t, c); b.NextSyncBuf != "next" || calls != 1 {
		t.Fatalf("next %q calls %d", b.NextSyncBuf, calls)
	}
}

func wxaText(v string, extra map[string]any) map[string]any {
	m := map[string]any{"type": json.Number("1"), "text_item": map[string]any{"text": v}}
	for k, x := range extra {
		m[k] = x
	}
	return m
}

func wxaVoice(transcript *string) map[string]any {
	item := map[string]any{"playtime": json.Number("1200")}
	if transcript != nil {
		item["text"] = *transcript
	}
	return map[string]any{"type": json.Number("3"), "voice_item": item}
}

func wxaStr(s string) *string { return &s }

var wxaImage = map[string]any{"type": json.Number("2"), "image_item": map[string]any{"mid_size": json.Number("16")}}

// wxaItems decodes items the way a poll does, numbers as json.Number.
func wxaItems(t *testing.T, items ...any) []any {
	t.Helper()
	raw, err := json.Marshal(items)
	mustNoErr(t, err)
	parsed, err := ParseILinkJSON(string(raw))
	mustNoErr(t, err)
	return parsed.([]any)
}

// from: gateway/tests/wechat/inbound-content.test.ts: voice speech-to-text becomes the message text
func TestWxaVoiceTranscriptIsText(t *testing.T) {
	got := ParseInboundContent(wxaItems(t, wxaVoice(wxaStr("帮我看下今天的部署"))))
	if !reflect.DeepEqual(got, &InboundContent{Text: "帮我看下今天的部署", OwnText: "帮我看下今天的部署"}) {
		t.Fatalf("%+v", got)
	}
}

// from: gateway/tests/wechat/inbound-content.test.ts: quoted text or voice is prefixed, delimited and truncated to the named limit
func TestWxaQuotePrefix(t *testing.T) {
	quoted := ParseInboundContent(wxaItems(t, wxaText("这个结论对吗？", map[string]any{"ref_msg": map[string]any{"title": "摘要", "message_item": wxaText("昨天\n上线了 3 个服务", nil)}})))
	// Quote is an added field: the quoted text, also passed to Collie on its own.
	if quoted.Text != "引用：「昨天 上线了 3 个服务」\n这个结论对吗？" || quoted.OwnText != "这个结论对吗？" || quoted.Quote != "昨天 上线了 3 个服务" {
		t.Fatalf("%+v", quoted)
	}
	if got := ParseInboundContent(wxaItems(t, wxaText("继续", map[string]any{"ref_msg": map[string]any{"message_item": wxaVoice(wxaStr("先查日志"))}}))); got.Text != "引用：「先查日志」\n继续" {
		t.Fatalf("voice quote %q", got.Text)
	}
	if got := ParseInboundContent(wxaItems(t, wxaText("看看这个", map[string]any{"ref_msg": map[string]any{"title": "季度报告.pdf", "message_item": map[string]any{"type": 4}}}))); got.Text != "引用：「季度报告.pdf」\n看看这个" {
		t.Fatalf("title quote %q", got.Text)
	}
	long := ParseInboundContent(wxaItems(t, wxaText("总结", map[string]any{"ref_msg": map[string]any{"message_item": wxaText(strings.Repeat("字", QuoteMaxChars-1)+"🦊尾巴", nil)}})))
	if long.Text != "引用：「"+strings.Repeat("字", QuoteMaxChars-1)+"…」\n总结" {
		t.Fatalf("truncation splits a surrogate pair: %q", long.Text)
	}
	if got := ParseInboundContent(wxaItems(t, wxaText("没有可引用内容", map[string]any{"ref_msg": map[string]any{"message_item": wxaImage}}))); got.Text != "没有可引用内容" {
		t.Fatalf("image quote %q", got.Text)
	}
}

// from: gateway/tests/wechat/inbound-content.test.ts: messages without usable text keep typed unsupported kinds
func TestWxaUnsupportedKinds(t *testing.T) {
	if got := ParseInboundContent(wxaItems(t, wxaImage)); !reflect.DeepEqual(got, &InboundContent{UnsupportedKinds: []string{"image"}}) {
		t.Fatalf("%+v", got)
	}
	got := ParseInboundContent(wxaItems(t, wxaVoice(nil), map[string]any{"type": 4}, map[string]any{"type": 5}, map[string]any{"type": 99}, wxaImage))
	if !reflect.DeepEqual(got, &InboundContent{UnsupportedKinds: []string{"voice", "file", "video", "unknown", "image"}}) {
		t.Fatalf("%+v", got)
	}
	if ParseInboundContent(nil) != nil || ParseInboundContent(wxaItems(t, wxaText("", nil), "not-an-item", nil)) != nil {
		t.Fatal("empty content parsed")
	}
}

// from: gateway/tests/wechat/inbound-content.test.ts: mixed text and media forwards the text only
func TestWxaMixedTextAndMedia(t *testing.T) {
	got := ParseInboundContent(wxaItems(t, wxaImage, wxaText("图里是什么问题？", nil)))
	// IgnoredKinds is an added field: Collie is told what was left out.
	if !reflect.DeepEqual(got, &InboundContent{Text: "图里是什么问题？", OwnText: "图里是什么问题？", IgnoredKinds: []string{"image"}}) {
		t.Fatalf("%+v", got)
	}
}

// from: gateway/tests/wechat/inbound-content.test.ts: pollBatch keeps voice, quoted and image-only messages with stable identities
func TestWxaPollKeepsStableIdentities(t *testing.T) {
	s := wxaStateWith(t, t.TempDir(), &Account{Token: "synthetic-token", AccountID: "content-fixture"})
	message := func(id string, items ...any) map[string]any {
		m := map[string]any{"message_type": 1, "from_user_id": "chat-a", "create_time_ms": 1_800_000_000_000, "item_list": items}
		if id != "" {
			m["client_id"] = id
		}
		return m
	}
	msgs := []any{message("v", wxaVoice(wxaStr("语音转文字"))), message("q", wxaText("回复", map[string]any{"ref_msg": map[string]any{"message_item": wxaText("原文", nil)}})),
		message("i", wxaImage), message("", wxaImage), message("", wxaText("legacy", nil)), message("e")}
	poll := func() PollBatch {
		c, err := NewILinkClient(s, &loop{}, ILinkOptions{ConfiguredBaseURL: "https://ilink.invalid", HTTP: fakeHTTP(func(*http.Request, []byte) (*http.Response, error) {
			return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "next", "msgs": msgs}), nil
		})})
		mustNoErr(t, err)
		return wxaPoll(t, c)
	}
	batch := poll()
	if batch.RawMessageCount != 6 {
		t.Fatalf("raw %d", batch.RawMessageCount)
	}
	var got []string
	for _, m := range batch.Messages {
		id := m.MessageID
		if strings.HasPrefix(id, "det:") {
			id = "det"
		}
		got = append(got, fmt.Sprintf("%s|%s|%v", id, m.Text, m.UnsupportedKinds))
	}
	want := []string{"v|语音转文字|[]", "q|引用：「原文」\n回复|[]", "i||[image]", "det||[image]", "det|legacy|[]"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	digest := sha256Hex(fmt.Sprintf("content-fixture:chat-a:1800000000000:%s:4:legacy", s.SyncBuf))[:24]
	if batch.Messages[4].MessageID != "det:"+digest {
		t.Fatalf("text-only identity changed: %s", batch.Messages[4].MessageID)
	}
	again := poll()
	for i := range batch.Messages {
		if again.Messages[i].MessageID != batch.Messages[i].MessageID {
			t.Fatal("identities differ between polls")
		}
	}
}

const (
	wxaIdentityAccount = "identity-fixture"
	wxaServerID        = "18446744073709551615"
	wxaFixtureURL      = "https://ilink.invalid/ilink/bot/getupdates"
)

func wxaTextMessage(fields map[string]any) map[string]any {
	m := map[string]any{"message_type": 1, "from_user_id": "chat-a", "create_time_ms": 1_800_000_000_000,
		"item_list": []any{map[string]any{"type": 1, "text_item": map[string]any{"text": "same command"}}}}
	for k, v := range fields {
		m[k] = v
	}
	return m
}

func wxaIdentityClient(t *testing.T, s *StateManager, f fetchFunc) *ILinkClient {
	t.Helper()
	c, err := NewILinkClient(s, &loop{}, ILinkOptions{ConfiguredBaseURL: "https://ilink.invalid", HTTP: fakeHTTP(f)})
	mustNoErr(t, err)
	return c
}

func wxaIdentityAccountState(t *testing.T, dir string) *StateManager {
	return wxaStateWith(t, dir, &Account{Token: "synthetic-token", AccountID: wxaIdentityAccount})
}

// from: gateway/tests/wechat/inbound-identity.test.ts: server ${encoding} ID deduplicates a moved replay after restart and stays isolated by chat
func TestWxaServerIDDeduplicatesReplay(t *testing.T) {
	for _, encoding := range []string{"string", "integer"} {
		t.Run(encoding, func(t *testing.T) {
			dir := t.TempDir()
			s := wxaIdentityAccountState(t, dir)
			mustNoErr(t, s.CommitCursor("cursor-initial"))
			wire := func(chat string) string {
				b, _ := json.Marshal(wxaTextMessage(map[string]any{"message_id": wxaServerID, "from_user_id": chat}))
				out := string(b)
				if encoding == "integer" {
					out = strings.Replace(out, `"message_id":"`+wxaServerID+`"`, `"message_id":`+wxaServerID, 1)
				}
				return out
			}
			var cursors []string
			var mu sync.Mutex
			poll := func(state *StateManager, messages []string, next string) PollBatch {
				return wxaPoll(t, wxaIdentityClient(t, state, func(req *http.Request, body []byte) (*http.Response, error) {
					if req.URL.String() != wxaFixtureURL {
						t.Errorf("url %s", req.URL)
					}
					mu.Lock()
					cursors = append(cursors, decodeBody(t, body)["get_updates_buf"].(string))
					mu.Unlock()
					nb, _ := json.Marshal(next)
					return rawResponse(200, `{"ret":0,"get_updates_buf":`+string(nb)+`,"msgs":[`+strings.Join(messages, ",")+`]}`), nil
				}))
			}
			original := poll(s, []string{wire("chat-a")}, "cursor-next")
			if len(original.Messages) != 1 || original.Messages[0].MessageID != "server:"+wxaServerID {
				t.Fatalf("%+v", original.Messages)
			}
			key := original.Messages[0].DeliveryKey
			if key != jsonText([]string{wxaIdentityAccount, "chat-a", "server:" + wxaServerID}) {
				t.Fatalf("key %s", key)
			}
			mustNoErr(t, s.RecordProcessedMessageKey(key, false))
			mustNoErr(t, s.CommitCursor(original.NextSyncBuf))
			reloaded := wxaIdentityAccountState(t, dir)
			replay := poll(reloaded, []string{`{"message_type":2}`, wire("chat-b"), wire("chat-a")}, "cursor-last")
			if len(replay.Messages) != 1 || replay.Messages[0].ChatID != "chat-b" || replay.Messages[0].MessageID != "server:"+wxaServerID || replay.Messages[0].DeliveryKey == key {
				t.Fatalf("%+v", replay.Messages)
			}
			mustNoErr(t, reloaded.RecordProcessedMessageKey(replay.Messages[0].DeliveryKey, false))
			mustNoErr(t, reloaded.CommitCursor(replay.NextSyncBuf))
			if n := len(poll(wxaIdentityAccountState(t, dir), []string{wire("chat-a"), wire("chat-b")}, "cursor-empty").Messages); n != 0 {
				t.Fatalf("%d replayed", n)
			}
			if !reflect.DeepEqual(cursors, []string{"cursor-initial", "cursor-next", "cursor-last"}) {
				t.Fatalf("cursors %v", cursors)
			}
		})
	}
}

// from: gateway/tests/wechat/inbound-identity.test.ts: old processed-message checkpoints retain client identities when a server ID is now present
func TestWxaOldCheckpointsKeepClientIdentities(t *testing.T) {
	dir := t.TempDir()
	var keys []string
	for _, id := range []string{"legacy-client", "0", "server:123"} {
		keys = append(keys, jsonText([]string{wxaIdentityAccount, "chat-a", id}))
	}
	checkpoint, _ := json.Marshal(map[string]any{"sync_buf": "cursor-initial", "context_tokens": map[string]string{"chat-a": "synthetic-context"},
		"processed_message_keys": keys})
	path := filepath.Join(dir, stateFileName)
	mustNoErr(t, os.WriteFile(path, checkpoint, 0o600))
	restored := wxaIdentityAccountState(t, dir)
	updates := `{"ret":0,"get_updates_buf":"cursor-next","msgs":[` +
		`{"message_type":1,"from_user_id":"chat-a","create_time_ms":1800000000000,"client_id":"legacy-client","message_id":"` + wxaServerID + `","item_list":[{"type":1,"text_item":{"text":"same command"}}]},` +
		`{"message_type":1,"from_user_id":"chat-a","create_time_ms":1800000000000,"client_id":0,"message_id":"999","item_list":[{"type":1,"text_item":{"text":"same command"}}]},` +
		`{"message_type":1,"from_user_id":"chat-a","create_time_ms":1800000000000,"message_id":123,"item_list":[{"type":1,"text_item":{"text":"same command"}}]},` +
		`{"message_type":1,"from_user_id":"chat-a","create_time_ms":1800000000000,"client_id":"new-client","message_id":"456","item_list":[{"type":1,"text_item":{"text":"same command"}}]}]}`
	incoming := wxaPoll(t, wxaIdentityClient(t, restored, func(req *http.Request, body []byte) (*http.Response, error) {
		if decodeBody(t, body)["get_updates_buf"] != "cursor-initial" {
			t.Error("wrong cursor")
		}
		return rawResponse(200, updates), nil
	}))
	if len(incoming.Messages) != 1 || incoming.Messages[0].MessageID != "new-client" {
		t.Fatalf("%+v", incoming.Messages)
	}
	if raw, _ := os.ReadFile(path); string(raw) != string(checkpoint) {
		t.Fatal("polling rewrote a legacy checkpoint")
	}
	mustNoErr(t, restored.CommitCursor(incoming.NextSyncBuf))
	reloaded := wxaIdentityAccountState(t, dir)
	if !reflect.DeepEqual(reloaded.ProcessedMessageKeys, keys) || reloaded.SyncBuf != "cursor-next" || reloaded.ContextTokens["chat-a"] != "synthetic-context" {
		t.Fatalf("reloaded %v %q %v", reloaded.ProcessedMessageKeys, reloaded.SyncBuf, reloaded.ContextTokens)
	}
}

// from: gateway/tests/wechat/inbound-identity.test.ts: messages without stable IDs preserve distinct commands and deduplicate only the same page position
func TestWxaDeterministicIDsByPagePosition(t *testing.T) {
	dir := t.TempDir()
	s := wxaIdentityAccountState(t, dir)
	mustNoErr(t, s.CommitCursor("cursor-initial"))
	poll := func(state *StateManager) PollBatch {
		return wxaPoll(t, wxaIdentityClient(t, state, func(*http.Request, []byte) (*http.Response, error) {
			return jsonResponse(200, map[string]any{"ret": 0, "get_updates_buf": "cursor-next", "msgs": []any{wxaTextMessage(nil), wxaTextMessage(nil)}}), nil
		}))
	}
	original := poll(s)
	if len(original.Messages) != 2 || original.Messages[0].MessageID == original.Messages[1].MessageID ||
		!strings.HasPrefix(original.Messages[0].MessageID, "det:") || !strings.HasPrefix(original.Messages[1].MessageID, "det:") {
		t.Fatalf("%+v", original.Messages)
	}
	if !reflect.DeepEqual(poll(wxaIdentityAccountState(t, dir)).Messages, original.Messages) {
		t.Fatal("same page position gave other identities")
	}
	mustNoErr(t, s.RecordProcessedMessageKey(original.Messages[0].DeliveryKey, false))
	if got := poll(s).Messages; !reflect.DeepEqual(got, original.Messages[1:]) {
		t.Fatalf("%+v", got)
	}
	mustNoErr(t, s.CommitCursor(original.NextSyncBuf))
	another := poll(wxaIdentityAccountState(t, dir))
	if len(another.Messages) != 2 {
		t.Fatal("equal text alone cannot prove that another page is a redelivery")
	}
	for _, m := range another.Messages {
		for _, p := range original.Messages {
			if m.MessageID == p.MessageID {
				t.Fatal("identity reused across pages")
			}
		}
	}
}
