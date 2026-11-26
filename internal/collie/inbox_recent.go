package collie

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

const (
	capInboxRecent  = "inbox_recent"
	inboxRecentWait = 8 * time.Second
	inboxMediaMost  = 20 << 20
)

func (s *Server) inboxRecent(ctx context.Context, c *InboxConv, stored []InboxMsg, outs map[string]*OutboxItem) ([]InboxMsg, bool) {
	if m := s.ext.manifest(c.Ext); m == nil || !m.has(capInboxRecent) || c.Kind == "doc" {
		return stored, false
	}
	run := s.ext.run(c.Ext)
	if run == nil || !run.running() {
		return stored, true
	}
	ctx, cancel := context.WithTimeout(ctx, inboxRecentWait)
	defer cancel()
	var recent extproto.InboxRecentResult
	if err := run.call(ctx, extproto.MInboxRecent, extproto.InboxRecentRequest{ChatID: c.Chat}, &recent); err != nil {
		log.Printf("ext %s: inbox history: %v", c.Ext, err)
		return stored, true
	}
	s.repairInboxPreview(c, recent.Messages)
	merged := mergeInboxRecent(*c, stored, recent.Messages, outs, s.ext.footer())
	// The chat app may have a message newer than the list shows (one that
	// came while nothing passed it on): the list follows it.
	if at, preview := latestLine(merged); at.After(c.LastAt) {
		s.ext.store.noteLatest(c.ID, at, preview)
		if cur, ok := s.ext.store.conv(c.ID); ok {
			c.LastAt, c.Preview = cur.LastAt, cur.Preview
		}
	}
	return merged, false
}

// latestLine is the newest message as the list shows it: who sent it (AI
// 代答 for Collie's answers from your account) and its first words.
func latestLine(msgs []InboxMsg) (time.Time, string) {
	if len(msgs) == 0 {
		return time.Time{}, ""
	}
	last := msgs[len(msgs)-1]
	if last.Ref != "" || last.Bot {
		text := strings.TrimSpace(last.Text)
		if i := strings.LastIndex(text, "\n\n"); i > 0 {
			text = text[:i] // the footer, its last paragraph
		}
		return last.At, inboxPreview(text, aiSender, false)
	}
	return last.At, inboxPreview(last.Text, last.SenderName, last.Mine)
}

// Replace an old placeholder only when the source still has that last message.
func (s *Server) repairInboxPreview(c *InboxConv, messages []extproto.Message) {
	if c.Ext != "larkx" || !legacyInboxPreview.MatchString(c.Preview) {
		return
	}
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.ChatID != c.Chat || m.Preview == "" || m.View == nil || !parseAt(m.At).Equal(c.LastAt) ||
			(m.View.Kind != "card" && m.View.Kind != "calendar") {
			continue
		}
		preview := inboxPreview(m.Preview, m.SenderName, m.Owner != nil && *m.Owner)
		if preview == c.Preview {
			return
		}
		current, ok := s.ext.store.update(c.ID, func(current *InboxConv) {
			if current.LastAt.Equal(c.LastAt) && current.Preview == c.Preview {
				current.Preview = preview
			}
		})
		if ok {
			c.Preview = current.Preview
		}
		return
	}
}

var legacyInboxPreview = regexp.MustCompile(`\[卡片\]$|\[(日历|分享日程)\] [\[{]`)

