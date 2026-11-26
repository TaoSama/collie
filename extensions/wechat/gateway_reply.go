package wechat

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

const fileReplyPrefix = "file-reply:"

// replyContent is what one reply sends: pictures, then files, then text.
type replyContent struct {
	Text   string
	Images []ImageAttachment
	Files  []FrozenFile
}

// errReplyUnavailable: the frozen reply is gone; recovery cannot send it.
var errReplyUnavailable = errors.New("Reply is unavailable or already closed")

// permanentAttachmentError: a reply's attachment can never be read again.
type permanentAttachmentError struct{ reason string }

func (e *permanentAttachmentError) Error() string {
	return "WeChat reply attachment unavailable: " + e.reason
}

func fileReplyKey(key string, index int) string {
	return fmt.Sprintf("%s%s:%d", fileReplyPrefix, key, index)
}

func (g *Gateway) hasUncertainFile(key string) bool {
	prefix := fileReplyPrefix + key + ":"
	for k, item := range g.state.ReplyDeliveries {
		if strings.HasPrefix(k, prefix) && (item.InFlight || item.FailureReason != "") {
			return true
		}
	}
	return false
}

func sortedOutbox(s *StateManager) []DeliveryOutboxItem {
	keys := make([]string, 0, len(s.Outbox))
	for k := range s.Outbox {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]DeliveryOutboxItem, 0, len(keys))
	for _, k := range keys {
		out = append(out, s.Outbox[k])
	}
	return out
}

// deliverReply sends one outbox reply: refused too often means expired; the
// whole reply has REPLY_TIMEOUT_MS; deferred stops here and waits; an
// unknown outcome is isolated and reported, never sent again.
func (g *Gateway) deliverReply(ctx context.Context, item DeliveryOutboxItem, reply replyContent) error {
	if g.stopped || item.Status != OutboxPending || g.activeReplyDeliveries[item.DeliveryKey] {
		return nil
	}
	g.activeReplyDeliveries[item.DeliveryKey] = true
	defer delete(g.activeReplyDeliveries, item.DeliveryKey)
	trace := createMessageTrace(item.ChatID, item.MessageID)
	if inherited := currentTrace(ctx); inherited == nil || inherited.TraceID != trace.TraceID {
		ctx = withTrace(ctx, trace)
	}
	started := time.Now()
	receipt := "error"
	traceEvent(ctx, "delivery.start", Fields{"source": item.Source})
	defer func() {
		traceEvent(ctx, "delivery.end", Fields{"receiptStatus": receipt, "durationMs": msSince(started)})
	}()
	// A restart can resume a delivery whose last refusal was recorded before its outbox expired.
	if g.hasExhaustedReplyRejections(item.DeliveryKey) {
		receipt = "rejected"
		return g.expireRejectedReply(item)
	}
	messageID, err := g.sendReply(ctx, item, reply)
	if err == nil {
		if item.Source == SourceEngine {
			if err := g.state.UpdateOutbox(item.DeliveryKey, func(i *DeliveryOutboxItem) {
				i.Status, i.ReceiptStatus, i.ReceiptMessageID = OutboxReceipt, "accepted", messageID
			}); err != nil {
				return err
			}
		}
		if err := traceStep(ctx, "delivery.journal.ack", nil, func() error { return g.ackEngineReply(item, "accepted", messageID) }); err != nil {
			return err
		}
		if err := traceStep(ctx, "delivery.outbox.remove", nil, func() error { return g.state.RemoveOutbox(item.DeliveryKey) }); err != nil {
			return err
		}
		receipt = "wechat_api_accepted"
		return nil
	}
	if _, ok := asDeferred(err); ok {
		receipt = "deferred"
		return nil
	}
	var permanent *permanentAttachmentError
	if errors.As(err, &permanent) {
		receipt = "expired"
		if err := g.state.UpdateOutbox(item.DeliveryKey, setStatus(OutboxExpired)); err != nil {
			return err
		}
		return g.ackEngineReply(item, "expired", "")
	}
	if IsRejectedSendReceipt(err) && g.hasExhaustedReplyRejections(item.DeliveryKey) {
		receipt = "rejected"
		return g.expireRejectedReply(item)
	}
	if g.hasQuarantinedInboundReply(item.DeliveryKey) {
		receipt = "uncertain"
		if err := g.state.UpdateOutbox(item.DeliveryKey, setStatus(OutboxUncertain)); err != nil {
			return err
		}
		return g.ackEngineReply(item, "uncertain", "")
	}
	return err
}

