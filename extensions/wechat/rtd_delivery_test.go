package wechat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const rtdNow = int64(1_800_000_000_000)

var rtdBytes = []byte("完整研究成果")

func rtdPlan(change func(*ResultDelivery)) ResultDelivery {
	id, artifactID := newUUID(), newUUID()
	p := ResultDelivery{ID: id, Key: id, ChatID: "chat", Source: ResultSource{Type: "plan", ID: "task"}, Kind: "completed", Status: ResultClaimed,
		Mode: ModeText, Messages: []string{"甲", "乙", "丙"},
		Artifact: ResultArtifact{ID: artifactID, ChatID: "chat", Name: "result.md", Size: len(rtdBytes), SHA256: sha256Hex(string(rtdBytes)),
			ExpiresAt: rtdNow + 200_000, DownloadURL: "https://controller.test/v1/result-artifacts/" + artifactID + "/download"},
		CreatedAt: rtdNow - 100_000, ExpiresAt: rtdNow + 100_000, RetainedUntil: rtdNow + 200_000,
		AttemptID: newUUID(), AttemptedAt: rtdNow - 1, AttemptExpiresAt: rtdNow + 90_000}
	if change != nil {
		change(&p)
	}
	return p
}

type rtdSend struct {
	key   string
	value []byte
}

// rtdRunnerFixture is a runner over a real state whose transport records
// each step once, like the shared transport's checkpoints.
type rtdRunnerFixture struct {
	dir, statePath string
	account        *Account
	state          *StateManager
	sends          []rtdSend
	opts           ResultRunnerOptions
	loop           *loop
}

func rtdRunnerSetup(t *testing.T) *rtdRunnerFixture {
	f := &rtdRunnerFixture{dir: t.TempDir(), account: rtdAccount("synthetic-token", "synthetic-bot"), loop: &loop{}}
	f.statePath = filepath.Join(f.dir, stateFileName)
	f.state = rtdState(t, f.dir, f.account, PolicyOptions{})
	send := func(value []byte, key string) error {
		d, err := f.state.PrepareReplyDelivery(key, sha256Hex(string(value)), 1)
		if err != nil {
			return err
		}
		if d.AcknowledgedChunks == 1 {
			return nil
		}
		if err := f.state.BeginReplyChunk(key, 0); err != nil {
			return err
		}
		f.sends = append(f.sends, rtdSend{key, value})
		return f.state.ConfirmReplyChunk(key, 0)
	}
	f.opts = ResultRunnerOptions{State: f.state, Now: func() int64 { return rtdNow }, ContextFingerprint: func(string) string { return "synthetic-context" },
		Budget: func(string, string) ResultBudget { return ResultBudget{Allowed: true, Remaining: 10} }, IsReplyPending: func(string) bool { return false },
		DownloadArtifact: func(context.Context, ResultArtifact) ([]byte, error) { return rtdBytes, nil },
		Transport: &rtdTransport{text: func(_ string, text string, o ResultSendOptions) error { return send([]byte(text), o.DeliveryKey) },
			file: func(data []byte, o ResultSendOptions) error { return send(data, o.DeliveryKey) }}}
	return f
}

type rtdTransport struct {
	text func(chat, text string, o ResultSendOptions) error
	file func(data []byte, o ResultSendOptions) error
}

func (r *rtdTransport) SendText(_ context.Context, chat, text string, o ResultSendOptions) error {
	return r.text(chat, text, o)
}
func (r *rtdTransport) SendFile(_ context.Context, _ string, _ ResultArtifact, data []byte, o ResultSendOptions) error {
	return r.file(data, o)
}

func (f *rtdRunnerFixture) reload(t *testing.T) {
	f.state = rtdState(t, f.dir, f.account, PolicyOptions{})
	f.opts.State = f.state
}

func (f *rtdRunnerFixture) deliver(t *testing.T, p ResultDelivery) ResultDeliveryOutcome {
	t.Helper()
	var out ResultDeliveryOutcome
	var err error
	f.loop.do(func() { out, err = NewResultDeliveryRunner(f.loop, f.opts).deliver(context.Background(), p) })
	mustNoErr(t, err)
	return out
}

func (f *rtdRunnerFixture) values() []string {
	var out []string
	for _, s := range f.sends {
		out = append(out, string(s.value))
	}
	return out
}

