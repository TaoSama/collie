package wechat

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

var rtdHTTP404 = errors.New("Result HTTP 404")

// rtdService is the result service of the original tests: a map of plans,
// with lost responses on demand.
type rtdService struct {
	f                                        *rtdDispatch
	records                                  map[string]ResultDelivery
	get, receiptHook                         func(id string) (ResultDelivery, error, bool)
	claims, receipts, blocked                int
	loss                                     string
	onRecordReceipt                          func(id, attempt, status string) (ResultDelivery, error)
	resumeAll                                bool
	listOrder                                []string
	lastBlockedReason, lastResumedGeneration string
}

func (s *rtdService) lost(stage string) error {
	if s.loss == stage {
		s.loss = ""
		return errors.New(stage + " response lost")
	}
	return nil
}

func (s *rtdService) listDeliveries(chats []string) ([]ResultDelivery, error) {
	var out []ResultDelivery
	for _, id := range s.listOrder {
		if item, ok := s.records[id]; ok && containsString(chats, item.ChatID) && item.Status == ResultReady {
			out = append(out, item)
		}
	}
	return out, nil
}

func (s *rtdService) getDelivery(id string) (ResultDelivery, error) {
	if s.get != nil {
		if item, err, handled := s.get(id); handled {
			return item, err
		}
	}
	item, ok := s.records[id]
	if !ok {
		return ResultDelivery{}, errResultNotFound
	}
	return item, nil
}

func (s *rtdService) claimDelivery(id, attempt string) (ResultDelivery, error) {
	s.claims++
	item := s.records[id]
	item.Status, item.AttemptID, item.AttemptedAt, item.AttemptExpiresAt = ResultClaimed, attempt, s.f.now, s.f.now+100_000
	s.records[id] = item
	return item, s.lost("claim")
}

func (s *rtdService) materializeDelivery(id, _ string) (ResultDelivery, error) {
	item := s.records[id]
	item.Artifact.DownloadURL = "https://controller.test/v1/result-artifacts/" + item.Artifact.ID + "/download"
	return item, nil
}

func (s *rtdService) recordReceipt(id, attempt, status string) (ResultDelivery, error) {
	if s.onRecordReceipt != nil {
		return s.onRecordReceipt(id, attempt, status)
	}
	s.receipts++
	item := s.records[id]
	if item.AttemptID != attempt {
		s.f.t.Fatalf("receipt attempt %s, want %s", attempt, item.AttemptID)
	}
	item.Status = status
	s.records[id] = item
	return item, s.lost("receipt")
}

func (s *rtdService) deferDelivery(id, attempt, reason string, submitted int) (ResultDelivery, error) {
	item := s.records[id]
	if item.AttemptID != attempt {
		s.f.t.Fatalf("defer attempt %s, want %s", attempt, item.AttemptID)
	}
	item.AttemptID, item.AttemptedAt, item.AttemptExpiresAt = "", 0, 0
	item.Status, item.SubmittedMessages = ResultWaitingForUser, submitted
	item.LastDeferredAttemptID, item.LastDeferReason, item.LastDeferredAt = attempt, reason, s.f.now
	s.records[id] = item
	return item, s.lost("defer")
}

func (s *rtdService) resumeDeliveries(chat, generation string) error {
	s.lastResumedGeneration = generation
	for id, item := range s.records {
		if item.ChatID == chat && item.Status == ResultWaitingForUser {
			item.Status = ResultReady
			s.records[id] = item
		}
	}
	return nil
}

func (s *rtdService) blockChatDeliveries(chat, reason string) error {
	s.blocked++
	s.lastBlockedReason = reason
	for id, item := range s.records {
		if item.ChatID == chat && item.Status == ResultReady {
			item.Status, item.DeferReason = ResultWaitingForUser, reason
			s.records[id] = item
		}
	}
	return nil
}

type rtdDispatch struct {
	t                    *testing.T
	now                  int64
	sends                int
	pendingReply         bool
	allowRunner, unknown bool
	state                *StateManager
	outboxPath           string
	store                *FileResultOutbox
	svc                  *rtdService
	runner               *ResultDeliveryRunner
	id                   string
	loop                 *loop
}