// sendReply sends pictures, files, then text within one deadline.
func (g *Gateway) sendReply(ctx context.Context, item DeliveryOutboxItem, reply replyContent) (string, error) {
	deadline := nowMs() + ReplyTimeoutMs
	shouldContinue := func() bool { return !g.stopped }
	if len(reply.Images) > 0 {
		key := imageReplyPrefix + item.DeliveryKey
		_, err := g.ilink.sendImageReply(ctx, item.ChatID, reply.Images, ImageReplyOptions{DeliveryKey: key, TimeoutMs: ReplyTimeoutMs,
			ShouldContinue: shouldContinue,
			GetImageBytes: func(ctx context.Context, image ImageAttachment) ([]byte, error) {
				return traceSpan(ctx, "delivery.image.download", nil, func() ([]byte, error) { return g.attachment(key, image.SHA256) })
			}})
		if err != nil {
			return "", err
		}
	}
	for i, f := range reply.Files {
		key := fileReplyKey(item.DeliveryKey, i)
		data, err := g.attachment(key, f.SHA256)
		if err != nil {
			return "", err
		}
		if _, err := g.ilink.sendFileReply(ctx, item.ChatID, data, FileMetadata{FileName: f.Name, ByteLength: f.Size, SHA256: f.SHA256},
			FileReplyOptions{DeliveryKey: key, ShouldContinue: shouldContinue}); err != nil {
			return "", err
		}
	}
	if reply.Text == "" {
		return "", nil
	}
	timeout := deadline - nowMs()
	if timeout <= 0 {
		return "", errors.New("WeChat reply timed out before completion")
	}
	_, id, err := g.ilink.sendMarkdownReply(ctx, item.ChatID, reply.Text, ReplyOptions{DeliveryKey: item.DeliveryKey,
		MaxCharsPerChunk: item.SplitMaxChars, TimeoutMs: timeout, ShouldContinue: shouldContinue})
	return id, err
}

// attachment reads a frozen reply's bytes; a missing one fails its plan
// for good so it is never retried.
func (g *Gateway) attachment(planKey, sum string) ([]byte, error) {
	data, err := g.answers.blob(sum)
	if err == nil {
		return data, nil
	}
	if _, ok := g.state.ReplyDeliveries[planKey]; ok {
		if ferr := g.state.RecordReplyFailure(planKey, FailureAttachmentUnavailable); ferr != nil {
			return nil, ferr
		}
	}
	return nil, &permanentAttachmentError{FailureAttachmentUnavailable}
}

