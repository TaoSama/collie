package wechat

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	gweHour        = int64(60 * 60 * 1000)
	gweRefreshedAt = int64(1_800_000_000_000)
)

type gweRenewalFixture struct {
	t        *testing.T
	state    *StateManager
	sent     []string // delivery keys
	texts    []string
	send     RenewalSend
	notifier *RenewalNotifier
}

// gweRenewal is a state 22 hours into a 23-hour window with two sends left.
func gweRenewal(t *testing.T) *gweRenewalFixture {
	t.Helper()
	setNow(t, gweRefreshedAt+22*gweHour)
	s := newTestState(t, t.TempDir(), PolicyOptions{})
	s.ContextWindows[testChat] = ChatContextWindow{RefreshedAt: gweRefreshedAt, LastInboundKey: "inbound"}
	s.ContextTokens[testChat] = "context"
	s.AccountBudget = &AccountSendBudget{InboundCreatedAt: gweRefreshedAt, ContextBudgetState: ContextBudgetState{
		RefreshedAt: gweRefreshedAt, LastInboundKey: "inbound", Remaining: 2}}
	f := &gweRenewalFixture{t: t, state: s}
	f.send = func(_ context.Context, chat, text string, o ReplyOptions) (int, error) {
		if o.Kind != SendFinal || !o.ShouldContinue() || !o.RenewalReminder {
			t.Fatalf("reminder options = %+v", o)
		}
		f.sent = append(f.sent, o.DeliveryKey)
		f.texts = append(f.texts, text)
		s.ReplyDeliveries[o.DeliveryKey] = ReplyDeliveryState{Digest: "digest", TotalChunks: 1, AcknowledgedChunks: 1}
		s.AccountBudget.Remaining--
		return 1, nil
	}
	f.notifier = f.newNotifier()
	return f
}

func (f *gweRenewalFixture) newNotifier() *RenewalNotifier {
	return NewRenewalNotifier(context.Background(), f.state, func(ctx context.Context, chat, text string, o ReplyOptions) (int, error) {
		return f.send(ctx, chat, text, o)
	}, nil, nil)
}

func (f *gweRenewalFixture) deliver(chats ...string) (int, int) {
	f.t.Helper()
	sent, skipped, err := f.notifier.DeliverOnce(chats)
	mustNoErr(f.t, err)
	return sent, skipped
}

// from: gateway/tests/results/renewal.test.ts: reminder becomes eligible exactly one hour before expiry and never after the window
func TestGweRenewalEligibleInLastHour(t *testing.T) {
	for _, age := range []int64{0, 21 * gweHour, 22*gweHour - 1, 23 * gweHour, 24 * gweHour, -1} {
		f := gweRenewal(t)
		setNow(t, gweRefreshedAt+age)
		if sent, _ := f.deliver(testChat); sent != 0 || len(f.sent) != 0 {
			t.Fatalf("age %d sent a reminder", age)
		}
	}
	f := gweRenewal(t)
	if sent, skipped := f.deliver(testChat); sent != 1 || skipped != 0 {
		t.Fatalf("sent %d skipped %d", sent, skipped)
	}
	if !strings.Contains(f.texts[0], "主动发一条消息") || !strings.Contains(f.texts[0], "刷新本轮 10 条消息额度") {
		t.Fatalf("text = %q", f.texts[0])
	}
}

// from: gateway/tests/results/renewal.test.ts: uninterested chats, short windows and inadmissible chats never get a reminder
func TestGweRenewalNeverForInadmissible(t *testing.T) {
	f := gweRenewal(t)
	if sent, _ := f.deliver(); sent != 0 {
		t.Fatal("sent with no interested chat")
	}
	for _, w := range []int64{gweHour, gweHour - 1, 0} {
		f.state.Policy.ContextWindowMs = w
		if sent, _ := f.deliver(testChat); sent != 0 {
			t.Fatalf("window %d sent", w)
		}
	}
	f.state.Policy.ContextWindowMs = 23 * gweHour
	// Not admissible: the chat has no window to send in.
	window := f.state.ContextWindows[testChat]
	delete(f.state.ContextWindows, testChat)
	if sent, _ := f.deliver(testChat); sent != 0 {
		t.Fatal("sent to an inadmissible chat")
	}
	f.state.ContextWindows[testChat] = window
	f.state.Policy.SendBudget = 0
	if sent, _ := f.deliver(testChat); sent != 0 || len(f.sent) != 0 {
		t.Fatal("sent with a zero budget policy")
	}
}

