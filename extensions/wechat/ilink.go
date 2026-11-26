package wechat

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ChannelVersion is the protocol version iLink is told.
const ChannelVersion = "0.3.0"

const (
	DefaultILinkBaseURL       = "https://ilinkai.weixin.qq.com"
	MessageTypeInboundText    = 1
	MessageTypeOutboundText   = 2
	MessageStateOutbound      = 2
	errcodeSessionExpired     = -14
	maxILinkResponseBytes     = 4 << 20
	endpointSendMessage       = "ilink/bot/sendmessage"
	endpointGetUpdates        = "ilink/bot/getupdates"
	endpointGetConfig         = "ilink/bot/getconfig"
	endpointSendTyping        = "ilink/bot/sendtyping"
	endpointGetUploadURL      = "ilink/bot/getuploadurl"
	clientIDPrefix            = "collie:"
	deterministicIDHexChars   = 24
	invalidBaseURLDescription = "iLink base URL must use HTTPS without userinfo, query, fragment or control characters"
)

var (
	// ErrSessionExpired: iLink answered errcode -14, the token is no longer
	// active (signed in elsewhere or expired). Scan again to continue.
	ErrSessionExpired = errors.New("WeChat login expired")
	errNotLoggedIn    = errors.New("WeChat is not logged in")
	errNonObject      = errors.New("WeChat iLink returned a non-object JSON response")
	errInvalidPoll    = errors.New("Invalid iLink poll timeout budget")
	errRedirect       = errors.New("redirect refused")
	unsafeURLChars    = regexp.MustCompile(`[\x00-\x20\x7f\\]`)
	digitsOnly        = regexp.MustCompile(`^\d+$`)
)

// validateILinkBaseURL never echoes the input or the parser's error: the
// value can contain credentials.
func validateILinkBaseURL(value string) (string, error) {
	invalid := errors.New(invalidBaseURLDescription)
	if unsafeURLChars.MatchString(value) {
		return "", invalid
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		strings.Contains(value, "?") || strings.Contains(value, "#") {
		return "", invalid
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// PollBatch is one getupdates answer.
type PollBatch struct {
	NextSyncBuf     string
	Messages        []InboundMessage
	RawMessageCount int
}

// ILinkClient talks to iLink for one bound account. Sends to one chat go
// out in order, one at a time.
type ILinkClient struct {
	BaseURL         string
	state           *StateManager
	http            *http.Client
	loop            *loop
	pollTimeoutMs   int64
	typing          *TypingIndicator
	chatSends       map[string]chan struct{}
	imageUploader   *ImageUploader
	fileUploader    *FileUploader
	beforeSendCheck func(chatID, deliveryKey string) error
}

// ILinkOptions configure an ILinkClient.
type ILinkOptions struct {
	ConfiguredBaseURL string
	PollTimeoutMs     int64
	HTTP              *http.Client
	MaxFileBytes      int64
}

func NewILinkClient(state *StateManager, l *loop, o ILinkOptions) (*ILinkClient, error) {
	poll := o.PollTimeoutMs
	if poll == 0 {
		poll = DefaultPollTimeoutMs
	}
	if poll <= 0 || poll > MaxPollTimeoutMs {
		return nil, errInvalidPoll
	}
	selected := o.ConfiguredBaseURL
	if selected == "" && state.Account != nil {
		selected = state.Account.BaseURL
	}
	if selected == "" {
		selected = DefaultILinkBaseURL
	}
	base, err := validateILinkBaseURL(selected)
	if err != nil {
		return nil, err
	}
	hc := http.Client{}
	if o.HTTP != nil {
		hc = *o.HTTP
	}
	// Never follow a redirect: the token must not leave iLink's origin.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirect }
	c := &ILinkClient{BaseURL: base, state: state, http: &hc, loop: l, pollTimeoutMs: poll, chatSends: map[string]chan struct{}{}}
	c.typing = newTypingIndicator(typingOptions{
		request: c.request,
		account: func() *typingAccount {
			if state.Account == nil {
				return nil
			}
			return &typingAccount{token: state.Account.Token, fingerprint: state.LoginFingerprint()}
		},
		contextToken: func(chat string) string { return state.ContextTokens[chat] },
	})
	upload := func(ctx context.Context, endpoint string, payload map[string]any, timeoutMs int64) (map[string]any, error) {
		if state.Account == nil || state.Account.Token == "" {
			return nil, errNotLoggedIn
		}
		payload["base_info"] = map[string]any{"channel_version": ChannelVersion}
		return c.request(ctx, endpoint, payload, state.Account.Token, timeoutMs, nil)
	}
	if c.imageUploader, err = newImageUploader(c, upload); err != nil {
		return nil, err
	}
	if c.fileUploader, err = newFileUploader(c, upload, o.MaxFileBytes); err != nil {
		return nil, err
	}
	return c, nil
}

// SetBeforeSendCheck runs under the chat's send lock, before any quota is
// spent or any intent persisted.
func (c *ILinkClient) SetBeforeSendCheck(check func(chatID, deliveryKey string) error) {
	c.beforeSendCheck = check
}

func (c *ILinkClient) consumeSendBudget(chatID, deliveryKey, kind string) error {
	if c.beforeSendCheck != nil {
		if err := c.beforeSendCheck(chatID, deliveryKey); err != nil {
			return err
		}
	}
	if kind == "" {
		kind = SendFinal
	}
	return c.state.ConsumeSendBudget(chatID, kind)
}

// PollBudgetMs is how long one getupdates may take.
func (c *ILinkClient) PollBudgetMs() int64 {
	return (c.pollTimeoutMs + PollNetworkMarginMs) * MaxPollAttempts
}

// sendTyping is best effort and never gates real work.
func (c *ILinkClient) sendTyping(chatID string) {
	go c.loop.do(func() { c.typing.send(context.Background(), chatID, typingOn) })
}

func randomUIN() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("wechat: no randomness: " + err.Error())
	}
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(binary.BigEndian.Uint32(b[:])), 10)))
}