func mergeInboxRecent(c InboxConv, stored []InboxMsg, recent []extproto.Message, outs map[string]*OutboxItem, footer string) []InboxMsg {
	ids := make(map[string]int, len(stored))
	for i, m := range stored {
		ids[m.ID] = i
		if o := outs[m.Ref]; o != nil && o.MessageID != "" {
			ids[o.MessageID] = i
		}
	}
	for _, m := range recent {
		if m.ID == "" || m.ChatID != c.Chat {
			continue
		}
		if i, ok := ids[m.ID]; ok {
			if stored[i].Ref == "" && m.View != nil {
				stored[i].View = m.View
			}
			stored[i].Avatar = m.SenderAvatar
			continue
		}
		at := parseAt(m.At)
		if at.IsZero() {
			continue
		}
		ids[m.ID] = len(stored)
		mine := m.Owner != nil && *m.Owner
		stored = append(stored, InboxMsg{ID: m.ID, Conv: c.ID, At: at, Sender: m.Sender, SenderName: m.SenderName,
			Avatar: m.SenderAvatar, Mine: mine, Bot: mine && endsInFooter(m.Text, footer), Text: m.Text, View: m.View})
	}
	slices.SortStableFunc(stored, func(a, b InboxMsg) int { return a.At.Compare(b.At) })
	if len(stored) > inboxKeepMsgs {
		stored = stored[len(stored)-inboxKeepMsgs:]
	}
	return stored
}

var inboxMediaKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

func (s *Server) inboxMediaSig(conv, mid, key, kind string) string {
	mac := hmac.New(sha256.New, []byte("collie-inbox-media\n"+s.token))
	fmt.Fprintf(mac, "%q %q %q %q", conv, mid, key, kind)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) inboxMediaURLs(conv string, m *InboxMsg) {
	if m.View == nil {
		return
	}
	var walk func([]extproto.MessageNode)
	walk = func(nodes []extproto.MessageNode) {
		for i := range nodes {
			n := &nodes[i]
			if n.Kind == "image" || n.Kind == "file" {
				n.URL = ""
				if inboxMediaKey.MatchString(n.Key) {
					q := url.Values{"id": {conv}, "message": {m.ID}, "key": {n.Key}, "kind": {n.Kind},
						"name": {clip(n.Text, 240)}, "sig": {s.inboxMediaSig(conv, m.ID, n.Key, n.Kind)}}
					n.URL = "/api/inbox/media?" + q.Encode()
				}
				n.Key = ""
			}
			walk(n.Children)
		}
	}
	walk(m.View.Nodes)
}

func (s *Server) inboxMedia(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, mid, key, kind := q.Get("id"), q.Get("message"), q.Get("key"), q.Get("kind")
	c, ok := s.ext.store.conv(id)
	if !ok || (kind != "image" && kind != "file") || !inboxMediaKey.MatchString(key) ||
		!hmac.Equal([]byte(q.Get("sig")), []byte(s.inboxMediaSig(id, mid, key, kind))) {
		http.NotFound(w, r)
		return
	}
	run := s.ext.run(c.Ext)
	if run == nil || !run.man.has(capInboxRecent) || !run.running() {
		http.Error(w, "飞书扩展暂不可用", http.StatusServiceUnavailable)
		return
	}
	var blob extproto.Blob
	if err := run.call(r.Context(), extproto.MInboxMedia, extproto.InboxMediaRequest{ChatID: c.Chat, MessageID: mid, Key: key}, &blob); err != nil {
		log.Printf("ext %s: inbox media: %v", c.Ext, err)
		http.Error(w, "无法读取附件", http.StatusBadGateway)
		return
	}
	if len(blob.Data) > base64.StdEncoding.EncodedLen(inboxMediaMost) {
		http.Error(w, "附件过大", http.StatusBadGateway)
		return
	}
	data, err := base64.StdEncoding.DecodeString(blob.Data)
	if err != nil || len(data) == 0 || len(data) > inboxMediaMost {
		http.Error(w, "附件内容无效", http.StatusBadGateway)
		return
	}
	mime := http.DetectContentType(data)
	if kind == "image" {
		if _, ok := imageExt[mime]; !ok {
			http.Error(w, "无法显示此图片格式", http.StatusUnsupportedMediaType)
			return
		}
	} else {
		mime = "application/octet-stream"
		name := strings.TrimSpace(clip(q.Get("name"), 240))
		if name == "" {
			name = "飞书附件"
		}
		w.Header().Set("Content-Disposition", attachmentDisposition(name))
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}
