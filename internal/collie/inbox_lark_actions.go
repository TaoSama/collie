package collie

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/TaoSama/collie/internal/extproto"
)

const capLarkInbox = "lark_inbox"

var larkInboxID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (s *Server) larkInboxRun(c InboxConv) *extRun {
	if c.Ext != larkxID || c.Kind == "doc" || c.Owner {
		return nil
	}
	run := s.ext.run(c.Ext)
	if run == nil || !run.running() || !run.man.has(capLarkInbox) {
		return nil
	}
	return run
}

func (s *Server) larkInboxMessageID(c InboxConv, id string) string {
	if ref, ok := strings.CutPrefix(id, "out:"); ok {
		o := s.ext.store.out(ref)
		if o == nil || o.Conv != c.ID || o.State != extproto.DeliverySent {
			return ""
		}
		id = o.MessageID
	}
	if !larkInboxID.MatchString(id) {
		return ""
	}
	return id
}

func (s *Server) larkReplyTarget(c InboxConv, id, mode string) (string, error) {
	if s.larkInboxRun(c) == nil {
		return "", fmt.Errorf("飞书扩展暂不支持这个回复方式")
	}
	switch mode {
	case "publish":
		if id == "" && c.Kind == "group" {
			return "", nil
		}
	case "quote", "create_topic", "comment":
		if mode == "quote" || c.Kind == "group" {
			if target := s.larkInboxMessageID(c, id); target != "" {
				// The connector checks the message's native chat membership before sending.
				return target, nil
			}
		}
	}
	return "", fmt.Errorf("无效的飞书回复目标")
}

func (s *Server) inboxLarkAction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
		extproto.InboxLarkActionRequest
	}
	if !decode(w, r, &body) {
		return
	}
	c, ok := s.ext.store.conv(body.ID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "找不到这个会话"})
		return
	}
	run := s.larkInboxRun(c)
	if run == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "飞书扩展暂不可用"})
		return
	}
	req := body.InboxLarkActionRequest
	req.ChatID = c.Chat
	if req.MessageID != "" {
		req.MessageID = s.larkInboxMessageID(c, req.MessageID)
		if req.MessageID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "这条飞书消息暂不可操作"})
			return
		}
	}
	if !validLarkInboxAction(req) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无效的飞书消息或话题操作"})
		return
	}
	if req.Action == "forward" {
		dst, ok := s.ext.store.conv(req.Target)
		if !ok || dst.Ext != c.Ext || dst.Owner || dst.Kind == "doc" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无法转发到这个会话"})
			return
		}
		req.Target = dst.Chat
	}
	ctx, cancel := context.WithTimeout(r.Context(), inboxActionWait)
	defer cancel()
	var res extproto.InboxLarkActionResult
	// Native action IDs are checked against the actual chat by the connector.
	if err := run.call(ctx, extproto.MInboxLarkAction, req, &res); err != nil {
		log.Printf("ext %s: inbox %s: %v", c.Ext, req.Action, err)
		message := err.Error()
		if req.Action == "forward" && (extproto.IsCode(err, extproto.ErrTimeout) || extproto.IsCode(err, extproto.ErrClosed)) {
			message = "没有收到转发回执，消息可能已经转发，请到飞书确认"
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": message})
		return
	}
	if req.Action == "topic" {
		msgs := mergeInboxRecent(c, []InboxMsg{}, res.Messages, nil, s.ext.footer())
		for i := range msgs {
			s.inboxMediaURLs(c.ID, &msgs[i])
			msgs[i].Avatar = s.senderAvatarPath(c.ID, msgs[i].Avatar)
		}
		writeJSON(w, http.StatusOK, map[string]any{"msgs": msgs})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func validLarkInboxAction(req extproto.InboxLarkActionRequest) bool {
	if req.ThreadID != "" && !larkInboxID.MatchString(req.ThreadID) {
		return false
	}
	switch req.Action {
	case "reaction":
		return req.MessageID != "" && req.Active != nil && larkInboxID.MatchString(req.Key) && req.Target == ""
	case "topic":
		return req.ThreadID != "" && req.Target == ""
	case "subscribe":
		return req.ThreadID != "" && req.Active != nil && req.Target == ""
	case "forward":
		return req.MessageID != "" && req.Target != ""
	default:
		return false
	}
}
