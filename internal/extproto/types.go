package extproto

import "encoding/json"

// Error codes. Extensions may add their own for their methods.
const (
	ErrNotInitialized = "not_initialized"
	ErrUnknownMethod  = "unknown_method"
	ErrWrongProtocol  = "wrong_protocol"
	ErrNoContribution = "no_contribution"
	ErrInvalidParams  = "invalid_params"
	ErrTimeout        = "timeout"
	ErrClosed         = "closed"
	ErrInternal       = "internal"
	ErrNotReady       = "not_ready" // channel.send: the connector is starting
	ErrNotSignedIn    = "not_signed_in"
	ErrCannotSend     = "cannot_send"
	ErrHubUnavailable = "hub_unavailable"
	ErrNotRunning     = "not_running" // the extension's program isn't up (the process side answers)
	ErrUncertain      = "uncertain"   // channel.send: it may have gone out; never sent again
)

// Methods the host calls.
const (
	MInitialize     = "initialize"
	MPing           = "ping"
	MStatus         = "connector.status"
	MLoginStep      = "connector.login.step"
	MLogout         = "connector.logout"
	MArchiveChanges = "connector.archive.changes"
	MActionsDo      = "connector.actions.do"
	MHeaderStatus   = "connector.header_status"
	MChannelSend    = "channel.send"
	MInboxRecent    = "connector.inbox.recent"
	MInboxMedia     = "connector.inbox.media"
	MInboxAction    = "connector.inbox.action"
)

// Methods and events the extension sends.
const (
	MSecretsLoad     = "connector.secrets.load"
	MSecretsSave     = "connector.secrets.save"
	MSecretsDelete   = "connector.secrets.delete"
	MChannelReceive  = "channel.receive"
	EChanged         = "connector.changed"
	EDelivery        = "channel.delivery" // legacy notification
	MChannelDelivery = EDelivery
)

// InitializeParams is the first call after the program starts.
type InitializeParams struct {
	Protocol  int            `json:"protocol"`
	Host      HostInfo       `json:"hub"`
	Extension ExtensionInfo  `json:"extension"`
	Config    map[string]any `json:"config"`
}

type HostInfo struct {
	Name    string `json:"name"`    // "collie"
	Version string `json:"version"` // Collie's version
}

type ExtensionInfo struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Dir     string `json:"dir"`      // the package directory
	DataDir string `json:"data_dir"` // its own data, kept across upgrades
}

type InitializeResult struct {
	Protocol int    `json:"protocol"`
	ID       string `json:"id"`
	Version  string `json:"version"`
}

// Contribution names the connector a call is about.
type Contribution struct {
	Contribution string `json:"contribution,omitempty"`
}

// Status is connector.status's answer.
type Status struct {
	State   string `json:"state"` // ok | not_signed_in | error
	Error   string `json:"error,omitempty"`
	Account string `json:"account,omitempty"`
	// AccountID: the account's id, when Account is a name for people
	// (shown on hover).
	AccountID string   `json:"account_id,omitempty"`
	Guide     []string `json:"guide,omitempty"`
	LastSync  string   `json:"last_sync,omitempty"`
	// Extensions may add their own fields: the host shows Detail as is.
	Detail map[string]any `json:"detail,omitempty"`
}

const (
	StateOK          = "ok"
	StateNotSignedIn = "not_signed_in"
	StateError       = "error"
)

// LoginStep is connector.login.step's answer: a QR code to scan (Kind "qr",
// URL the text to encode), or Done.
type LoginStep struct {
	Step     string `json:"step,omitempty"`
	Kind     string `json:"kind,omitempty"`
	URL      string `json:"url,omitempty"`
	Interval int    `json:"interval,omitempty"` // seconds until the next step call
	Hint     string `json:"hint,omitempty"`
	Error    string `json:"error,omitempty"`
	Done     bool   `json:"done,omitempty"`
	Account  any    `json:"account,omitempty"`
	// Kind "secret": ask for a key to paste. Label names it; the next step
	// call carries it as "secret".
	Label string `json:"label,omitempty"`
}

