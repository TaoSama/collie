package wechat

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

const (
	outcomeProcessed = "processed"
	outcomeSkipped   = "skipped"
)

// processMessage takes one message from admission to Collie: isolation,
// window, a flush of waiting replies when a window opens, capacity, media,
// the fixed answers, then the hand-over. A message handed to Collie is
// done here; its answer arrives later through channel.send.
func (g *Gateway) processMessage(ctx context.Context, m InboundMessage, batch *batchPosition) (outcome string, err error) {
	if g.state.IsMessageProcessed(m.DeliveryKey) {
		return outcomeSkipped, nil
	}
	starting := make(chan struct{})
	readyOnce := false
	ready := func() {
		if !readyOnce {
			readyOnce = true
			close(starting)
		}
	}
	g.queryStarts[m.ChatID] = starting
	g.activeInboundChats[m.ChatID] = true
	fields := Fields{}
	if batch != nil {
		fields = Fields{"batchId": batch.id, "batchIndex": batch.index, "batchWaitMs": msSince(batch.clock)}
	}
	traceEvent(ctx, "message.processing.start", fields)
	outcome = "error"
	defer func() {
		ready()
		if g.queryStarts[m.ChatID] == starting {
			delete(g.queryStarts, m.ChatID)
		}
		traceEvent(ctx, "message.processing.end", Fields{"outcome": outcome})
		delete(g.activeInboundChats, m.ChatID)
	}()
	outcome, err = g.process(ctx, m, ready)
	if err == nil {
		return outcome, nil
	}
	outcome = "error"
	if g.hasQuarantinedInboundReply(m.DeliveryKey) {
		outcome = outcomeSkipped
		return outcome, g.quarantineInboundReply(m.DeliveryKey)
	}
	if errors.Is(err, ErrChatNotAllowed) {
		// Poison protection: a message from anyone else is marked done so
		// the cursor moves and later messages are not starved.
		outcome = outcomeSkipped
		if err := g.state.RecordProcessedMessageKey(m.DeliveryKey, false); err != nil {
			return outcome, err
		}
		g.health.RecordMessageProgress()
		return outcome, nil
	}
	return outcome, err
}

func (g *Gateway) process(ctx context.Context, m InboundMessage, ready func()) (string, error) {
	s := g.state
	if g.hasQuarantinedInboundReply(m.DeliveryKey) {
		return outcomeSkipped, g.quarantineInboundReply(m.DeliveryKey)
	}
	if err := traceStep(ctx, "message.authorization", nil, func() error { return g.assertChatAllowed(m.ChatID) }); err != nil {
		return "", err
	}
	fresh, err := traceSpan(ctx, "message.context.persist", nil, func() (bool, error) {
		return s.RecordInboundContext(m.DeliveryKey, m.ChatID, m.CreatedAtMs, strings.TrimSpace(m.ContextToken) != "", m.ContextToken)
	})
	if err != nil {
		return "", err
	}
	retrying := s.ContextWindows[m.ChatID].LastInboundKey == m.DeliveryKey && !s.IsMessageProcessed(m.DeliveryKey)
	if fresh || retrying {
		if err := g.noteAccountInbound(m); err != nil {
			return "", err
		}
		if strings.TrimSpace(m.ContextToken) != "" {
			// The refreshed budget goes to waiting replies before new progress or results.
			if err := g.flushRepliesOnce(ctx, m.ChatID, m.ChatID); err != nil {
				traceEvent(ctx, "message.admission.flush.failed", Fields{"reason": SanitizeErrorType(err)})
			}
		}
	}
	if _, ok := s.Outbox[m.DeliveryKey]; ok {
		return outcomeProcessed, s.RecordProcessedMessageKey(m.DeliveryKey, true)
	}
	if err := s.AssertOutboxCapacity(m.DeliveryKey); err != nil {
		if !errors.Is(err, OutboxBackpressureError) {
			return "", err
		}
		// This message may have restored the budget a full queue needs.
		if err := g.flushRepliesOnce(ctx, "", m.ChatID); err != nil {
			return "", err
		}
		if err := s.AssertOutboxCapacity(m.DeliveryKey); err != nil {
			return "", err
		}
	}
	var media []DownloadedMedia
	stored := true
	if len(m.Media) > 0 {
		media, stored, err = g.storeInboundMedia(ctx, m)
		if err != nil {
			return "", err
		}
	}
	if len(m.Media) > 0 && m.Text == "" && stored {
		// An attachment alone is held as context for the next text: no answer spends the budget.
		// While Collie is still answering this chat it joins that request instead.
		ready()
		if g.awaiting[m.ChatID] > nowMs() {
			held, err := g.stash.held(m.ChatID)
			if err != nil {
				return "", err
			}
			if err := g.forward(ctx, m, "", append(held, media...)); err != nil {
				return "", err
			}
			if err := g.stash.release(m.ChatID); err != nil {
				return "", err
			}
		} else if err := g.stash.hold(m.ChatID, media); err != nil {
			return "", err
		}
		if err := s.RecordProcessedMessageKey(m.DeliveryKey, true); err != nil {
			return "", err
		}
		g.health.RecordMessageProgress()
		return outcomeProcessed, nil
	}
	g.ilink.sendTyping(m.ChatID)
	// Content that can't be passed on (a video, an attachment that failed)
	// goes to Collie too, named in Unsupported: the Router decides what to say.
	text := m.Text
	if !stored {
		text = strings.TrimSpace(text + MediaFailedNote)
	}
	ready()
	held, err := g.stash.held(m.ChatID)
	if err != nil {
		return "", err
	}
	if g.conflicts(m, text) {
		// The same id with other content: taken as the message already handed over.
		writeLogLine(map[string]any{"event": "message_conflict_ignored"}, "")
		if err := s.RecordProcessedMessageKey(m.DeliveryKey, true); err != nil {
			return "", err
		}
		g.health.RecordMessageProgress()
		return outcomeSkipped, nil
	}
	if err := traceStep(ctx, "message.host", nil, func() error { return g.forward(ctx, m, text, append(held, media...)) }); err != nil {
		return "", err
	}
	if err := g.stash.release(m.ChatID); err != nil {
		return "", err
	}
	if err := s.RecordProcessedMessageKey(m.DeliveryKey, true); err != nil {
		return "", err
	}
	g.health.RecordMessageProgress()
	return outcomeProcessed, nil
}