// from: gateway/tests/results/delivery.test.ts: three text messages are preserved verbatim and never replayed after process restart
func TestRtdDeliveryThreeTextMessages(t *testing.T) {
	f := rtdRunnerSetup(t)
	item := rtdPlan(nil)
	if got := f.deliver(t, item); got != (ResultDeliveryOutcome{Status: OutcomeAccepted, AcknowledgedSteps: 3, TotalSteps: 3}) {
		t.Fatalf("outcome = %+v", got)
	}
	rtdEqual(t, f.values(), item.Messages, "sent")
	f.reload(t)
	if got := f.deliver(t, item); got.Status != OutcomeAccepted || len(f.sends) != 3 {
		t.Fatalf("restart outcome %+v, %d sends", got, len(f.sends))
	}
}

// from: gateway/tests/results/delivery.test.ts: long results send one model summary then complete attachment bytes
func TestRtdDeliverySummaryThenAttachment(t *testing.T) {
	f := rtdRunnerSetup(t)
	item := rtdPlan(func(p *ResultDelivery) { p.Mode, p.Messages = ModeAttachment, []string{"模型生成的结果摘要"} })
	if got := f.deliver(t, item); got.Status != OutcomeAccepted {
		t.Fatalf("outcome = %+v", got)
	}
	rtdEqual(t, f.values(), []string{item.Messages[0], string(rtdBytes)}, "sent")
	if f.sends[0].key != "result:"+item.ID+":step:0" || f.sends[1].key != "result:"+item.ID+":step:1" {
		t.Fatalf("keys = %s %s", f.sends[0].key, f.sends[1].key)
	}
}

// from: gateway/tests/results/delivery.test.ts: unknown send persists inFlight and restart never retries it
func TestRtdDeliveryUnknownSendNeverRetried(t *testing.T) {
	f := rtdRunnerSetup(t)
	item := rtdPlan(nil)
	f.opts.Transport.(*rtdTransport).text = func(_ string, text string, o ResultSendOptions) error {
		if _, err := f.state.PrepareReplyDelivery(o.DeliveryKey, sha256Hex(text), 1); err != nil {
			return err
		}
		mustNoErr(t, f.state.BeginReplyChunk(o.DeliveryKey, 0))
		f.sends = append(f.sends, rtdSend{o.DeliveryKey, []byte(text)})
		return errors.New("response lost")
	}
	if got := f.deliver(t, item); got.Status != OutcomeUncertain {
		t.Fatalf("outcome = %+v", got)
	}
	f.reload(t)
	if got := f.deliver(t, item); got.Reason != "previous_send_unknown" || len(f.sends) != 1 {
		t.Fatalf("restart outcome %+v, %d sends", got, len(f.sends))
	}
}

// from: gateway/tests/results/delivery.test.ts: definitely unsubmitted step resumes after preceding acknowledged text without duplication
func TestRtdDeliveryUnsubmittedStepResumes(t *testing.T) {
	f := rtdRunnerSetup(t)
	item := rtdPlan(nil)
	tr := f.opts.Transport.(*rtdTransport)
	original := tr.text
	f.opts.ClassifyError = func(error) string { return "not_submitted" }
	tr.text = func(chat, text string, o ResultSendOptions) error {
		if text == "乙" {
			return errors.New("context unavailable before submit")
		}
		return original(chat, text, o)
	}
	if got := f.deliver(t, item); got != (ResultDeliveryOutcome{Status: OutcomeDeferred, Reason: "not_submitted", AcknowledgedSteps: 1, TotalSteps: 3}) {
		t.Fatalf("outcome = %+v", got)
	}
	tr.text = original
	f.reload(t)
	if got := f.deliver(t, item); got.Status != OutcomeAccepted {
		t.Fatalf("outcome = %+v", got)
	}
	rtdEqual(t, f.values(), item.Messages, "sent")
}

// from: gateway/tests/results/delivery.test.ts: normal replies preempt notifications before claim-side sends
func TestRtdDeliveryReplyPreempts(t *testing.T) {
	f := rtdRunnerSetup(t)
	f.opts.IsReplyPending = func(string) bool { return true }
	if got := f.deliver(t, rtdPlan(nil)); got.Reason != "reply_pending" || len(f.sends) != 0 {
		t.Fatalf("outcome %+v, %d sends", got, len(f.sends))
	}
}

