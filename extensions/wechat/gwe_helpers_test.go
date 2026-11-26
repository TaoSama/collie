package wechat

import (
	"context"
	"path/filepath"
	"testing"
)

// gweFreeze stores Collie's answer under ref and its outbox reference, as
// a reply that arrived and is still to deliver.
func gweFreeze(t *testing.T, g *Gateway, ref, chat, text string, splitMax int) {
	t.Helper()
	g.loop.do(func() {
		_, err := g.answers.put(FrozenReply{Ref: ref, ChatID: chat, Kind: "answer", Text: text, CreatedAt: nowMs(),
			Identity: hashJSON([]any{chat, text})}, nil, nil)
		mustNoErr(t, err)
		mustNoErr(t, g.state.PutOutbox(DeliveryOutboxItem{Kind: "reply", Source: SourceEngine, ChatID: chat, MessageID: "message-" + ref,
			DeliveryKey: ref, SplitMaxChars: splitMax, Status: OutboxPending}))
	})
}

// gweReopen is a restart: the state read back from disk and a new gateway
// over the same data directory and fake network.
func gweReopen(t *testing.T, h *harness) (*StateManager, *Gateway) {
	t.Helper()
	s, err := NewStateManager(testAccountFor(testToken), filepath.Join(h.dir, stateFileName), PolicyOptions{})
	mustNoErr(t, err)
	return s, gweOver(t, h, s)
}

// gweOver is another gateway on h's directory and network for state s.
func gweOver(t *testing.T, h *harness, s *StateManager) *Gateway {
	t.Helper()
	l := &loop{}
	c := newTestClient(t, s, l, h.http)
	var g *Gateway
	var err error
	l.do(func() {
		g, err = NewGateway(GatewayOptions{State: s, ILink: c, Host: h.host, DataDir: h.dir, MediaHTTP: h.http, Health: &HealthTracker{}})
	})
	mustNoErr(t, err)
	t.Cleanup(g.Stop)
	return g
}

func gweNotify(g *Gateway) (ResultDispatchSummary, error) {
	var s ResultDispatchSummary
	var err error
	g.loop.do(func() { s, err = g.deliverNotificationsOnce(context.Background()) })
	return s, err
}

func gweStates(h *harness) []string {
	var out []string
	for _, e := range h.host.events() {
		out = append(out, e.State)
	}
	return out
}