// contentHash identifies what a message carried, to notice an id reused
// with other content.
func contentHash(m InboundMessage, text string) string {
	return hashJSON([]any{text, m.Media})
}

func (g *Gateway) conflicts(m InboundMessage, text string) bool {
	previous, ok := g.state.receivedHash(m.DeliveryKey)
	return ok && previous != contentHash(m, text)
}

// forward is the hand-over to Collie: channel.receive with the person's
// attachments. Collie takes each id once, so a failed call is retried.
func (g *Gateway) forward(ctx context.Context, m InboundMessage, text string, media []DownloadedMedia) error {
	owner := true
	msg := extproto.Message{ID: m.DeliveryKey, Text: text, ChatID: m.ChatID, ChatKind: "dm", Sender: m.ChatID, Owner: &owner,
		Quote: m.Quote, Unsupported: append(append([]string(nil), m.UnsupportedKinds...), m.IgnoredKinds...)}
	if m.CreatedAtMs != nil {
		msg.At = time.UnixMilli(*m.CreatedAtMs).UTC().Format(time.RFC3339Nano)
	}
	for _, item := range media {
		blob := extproto.Blob{Name: item.Name, Mime: item.Mime, Data: base64.StdEncoding.EncodeToString(item.Bytes)}
		if item.Kind == KindImage {
			msg.Images = append(msg.Images, blob)
		} else {
			msg.Files = append(msg.Files, blob)
		}
	}
	// Set before the call: Collie may answer before Receive returns.
	previous, had := g.awaiting[m.ChatID]
	g.awaiting[m.ChatID] = nowMs() + MaxEngineRequestMs
	var err error
	g.loop.await(func() { err = g.host.Receive(ctx, msg) })
	if err != nil {
		if had {
			g.awaiting[m.ChatID] = previous
		} else {
			delete(g.awaiting, m.ChatID)
		}
		return err
	}
	return g.state.recordReceived(m.DeliveryKey, contentHash(m, text))
}

// storeInboundMedia downloads every attachment. stored false means one
// failed, so the sender is told; only a stop aborts.
func (g *Gateway) storeInboundMedia(ctx context.Context, m InboundMessage) ([]DownloadedMedia, bool, error) {
	var out []DownloadedMedia
	for _, ref := range m.Media {
		media, err := traceSpan(ctx, "message.media.download", Fields{"kind": ref.Kind}, func() (DownloadedMedia, error) {
			return downloadInboundMedia(g.ctx, g.loop, g.mediaHTTP, ref)
		})
		if err != nil {
			if g.ctx.Err() != nil {
				return nil, false, err
			}
			traceEvent(ctx, "message.media.failed", Fields{"reason": SanitizeErrorType(err)})
			return nil, false, nil
		}
		out = append(out, media)
	}
	return out, true, nil
}

