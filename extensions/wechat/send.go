package wechat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

const (
	// verbatimDigestVersion marks plans that send the text as written: every
	// reply, Markdown included, goes out verbatim.
	verbatimDigestVersion = "wechat-verbatim-v2"
	errReplyInterrupted   = "WeChat reply interrupted before completion"
)

var (
	errReplyTimeoutBudget = errors.New("Invalid WeChat reply timeout budget")
	errReplyTimedOut      = errors.New("WeChat reply timed out")
	errImageTimedOut      = errors.New("WeChat image reply timed out")
	errFileTimedOut       = errors.New("WeChat file reply timed out")
)

// ReplyOptions shape one text reply.
type ReplyOptions struct {
	DeliveryKey      string
	MaxCharsPerChunk int
	TimeoutMs        int64
	ShouldContinue   func() bool
	Kind             string // final | progress
	// RenewalReminder may replace only an unsent reminder's plan.
	RenewalReminder bool
}

// sendMarkdownReply sends text as written, cut into a frozen chunk plan.
// Each chunk is charged, its intent persisted, sent, then confirmed; a
// chunk without a receipt is never sent again. A replay continues the plan
// where it stopped; other content under the same key is refused. Answers
// the chunk count and the last message id WeChat named.
func (c *ILinkClient) sendMarkdownReply(ctx context.Context, chatID, text string, o ReplyOptions) (int, string, error) {
	maxChars := o.MaxCharsPerChunk
	if maxChars == 0 {
		maxChars = DefaultMessageMaxChars
	}
	key := o.DeliveryKey
	if key == "" {
		key = "reply:" + newUUID()
	}
	digest := hashJSON([]any{c.state.LoginFingerprint(), chatID, maxChars, text, verbatimDigestVersion, text})
	chunks, err := SplitWeChatReply(text, maxChars)
	if err != nil || len(chunks) == 0 {
		return 0, "", err
	}
	timeoutMs := o.TimeoutMs
	if timeoutMs == 0 {
		timeoutMs = ReplyTimeoutMs
	}
	if timeoutMs <= 0 || timeoutMs > NotificationReplyMs {
		return 0, "", errReplyTimeoutBudget
	}
	if err := abortCause(ctx); err != nil {
		return 0, "", err
	}
	traceEvent(ctx, "wechat.reply.rendered", Fields{"chars": utf16Len(text), "chunkCount": len(chunks)})
	delivery, err := traceSpan(ctx, "wechat.reply.plan.persist", nil, func() (ReplyDeliveryState, error) {
		if o.RenewalReminder {
			return c.state.PrepareRenewalReplyDelivery(key, digest, len(chunks))
		}
		return c.state.PrepareReplyDelivery(key, digest, len(chunks))
	})
	if err != nil {
		return 0, "", err
	}
	traceEvent(ctx, "wechat.reply.plan", Fields{"chunkCount": len(chunks), "acknowledgedChunks": delivery.AcknowledgedChunks})
	if delivery.AcknowledgedChunks == len(chunks) {
		traceEvent(ctx, "wechat.reply.reused", Fields{"outcome": "replayed"})
		return len(chunks), "", nil
	}
	rctx, cancel := withTimeout(ctx, timeoutMs, errReplyTimedOut)
	defer cancel()
	interrupted := func() error {
		if o.ShouldContinue != nil && !o.ShouldContinue() {
			return deferred(DeferContextChanged, errReplyInterrupted)
		}
		return nil
	}
	c.sendTyping(chatID)
	lastID := ""
	for idx := delivery.AcknowledgedChunks; idx < len(chunks); idx++ {
		clientID := clientIDPrefix + hashJSON([]any{c.state.LoginFingerprint(), chatID, key, idx})[:deterministicIDHexChars]
		if err := abortCause(rctx); err != nil {
			return 0, "", err
		}
		if err := interrupted(); err != nil {
			return 0, "", err
		}
		err := c.withChatSend(rctx, chatID, func() error {
			if err := interrupted(); err != nil {
				return err
			}
			if err := c.consumeSendBudget(chatID, key, o.Kind); err != nil {
				return err
			}
			// Charging the ledger, persisting intent and starting the request do not yield in between.
			traceEvent(rctx, "wechat.chunk.start", Fields{"chunkIndex": idx, "chunkCount": len(chunks)})
			if err := c.state.BeginReplyChunk(key, idx); err != nil {
				return err
			}
			traceEvent(rctx, "wechat.chunk.intent.persist", Fields{"chunkIndex": idx})
			id, err := traceSpan(rctx, "wechat.chunk.send", Fields{"chunkIndex": idx}, func() (string, error) {
				return c.sendMessageItem(rctx, chatID, map[string]any{"type": ItemTypeText, "text_item": map[string]any{"text": chunks[idx]}}, clientID)
			})
			if err != nil {
				if IsRejectedSendReceipt(err) {
					if rerr := c.state.RejectReplyChunk(key, idx, o.RenewalReminder); rerr != nil {
						return rerr
					}
					traceEvent(rctx, "wechat.chunk.rejected", Fields{"chunkIndex": idx})
				}
				status := "rejected"
				if c.state.HasUncertainReply(key) {
					status = "uncertain"
				}
				traceEvent(rctx, "wechat.chunk.failed", Fields{"chunkIndex": idx, "receiptStatus": status})
				return err
			}
			if id != "" {
				lastID = id
			}
			if err := traceStep(rctx, "wechat.chunk.receipt.persist", Fields{"chunkIndex": idx}, func() error {
				return c.state.ConfirmReplyChunk(key, idx)
			}); err != nil {
				return err
			}
			traceEvent(rctx, "wechat.chunk.accepted", Fields{"chunkIndex": idx, "chunkCount": len(chunks)})
			return nil
		})
		if err != nil {
			return 0, "", err
		}
	}
	return len(chunks), lastID, nil
}

