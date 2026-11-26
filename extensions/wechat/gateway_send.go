package wechat

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"

	"github.com/TaoSama/collie/internal/extproto"
)

// Reasons channel.send answers when a send waits.
const (
	reasonQueued   = "queued"
	reasonRetrying = "retrying"
	// maxProgressRefs bounds the refs remembered to answer a repeated
	// progress call without sending it again.
	maxProgressRefs = 1000
)

// HandleSend is channel.send. Answers and pushes go through the reply
// outbox, progress through the request's allowance, results through the
// result queue. The same ref is sent at most once: a repeated call is
// answered from what happened to the first.
func (g *Gateway) HandleSend(ctx context.Context, p extproto.Send) (res extproto.SendResult, err error) {
	g.loop.do(func() { res, err = g.handleSend(ctx, p) })
	return res, err
}

func (g *Gateway) handleSend(ctx context.Context, p extproto.Send) (extproto.SendResult, error) {
	if strings.TrimSpace(p.Ref) == "" || strings.TrimSpace(p.Chat) == "" || len(p.Ref) > inboxIDMaxBytes {
		return extproto.SendResult{}, extproto.Errorf(extproto.ErrInvalidParams, "channel.send needs ref and chat")
	}
	if !g.active() {
		if !g.stopped && !g.state.IsReadyForActivePolling() {
			return extproto.SendResult{}, extproto.Errorf(extproto.ErrNotReady, "WeChat is establishing its baseline")
		}
		return extproto.SendResult{}, extproto.Errorf(extproto.ErrNotSignedIn, "WeChat is not signed in")
	}
	ctx = withTrace(ctx, createMessageTrace(p.Chat, p.Ref))
	switch p.Kind {
	case extproto.KindAnswer, extproto.KindPush, "":
		return g.sendAnswer(ctx, p)
	case extproto.KindProgress:
		return g.sendProgress(ctx, p)
	case extproto.KindResult:
		return g.sendResult(p)
	}
	return extproto.SendResult{}, extproto.Errorf(extproto.ErrInvalidParams, "unknown send kind")
}

func decodeBlobs(blobs []extproto.Blob) ([][]byte, error) {
	out := make([][]byte, 0, len(blobs))
	for _, b := range blobs {
		data, err := base64.StdEncoding.DecodeString(b.Data)
		if err != nil || len(data) == 0 {
			return nil, extproto.Errorf(extproto.ErrInvalidParams, "attachment is not base64")
		}
		out = append(out, data)
	}
	return out, nil
}

// sendAnswer freezes the answer, puts it in the outbox and delivers it now
// when the window allows; otherwise it waits for the next message.
func (g *Gateway) sendAnswer(ctx context.Context, p extproto.Send) (extproto.SendResult, error) {
	if r := g.answers.get(p.Ref); r != nil && r.State != "" {
		return answerOutcome(r)
	}
	images, err := decodeBlobs(p.Images)
	if err != nil {
		return extproto.SendResult{}, err
	}
	files, err := decodeBlobs(p.Files)
	if err != nil {
		return extproto.SendResult{}, err
	}
	if len(images) > MaxReplyImages {
		return extproto.SendResult{}, extproto.Errorf(extproto.ErrInvalidParams, "at most %d pictures per reply", MaxReplyImages)
	}
	text := p.Text
	if p.Link != "" {
		text = strings.TrimRight(text, "\n") + "\n" + p.Link
	}
	frozen := FrozenReply{Ref: p.Ref, ChatID: p.Chat, ReplyTo: p.ReplyTo, Kind: p.Kind, Text: text, CreatedAt: nowMs()}
	for i, data := range images {
		a, err := imageAttachmentFor(strconv.Itoa(i), data)
		if err != nil {
			return extproto.SendResult{}, extproto.Errorf(extproto.ErrInvalidParams, "picture %d: %s", i+1, err.Error())
		}
		frozen.Images = append(frozen.Images, a)
	}
	for i, data := range files {
		name := safeFileName(p.Files[i].Name, defaultFileName)
		m := fileMetadataFor(name, data)
		frozen.Files = append(frozen.Files, FrozenFile{Name: name, Mime: p.Files[i].Mime, Size: m.ByteLength, SHA256: m.SHA256})
	}
	if strings.TrimSpace(text) == "" && len(frozen.Images) == 0 && len(frozen.Files) == 0 {
		return extproto.SendResult{}, extproto.Errorf(extproto.ErrInvalidParams, "nothing to send")
	}
	frozen.Identity = hashJSON([]any{frozen.ChatID, frozen.Text, frozen.Images, frozen.Files})
	if _, err := g.answers.put(frozen, images, files); err != nil {
		if errors.Is(err, errAnswerIdentity) {
			return extproto.SendResult{}, extproto.Errorf(extproto.ErrInvalidParams, "%s", err.Error())
		}
		return extproto.SendResult{}, extproto.Errorf(extproto.ErrCannotSend, "%s", SanitizeErrorType(err))
	}
	if p.Kind != extproto.KindPush {
		delete(g.awaiting, p.Chat)
	}
	messageID := p.ReplyTo
	if messageID == "" {
		messageID = p.Ref
	}
	if err := g.state.PutOutbox(DeliveryOutboxItem{Kind: "reply", Source: SourceEngine, ChatID: p.Chat, MessageID: messageID,
		DeliveryKey: p.Ref, SplitMaxChars: g.splitMaxChars, Status: OutboxPending}); err != nil {
		return extproto.SendResult{}, extproto.Errorf(extproto.ErrCannotSend, "%s", SanitizeErrorType(err))
	}
	item := g.state.Outbox[p.Ref]
	var sendErr error
	if g.state.CanSendToChat(p.Chat, SendFinal) {
		sendErr = g.deliverReply(ctx, item, replyContent{Text: frozen.Text, Images: frozen.Images, Files: frozen.Files})
	}
	if r := g.answers.get(p.Ref); r != nil && r.State != "" {
		return answerOutcome(r)
	}
	reason := g.state.SendDeferReason(p.Chat, SendFinal)
	if sendErr != nil {
		reason = reasonRetrying
		traceEvent(ctx, "delivery.retry", Fields{"reason": SanitizeErrorType(sendErr)})
	}
	if reason == "" {
		reason = DeferContextChanged
	}
	g.signal()
	return extproto.SendResult{Deferred: true, Reason: reason}, nil
}

