package extproto

const MInboxLarkAction = "connector.inbox.lark_action"

// LarkInbox describes the current signed-in account's emoji picker and chat mode.
type LarkInbox struct {
	TopicMode   bool        `json:"topic_mode"`
	Emoji       []LarkEmoji `json:"emoji"`
	RecentEmoji []string    `json:"recent_emoji,omitempty"`
}

type LarkEmoji struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	URL   string `json:"url,omitempty"`
}

type LarkReaction struct {
	Key   string   `json:"key"`
	Count int      `json:"count"`
	Names []string `json:"names,omitempty"`
	Mine  bool     `json:"mine"`
}

type LarkMessage struct {
	Reactions  []LarkReaction `json:"reactions,omitempty"`
	ThreadID   string         `json:"thread_id,omitempty"`
	RootID     string         `json:"root_id,omitempty"`
	ReplyCount int            `json:"reply_count,omitempty"`
	Subscribed *bool          `json:"subscribed,omitempty"`
}

type InboxLarkActionRequest struct {
	ChatID    string `json:"chat_id"`
	Action    string `json:"action"`
	MessageID string `json:"message_id,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
	Key       string `json:"key,omitempty"`
	Active    *bool  `json:"active,omitempty"`
	Target    string `json:"target,omitempty"`
}

type InboxLarkActionResult struct {
	Messages []Message `json:"messages,omitempty"`
}
