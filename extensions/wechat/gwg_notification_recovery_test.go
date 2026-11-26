package wechat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The legacy notification outbox was merged into the result queue: these
// tests drive the ResultDispatcher against a fake result service that plays
// the original fake Controller, with the real state, result outbox, runner
// and iLink client.

type gwgSvc struct {
	mu      sync.Mutex
	plans   map[string]ResultDelivery
	calls   []string
	claim   func(id string) error // after the change, like a lost response
	defer_  func() error          // after the change
	receipt func() error          // before the change
	list    func(chat string) error
}

func (s *gwgSvc) call(c string) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
}

func (s *gwgSvc) resetCalls() { s.mu.Lock(); s.calls = nil; s.mu.Unlock() }

func (s *gwgSvc) callList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *gwgSvc) has(prefix string) bool {
	for _, c := range s.callList() {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (s *gwgSvc) get(id string) ResultDelivery { s.mu.Lock(); defer s.mu.Unlock(); return s.plans[id] }

func (s *gwgSvc) set(p ResultDelivery) { s.mu.Lock(); s.plans[p.ID] = p; s.mu.Unlock() }

func (s *gwgSvc) listDeliveries(chats []string) ([]ResultDelivery, error) {
	s.call("list:" + strings.Join(chats, ","))
	if s.list != nil {
		if err := s.list(chats[0]); err != nil {
			return nil, err
		}
	}
	var out []ResultDelivery
	for _, p := range s.plans {
		if containsString(chats, p.ChatID) && p.Status == ResultReady && p.ExpiresAt > nowMs() {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Kind == KindProgressResult) != (out[j].Kind == KindProgressResult) {
			return out[j].Kind == KindProgressResult
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > 1 {
		out = out[:1]
	}
	return out, nil
}

func (s *gwgSvc) getDelivery(id string) (ResultDelivery, error) {
	s.call("get:" + id)
	p, ok := s.plans[id]
	if !ok {
		return ResultDelivery{}, errResultNotFound
	}
	if p.Status == ResultClaimed && p.AttemptExpiresAt <= nowMs() {
		p.Status = ResultUncertain
	}
	return p, nil
}

func (s *gwgSvc) claimDelivery(id, attempt string) (ResultDelivery, error) {
	s.call("claim:" + id)
	next := claimDelivery(s.plans[id], attempt, nowMs())
	if next == nil {
		return ResultDelivery{}, errors.New("Result request HTTP 409")
	}
	s.plans[id] = *next
	if s.claim != nil {
		if err := s.claim(id); err != nil {
			return ResultDelivery{}, err
		}
	}
	return s.plans[id], nil
}

func (s *gwgSvc) materializeDelivery(id, _ string) (ResultDelivery, error) {
	s.call("materialize:" + id)
	return s.plans[id], nil
}

func (s *gwgSvc) recordReceipt(id, attempt, status string) (ResultDelivery, error) {
	s.call("receipt:" + id)
	if s.receipt != nil {
		if err := s.receipt(); err != nil {
			return ResultDelivery{}, err
		}
	}
	next := receiptDelivery(s.plans[id], status, attempt)
	if next == nil {
		return ResultDelivery{}, errors.New("Result request HTTP 409")
	}
	s.plans[id] = *next
	return *next, nil
}

func (s *gwgSvc) deferDelivery(id, attempt, reason string, submitted int) (ResultDelivery, error) {
	s.call("defer:" + id)
	next := deferDelivery(s.plans[id], attempt, reason, nowMs(), submitted)
	if next == nil {
		return ResultDelivery{}, errors.New("Result request HTTP 409")
	}
	s.plans[id] = *next
	if s.defer_ != nil {
		if err := s.defer_(); err != nil {
			return ResultDelivery{}, err
		}
	}
	return *next, nil
}

func (s *gwgSvc) resumeDeliveries(chat, _ string) error {
	s.call("resume:" + chat)
	for id, p := range s.plans {
		if p.ChatID == chat && p.Status == ResultWaitingForUser {
			p.Status, p.DeferReason = ResultReady, ""
			s.plans[id] = p
		}
	}
	return nil
}

func (s *gwgSvc) blockChatDeliveries(chat, reason string) error {
	s.call("block:" + chat)
	for id, p := range s.plans {
		if p.ChatID == chat && p.Status == ResultReady && p.SubmittedMessages == 0 {
			p.Status, p.DeferReason = ResultWaitingForUser, reason
			s.plans[id] = p
		}
	}
	return nil
}

// gwgNotification is the original fixture's notification(index, chat).
func gwgNotification(index int, chat string) ResultDelivery {
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
	now := nowMs()
	text := "计划 " + id + " 已收到精确完成回执。"
	retained := now + 86400000
	m := fileMetadataFor(resultArtifactName, []byte(text))
	return ResultDelivery{ID: id, Key: "notification:" + id, ChatID: chat, Source: ResultSource{Type: "plan", ID: id}, Kind: "completed",
		Status: ResultReady, Mode: ModeText, Messages: []string{text}, CreatedAt: now - 1000, ExpiresAt: retained, RetainedUntil: retained,
		Artifact: ResultArtifact{ID: fmt.Sprintf("00000000-0000-4000-9000-%012d", index), ChatID: chat, Name: resultArtifactName,
			Size: m.ByteLength, SHA256: m.SHA256, ExpiresAt: retained}}
}

func gwgWithText(p ResultDelivery, text string) ResultDelivery { p.Messages = []string{text}; return p }

func gwgProgress(index int, source ResultDelivery) ResultDelivery {
	p := gwgNotification(index, source.ChatID)
	p.Source, p.Kind = source.Source, KindProgressResult
	return p
}

type gwgSent struct{ chat, text, clientID string }

type gwgRecovery struct {
	t     *testing.T
	dir   string
	now   interface{ Add(int64) int64 }
	svc   *gwgSvc
	loop  *loop
	state *StateManager
	store *FileResultOutbox

	mu         sync.Mutex
	sends      []gwgSent
	sendHook   func() error
	typingHook func()
	budgetHook func()
	inbounds   int
}

func gwgRecoverySetup(t *testing.T, items ...ResultDelivery) *gwgRecovery {
	f := &gwgRecovery{t: t, dir: t.TempDir(), svc: &gwgSvc{plans: map[string]ResultDelivery{}}, loop: &loop{}}
	f.now = setNow(t, time.Now().UnixMilli())
	// Typing runs in the background and reads the clock: let it settle before the clock is restored.
	t.Cleanup(func() { time.Sleep(30 * time.Millisecond); f.loop.do(func() {}) })
	if len(items) == 0 {
		items = []ResultDelivery{gwgNotification(1, testChat)}
	}
	f.restart()
	mustNoErr(t, f.state.MarkBaselineEstablished())
	for _, item := range items {
		f.svc.plans[item.ID] = item
		f.state.SetContextToken(item.ChatID, testContext)
		_, err := f.state.RecordInboundContext("inbound:"+item.ChatID, item.ChatID, i64(nowMs()), true, "")
		mustNoErr(t, err)
	}
	return f
}

// restart is a new state and result outbox from disk.
func (f *gwgRecovery) restart() {
	s, err := NewStateManager(testAccountFor("synthetic"), filepath.Join(f.dir, stateFileName), PolicyOptions{})
	mustNoErr(f.t, err)
	o, err := NewFileResultOutbox(gwgOutboxPath(f.dir), s.LoginFingerprint)
	mustNoErr(f.t, err)
	f.state, f.store = s, o
}

// durable is the state as a restart would see it.
func (f *gwgRecovery) durable() (*StateManager, *FileResultOutbox) {
	s, err := NewStateManager(testAccountFor("synthetic"), filepath.Join(f.dir, stateFileName), PolicyOptions{})
	mustNoErr(f.t, err)
	o, err := NewFileResultOutbox(gwgOutboxPath(f.dir), s.LoginFingerprint)
	mustNoErr(f.t, err)
	return s, o
}

func (f *gwgRecovery) durableRef(id string) (ResultReference, bool) {
	_, o := f.durable()
	refs, err := o.References()
	mustNoErr(f.t, err)
	for _, r := range refs {
		if r.ID == id {
			return r, true
		}
	}
	return ResultReference{}, false
}

// inbound is a real new message of chat: a fresh window and a resume intent.
func (f *gwgRecovery) inbound(chat, token string) {
	f.inbounds++
	key := fmt.Sprintf("new-inbound-%d", f.inbounds)
	_, err := f.state.RecordInboundContext(key, chat, i64(nowMs()), true, token)
	mustNoErr(f.t, err)
	mustNoErr(f.t, f.store.QueueResume(chat, key))
}

func (f *gwgRecovery) setRemaining(n int64) {
	f.state.AccountBudget.Remaining = n
	mustNoErr(f.t, f.state.SaveState())
}

func (f *gwgRecovery) fetch(req *http.Request, body []byte) (*http.Response, error) {
	switch endpoint(req) {
	case "getconfig":
		if f.typingHook != nil {
			f.typingHook()
		}
		return jsonResponse(200, map[string]any{"ret": 0, "typing_ticket": "ticket"}), nil
	case "sendtyping":
		return jsonResponse(200, map[string]any{"ret": 0}), nil
	}
	msg := decodeBody(f.t, body)["msg"].(map[string]any)
	f.mu.Lock()
	f.sends = append(f.sends, gwgSent{msg["to_user_id"].(string), gwgText(msg), msg["client_id"].(string)})
	hook := f.sendHook
	f.mu.Unlock()
	if hook != nil {
		if err := hook(); err != nil {
			return nil, err
		}
	}
	return jsonResponse(200, map[string]any{"ret": 0}), nil
}

func (f *gwgRecovery) sent() []gwgSent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gwgSent(nil), f.sends...)
}

func (f *gwgRecovery) texts() []string {
	var out []string
	for _, s := range f.sent() {
		out = append(out, s.text)
	}
	return out
}

type gwgTransport struct {
	c        *ILinkClient
	maxChars int
}

func (t gwgTransport) SendText(ctx context.Context, chat, text string, o ResultSendOptions) error {
	_, _, err := t.c.sendMarkdownReply(ctx, chat, text, ReplyOptions{DeliveryKey: o.DeliveryKey, Kind: o.Kind, ShouldContinue: o.ShouldContinue, MaxCharsPerChunk: t.maxChars})
	return err
}

func (t gwgTransport) SendFile(context.Context, string, ResultArtifact, []byte, ResultSendOptions) error {
	return errors.New("no files here")
}

func (f *gwgRecovery) replyPending(string) bool {
	for _, item := range f.state.Outbox {
		if item.Kind == "reply" && item.Status == OutboxPending {
			return true
		}
	}
	return false
}
