package wechat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// pollBatch long-polls once from the stored cursor. Messages already
// processed are left out; the cursor is not committed here.
func (c *ILinkClient) pollBatch(ctx context.Context) (PollBatch, error) {
	account := c.state.Account
	if account == nil || account.Token == "" {
		return PollBatch{}, errors.New("WeChat is not logged in; missing valid credentials token")
	}
	current := c.state.SyncBuf
	response, err := c.request(ctx, endpointGetUpdates, map[string]any{
		"get_updates_buf": current, "base_info": map[string]any{"channel_version": ChannelVersion},
	}, account.Token, c.PollBudgetMs(), nil)
	if err != nil {
		return PollBatch{}, err
	}
	if n, ok := safeInt(response["errcode"]); ok && n == errcodeSessionExpired {
		return PollBatch{}, ErrSessionExpired
	}
	if nonZeroCode(response["ret"]) || nonZeroCode(response["errcode"]) {
		return PollBatch{}, fmt.Errorf("WeChat getupdates failed: ret=%s errcode=%s", jsString(response["ret"]), jsString(response["errcode"]))
	}
	// Invalid or unbounded hints never change the last usable local budget.
	if hint, ok := safeInt(response["longpolling_timeout_ms"]); ok && isJSONNumber(response["longpolling_timeout_ms"]) &&
		hint > 0 && hint <= MaxPollTimeoutMs {
		c.pollTimeoutMs = hint
	}
	next := c.state.SyncBuf
	if s, ok := response["get_updates_buf"].(string); ok && s != "" {
		next = s
	}
	rawMsgs, _ := response["msgs"].([]any)
	batch := PollBatch{NextSyncBuf: next, RawMessageCount: len(rawMsgs)}
	for idx, raw := range rawMsgs {
		msg := record(raw)
		if msg == nil {
			continue
		}
		if t, ok := safeInt(msg["message_type"]); !ok || !isJSONNumber(msg["message_type"]) || t != MessageTypeInboundText {
			continue
		}
		sender := "unknown"
		if truthy(msg["from_user_id"]) {
			sender = jsString(msg["from_user_id"])
		}
		items, _ := msg["item_list"].([]any)
		content := ParseInboundContent(items)
		if content == nil {
			continue
		}
		// Media-only messages hash their CDN references so distinct pictures stay distinct.
		identityText := content.OwnText
		if len(content.UnsupportedKinds) > 0 {
			identityText = "unsupported:" + strings.Join(content.UnsupportedKinds, ",")
		} else if identityText == "" {
			media := content.Media
			if media == nil {
				media = []InboundMediaRef{}
			}
			identityText = "media:" + sha256Hex(jsonText(media))
		}
		contextToken, _ := msg["context_token"].(string)
		clientID := ""
		switch id := msg["client_id"].(type) {
		case string:
			if strings.TrimSpace(id) != "" {
				clientID = id
			}
		case json.Number:
			if n, ok := safeInt(id); ok && n >= 0 {
				clientID = strconv.FormatInt(n, 10)
			}
		}
		serverID := ""
		if s, ok := msg["message_id"].(string); ok && digitsOnly.MatchString(s) {
			serverID = "server:" + s
		}
		stableID := clientID
		if stableID == "" {
			stableID = serverID
		}
		createTime := ""
		switch t := msg["create_time_ms"].(type) {
		case string:
			createTime = t
		case json.Number:
			createTime = t.String()
		}
		var createdAt *int64
		if digitsOnly.MatchString(createTime) {
			if n, err := strconv.ParseInt(createTime, 10, 64); err == nil && n <= maxSafeInteger {
				createdAt = &n
			}
		}
		// Client ids already stored in checkpoints win over server ids.
		messageID := stableID
		if messageID == "" {
			// Without a stable id only retries of the same page position are identifiable;
			// positions stay distinct so identical commands are not merged across pages.
			digest := sha256Hex(fmt.Sprintf("%s:%s:%s:%s:%d:%s", account.AccountID, sender, createTime, current, idx, identityText))
			messageID = "det:" + digest[:deterministicIDHexChars]
		}
		key := c.state.ComputeDeliveryKey(sender, messageID)
		if key != "" && c.state.IsMessageProcessed(key) {
			continue
		}
		batch.Messages = append(batch.Messages, InboundMessage{ChatID: sender, Text: content.Text, MessageID: messageID,
			DeliveryKey: key, ContextToken: contextToken, CreatedAtMs: createdAt, UnsupportedKinds: content.UnsupportedKinds,
			Media: content.Media, Quote: content.Quote, IgnoredKinds: content.IgnoredKinds})
	}
	return batch, nil
}