// from: gateway/tests/results/renewal.test.ts: expiry reminder qualifies with any positive effective budget, regardless of the raw persisted counter
func TestGweRenewalAnyPositiveEffectiveBudget(t *testing.T) {
	f := gweRenewal(t)
	// The raw counter says 10, but the sends already spent leave no effective budget.
	f.state.AccountBudget.Remaining = 10
	for _, granted := range []int64{20, 25} {
		g := granted
		f.state.AccountBudget.GrantedBudget = &g
		if sent, _ := f.deliver(testChat); sent != 0 {
			t.Fatalf("sent with no effective budget (granted %d)", granted)
		}
	}
	for _, remaining := range []int64{1, 2, 3, 8, 10} {
		a := gweRenewal(t)
		a.state.AccountBudget.Remaining = remaining
		if sent, _ := a.deliver(testChat); sent != 1 {
			t.Fatalf("remaining %d did not send", remaining)
		}
	}
}

// from: gateway/tests/results/renewal.test.ts: shared persisted acknowledgement, unknown and terminal failures suppress all repeat sends
func TestGweRenewalEvidenceSuppressesRepeats(t *testing.T) {
	for _, patch := range []ReplyDeliveryState{{AcknowledgedChunks: 1}, {InFlight: true}, {RenewalRejected: true},
		{FailureReason: FailureAttachmentUnavailable}} {
		f := gweRenewal(t)
		patch.Digest, patch.TotalChunks = "digest", 1
		f.state.ReplyDeliveries[renewalDeliveryKey(f.state.LoginFingerprint(), testChat, gweRefreshedAt)] = patch
		if sent, _ := f.deliver(testChat); sent != 0 || len(f.sent) != 0 {
			t.Fatalf("evidence %+v did not suppress the reminder", patch)
		}
	}
	f := gweRenewal(t)
	f.deliver(testChat, testChat)
	f.notifier = f.newNotifier() // a restart reuses the persisted checkpoint
	if sent, _ := f.deliver(testChat); sent != 0 || len(f.sent) != 1 {
		t.Fatalf("reminder repeated: %d sends", len(f.sent))
	}
}

// from: gateway/tests/results/renewal.test.ts: a genuine new refreshedAt produces one new stable reminder after its own lead time
func TestGweRenewalNewWindowNewReminder(t *testing.T) {
	f := gweRenewal(t)
	f.deliver(testChat)
	f.state.AccountBudget.RefreshedAt = gweRefreshedAt + 22*gweHour
	f.state.ContextWindows[testChat] = ChatContextWindow{RefreshedAt: gweRefreshedAt + 22*gweHour, LastInboundKey: "inbound"}
	if sent, _ := f.deliver(testChat); sent != 0 {
		t.Fatal("reminder before the new window's lead time")
	}
	f.state.AccountBudget.Remaining = 2
	setNow(t, gweRefreshedAt+44*gweHour)
	if sent, _ := f.deliver(testChat); sent != 1 || f.sent[0] == f.sent[1] {
		t.Fatalf("new window: %v", f.sent)
	}
	if sent, _ := f.deliver(testChat); sent != 0 {
		t.Fatal("repeated reminder in the new window")
	}
}

