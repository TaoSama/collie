package wechat

import (
	"context"
	"errors"

	"github.com/TaoSama/collie/internal/extproto"
)

// Delivery outcomes Collie is told about.
const (
	DeliverySent      = extproto.DeliverySent
	DeliveryDeferred  = extproto.DeliveryDeferred
	DeliveryUncertain = extproto.DeliveryUncertain
	DeliveryFailed    = extproto.DeliveryFailed
	DeliveryExpired   = extproto.DeliveryExpired
)

// Host is Collie as the gateway sees it: where messages go and outcomes
// are reported.
type Host interface {
	// Receive hands one message over; nil means Collie has it.
	Receive(ctx context.Context, m extproto.Message) error
	// Delivery reports what became of a send. Nil means Collie recorded it.
	Delivery(ctx context.Context, d extproto.Delivery) error
	// Changed tells Collie something it shows changed (status, outbox).
	Changed(scope string) error
}

// ErrChatNotAllowed: the message is not from the bound person.
var ErrChatNotAllowed = errors.New("CHAT_NOT_ALLOWED")

// peerHost is Host over the extension protocol.
type peerHost struct {
	peer         *extproto.Peer
	contribution string
}

func (h *peerHost) Receive(ctx context.Context, m extproto.Message) error {
	ctx, cancel := context.WithTimeout(ctx, hostCallTimeout)
	defer cancel()
	return h.peer.Call(ctx, extproto.MChannelReceive, extproto.Receive{Contribution: h.contribution, Message: m}, nil)
}

func (h *peerHost) Delivery(ctx context.Context, d extproto.Delivery) error {
	d.Contribution = h.contribution
	d.Reason = humanReason(d.State, d.Reason)
	ctx, cancel := context.WithTimeout(ctx, hostCallTimeout)
	defer cancel()
	return h.peer.Call(ctx, extproto.MChannelDelivery, d, nil)
}

func (h *peerHost) Changed(scope string) error {
	return h.peer.Notify(extproto.EChanged, extproto.Changed{Contribution: h.contribution, Scope: scope})
}

// reasonTexts say, for the person reading Collie's screen, why a send waits
// or ended; Collie shows them as they are. Codes stay inside the gateway.
var reasonTexts = map[string]string{
	DeferWindowExpired:      "微信回复窗口已过期，等你在微信发一条消息后送达",
	ResultWaitingForUser:    "微信回复窗口已过期，等你在微信发一条消息后送达",
	DeferBudgetExhausted:    "本轮回复额度已用完，等你在微信发一条消息后送达",
	DeferContextChanged:     "微信会话刚刚刷新，稍后重试",
	"reply_pending":         "还有回复在发送，发完后再送达",
	"attempt_expired":       "上次发送超时，稍后重试",
	"artifact_unavailable":  "附件暂时读不到，稍后重试",
	"not_submitted":         "没有发出，稍后重试",
	"rejected":              "微信拒收了这条消息，等你在微信发一条消息后再试",
	reasonExpired:           "结果已过期，没有送达",
	reasonSuperseded:        "被更新的结果取代，没有送达",
	reasonSendUnknown:       "可能已发出，去微信确认",
	"previous_send_unknown": "可能已发出，去微信确认",
	reasonQueued:            "已排队，按微信回复窗口和额度依次送达",
	reasonRetrying:          "暂时发不出，稍后自动重试",
}

// stateTexts explain an outcome reported without a reason.
var stateTexts = map[string]string{
	DeliveryUncertain: "可能已发出，去微信确认",
	DeliveryExpired:   "没有送达：微信多次拒收或附件已失效",
	DeliveryFailed:    "发送失败",
	DeliveryDeferred:  "暂时发不出，稍后自动重试",
}

// humanReason is the sentence Collie shows for a reason code (or for the
// state when there is none). Unknown codes fall back to the state's text.
func humanReason(state, code string) string {
	if t, ok := reasonTexts[code]; ok {
		return t
	}
	if state == DeliverySent {
		return ""
	}
	return stateTexts[state]
}