// ImageReplyOptions shape one picture reply.
type ImageReplyOptions struct {
	DeliveryKey    string
	GetImageBytes  func(ctx context.Context, image ImageAttachment) ([]byte, error)
	TimeoutMs      int64
	ShouldContinue func() bool
}

// sendImageReply sends pictures; it stops (deferred) as soon as the
// account, the chat's token, its window or the budget's epoch changes.
func (c *ILinkClient) sendImageReply(ctx context.Context, chatID string, images []ImageAttachment, o ImageReplyOptions) (int, error) {
	if err := validateImageAttachments(images); err != nil {
		return 0, err
	}
	if len(images) == 0 {
		return 0, nil
	}
	fingerprint, token := c.state.LoginFingerprint(), c.state.ContextTokens[chatID]
	budgetEpoch, windowKey := c.budgetEpoch(), c.state.ContextWindows[chatID].LastInboundKey
	if fingerprint == "" || token == "" {
		return 0, errors.New("WeChat image delivery requires an active account and chat context")
	}
	shouldContinue := func() bool {
		return fingerprint == c.state.LoginFingerprint() && token == c.state.ContextTokens[chatID] &&
			budgetEpoch == c.budgetEpoch() && windowKey == c.state.ContextWindows[chatID].LastInboundKey &&
			(o.ShouldContinue == nil || o.ShouldContinue())
	}
	timeoutMs := o.TimeoutMs
	if timeoutMs == 0 {
		timeoutMs = ReplyTimeoutMs
	}
	if timeoutMs <= 0 || timeoutMs > NotificationReplyMs {
		return 0, errors.New("Invalid WeChat image reply timeout budget")
	}
	ictx, cancel := withTimeout(ctx, timeoutMs, errImageTimedOut)
	defer cancel()
	var n int
	err := c.withChatSend(ictx, chatID, func() error {
		if !shouldContinue() {
			return deferred(DeferContextChanged, errImageInterrupted)
		}
		c.sendTyping(chatID)
		var err error
		n, err = deliverImages(ictx, c.state, chatID, imageReplyOptions{deliveryKey: o.DeliveryKey, images: images,
			getImageBytes: o.GetImageBytes,
			uploadImage: func(ctx context.Context, data []byte) (map[string]any, error) {
				if !shouldContinue() {
					return nil, deferred(DeferContextChanged, errImageInterrupted)
				}
				return c.imageUploader.upload(ctx, chatID, data)
			},
			sendImage: func(ctx context.Context, item map[string]any, clientID string) (string, error) {
				return c.sendMessageItem(ctx, chatID, item, clientID)
			},
			beforeBegin:    func() error { return c.consumeSendBudget(chatID, o.DeliveryKey, SendFinal) },
			shouldContinue: shouldContinue})
		return err
	})
	return n, err
}