// Secrets is what connector.secrets.load answers and save takes: the
// extension's own opaque blob, base64.
type Secrets struct {
	Contribution string `json:"contribution,omitempty"`
	Data         string `json:"data,omitempty"`
}

// ArchiveChanges is connector.archive.changes's answer.
type ArchiveChanges struct {
	Batches []ArchiveBatch `json:"batches"`
	Next    string         `json:"next"`
	More    bool           `json:"more"`
}

type ArchiveBatch struct {
	Chat     ArchiveChat      `json:"chat"`
	Messages []ArchiveMessage `json:"messages"`
}

type ArchiveChat struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"` // dm | group | self
	UpdatedAt string `json:"updated_at,omitempty"`
	Muted     *bool  `json:"muted,omitempty"`
}

type ArchiveMessage struct {
	ID         string   `json:"id"`
	SenderID   string   `json:"sender_id"`
	SenderName string   `json:"sender_name"`
	Mine       bool     `json:"mine,omitempty"`
	Type       string   `json:"type"`
	Text       string   `json:"text"`
	At         string   `json:"at"` // RFC 3339
	Media      []string `json:"media,omitempty"`
}

// ActionsDo calls one of the extension's actions; RequestID makes it run once.
type ActionsDo struct {
	Contribution string         `json:"contribution,omitempty"`
	Name         string         `json:"name"`
	Args         map[string]any `json:"args"`
	RequestID    string         `json:"request_id"`
}

type ActionResult struct {
	Text  string            `json:"text"`
	Files []ActionFile      `json:"files,omitempty"` // on the computer the extension runs on
	Items []json.RawMessage `json:"items,omitempty"` // structured, passed on as they are
}

type ActionFile struct {
	Path string `json:"path"`
	Mime string `json:"mime,omitempty"`
}

// Login kinds.
const (
	LoginQR     = "qr"
	LoginSecret = "secret"
)

// Blob is a picture or file, base64.
type Blob struct {
	Name string `json:"name,omitempty"`
	Mime string `json:"mime"`
	Data string `json:"data"`
}

// Message is what channel.receive hands to the host.
type Message struct {
	ID         string `json:"id"` // the chat app's id: delivered again, it is taken once
	Text       string `json:"text"`
	ChatID     string `json:"chat_id,omitempty"`
	Chat       string `json:"chat,omitempty"`      // display name
	ChatKind   string `json:"chat_kind,omitempty"` // dm | group | self | doc (a document's comments)
	Sender     string `json:"sender,omitempty"`
	SenderName string `json:"sender_name,omitempty"`
	Owner      *bool  `json:"owner,omitempty"`  // sent by the signed-in person
	Muted      *bool  `json:"muted,omitempty"`  // the chat is muted in the chat app (nil: not known)
	Folded     *bool  `json:"folded,omitempty"` // folded away in the chat app's list (nil: not known)
	// ChatTags: what the chat is besides its kind, for the 收件箱 rules
	// (x-hide "tag:<tag>"), e.g. a group a project tool opened for one issue.
	ChatTags []string `json:"chat_tags,omitempty"`
	// ChatAvatar: the chat's picture, an https address on one of the
	// manifest's permissions.network hosts; Collie fetches and keeps it.
	ChatAvatar string `json:"chat_avatar,omitempty"`
	// SenderAvatar: the sender's picture, the same kind of address, in a
	// recent window (inbox.recent).
	SenderAvatar string `json:"sender_avatar,omitempty"`
	// AtMe: someone else @-mentioned the signed-in person, or everyone.
	AtMe bool `json:"at_me,omitempty"`
	// Mention: the signed-in person @-mentioned themselves (the @ already
	// taken out of Text): a request like the trigger word.
	Mention     bool         `json:"mention,omitempty"`
	At          string       `json:"at,omitempty"`
	Quote       string       `json:"quote,omitempty"`
	Forward     string       `json:"forward,omitempty"`
	Thread      string       `json:"thread,omitempty"`
	Images      []Blob       `json:"images,omitempty"`
	Files       []Blob       `json:"files,omitempty"`
	Unsupported []string     `json:"unsupported,omitempty"`
	Preview     string       `json:"preview,omitempty"`
	View        *MessageView `json:"view,omitempty"`
	Updated     bool         `json:"updated,omitempty"` // in-place update of an existing message (e.g. card state changed)
}