// from: gateway/tests/results/delivery.test.ts: replies that arrive between result steps interrupt further steps safely
func TestRtdDeliveryReplyBetweenSteps(t *testing.T) {
	f := rtdRunnerSetup(t)
	f.opts.IsReplyPending = func(string) bool { return len(f.sends) > 0 }
	got := f.deliver(t, rtdPlan(nil))
	if got.Reason != "reply_pending" || got.AcknowledgedSteps != 1 || len(f.sends) != 1 {
		t.Fatalf("outcome %+v, %d sends", got, len(f.sends))
	}
}

// from: gateway/tests/results/delivery.test.ts: progress never consumes reserved final three messages
func TestRtdDeliveryProgressKeepsReserve(t *testing.T) {
	f := rtdRunnerSetup(t)
	f.opts.Budget = func(_, kind string) ResultBudget {
		return ResultBudget{Allowed: kind != SendProgress, Remaining: 3, Reason: DeferBudgetExhausted}
	}
	got := f.deliver(t, rtdPlan(func(p *ResultDelivery) { p.Kind, p.Messages = KindProgressResult, []string{"进度"} }))
	if got.Reason != DeferBudgetExhausted || len(f.sends) != 0 {
		t.Fatalf("outcome %+v", got)
	}
}

// from: gateway/tests/results/delivery.test.ts: progress admission comes exclusively from the shared transport policy
func TestRtdDeliveryProgressAdmissionShared(t *testing.T) {
	f := rtdRunnerSetup(t)
	f.opts.Budget = func(_, kind string) ResultBudget {
		return ResultBudget{Allowed: kind != SendProgress || len(f.sends) < 3, Remaining: 10, Reason: DeferBudgetExhausted}
	}
	for i := 0; i < 3; i++ {
		if got := f.deliver(t, rtdPlan(func(p *ResultDelivery) { p.Kind, p.Messages = KindProgressResult, []string{fmt.Sprintf("进度%d", i)} })); got.Status != OutcomeAccepted {
			t.Fatalf("progress %d: %+v", i, got)
		}
	}
	f.reload(t)
	if got := f.deliver(t, rtdPlan(func(p *ResultDelivery) { p.Kind, p.Messages = KindProgressResult, []string{"第四次"} })); got.Reason != DeferBudgetExhausted || len(f.sends) != 3 {
		t.Fatalf("fourth progress %+v, %d sends", got, len(f.sends))
	}
}

// from: gateway/tests/results/delivery.test.ts: window and quota denial defer without creating ambiguous sends
func TestRtdDeliveryWindowAndQuotaDenial(t *testing.T) {
	f := rtdRunnerSetup(t)
	f.opts.Budget = func(string, string) ResultBudget { return ResultBudget{Reason: "window_closed"} }
	if got := f.deliver(t, rtdPlan(nil)); got.Reason != "window_closed" {
		t.Fatalf("outcome %+v", got)
	}
	f.opts.Budget = func(string, string) ResultBudget { return ResultBudget{Allowed: true, Remaining: 2} }
	if got := f.deliver(t, rtdPlan(nil)); got.Reason != DeferBudgetExhausted {
		t.Fatalf("outcome %+v", got)
	}
	if len(f.state.ReplyDeliveries) != 0 {
		t.Fatal("a denial created reply plans")
	}
}

// from: gateway/tests/results/delivery.test.ts: concurrent attempts for one immutable delivery share exactly one in-progress operation
func TestRtdDeliveryConcurrentAttemptsShare(t *testing.T) {
	f := rtdRunnerSetup(t)
	item := rtdPlan(nil)
	tr := f.opts.Transport.(*rtdTransport)
	original := tr.text
	tr.text = func(chat, text string, o ResultSendOptions) error {
		f.loop.await(func() { time.Sleep(10 * time.Millisecond) }) // the network releases the loop
		return original(chat, text, o)
	}
	runner := NewResultDeliveryRunner(f.loop, f.opts)
	results := make(chan ResultDeliveryOutcome, 2)
	for i := 0; i < 2; i++ {
		go f.loop.do(func() {
			out, err := runner.deliver(context.Background(), item)
			if err != nil {
				t.Error(err)
			}
			results <- out
		})
	}
	first, second := <-results, <-results
	if first != second || first.Status != OutcomeAccepted || len(f.sends) != 3 {
		t.Fatalf("outcomes %+v %+v, %d sends", first, second, len(f.sends))
	}
}