// steer hands a message to the request running in its chat. Content that
// can't be passed on is left to normal processing.
func (g *Gateway) steer(m InboundMessage) (bool, error) {
	if len(m.UnsupportedKinds) > 0 {
		return false, nil
	}
	if starting, ok := g.queryStarts[m.ChatID]; ok {
		g.loop.wait(context.Background(), starting)
	}
	ctx := withTrace(g.ctx, createMessageTrace(m.ChatID, m.MessageID))
	var media []DownloadedMedia
	if len(m.Media) > 0 {
		stored := false
		var err error
		// A failure falls back to normal processing, which retries and reports it.
		if media, stored, err = g.storeInboundMedia(ctx, m); err != nil || !stored {
			return false, err
		}
		if m.Text == "" {
			return false, nil
		}
	}
	if g.conflicts(m, m.Text) {
		return false, nil
	}
	if err := g.forward(ctx, m, m.Text, media); err != nil {
		// Normal processing hands it over again once the running request ends.
		traceEvent(ctx, "message.steer.failed", Fields{"reason": SanitizeErrorType(err)})
		return false, nil
	}
	return true, nil
}

// admitMessage persists a polled message to the inbox before its window is
// recorded; the runner processes it.
func (g *Gateway) admitMessage(ctx context.Context, m InboundMessage) (string, error) {
	s := g.state
	if s.IsMessageProcessed(m.DeliveryKey) {
		return outcomeSkipped, nil
	}
	if err := g.assertChatAllowed(m.ChatID); err != nil {
		return outcomeSkipped, s.RecordProcessedMessageKey(m.DeliveryKey, false)
	}
	retrying := s.ContextWindows[m.ChatID].LastInboundKey == m.DeliveryKey && !s.IsMessageProcessed(m.DeliveryKey)
	has, err := g.inbox.Has(m.DeliveryKey)
	if err != nil {
		return "", err
	}
	if has {
		if err := g.inbox.Put(m); err != nil {
			if !errors.Is(err, InboxConflictError) {
				return "", err
			}
			traceEvent(ctx, "message.inbox.conflict", nil)
		}
		if retrying {
			if err := g.noteAccountInbound(m); err != nil {
				return "", err
			}
		}
		return outcomeSkipped, nil
	}
	stored := m
	if stored.CreatedAtMs != nil && (*stored.CreatedAtMs < 0 || *stored.CreatedAtMs > maxSafeInteger) {
		stored.CreatedAtMs = nil
	}
	// The full message is durable before the window is refreshed: a stop in
	// between leaves it pending and no notification sees an unadmitted window.
	if err := g.inbox.Put(stored); err != nil {
		return "", err
	}
	fresh, err := s.RecordInboundContext(m.DeliveryKey, m.ChatID, m.CreatedAtMs, strings.TrimSpace(m.ContextToken) != "", m.ContextToken)
	if err != nil {
		return "", err
	}
	// The resume intent follows both checkpoints, so a retry sees the same generation.
	if fresh || retrying {
		if err := g.noteAccountInbound(m); err != nil {
			return "", err
		}
	}
	g.runner.Schedule()
	return outcomeProcessed, nil
}

// noteAccountInbound lets results waiting for this window go out.
func (g *Gateway) noteAccountInbound(m InboundMessage) error {
	s := g.state
	if strings.TrimSpace(m.ContextToken) == "" {
		return nil
	}
	refreshedAccount := s.AccountBudget != nil && s.AccountBudget.LastInboundKey == m.DeliveryKey
	now := nowMs()
	for chat, w := range s.ContextWindows {
		if !refreshedAccount && (chat != m.ChatID || w.LastInboundKey != m.DeliveryKey) {
			continue
		}
		if strings.TrimSpace(s.ContextTokens[chat]) != "" && now >= w.RefreshedAt && now-w.RefreshedAt < s.Policy.ContextWindowMs {
			if err := g.dispatcher.NoteInbound(chat, m.DeliveryKey); err != nil {
				return err
			}
		}
	}
	return nil
}

// progressSink sends one progress note within the shared budget.
func (g *Gateway) progressSink(chatID string) ProgressSink {
	return func(ctx context.Context, text, id string) (bool, string, string) {
		if !g.active() {
			return false, "unavailable", ""
		}
		if strings.TrimSpace(text) == "" || utf16Len(text) > MaxProgressChars {
			return false, "rejected", ""
		}
		if g.state.HasUncertainReply(id) {
			return false, "previous_send_unknown", ""
		}
		_, messageID, err := g.ilink.sendMarkdownReply(g.ctx, chatID, text, ReplyOptions{DeliveryKey: id, Kind: SendProgress,
			MaxCharsPerChunk: MaxProgressChars, ShouldContinue: func() bool { return !g.stopped }})
		if err == nil {
			return true, "", messageID
		}
		if d, ok := asDeferred(err); ok {
			return false, d.Reason, ""
		}
		traceEvent(ctx, "query.progress.failed", nil)
		if g.state.HasUncertainReply(id) {
			return false, "send_unknown", ""
		}
		return false, "delivery_failed", ""
	}
}