// answerOutcome is a finished answer's result: sent, or why not.
func answerOutcome(r *FrozenReply) (extproto.SendResult, error) {
	if r.State == DeliverySent {
		return extproto.SendResult{MessageID: r.MessageID}, nil
	}
	return extproto.SendResult{}, extproto.Errorf(extproto.ErrCannotSend, "%s", humanReason(r.State, r.Reason))
}

// sendProgress sends one progress note under the request's allowance:
// three per request, none once the window or budget ran out, and the last
// three of the budget kept for the answer.
func (g *Gateway) sendProgress(ctx context.Context, p extproto.Send) (extproto.SendResult, error) {
	if r, ok := g.progressRefs[p.Ref]; ok {
		return progressOutcome(r)
	}
	request := p.ReplyTo
	if request == "" {
		request = p.Ref
	}
	key := jsonText([]string{p.Chat, request})
	q, ok := g.progress[key]
	if !ok {
		q = NewQueryProgress(g.loop, p.Chat, request, g.progressSink(p.Chat))
		g.progress[key] = q
	}
	r := q.Report(ctx, p.Text)
	if len(g.progressRefs) >= maxProgressRefs {
		g.progressRefs = map[string]QueryProgressResult{}
	}
	g.progressRefs[p.Ref] = r
	return progressOutcome(r)
}

func progressOutcome(r QueryProgressResult) (extproto.SendResult, error) {
	if r.Sent {
		return extproto.SendResult{MessageID: r.MessageID}, nil
	}
	return extproto.SendResult{}, extproto.Errorf(extproto.ErrCannotSend, "进度没有发出：%s", progressText(r))
}

// sendResult queues a timer's or task's result: up to three messages of
// text; one message and the file when it carries one; the summary and the
// link when it carries a link.
func (g *Gateway) sendResult(p extproto.Send) (extproto.SendResult, error) {
	blobs := append(append([]extproto.Blob(nil), p.Files...), p.Images...)
	if len(blobs) > 1 {
		return extproto.SendResult{}, extproto.Errorf(extproto.ErrInvalidParams, "a result carries at most one file")
	}
	data, err := decodeBlobs(blobs)
	if err != nil {
		return extproto.SendResult{}, err
	}
	now := nowMs()
	retained := now + resultRetentionMs
	plan := ResultDelivery{ID: newUUID(), Key: p.Ref, ChatID: p.Chat, Source: ResultSource{Type: "collie", ID: utf16Prefix(p.Ref, maxResultKeyChars)},
		Kind: "completed", Status: ResultReady, CreatedAt: now, ExpiresAt: now + terminalResultTTLMs, RetainedUntil: retained}
	artifact := []byte(p.Text)
	name := resultArtifactName
	messages := splitResultMessages(p.Text, ResultTextChars)
	summary := utf16Prefix(p.Text, ResultTextChars)
	switch {
	case p.Link != "":
		plan.Mode = ModeDownload
		plan.Messages = []string{summary}
		plan.Artifact.DownloadURL = p.Link
	case len(data) == 1:
		plan.Mode, artifact = ModeAttachment, data[0]
		name = safeFileName(blobs[0].Name, defaultFileName)
		plan.Messages = []string{summary}
	case len(messages) <= ResultTextMessages:
		plan.Mode, plan.Messages = ModeText, messages
	default:
		// Longer than three messages: the beginning, and the whole text as a file.
		plan.Mode, plan.Messages = ModeAttachment, []string{summary}
	}
	if strings.TrimSpace(plan.Messages[0]) == "" {
		plan.Messages = []string{name}
	}
	if len(artifact) == 0 {
		artifact = []byte(plan.Messages[0])
	}
	if len(artifact) > ResultFileBytes {
		return extproto.SendResult{}, extproto.Errorf(extproto.ErrInvalidParams, "result file over %d bytes", ResultFileBytes)
	}
	m := fileMetadataFor(name, artifact)
	plan.Artifact = ResultArtifact{ID: newUUID(), ChatID: p.Chat, Name: name, Size: m.ByteLength, SHA256: m.SHA256, ExpiresAt: retained,
		DownloadURL: plan.Artifact.DownloadURL}
	if _, err := g.results.create(plan, artifact); err != nil {
		if errors.Is(err, errResultConflict) || errors.Is(err, errResultSchemaInvalid) {
			return extproto.SendResult{}, extproto.Errorf(extproto.ErrInvalidParams, "%s", err.Error())
		}
		return extproto.SendResult{}, extproto.Errorf(extproto.ErrCannotSend, "%s", SanitizeErrorType(err))
	}
	g.refreshInterest()
	g.signal()
	return extproto.SendResult{Deferred: true, Reason: reasonQueued}, nil
}

// signal wakes the notification loop.
func (g *Gateway) signal() {
	select {
	case g.kick <- struct{}{}:
	default:
	}
}

// progressText says why a progress note did not go out.
func progressText(r QueryProgressResult) string {
	if r.Status == ProgressLimitReached {
		return "这次请求的 3 条进度已用完"
	}
	if t, ok := reasonTexts[r.Reason]; ok {
		return t
	}
	return "发送失败"
}
