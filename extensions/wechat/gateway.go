package wechat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

const (
	// UnsupportedMediaReply answers content that cannot be passed on.
	UnsupportedMediaReply = "暂时无法处理这条消息。目前支持文字、语音、图片和文件，视频等内容暂不支持；如果发送的是图片或文件，可能是接收失败，请稍后重发或改用文字描述。"
	// MediaFailedNote is appended when a message's attachments could not be
	// received, so nobody assumes they exist.
	MediaFailedNote = "\n（用户随消息发送的图片或文件未能接收，请勿假设已收到附件。）"
	// ConflictReply answers a message id seen before with other content.
	ConflictReply = "这条消息的编号与已处理消息相同，但内容不同。为避免误执行，我没有处理新内容；请重新发送一条消息。"

	minNotificationIntervalMs   = 1
	notificationInterestCacheMs = 30_000
	defaultBaselineBatches      = 20
	inboxFileName               = "state.json.inbox.json"
	resultOutboxFileName        = "result-outbox.json"
	recoveryFileName            = "notification-recovery.json"
)

// PollCycleSummary counts one poll.
type PollCycleSummary struct {
	PolledCount, ProcessedCount, SkippedCount int
	NextSyncBuf                               string
	Interrupted                               bool
}

// BaselineSummary is what establishing the baseline drained.
type BaselineSummary struct {
	BatchesDrained, RawMessagesSkipped int
	FinalSyncBuf, EstablishedAt        string
}

// GatewayOptions wire a gateway.
type GatewayOptions struct {
	State         *StateManager
	ILink         *ILinkClient
	Host          Host
	DataDir       string
	SplitMaxChars int
	Health        *HealthTracker
	MediaHTTP     *http.Client
	Diagnostics   *NotificationCycleDiagnostics
}

type notificationInterest struct {
	fetchedAt  int64
	chatIDs    []string
	interested []string
}

// Gateway is one bound account at work: it polls, hands the person's
// messages to Collie, and delivers Collie's answers and results within the
// reply window.
type Gateway struct {
	state         *StateManager
	ilink         *ILinkClient
	host          Host
	loop          *loop
	splitMaxChars int
	health        *HealthTracker
	mediaHTTP     *http.Client
	diagnostics   *NotificationCycleDiagnostics

	ctx    context.Context // ends at stop: the notification abort
	cancel context.CancelFunc
	kick   chan struct{}

	stopped        bool
	inbox          *FileInboundInbox
	runner         *InboundRunner
	inboundErr     error
	onInboundError func(error)

	queryStarts           map[string]chan struct{}
	activeInboundChats    map[string]bool
	replyFlush            chan struct{}
	activeReplyDeliveries map[string]bool
	replyOutboxCursor     int

	results      *resultStore
	resultOutbox *FileResultOutbox
	dispatcher   *ResultDispatcher
	renewal      *RenewalNotifier
	interest     *notificationInterest
	hostInterest []string

	answers      *answerStore
	stash        *mediaStash
	progress     map[string]*QueryProgress
	progressRefs map[string]QueryProgressResult
	// awaiting is when a chat's request handed to Collie stops counting as
	// a reply still to come.
	awaiting map[string]int64
}