// flushRepliesOnce resumes waiting replies from their frozen content: an
// old answer is never asked for again. One reply per pass, in turn, so
// polling and other chats keep moving. chatID limits it to one chat;
// callerChatID may pass even while that chat's message is being processed.
func (g *Gateway) flushRepliesOnce(ctx context.Context, chatID, callerChatID string) error {
	// A concurrent pass must not bypass a newly refreshed chat's turn.
	for g.replyFlush != nil {
		g.loop.wait(context.Background(), g.replyFlush)
	}
	if g.stopped {
		return nil
	}
	done := make(chan struct{})
	g.replyFlush = done
	defer func() {
		g.replyFlush = nil
		close(done)
	}()
	var first error
	canRecover := func(item DeliveryOutboxItem) bool {
		return (chatID == "" || item.ChatID == chatID) && (item.ChatID == callerChatID || !g.activeInboundChats[item.ChatID])
	}
	// Terminal acknowledgements that an earlier save failed to finish.
	for _, item := range sortedOutbox(g.state) {
		if !canRecover(item) || g.activeReplyDeliveries[item.DeliveryKey] || item.Kind != "reply" || item.Source != SourceEngine {
			continue
		}
		switch {
		case item.Status == OutboxReceipt && item.ReceiptStatus == "accepted":
			first = firstErr(first, g.withRecoveryTrace(ctx, item, func(ctx context.Context) error { return g.recoverAccepted(ctx, item) }))
		case !item.EngineAcked && (item.Status == OutboxUncertain || item.Status == OutboxExpired):
			first = firstErr(first, g.withRecoveryTrace(ctx, item, func(ctx context.Context) error { return g.recoveryAck(ctx, item, item.Status) }))
		}
	}
	var items []DeliveryOutboxItem
	for _, item := range sortedOutbox(g.state) {
		if item.Kind == "reply" && item.Status == OutboxPending && canRecover(item) && !g.activeReplyDeliveries[item.DeliveryKey] {
			items = append(items, item)
		}
	}
	offset := g.replyOutboxCursor % max(1, len(items))
	for _, item := range append(append([]DeliveryOutboxItem(nil), items[offset:]...), items[:offset]...) {
		if !canRecover(item) || g.activeReplyDeliveries[item.DeliveryKey] {
			continue
		}
		if g.hasQuarantinedInboundReply(item.DeliveryKey) {
			if err := g.withRecoveryTrace(ctx, item, func(ctx context.Context) error {
				if err := g.state.UpdateOutbox(item.DeliveryKey, setStatus(OutboxUncertain)); err != nil {
					return err
				}
				return g.recoveryAck(ctx, item, "uncertain")
			}); err != nil {
				return err
			}
			continue
		}
		if text, ok := g.state.ReplyDeliveries[item.DeliveryKey]; ok && text.AcknowledgedChunks == text.TotalChunks && !text.InFlight {
			if err := g.withRecoveryTrace(ctx, item, func(ctx context.Context) error { return g.recoverAccepted(ctx, item) }); err != nil {
				return err
			}
			continue
		}
		if !g.state.CanSendToChat(item.ChatID, SendFinal) {
			continue
		}
		for i, candidate := range items {
			if candidate.DeliveryKey == item.DeliveryKey {
				g.replyOutboxCursor = (i + 1) % len(items)
			}
		}
		err := g.withRecoveryTrace(ctx, item, func(ctx context.Context) error {
			err := traceStep(ctx, "delivery.recovery.attempt", nil, func() error {
				reply, err := traceSpan(ctx, "delivery.recovery.read", nil, func() (replyContent, error) { return g.frozenReply(item) })
				if err != nil {
					return err
				}
				return g.deliverReply(ctx, item, reply)
			})
			if errors.Is(err, errReplyUnavailable) {
				status := OutboxExpired
				if g.hasQuarantinedInboundReply(item.DeliveryKey) {
					status = OutboxUncertain
				}
				if err := g.state.UpdateOutbox(item.DeliveryKey, setStatus(status)); err != nil {
					return err
				}
				return g.recoveryAck(ctx, item, status)
			}
			return err
		})
		first = firstErr(first, err)
		break
	}
	return first
}

// recoverAccepted finishes a reply every chunk of which was accepted.
func (g *Gateway) recoverAccepted(ctx context.Context, item DeliveryOutboxItem) error {
	if err := g.recoveryAck(ctx, item, "accepted"); err != nil {
		return err
	}
	return traceStep(ctx, "delivery.recovery.outbox.remove", nil, func() error { return g.state.RemoveOutbox(item.DeliveryKey) })
}

func (g *Gateway) recoveryAck(ctx context.Context, item DeliveryOutboxItem, status string) error {
	return traceStep(ctx, "delivery.recovery.ack", nil, func() error { return g.ackEngineReply(item, status, item.ReceiptMessageID) })
}

func (g *Gateway) withRecoveryTrace(ctx context.Context, item DeliveryOutboxItem, action func(context.Context) error) error {
	// Recovery is its own attempt even when a new message triggered it.
	rctx := withTrace(ctx, createMessageTrace(item.ChatID, item.MessageID))
	return traceStep(rctx, "delivery.recovery", Fields{"recovery": true, "source": item.Source, "status": item.Status},
		func() error { return action(rctx) })
}

func (g *Gateway) frozenReply(item DeliveryOutboxItem) (replyContent, error) {
	switch item.Source {
	case SourceEngine:
		r := g.answers.get(item.DeliveryKey)
		if r == nil || r.State != "" {
			return replyContent{}, errReplyUnavailable
		}
		return replyContent{Text: r.Text, Images: r.Images, Files: r.Files}, nil
	}
	return replyContent{}, errReplyUnavailable
}