// from: gateway/tests/results/renewal.test.ts: queued send aborts on context, account, window or effective budget changes
func TestGweRenewalQueuedSendAborts(t *testing.T) {
	mutations := []func(f *gweRenewalFixture){
		func(f *gweRenewalFixture) { f.state.ContextTokens[testChat] = "new-context" },
		func(f *gweRenewalFixture) { f.state.Account.Token = "new-account" },
		func(f *gweRenewalFixture) { f.state.AccountBudget.RefreshedAt++ },
		func(f *gweRenewalFixture) { f.state.AccountBudget.LastInboundKey = "another-inbound" },
		func(f *gweRenewalFixture) { f.state.Policy.ContextWindowMs++ },
		func(f *gweRenewalFixture) { f.state.AccountBudget.Remaining = 0 },
		func(f *gweRenewalFixture) { setNow(f.t, gweRefreshedAt+23*gweHour) },
	}
	for i, mutate := range mutations {
		f := gweRenewal(t)
		f.send = func(_ context.Context, _, _ string, o ReplyOptions) (int, error) {
			if !o.ShouldContinue() {
				t.Fatalf("mutation %d: not admissible before the change", i)
			}
			mutate(f)
			if o.ShouldContinue() {
				t.Fatalf("mutation %d: still admissible after the change", i)
			}
			return 0, nil
		}
		if sent, _ := f.deliver(testChat); sent != 0 || len(f.sent) != 0 {
			t.Fatalf("mutation %d sent", i)
		}
	}
}

// from: gateway/tests/results/renewal.test.ts: unknown network failure remains visible and never resubmits its persisted in-flight item
func TestGweRenewalUnknownFailureNeverResubmits(t *testing.T) {
	f := gweRenewal(t)
	calls := 0
	f.send = func(_ context.Context, _, _ string, o ReplyOptions) (int, error) {
		calls++
		f.state.ReplyDeliveries[o.DeliveryKey] = ReplyDeliveryState{Digest: "digest", TotalChunks: 1, InFlight: true}
		return 0, errors.New("synthetic unknown send")
	}
	if _, _, err := f.notifier.DeliverOnce([]string{testChat}); err == nil || !strings.Contains(err.Error(), "synthetic unknown send") {
		t.Fatalf("error = %v", err)
	}
	if sent, _ := f.deliver(testChat); sent != 0 || calls != 1 {
		t.Fatalf("resubmitted: %d calls", calls)
	}
}

// from: gateway/tests/results/renewal.test.ts: single notifier does not enqueue concurrent duplicate reminders
func TestGweRenewalNoConcurrentDuplicates(t *testing.T) {
	f := gweRenewal(t)
	original := f.send
	f.send = func(ctx context.Context, chat, text string, o ReplyOptions) (int, error) {
		// A second pass while the first is still sending does nothing.
		if sent, skipped, err := f.notifier.DeliverOnce([]string{testChat}); sent != 0 || skipped != 0 || err != nil {
			t.Fatalf("nested pass: %d %d %v", sent, skipped, err)
		}
		return original(ctx, chat, text, o)
	}
	if sent, _ := f.deliver(testChat); sent != 1 {
		t.Fatal("first pass did not send")
	}
}

// from: gateway/tests/results/renewal.test.ts: priority admission yields without deadlocking and terminal evidence releases ordinary sends
func TestGweRenewalPriorityYield(t *testing.T) {
	f := gweRenewal(t)
	if !f.notifier.ShouldYield(testChat, "ordinary") {
		t.Fatal("ordinary send must yield to a due reminder")
	}
	if f.notifier.ShouldYield(testChat, accountRenewalDeliveryKey(f.state.LoginFingerprint(), f.state.AccountBudget.ContextBudgetState)) {
		t.Fatal("the reminder itself must not yield")
	}
	f.deliver(testChat)
	if f.notifier.ShouldYield(testChat, "ordinary") {
		t.Fatal("sent reminder still blocks")
	}
	u := gweRenewal(t)
	u.state.ReplyDeliveries[renewalDeliveryKey(u.state.LoginFingerprint(), testChat, gweRefreshedAt)] = ReplyDeliveryState{Digest: "digest", TotalChunks: 1, InFlight: true}
	if u.notifier.ShouldYield(testChat, "ordinary") {
		t.Fatal("an unknown reminder must release ordinary sends")
	}
}