func purposeOf(endpoint string) string {
	switch endpoint {
	case endpointSendMessage:
		return "sendmessage"
	case endpointGetUpdates:
		return "getupdates"
	case endpointGetConfig:
		return "getconfig"
	case endpointSendTyping:
		return "sendtyping"
	}
	return "media"
}

// request POSTs one iLink call and decodes its JSON object. The loop is
// released while the network works.
func (c *ILinkClient) request(ctx context.Context, endpoint string, payload map[string]any, token string, timeoutMs int64,
	diagnostics *RequestDiagnostics) (map[string]any, error) {
	if err := abortCause(ctx); err != nil {
		return nil, err
	}
	u := c.BaseURL + "/" + strings.TrimLeft(endpoint, "/")
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, msDuration(timeoutMs))
	defer cancel()
	started := time.Now()
	purpose := purposeOf(endpoint)
	outcome := "error"
	traceEvent(ctx, "wechat.http.start", Fields{"purpose": purpose})
	defer func() {
		traceEvent(ctx, "wechat.http.end", Fields{"purpose": purpose, "outcome": outcome, "durationMs": msSince(started)})
	}()
	fail := func(err error) (map[string]any, error) {
		if diagnostics != nil {
			err = diagnostics.failure(ctx, err, 1, 1, rctx.Err() != nil && ctx.Err() == nil, ctx.Err() != nil)
		}
		return nil, err
	}
	var status int
	var raw []byte
	var httpErr error
	c.loop.await(func() {
		req, err := http.NewRequestWithContext(rctx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			httpErr = err
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("AuthorizationType", "ilink_bot_token")
		req.Header.Set("X-WECHAT-UIN", randomUIN())
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.http.Do(req)
		if err != nil {
			httpErr = err
			return
		}
		defer resp.Body.Close()
		status = resp.StatusCode
		if diagnostics != nil {
			diagnostics.Phase, diagnostics.HTTPStatus = "headers", status
		}
		if status < 200 || status > 299 {
			return
		}
		if diagnostics != nil {
			diagnostics.Phase = "body"
		}
		raw, httpErr = io.ReadAll(io.LimitReader(resp.Body, maxILinkResponseBytes+1))
	})
	if httpErr != nil {
		if cause := abortCause(ctx); cause != nil {
			return fail(cause)
		}
		return fail(httpErr)
	}
	traceEvent(ctx, "wechat.http.headers", Fields{"purpose": purpose, "httpStatus": status, "durationMs": msSince(started)})
	if status < 200 || status > 299 {
		return fail(fmt.Errorf("WeChat iLink HTTP %d on %s", status, endpoint))
	}
	if len(raw) > maxILinkResponseBytes {
		return fail(errNonObject)
	}
	if diagnostics != nil {
		diagnostics.Phase = "decode"
	}
	if strings.TrimSpace(string(raw)) == "" && endpoint == endpointSendMessage {
		return fail(&SendReceiptError{ReceiptEmptyResponse})
	}
	parsed, err := ParseILinkJSON(string(raw))
	if err != nil {
		return fail(err)
	}
	if cause := abortCause(ctx); cause != nil {
		return fail(cause)
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return fail(errNonObject)
	}
	outcome = "ok"
	return obj, nil
}