// from: gateway/tests/results/delivery.test.ts: attachment preparation failures preserve an acknowledged summary for later continuation
func TestRtdDeliveryAttachmentFailureKeepsSummary(t *testing.T) {
	f := rtdRunnerSetup(t)
	item := rtdPlan(func(p *ResultDelivery) { p.Mode, p.Messages = ModeAttachment, []string{"摘要"} })
	f.opts.DownloadArtifact = func(context.Context, ResultArtifact) ([]byte, error) { return nil, errors.New("temporary offline") }
	if got := f.deliver(t, item); got.Reason != "artifact_unavailable" {
		t.Fatalf("outcome %+v", got)
	}
	f.opts.DownloadArtifact = func(context.Context, ResultArtifact) ([]byte, error) { return rtdBytes, nil }
	if got := f.deliver(t, item); got.Status != OutcomeAccepted {
		t.Fatalf("outcome %+v", got)
	}
	if len(f.sends) != 2 || string(f.sends[0].value) != "摘要" || !bytes.Equal(f.sends[1].value, rtdBytes) {
		t.Fatalf("sent %v", f.values())
	}
}

// from: gateway/tests/results/delivery.test.ts: immutable result identity changes when content changes
func TestRtdDeliveryIdentityChangesWithContent(t *testing.T) {
	f := rtdRunnerSetup(t)
	item := rtdPlan(nil)
	f.deliver(t, item)
	changed := item
	changed.Messages = []string{"篡改"}
	if resultIdentity(item) == resultIdentity(changed) || len(f.sends) != 3 {
		t.Fatal("identity ignores content")
	}
}

// from: gateway/tests/results/delivery.test.ts: per-pass selection chooses the newest terminal result per chat
func TestRtdDeliverySelectionNewestTerminal(t *testing.T) {
	items := []ResultDelivery{
		rtdPlan(func(p *ResultDelivery) { p.CreatedAt, p.Kind = rtdNow, KindProgressResult }),
		rtdPlan(func(p *ResultDelivery) { p.CreatedAt = rtdNow - 10 }),
		rtdPlan(func(p *ResultDelivery) { p.CreatedAt = rtdNow - 20 }),
		rtdPlan(func(p *ResultDelivery) { p.ChatID = "other" }),
	}
	var ids []string
	for _, item := range selectResultDeliveries(items) {
		ids = append(ids, item.ID)
	}
	rtdEqual(t, ids, []string{items[1].ID, items[3].ID}, "selected")
}

// from: gateway/tests/results/delivery.test.ts: a genuine context generation change interrupts the remaining result messages
func TestRtdDeliveryContextChangeInterrupts(t *testing.T) {
	f := rtdRunnerSetup(t)
	f.opts.ContextFingerprint = func(string) string {
		if len(f.sends) > 0 {
			return "new-context"
		}
		return "original-context"
	}
	got := f.deliver(t, rtdPlan(nil))
	if got.Reason != DeferContextChanged || got.AcknowledgedSteps != 1 || len(f.sends) != 1 {
		t.Fatalf("outcome %+v, %d sends", got, len(f.sends))
	}
}

// from: gateway/tests/results/delivery.test.ts: business and claim deadlines prevent new sends
func TestRtdDeliveryDeadlinesPreventSends(t *testing.T) {
	f := rtdRunnerSetup(t)
	if got := f.deliver(t, rtdPlan(func(p *ResultDelivery) { p.ExpiresAt = rtdNow })); got.Reason != "business_expired" {
		t.Fatalf("outcome %+v", got)
	}
	if got := f.deliver(t, rtdPlan(func(p *ResultDelivery) { p.AttemptExpiresAt = rtdNow })); got.Reason != "attempt_expired" {
		t.Fatalf("outcome %+v", got)
	}
	if len(f.sends) != 0 || strings.Contains(jsonText(f.values()), "x") {
		t.Fatal("expired plans sent")
	}
}
