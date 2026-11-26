package wechat

import "context"

// Sends the tests make from outside the loop. Production sends go through
// the frozen plans (deliverReply), never these.

// SendSingleText sends one text message, charging the budget.
func (c *ILinkClient) SendSingleText(ctx context.Context, chatID, text, clientID string) (err error) {
	c.loop.do(func() {
		err = c.withChatSend(ctx, chatID, func() error {
			if err := c.consumeSendBudget(chatID, "", SendFinal); err != nil {
				return err
			}
			_, err := c.sendMessageItem(ctx, chatID, map[string]any{"type": ItemTypeText, "text_item": map[string]any{"text": text}}, clientID)
			return err
		})
	})
	return err
}

// SendMarkdownReply is sendMarkdownReply for callers outside the loop.
func (c *ILinkClient) SendMarkdownReply(ctx context.Context, chatID, text string, o ReplyOptions) (n int, err error) {
	c.loop.do(func() { n, _, err = c.sendMarkdownReply(ctx, chatID, text, o) })
	return n, err
}