func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "undefined"
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprint(v)
}

// truthy is JavaScript's truthiness for a decoded JSON value.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case json.Number:
		f, err := x.Float64()
		return err == nil && f != 0
	}
	return true
}

func nonZeroCode(v any) bool {
	if v == nil {
		return false
	}
	n, ok := safeInt(v)
	return !(ok && isJSONNumber(v) && n == 0)
}

// withChatSend runs send after every earlier send to chat finished.
func (c *ILinkClient) withChatSend(ctx context.Context, chatID string, send func() error) error {
	previous := c.chatSends[chatID]
	// The next sender waits for this slot and every earlier one: a waiter
	// that gives up early cannot let a later send pass a running one.
	released, done := make(chan struct{}), make(chan struct{})
	c.chatSends[chatID] = done
	go func() {
		if previous != nil {
			<-previous
		}
		<-released
		close(done)
	}()
	defer close(released)
	queued := time.Now()
	if previous != nil && !c.loop.wait(ctx, previous) {
		return abortCause(ctx)
	}
	if err := abortCause(ctx); err != nil {
		return err
	}
	traceEvent(ctx, "wechat.send.lock.acquired", Fields{"durationMs": msSince(queued)})
	return send()
}

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

// sendMessageItem sends one item and validates the receipt; the answer's
// message id, when it names one.
func (c *ILinkClient) sendMessageItem(ctx context.Context, chatID string, item map[string]any, clientID string) (string, error) {
	account := c.state.Account
	if account == nil || account.Token == "" {
		return "", errNotLoggedIn
	}
	contextToken := c.state.ContextTokens[chatID]
	if contextToken == "" {
		return "", fmt.Errorf("No context token for chat %s; poll a new inbound message first", chatID)
	}
	if clientID == "" {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		clientID = fmt.Sprintf("%s%d-%s", clientIDPrefix, nowMs(), hex.EncodeToString(b[:]))
	}
	diagnostics := newRequestDiagnostics("wechat_sendmessage", clientID)
	response, err := c.request(ctx, endpointSendMessage, map[string]any{
		"msg": map[string]any{"from_user_id": "", "to_user_id": chatID, "client_id": clientID,
			"message_type": MessageTypeOutboundText, "message_state": MessageStateOutbound,
			"item_list": []any{item}, "context_token": contextToken},
		"base_info": map[string]any{"channel_version": ChannelVersion},
	}, account.Token, SendRequestTimeoutMs, diagnostics)
	if err != nil {
		return "", err
	}
	diagnostics.Phase = "validate"
	if err := traceStep(ctx, "wechat.send.receipt.validate", nil, func() error { return ValidateSendReceipt(response) }); err != nil {
		return "", diagnostics.failure(ctx, err, 1, 1, false, ctx.Err() != nil)
	}
	diagnostics.success(ctx)
	traceEvent(ctx, "wechat.send.accepted", nil)
	return receiptMessageID(response), nil
}