func rtdDispatchSetup(t *testing.T) *rtdDispatch {
	dir := t.TempDir()
	f := &rtdDispatch{t: t, now: rtdNow, allowRunner: true, loop: &loop{}, outboxPath: filepath.Join(dir, "results.json")}
	f.state = rtdState(t, dir, rtdAccount("synthetic", "synthetic"), PolicyOptions{})
	var err error
	f.store, err = NewFileResultOutbox(f.outboxPath, f.state.LoginFingerprint)
	mustNoErr(t, err)
	f.id = newUUID()
	item := ResultDelivery{ID: f.id, Key: f.id, ChatID: "chat", Source: ResultSource{Type: "plan", ID: "task"}, Kind: "completed",
		Status: ResultReady, Mode: ModeText, Messages: []string{"完整成果"},
		Artifact:  ResultArtifact{ID: newUUID(), ChatID: "chat", Name: "result.md", Size: 12, SHA256: strings.Repeat("a", 64), ExpiresAt: f.now + 1_000_000},
		CreatedAt: f.now - 1000, ExpiresAt: f.now + 500_000, RetainedUntil: f.now + 1_000_000}
	f.svc = &rtdService{f: f, records: map[string]ResultDelivery{f.id: item}, listOrder: []string{f.id}}
	f.runner = NewResultDeliveryRunner(f.loop, ResultRunnerOptions{State: f.state, Now: func() int64 { return f.now },
		ContextFingerprint: func(string) string { return "context" },
		Budget: func(string, string) ResultBudget {
			return ResultBudget{Allowed: f.allowRunner, Remaining: 10, Reason: DeferWindowExpired}
		},
		IsReplyPending:   func(string) bool { return f.pendingReply },
		DownloadArtifact: func(context.Context, ResultArtifact) ([]byte, error) { return []byte("artifact"), nil },
		Transport: &rtdTransport{text: func(_ string, text string, o ResultSendOptions) error {
			d, err := f.state.PrepareReplyDelivery(o.DeliveryKey, sha256Hex(text), 1)
			if err != nil || d.AcknowledgedChunks > 0 {
				return err
			}
			mustNoErr(t, f.state.BeginReplyChunk(o.DeliveryKey, 0))
			f.sends++
			if f.unknown {
				return errors.New("network response lost")
			}
			return f.state.ConfirmReplyChunk(o.DeliveryKey, 0)
		}, file: func([]byte, ResultSendOptions) error { return errors.New("Unexpected attachment") }}})
	return f
}

func (f *rtdDispatch) dispatcher(change func(*ResultDispatcherOptions)) *ResultDispatcher {
	o := ResultDispatcherOptions{Service: f.svc, Store: f.store, Runner: f.runner, State: f.state,
		CanDispatch: func(string, string) bool { return true }, IsReplyPending: func(string) bool { return f.pendingReply },
		Now: func() int64 { return f.now }}
	if change != nil {
		change(&o)
	}
	return NewResultDispatcher(o)
}

func (f *rtdDispatch) restart() *ResultDispatcher {
	var err error
	f.store, err = NewFileResultOutbox(f.outboxPath, f.state.LoginFingerprint)
	mustNoErr(f.t, err)
	return f.dispatcher(nil)
}

func (f *rtdDispatch) deliver(d *ResultDispatcher, chats ...string) (s ResultDispatchSummary, err error) {
	f.loop.do(func() { s, err = d.DeliverOnce(chats) })
	return s, err
}

func (f *rtdDispatch) refs() []ResultReference {
	refs, err := f.store.References()
	mustNoErr(f.t, err)
	return refs
}

func (f *rtdDispatch) mustFail(d *ResultDispatcher, part string) {
	f.t.Helper()
	if _, err := f.deliver(d, "chat"); err == nil || !strings.Contains(err.Error(), part) {
		f.t.Fatalf("error = %v, want %q", err, part)
	}
}

func (f *rtdDispatch) mustDeliver(d *ResultDispatcher, chats ...string) ResultDispatchSummary {
	f.t.Helper()
	if len(chats) == 0 {
		chats = []string{"chat"}
	}
	s, err := f.deliver(d, chats...)
	mustNoErr(f.t, err)
	return s
}

// from: gateway/tests/results/dispatcher.test.ts: claim response loss recovers through GET and never makes a second claim or send
func TestRtdDispatcherClaimLossRecovers(t *testing.T) {
	f := rtdDispatchSetup(t)
	f.svc.loss = "claim"
	f.mustFail(f.dispatcher(nil), "claim response lost")
	if refs := f.refs(); refs[0].Status != RefClaiming || f.sends != 0 {
		t.Fatalf("refs %+v, sends %d", refs, f.sends)
	}
	if s := f.mustDeliver(f.restart()); s != (ResultDispatchSummary{Accepted: 1}) {
		t.Fatalf("summary %+v", s)
	}
	if f.svc.claims != 1 || f.sends != 1 || len(f.refs()) != 0 {
		t.Fatalf("claims %d sends %d refs %d", f.svc.claims, f.sends, len(f.refs()))
	}
}

// from: gateway/tests/results/dispatcher.test.ts: accepted receipt response loss never replays a send after restart
func TestRtdDispatcherReceiptLossNoReplay(t *testing.T) {
	f := rtdDispatchSetup(t)
	f.svc.loss = "receipt"
	f.mustFail(f.dispatcher(nil), "receipt response lost")
	if f.refs()[0].Status != RefReceipt {
		t.Fatalf("refs %+v", f.refs())
	}
	f.mustDeliver(f.restart())
	if f.sends != 1 || f.svc.receipts != 1 || len(f.refs()) != 0 {
		t.Fatalf("sends %d receipts %d", f.sends, f.svc.receipts)
	}
}

// from: gateway/tests/results/dispatcher.test.ts: defer response loss recognizes exact lastDeferredAttemptId rather than marking unknown
func TestRtdDispatcherDeferLossRecognized(t *testing.T) {
	f := rtdDispatchSetup(t)
	f.svc.loss, f.allowRunner = "defer", false
	f.mustFail(f.dispatcher(nil), "defer response lost")
	if f.refs()[0].Status != RefDefer {
		t.Fatalf("refs %+v", f.refs())
	}
	f.mustDeliver(f.restart())
	if f.sends != 0 || len(f.refs()) != 0 || f.svc.records[f.id].Status != ResultWaitingForUser {
		t.Fatalf("sends %d refs %d status %s", f.sends, len(f.refs()), f.svc.records[f.id].Status)
	}
}