// MessageView carries readable content independently of the text used for requests.
type MessageView struct {
	Kind     string        `json:"kind"`
	Title    string        `json:"title,omitempty"`
	Subtitle string        `json:"subtitle,omitempty"`
	Tone     string        `json:"tone,omitempty"`
	URL      string        `json:"url,omitempty"`
	Start    string        `json:"start,omitempty"`
	End      string        `json:"end,omitempty"`
	AllDay   bool          `json:"all_day,omitempty"`
	Conflict bool          `json:"conflict,omitempty"` // calendar: overlaps another busy event
	Status   string        `json:"status,omitempty"`   // calendar: needs_action | accept | decline | tentative | cancelled | removed
	Action   string        `json:"action,omitempty"`   // calendar: interactive action id, signed by hub into Sig
	Sig      string        `json:"sig,omitempty"`      // hub HMAC signature authorizing POST /api/inbox/action
	Nodes    []MessageNode `json:"nodes,omitempty"`
}

type MessageNode struct {
	Kind        string          `json:"kind"`
	Text        string          `json:"text,omitempty"`
	Tone        string          `json:"tone,omitempty"`
	URL         string          `json:"url,omitempty"`
	Key         string          `json:"key,omitempty"` // source-specific attachment key, converted to a hub URL
	Action      string          `json:"action,omitempty"`
	Sig         string          `json:"sig,omitempty"`
	Value       string          `json:"value,omitempty"`
	Placeholder string          `json:"placeholder,omitempty"`
	Name        string          `json:"name,omitempty"`
	Disabled    bool            `json:"disabled,omitempty"`
	Options     []MessageOption `json:"options,omitempty"`
	Children    []MessageNode   `json:"children,omitempty"`
}

type MessageOption struct {
	Text   string `json:"text"`
	Value  string `json:"value,omitempty"`
	URL    string `json:"url,omitempty"`
	Action string `json:"action,omitempty"`
	Sig    string `json:"sig,omitempty"`
}

type InboxRecentRequest struct {
	ChatID string `json:"chat_id"`
}

type InboxRecentResult struct {
	Messages []Message `json:"messages"`
}

type InboxMediaRequest struct {
	ChatID    string `json:"chat_id"`
	MessageID string `json:"message_id"`
	Key       string `json:"key"`
}

type InboxActionRequest struct {
	ChatID    string            `json:"chat_id"`
	MessageID string            `json:"message_id"`
	ActionID  string            `json:"action_id"`
	Kind      string            `json:"kind,omitempty"`
	Value     string            `json:"value,omitempty"`
	Params    map[string]string `json:"params,omitempty"`
}

type InboxActionResult struct {
	Toast   string   `json:"toast,omitempty"`
	Message *Message `json:"message,omitempty"`
}

type Receive struct {
	Contribution string  `json:"contribution,omitempty"`
	Message      Message `json:"message"`
}

// Send kinds.
const (
	KindAnswer   = "answer"   // the answer to a message
	KindProgress = "progress" // a note while still working
	KindResult   = "result"   // a timer's or task's result
	KindPush     = "push"     // sent on the human's explicit ask
)

// Send is channel.send's params. Ref is the host's key for this send: the
// same Ref is sent at most once.
type Send struct {
	Contribution string `json:"contribution,omitempty"`
	Ref          string `json:"ref"`
	Chat         string `json:"chat,omitempty"`
	Text         string `json:"text"`
	// Footer is the footer Text ends with, for a channel that sets it apart
	// (larkx shows it as a quote); a plain channel just sends Text.
	Footer  string `json:"footer,omitempty"`
	Kind    string `json:"kind"`
	ReplyTo string `json:"reply_to,omitempty"`
	Images  []Blob `json:"images,omitempty"`
	Files   []Blob `json:"files,omitempty"`
	// Link is a download address for a file too big to send (result kind).
	Link string `json:"link,omitempty"`
}