// ackEngineReply tells Collie how its answer ended, once.
func (g *Gateway) ackEngineReply(item DeliveryOutboxItem, status, messageID string) error {
	if item.Source != SourceEngine || g.state.Outbox[item.DeliveryKey].EngineAcked {
		return nil
	}
	state := map[string]string{"accepted": DeliverySent, "uncertain": DeliveryUncertain, "expired": DeliveryExpired}[status]
	if err := g.host.Delivery(extproto.Delivery{Ref: item.DeliveryKey, State: state, MessageID: messageID}); err != nil {
		return err
	}
	if err := g.answers.finish(item.DeliveryKey, state, "", messageID); err != nil {
		return err
	}
	if _, ok := g.state.Outbox[item.DeliveryKey]; !ok {
		return nil
	}
	return g.state.UpdateOutbox(item.DeliveryKey, func(i *DeliveryOutboxItem) { i.EngineAcked = true })
}

// expireRejectedReply ends a reply WeChat keeps refusing, so retries cannot
// drain the shared budget.
func (g *Gateway) expireRejectedReply(item DeliveryOutboxItem) error {
	if err := g.state.UpdateOutbox(item.DeliveryKey, setStatus(OutboxExpired)); err != nil {
		return err
	}
	return g.ackEngineReply(item, "expired", "")
}

// resultTransport sends result steps verbatim through the shared client.
type resultTransport struct{ g *Gateway }

func (t resultTransport) SendText(ctx context.Context, chatID, text string, o ResultSendOptions) error {
	_, _, err := t.g.ilink.sendMarkdownReply(ctx, chatID, text, ReplyOptions{DeliveryKey: o.DeliveryKey, Kind: o.Kind,
		ShouldContinue: o.ShouldContinue, MaxCharsPerChunk: ResultTextChars})
	return err
}

func (t resultTransport) SendFile(ctx context.Context, chatID string, a ResultArtifact, data []byte, o ResultSendOptions) error {
	_, err := t.g.ilink.sendFileReply(ctx, chatID, data, FileMetadata{FileName: a.Name, ByteLength: a.Size, SHA256: a.SHA256},
		FileReplyOptions{DeliveryKey: o.DeliveryKey, Kind: o.Kind, ShouldContinue: o.ShouldContinue})
	return err
}

func (g *Gateway) downloadArtifact(_ context.Context, a ResultArtifact) ([]byte, error) {
	data, err := g.results.artifact(a.ID)
	if err != nil {
		return nil, err
	}
	if m := fileMetadataFor(a.Name, data); m.ByteLength != a.Size || m.SHA256 != a.SHA256 {
		return nil, errors.New("Result artifact integrity mismatch")
	}
	return data, nil
}

// reportResult tells Collie a result's outcome.
func (g *Gateway) reportResult(ref, state, reason string) error {
	return g.host.Delivery(extproto.Delivery{Ref: ref, State: state, Reason: reason})
}

// localResults is the result service over the local store.
type localResults struct{ g *Gateway }

func (l localResults) listDeliveries(chats []string) ([]ResultDelivery, error) {
	return l.g.results.listPending(chats, nowMs())
}
func (l localResults) getDelivery(id string) (ResultDelivery, error) {
	return l.g.results.get(id, nowMs())
}
func (l localResults) claimDelivery(id, attempt string) (ResultDelivery, error) {
	return l.g.results.claim(id, attempt, nowMs())
}
func (l localResults) materializeDelivery(id, attempt string) (ResultDelivery, error) {
	p, err := l.g.results.materialize(id, nowMs())
	if err == nil && p.AttemptID != attempt {
		return ResultDelivery{}, errors.New("Result materialize attempt mismatch")
	}
	return p, err
}
func (l localResults) recordReceipt(id, attempt, status string) (ResultDelivery, error) {
	return l.g.results.receipt(id, status, attempt)
}
func (l localResults) deferDelivery(id, attempt, reason string, submitted int) (ResultDelivery, error) {
	return l.g.results.deferResult(id, attempt, reason, nowMs(), submitted)
}
func (l localResults) resumeDeliveries(chat, generation string) error {
	_, err := l.g.results.resume(chat, generation, nowMs())
	return err
}
func (l localResults) blockChatDeliveries(chat, reason string) error {
	_, err := l.g.results.markChatWaiting(chat, reason, nowMs())
	return err
}