func (c *ILinkClient) budgetEpoch() string {
	if c.state.AccountBudget == nil {
		return ""
	}
	return c.state.AccountBudget.LastInboundKey
}

// FileReplyOptions shape one file reply.
type FileReplyOptions struct {
	DeliveryKey    string
	TimeoutMs      int64
	ShouldContinue func() bool
	Kind           string
}

// sendFileReply uploads (retryable) and then sends once. Only sendmessage
// crosses the persisted uncertainty boundary.
func (c *ILinkClient) sendFileReply(ctx context.Context, chatID string, data []byte, meta FileMetadata, o FileReplyOptions) (int, error) {
	key := o.DeliveryKey
	if strings.TrimSpace(chatID) == "" || strings.TrimSpace(key) == "" {
		return 0, errors.New("WeChat file delivery requires stable identity")
	}
	timeoutMs := o.TimeoutMs
	if timeoutMs == 0 {
		transfer, err := transferTimeoutMs(int64(len(data)), MediaUploadTimeoutMs)
		if err != nil {
			return 0, err
		}
		timeoutMs = max(ReplyTimeoutMs, transfer+SendRequestTimeoutMs+CheckpointMarginMs)
	}
	if timeoutMs <= 0 || timeoutMs > NotificationReplyMs {
		return 0, errors.New("Invalid WeChat file reply timeout budget")
	}
	if err := abortCause(ctx); err != nil {
		return 0, err
	}
	s := c.state
	account, token := s.LoginFingerprint(), s.ContextTokens[chatID]
	window, hasWindow := s.ContextWindows[chatID]
	budgetEpoch := c.budgetEpoch()
	if account == "" || token == "" {
		return 0, errors.New("WeChat file delivery requires an active account and chat context")
	}
	digest := hashJSON([]any{account, chatID, fileDigestVersion, meta.FileName, meta.ByteLength, meta.SHA256})
	prepare := func() (bool, error) {
		d, err := s.PrepareReplyDelivery(key, digest, 1)
		return err == nil && d.AcknowledgedChunks == 1, err
	}
	if done, err := prepare(); err != nil || done {
		return boolCount(done), err
	}
	fctx, cancel := withTimeout(ctx, timeoutMs, errFileTimedOut)
	defer cancel()
	assertCurrent := func() error {
		if err := abortCause(fctx); err != nil {
			return err
		}
		w, has := s.ContextWindows[chatID]
		if account != s.LoginFingerprint() || token != s.ContextTokens[chatID] || has != hasWindow || w.RefreshedAt != window.RefreshedAt ||
			budgetEpoch != c.budgetEpoch() || o.ShouldContinue != nil && !o.ShouldContinue() {
			return deferred(DeferContextChanged, errFileInterrupted)
		}
		return nil
	}
	if err := assertCurrent(); err != nil {
		return 0, err
	}
	item, err := c.fileUploader.upload(fctx, chatID, data, meta)
	if err != nil {
		return 0, err
	}
	if err := assertCurrent(); err != nil {
		return 0, err
	}
	var n int
	err = c.withChatSend(fctx, chatID, func() error {
		if err := assertCurrent(); err != nil {
			return err
		}
		// Another caller may have completed or become uncertain during the upload.
		if done, err := prepare(); err != nil || done {
			n = boolCount(done)
			return err
		}
		clientID := fileClientIDPrefix + hashJSON([]any{account, chatID, key})[:deterministicIDHexChars]
		if err := c.consumeSendBudget(chatID, key, o.Kind); err != nil {
			return err
		}
		if err := s.BeginReplyChunk(key, 0); err != nil {
			return err
		}
		if _, err := c.sendMessageItem(fctx, chatID, item, clientID); err != nil {
			if IsRejectedSendReceipt(err) {
				if rerr := s.RejectReplyChunk(key, 0, false); rerr != nil {
					return rerr
				}
			}
			return err
		}
		if err := s.ConfirmReplyChunk(key, 0); err != nil {
			return err
		}
		n = 1
		return nil
	})
	return n, err
}

func boolCount(b bool) int {
	if b {
		return 1
	}
	return 0
}

// fileMetadataFor describes file bytes for a frozen reply or result.
func fileMetadataFor(name string, data []byte) FileMetadata {
	sum := sha256.Sum256(data)
	return FileMetadata{FileName: name, ByteLength: len(data), SHA256: hex.EncodeToString(sum[:])}
}