// SendResult is channel.send's answer. A send that has to wait (WeChat's
// reply window) answers Deferred and reports later with a delivery event.
type SendResult struct {
	MessageID string `json:"message_id,omitempty"`
	Deferred  bool   `json:"deferred,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Delivery is the channel.delivery receipt: what became of a send.
type Delivery struct {
	Contribution string `json:"contribution,omitempty"`
	Ref          string `json:"ref"`
	State        string `json:"state"` // sent | deferred | uncertain | failed | expired
	MessageID    string `json:"message_id,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

const (
	DeliverySent      = "sent"
	DeliveryDeferred  = "deferred"
	DeliveryUncertain = "uncertain"
	DeliveryFailed    = "failed"
	DeliveryExpired   = "expired"
)

// Changed is the connector.changed event.
type Changed struct {
	Contribution string `json:"contribution,omitempty"`
	Scope        string `json:"scope"` // inbox | chats | status | outbox | header_status
}

// HeaderStatusParams asks a header_status-capable extension for its current
// session-header summary. Model is the active session's model id when known;
// Fresh asks the extension to bypass any short-lived cache.
type HeaderStatusParams struct {
	Contribution string `json:"contribution,omitempty"`
	Model        string `json:"model,omitempty"`
	Fresh        bool   `json:"fresh,omitempty"`
}

// HeaderStatus is connector.header_status's answer: compact items for the
// session header bar and optional structured sections for its detail popover.
type HeaderStatus struct {
	Title     string                `json:"title,omitempty"`
	Compact   string                `json:"compact,omitempty"`
	Subline   string                `json:"subline,omitempty"`
	Tone      string                `json:"tone,omitempty"` // "" | ok | warn | danger | muted
	UpdatedAt string                `json:"updated_at,omitempty"`
	Error     string                `json:"error,omitempty"`
	Items     []HeaderStatusItem    `json:"items,omitempty"`
	Sections  []HeaderStatusSection `json:"sections,omitempty"`
}

// HeaderStatusItem is one compact metric shown in the header bar or a detail row.
type HeaderStatusItem struct {
	Label   string   `json:"label,omitempty"`
	Value   string   `json:"value,omitempty"`
	Sub     string   `json:"sub,omitempty"`
	Icon    string   `json:"icon,omitempty"`    // optional built-in icon key (claude, codex, agy, gemini)
	Accent  string   `json:"accent,omitempty"`  // optional brand accent (claude, codex, agy, gemini)
	Percent *float64 `json:"percent,omitempty"` // 0..100 when a mini bar should be drawn
	Tone    string   `json:"tone,omitempty"`    // "" | ok | info | warn | hot | danger | muted
	Tooltip string   `json:"tooltip,omitempty"`
}

// HeaderStatusBadge is a compact semantic tag attached to a detail row.
type HeaderStatusBadge struct {
	Text string `json:"text"`
	Tone string `json:"tone,omitempty"` // "" | ok | info | warn | hot | danger | muted
}

// HeaderStatusAction describes an extension action triggered by clicking a
// selectable row or section chip inside the header status panel.
type HeaderStatusAction struct {
	Name  string         `json:"name"`
	Args  map[string]any `json:"args,omitempty"`
	Label string         `json:"label,omitempty"`
}

// HeaderStatusChip is a compact interactive toggle pill in a section header.
type HeaderStatusChip struct {
	Label    string              `json:"label"`
	Selected bool                `json:"selected,omitempty"`
	Tone     string              `json:"tone,omitempty"`
	Action   *HeaderStatusAction `json:"action,omitempty"`
}

// HeaderStatusSection is one group of detail rows inside the header status popover.
type HeaderStatusSection struct {
	Title       string             `json:"title,omitempty"`
	Meta        string             `json:"meta,omitempty"`
	Icon        string             `json:"icon,omitempty"`        // optional built-in icon key (e.g. claude, codex, agy, gemini)
	Accent      string             `json:"accent,omitempty"`      // optional brand accent
	Collapsible bool               `json:"collapsible,omitempty"` // allow collapsing/expanding this group
	Collapsed   bool               `json:"collapsed,omitempty"`   // initial collapsed state when Collapsible is true
	Chips       []HeaderStatusChip `json:"chips,omitempty"`       // optional quick toggle pills in the header
	Rows        []HeaderStatusRow  `json:"rows,omitempty"`
}

// HeaderStatusRow is one row inside a HeaderStatusSection.
type HeaderStatusRow struct {
	Label      string              `json:"label"`
	Sublabel   string              `json:"sublabel,omitempty"`
	Icon       string              `json:"icon,omitempty"`
	Accent     string              `json:"accent,omitempty"`
	Badge      string              `json:"badge,omitempty"`
	Badges     []HeaderStatusBadge `json:"badges,omitempty"`
	Value      string              `json:"value,omitempty"`
	Metric     *HeaderStatusItem   `json:"metric,omitempty"`
	Detail     string              `json:"detail,omitempty"`
	Tone       string              `json:"tone,omitempty"`
	Current    bool                `json:"current,omitempty"`
	Selected   bool                `json:"selected,omitempty"`
	Action     *HeaderStatusAction `json:"action,omitempty"`
	Meters     []HeaderStatusItem  `json:"meters,omitempty"`
	FoldedRows []HeaderStatusRow   `json:"folded_rows,omitempty"`
}

// MChannelWorking marks a message Collie took as a request: the chat app
// shows it is being worked on (larkx: a reaction).
const MChannelWorking = "channel.working"

// Working is channel.working's params.
type Working struct {
	Contribution string `json:"contribution,omitempty"`
	Chat         string `json:"chat"`
	MessageID    string `json:"message_id"`
}

// MChannelSettings tells a working-capable extension the trigger word, so
// it can mark your own request at once, before Collie's answer to it.
const MChannelSettings = "channel.settings"

// ChannelSettings is channel.settings's params.
type ChannelSettings struct {
	Contribution string `json:"contribution,omitempty"`
	Trigger      string `json:"trigger"`
	// Ignored: 来源策略 ignores this extension's requests, so none is taken
	// and none may be marked as being worked on.
	Ignored bool `json:"ignored,omitempty"`
}

// MChannelInterest tells the extension which chats wait for results.
const MChannelInterest = "channel.interest"

// Interest is channel.interest's params.
type Interest struct {
	Contribution string   `json:"contribution,omitempty"`
	Chats        []string `json:"chats"`
}

// MReadState asks a read-capable extension how many messages each chat has
// unread in the chat app itself.
const MReadState = "connector.read.state"

// MReadMark marks chats read in the chat app.
const MReadMark = "connector.read.mark"

// ReadChats is the params of connector.read.state and connector.read.mark.
type ReadChats struct {
	Contribution string   `json:"contribution,omitempty"`
	Chats        []string `json:"chats"`
}

// ReadStates is connector.read.state's result: the chats it could read,
// the rest left as they are.
type ReadStates struct {
	Chats map[string]ChatRead `json:"chats"`
}

type ChatRead struct {
	Unread int `json:"unread"`
	// AtMe: an unread message there @-mentions the signed-in person or
	// everyone, as the app keeps it (nil: not known).
	AtMe *bool `json:"at_me,omitempty"`
	// Avatar: the chat's picture, as chat_avatar ("" not known).
	Avatar string `json:"avatar,omitempty"`
	// Name: the chat's name as the app shows it now ("" not known), so a
	// name given before (or by an older extension) is put right.
	Name string `json:"name,omitempty"`
	// Tags: as chat_tags, so a chat that has been there all along gets them (nil: not known).
	Tags []string `json:"tags,omitempty"`
}

// ReadMarked is connector.read.mark's result: the chats marked, and why the others weren't.
type ReadMarked struct {
	Marked []string          `json:"marked"`
	Failed map[string]string `json:"failed,omitempty"`
}