func NewGateway(o GatewayOptions) (*Gateway, error) {
	if o.ILink == nil || o.State == nil || o.Host == nil {
		return nil, errors.New("gateway requires state, iLink client and host")
	}
	split := o.SplitMaxChars
	if split == 0 {
		split = DefaultMessageMaxChars
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	g := &Gateway{state: o.State, ilink: o.ILink, host: o.Host, loop: o.ILink.loop, splitMaxChars: split, health: o.Health,
		mediaHTTP: o.MediaHTTP, diagnostics: o.Diagnostics, ctx: ctx, kick: make(chan struct{}, 1),
		queryStarts: map[string]chan struct{}{}, activeInboundChats: map[string]bool{}, activeReplyDeliveries: map[string]bool{},
		progress: map[string]*QueryProgress{}, progressRefs: map[string]QueryProgressResult{}, awaiting: map[string]int64{}}
	g.cancel = func() { cancel(errors.New("WeChat notification delivery stopped")) }
	fingerprint := o.State.LoginFingerprint
	var err error
	if g.inbox, err = NewFileInboundInbox(filepath.Join(o.DataDir, inboxFileName), fingerprint, int(o.State.Policy.OutboxLimit)); err != nil {
		return nil, err
	}
	if g.answers, err = newAnswerStore(o.DataDir, fingerprint); err != nil {
		return nil, err
	}
	if g.stash, err = newMediaStash(o.DataDir, fingerprint, o.State.Policy.ContextWindowMs); err != nil {
		return nil, err
	}
	if g.results, err = newResultStore(o.DataDir, fingerprint, g.reportResult); err != nil {
		return nil, err
	}
	g.results.attempted = func(p ResultDelivery) bool {
		for i := 0; i < max(resultTotalSteps(p), 2); i++ {
			prefix := fmt.Sprintf("result:%s:step:%d", p.ID, i)
			for key := range o.State.ReplyDeliveries {
				if key == prefix || strings.HasPrefix(key, prefix+":") {
					return true
				}
			}
		}
		return false
	}
	if g.resultOutbox, err = NewFileResultOutbox(filepath.Join(o.DataDir, resultOutboxFileName), fingerprint); err != nil {
		return nil, err
	}
	g.runner = NewInboundRunner(g.loop, InboundRunnerOptions{Inbox: g.inbox,
		Process: func(m InboundMessage) error {
			_, err := g.processMessage(withTrace(g.ctx, createMessageTrace(m.ChatID, m.MessageID)), m, nil)
			return err
		},
		Steer:   g.steer,
		Covered: func(m InboundMessage) error { return g.state.RecordProcessedMessageKey(m.DeliveryKey, true) },
		OnError: g.inboundError})
	g.wireResults()
	return g, nil
}

func (g *Gateway) inboundError(err error) {
	g.health.RecordPollError(err)
	if g.onInboundError != nil {
		g.onInboundError(err)
		return
	}
	g.inboundErr = err
	g.stop()
}

func (g *Gateway) active() bool {
	return !g.stopped && g.state.IsReadyForActivePolling() && g.state.Account != nil && g.state.Account.Token != ""
}

func (g *Gateway) wireResults() {
	s := g.state
	canDispatch := func(chat, kind string) bool { return s.ContextTokens[chat] != "" && s.CanSendToChat(chat, kind) }
	deferReason := func(chat, kind string) string {
		if r := s.SendDeferReason(chat, kind); r != "" {
			return r
		}
		return DeferContextChanged
	}
	isReplyPendingAnywhere := func(string) bool {
		for chat := range s.ContextWindows {
			if strings.TrimSpace(s.ContextTokens[chat]) != "" && s.CanSendToChat(chat, SendFinal) && g.isReplyPending(chat) {
				return true
			}
		}
		return false
	}
	runner := NewResultDeliveryRunner(g.loop, ResultRunnerOptions{State: s, Transport: resultTransport{g},
		DownloadArtifact: g.downloadArtifact,
		Budget: func(chat, kind string) ResultBudget {
			return ResultBudget{Allowed: canDispatch(chat, kind), Remaining: s.RemainingSendBudget(chat), Reason: deferReason(chat, kind)}
		},
		ContextFingerprint: func(chat string) string {
			return jsonText([]string{s.ContextTokens[chat], s.ContextWindows[chat].LastInboundKey, g.ilink.budgetEpoch()})
		},
		IsReplyPending: isReplyPendingAnywhere})
	g.dispatcher = NewResultDispatcher(ResultDispatcherOptions{Service: localResults{g}, Store: g.resultOutbox, Runner: runner, State: s,
		CanDispatch: canDispatch, DeferReason: deferReason, IsReplyPending: isReplyPendingAnywhere, Active: g.active, Ctx: g.ctx,
		OnSendUnknown: func(id, attempt, errorType string) error {
			return reportNotificationSendFailure(id, attempt, errorType)
		}})
	g.renewal = NewRenewalNotifier(g.ctx, s, func(ctx context.Context, chat, text string, o ReplyOptions) (int, error) {
		n, _, err := g.ilink.sendMarkdownReply(ctx, chat, text, o)
		return n, err
	}, g.active, nil)
	g.ilink.SetBeforeSendCheck(func(_ string, key string) error {
		for _, chat := range g.renewalInterestedChats() {
			if g.renewal.ShouldYield(chat, key) {
				return deferred(DeferBudgetExhausted, "Local send deferred for a priority renewal reminder")
			}
		}
		return nil
	})
	s.SetReplyEvidenceProtection(func(key string) bool {
		now := nowMs()
		if g.resultOutbox.ProtectsDeliveryKey(key) {
			return true
		}
		if b := s.AccountBudget; b != nil && now >= b.RefreshedAt && now-b.RefreshedAt < s.Policy.ContextWindowMs &&
			key == accountRenewalDeliveryKey(s.LoginFingerprint(), b.ContextBudgetState) {
			return true
		}
		for chat, w := range s.ContextWindows {
			if now >= w.RefreshedAt && now-w.RefreshedAt < s.Policy.ContextWindowMs && key == renewalDeliveryKey(s.LoginFingerprint(), chat, w.RefreshedAt) {
				return true
			}
		}
		return false
	})
}

// reportNotificationSendFailure is the line kept for a result that may
// have been sent; diagnostics cannot reopen its delivery.
func reportNotificationSendFailure(id, attemptID, errorType string) error {
	writeLogLine(map[string]any{"event": "notification_send_failed", "notificationId": id, "attemptId": attemptID, "errorType": errorType},
		"[gateway] notification diagnostic write failed\n")
	return nil
}

func (g *Gateway) stop() {
	g.stopped = true
	g.runner.Halt()
	g.cancel()
}

// Stop ends the gateway's loops.
func (g *Gateway) Stop() { g.loop.do(g.stop) }

// deliverNotificationsOnce is one notification cycle: renewal reminders
// first, then resumed replies, then results, then renewal again (a
// multipart reply may have crossed into the reminder's threshold).
func (g *Gateway) deliverNotificationsOnce(ctx context.Context) (ResultDispatchSummary, error) {
	var summary ResultDispatchSummary
	if !g.active() {
		return summary, nil
	}
	var first, renewalErr error
	if g.interest == nil || nowMs()-g.interest.fetchedAt >= notificationInterestCacheMs {
		g.refreshInterest()
	}
	if _, _, err := g.renewal.DeliverOnce(g.renewalInterestedChats()); err != nil {
		renewalErr = rememberNotificationStage(ctx, err, StageRenewal)
	}
	if err := g.flushRepliesOnce(ctx, "", ""); err != nil {
		first = rememberNotificationStage(ctx, err, StageReplyRecovery)
	}
	g.refreshInterest()
	if s, err := g.dispatcher.DeliverOnce(g.interest.chatIDs); err != nil {
		first = firstErr(first, rememberNotificationStage(ctx, err, StageResultDelivery))
	} else {
		summary = s
	}
	if _, _, err := g.renewal.DeliverOnce(g.renewalInterestedChats()); err != nil && renewalErr == nil {
		renewalErr = rememberNotificationStage(ctx, err, StageRenewal)
	}
	if err := g.results.prune(nowMs()); err != nil {
		first = firstErr(first, rememberNotificationStage(ctx, err, StageResultDelivery))
	}
	if first != nil {
		return summary, first
	}
	return summary, renewalErr
}

// refreshInterest joins what Collie said is waiting with the results held
// here.
func (g *Gateway) refreshInterest() {
	chats := append([]string(nil), g.hostInterest...)
	for _, c := range g.results.chats(nowMs()) {
		if !containsString(chats, c) {
			chats = append(chats, c)
		}
	}
	fetched := nowMs()
	if g.interest != nil {
		fetched = g.interest.fetchedAt
	}
	g.interest = &notificationInterest{fetchedAt: fetched, chatIDs: chats, interested: append([]string(nil), g.hostInterest...)}
}

// SetInterest is channel.interest: chats with active timers or waiting results.
func (g *Gateway) SetInterest(chats []string) {
	g.loop.do(func() {
		g.hostInterest = append([]string(nil), chats...)
		g.interest = nil
		g.refreshInterest()
		g.interest.fetchedAt = nowMs()
	})
}

// establishBaseline drains history until a raw empty page: nothing is
// answered or handed on, only tokens and dedupe keys are kept.
func (g *Gateway) establishBaseline(ctx context.Context, maxBatches int) (BaselineSummary, error) {
	if maxBatches == 0 {
		maxBatches = defaultBaselineBatches
	}
	if err := g.state.BeginBaseline(); err != nil {
		return BaselineSummary{}, err
	}
	var out BaselineSummary
	for out.BatchesDrained < maxBatches {
		batch, err := g.ilink.pollBatch(ctx)
		if err != nil {
			return out, err
		}
		out.BatchesDrained++
		out.RawMessagesSkipped += batch.RawMessageCount
		for _, m := range batch.Messages {
			if _, err := g.state.RecordInboundContext(m.DeliveryKey, m.ChatID, m.CreatedAtMs, false, m.ContextToken); err != nil {
				return out, err
			}
			if m.ContextToken != "" && g.state.ContextTokens[m.ChatID] == "" {
				g.state.SetContextToken(m.ChatID, m.ContextToken)
			}
			if err := g.state.RecordProcessedMessageKey(m.DeliveryKey, false); err != nil {
				return out, err
			}
		}
		// Committed per batch so an interrupted drain resumes.
		if err := g.state.CommitCursor(batch.NextSyncBuf); err != nil {
			return out, err
		}
		// The server's raw count, not the filtered one: events may filter
		// to nothing while history continues.
		if batch.RawMessageCount == 0 {
			if err := g.state.MarkBaselineEstablished(); err != nil {
				return out, err
			}
			out.FinalSyncBuf, out.EstablishedAt = batch.NextSyncBuf, g.state.BaselineEstablishedAt
			return out, nil
		}
	}
	return out, fmt.Errorf("Baseline drain exceeded bounded limit of %d batches without encountering an empty server page; baseline incomplete", maxBatches)
}

// pollOnce is one poll, admission and checkpoint. waitForReplies processes
// each message in turn; otherwise they are admitted to the inbox.
func (g *Gateway) pollOnce(ctx context.Context, waitForReplies bool) (PollCycleSummary, error) {
	if !g.state.IsReadyForActivePolling() {
		return PollCycleSummary{}, errors.New("Gateway cannot poll for active messages: baseline is not established. Sign in again to establish it.")
	}
	if err := g.health.RecordPollStart(g.ilink.PollBudgetMs() + CheckpointMarginMs); err != nil {
		return PollCycleSummary{}, err
	}
	summary, err := g.poll(ctx, waitForReplies)
	if err != nil {
		g.health.RecordPollError(err)
	}
	return summary, err
}

func (g *Gateway) poll(ctx context.Context, waitForReplies bool) (PollCycleSummary, error) {
	batchID := newUUID()
	pctx := withTrace(ctx, createMessageTrace("poll", batchID))
	batch, err := traceSpan(pctx, "wechat.poll", nil, func() (PollBatch, error) { return g.ilink.pollBatch(pctx) })
	if err != nil {
		return PollCycleSummary{}, err
	}
	received, receivedClock := nowMs(), time.Now()
	traces := make([]*MessageTrace, len(batch.Messages))
	for i, m := range batch.Messages {
		traces[i] = createMessageTrace(m.ChatID, m.MessageID)
		fields := Fields{"batchId": batchID, "batchIndex": i, "batchCount": len(batch.Messages), "chars": utf16Len(m.Text)}
		if m.CreatedAtMs != nil {
			fields["platformCreatedAtMs"], fields["platformToReceivedMs"], fields["clockSkew"] = *m.CreatedAtMs, received-*m.CreatedAtMs, *m.CreatedAtMs > received
		}
		traceEvent(withTrace(ctx, traces[i]), "message.received", fields)
	}
	out := PollCycleSummary{PolledCount: len(batch.Messages)}
	for i, m := range batch.Messages {
		if g.stopped {
			out.Interrupted = true
			break
		}
		mctx := withTrace(ctx, traces[i])
		var outcome string
		if waitForReplies {
			if _, err := g.health.RecordMessageStart(ChatAuthorizationMs + MaxEngineRequestMs + ReplyTimeoutMs + CheckpointMarginMs); err != nil {
				return out, err
			}
			outcome, err = g.processMessage(mctx, m, &batchPosition{batchID, i, receivedClock})
		} else {
			outcome, err = g.admitMessage(mctx, m)
		}
		if err != nil {
			return out, err
		}
		if outcome == "processed" {
			out.ProcessedCount++
		} else {
			out.SkippedCount++
		}
	}
	if out.Interrupted {
		g.health.RecordProcessingEnd(0)
	} else {
		if err := g.state.CommitCursor(batch.NextSyncBuf); err != nil {
			return out, err
		}
		if err := g.flushRepliesOnce(ctx, "", ""); err != nil {
			return out, err
		}
		g.health.RecordPollSuccess()
	}
	out.NextSyncBuf = g.state.SyncBuf
	return out, nil
}

type batchPosition struct {
	id    string
	index int
	clock time.Time
}

// renewalInterestedChats: chats Collie says wait for results, and chats
// with an unsent reply.
func (g *Gateway) renewalInterestedChats() []string {
	var chats []string
	if g.interest != nil {
		chats = append(chats, g.interest.interested...)
	}
	for _, item := range sortedOutbox(g.state) {
		if item.Kind != "reply" || item.Status != OutboxPending || g.hasQuarantinedInboundReply(item.DeliveryKey) {
			continue
		}
		if text, ok := g.state.ReplyDeliveries[item.DeliveryKey]; ok && text.AcknowledgedChunks >= text.TotalChunks {
			continue
		}
		if !containsString(chats, item.ChatID) {
			chats = append(chats, item.ChatID)
		}
	}
	return chats
}

// assertChatAllowed serves only the person who scanned the login code.
func (g *Gateway) assertChatAllowed(chatID string) error {
	if g.state.Account == nil || g.state.Account.UserID == "" || chatID != g.state.Account.UserID {
		return ErrChatNotAllowed
	}
	return nil
}

func (g *Gateway) isReplyPending(chatID string) bool {
	if g.activeInboundChats[chatID] || g.runner.HasPending(chatID) || g.awaiting[chatID] > nowMs() {
		return true
	}
	for _, item := range g.state.Outbox {
		if item.Kind == "reply" && item.ChatID == chatID && item.Status == OutboxPending {
			return true
		}
	}
	return false
}

func (g *Gateway) hasQuarantinedInboundReply(key string) bool {
	return key != "" && (g.state.HasUncertainReply(key) || g.state.HasUncertainReply(imageReplyPrefix+key) ||
		g.state.HasPermanentReplyFailure(imageReplyPrefix+key) || g.hasUncertainFile(key))
}

func (g *Gateway) quarantineInboundReply(key string) error {
	// Isolates this message while keeping its delivery failure evidence.
	if err := g.state.RecordProcessedMessageKey(key, true); err != nil {
		return err
	}
	g.health.RecordMessageProgress()
	return nil
}

func (g *Gateway) hasExhaustedReplyRejections(key string) bool {
	return !g.hasQuarantinedInboundReply(key) && (g.state.HasExhaustedReplyRejections(key) ||
		g.state.HasExhaustedReplyRejections(imageReplyPrefix+key))
}
