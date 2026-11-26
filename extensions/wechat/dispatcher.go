package wechat

import (
	"context"
	"errors"
)

// ResultDispatchSummary counts one pass.
type ResultDispatchSummary struct{ Accepted, Uncertain, Skipped int }

// resultService is what the dispatcher drives: the local result store.
type resultService interface {
	listDeliveries(chats []string) ([]ResultDelivery, error)
	getDelivery(id string) (ResultDelivery, error)
	claimDelivery(id, attemptID string) (ResultDelivery, error)
	materializeDelivery(id, attemptID string) (ResultDelivery, error)
	recordReceipt(id, attemptID, status string) (ResultDelivery, error)
	deferDelivery(id, attemptID, reason string, submitted int) (ResultDelivery, error)
	resumeDeliveries(chatID, generation string) error
	blockChatDeliveries(chatID, reason string) error
}

// ResultDispatcherOptions wire the dispatcher.
type ResultDispatcherOptions struct {
	Service        resultService
	Store          *FileResultOutbox
	Runner         *ResultDeliveryRunner
	State          *StateManager
	CanDispatch    func(chatID, kind string) bool
	DeferReason    func(chatID, kind string) string
	IsReplyPending func(chatID string) bool
	Active         func() bool
	Ctx            context.Context
	Now            func() int64
	// OnUncertain reports a send that may have gone out (diagnostics).
	OnUncertain func(id, attemptID string) error
	// OnSendUnknown, when set, replaces OnUncertain and also names the
	// send's sanitized error type.
	OnSendUnknown func(id, attemptID, errorType string) error
}

// ResultDispatcher resolves durable claim, defer and receipt intents
// through the service before any new send is considered.
type ResultDispatcher struct {
	o          ResultDispatcherOptions
	delivering bool
	chatCursor int
	blockedAt  map[string]int64
}

func NewResultDispatcher(o ResultDispatcherOptions) *ResultDispatcher {
	if o.Ctx == nil {
		o.Ctx = context.Background()
	}
	return &ResultDispatcher{o: o, blockedAt: map[string]int64{}}
}

func (d *ResultDispatcher) now() int64 {
	if d.o.Now != nil {
		return d.o.Now()
	}
	return nowMs()
}

func (d *ResultDispatcher) active() bool {
	return d.o.Ctx.Err() == nil && (d.o.Active == nil || d.o.Active())
}

func (d *ResultDispatcher) kindOf(item ResultDelivery) string {
	if item.Kind == KindProgressResult {
		return SendProgress
	}
	return SendFinal
}

func (d *ResultDispatcher) totalSteps(item ResultDelivery) int {
	if item.Mode == ModeText {
		return len(item.Messages)
	}
	return 2
}

// acknowledged counts accepted steps; an unmaterialized download plan has
// only its summary, its link is a second logical step.
func (d *ResultDispatcher) acknowledged(item ResultDelivery) int {
	if item.Mode != ModeDownload {
		return resultAcknowledgedSteps(d.o.State, item)
	}
	count := 0
	for count < 2 && inspectResultStep(d.o.State, item, count) == OutcomeAccepted {
		count++
	}
	return count
}

// NoteInbound queues a resume for a genuine new message of the person.
func (d *ResultDispatcher) NoteInbound(chatID, generation string) error {
	return d.o.Store.QueueResume(chatID, generation)
}

func (d *ResultDispatcher) flushResumes() error {
	resumes, err := d.o.Store.PendingResumes()
	if err != nil {
		return err
	}
	var first error
	for _, r := range resumes {
		if !d.active() {
			break
		}
		if d.o.IsReplyPending(r.ChatID) || !d.o.CanDispatch(r.ChatID, SendFinal) {
			continue
		}
		if err := d.o.Service.resumeDeliveries(r.ChatID, r.Generation); err != nil {
			first = firstErr(first, err)
			continue
		}
		if err := d.o.Store.CompleteResume(r.ChatID, r.Generation); err != nil {
			first = firstErr(first, err)
			continue
		}
		delete(d.blockedAt, r.ChatID)
	}
	return first
}

var errRecoveryIdentity = errors.New("Result recovery identity mismatch")

func (d *ResultDispatcher) verify(ref ResultReference, item ResultDelivery) error {
	if ref.ID != item.ID || ref.ChatID != item.ChatID || ref.Identity != resultIdentity(item) {
		return errRecoveryIdentity
	}
	return nil
}

func (d *ResultDispatcher) maySend(item ResultDelivery) (bool, error) {
	last, err := d.o.Store.LastDispatchAt(item.ChatID)
	if err != nil {
		return false, err
	}
	now := d.now()
	return d.active() && !d.o.IsReplyPending(item.ChatID) && item.ExpiresAt > now && item.RetainedUntil > now &&
		d.o.CanDispatch(item.ChatID, d.kindOf(item)) && now-last >= ResultDispatchMs, nil
}

// DeliverOnce resumes, recovers every reference, then offers each chat one
// new result in turn.
func (d *ResultDispatcher) DeliverOnce(chats []string) (ResultDispatchSummary, error) {
	var summary ResultDispatchSummary
	if d.delivering || !d.active() {
		return summary, nil
	}
	if len(chats) > ResultOutboxLimit {
		return summary, errors.New("Invalid result dispatcher chats")
	}
	for _, c := range chats {
		if c == "" {
			return summary, errors.New("Invalid result dispatcher chats")
		}
	}
	d.delivering = true
	defer func() { d.delivering = false }()
	attempted := map[string]bool{}
	first := d.flushResumes()
	refs, err := d.o.Store.References()
	if err != nil {
		return summary, firstErr(first, err)
	}
	for _, ref := range refs {
		if !d.active() {
			break
		}
		item, err := d.o.Service.getDelivery(ref.ID)
		if errors.Is(err, errResultNotFound) {
			// Purged plans cannot be resumed or receipted; retire only their reference.
			if err := d.o.Store.Remove(ref.ID); err != nil {
				first = firstErr(first, err)
			}
			summary.Skipped++
			continue
		}
		if err == nil {
			err = d.verify(ref, item)
		}
		var sent bool
		if err == nil {
			sent, err = d.process(ref, item, &summary)
		}
		if err != nil {
			first = firstErr(first, err)
			attempted[ref.ChatID] = true
		} else if sent {
			attempted[item.ChatID] = true
		}
	}
	var pending []string
	for _, c := range chats {
		if !attempted[c] && !containsString(pending, c) {
			pending = append(pending, c)
		}
	}
	if len(pending) == 0 || !d.active() {
		return summary, first
	}
	offset := d.chatCursor % len(pending)
	rotated := append(append([]string(nil), pending[offset:]...), pending[:offset]...)
	d.chatCursor = (offset + 1) % len(pending)
	// Each chat is asked separately so an exhausted one cannot occupy the pending limit.
	for _, chat := range rotated {
		if !d.active() {
			break
		}
		if err := d.offer(chat, &summary); err != nil {
			first = firstErr(first, err)
		}
	}
	return summary, first
}

func (d *ResultDispatcher) offer(chat string, summary *ResultDispatchSummary) error {
	if d.o.IsReplyPending(chat) {
		return nil
	}
	if !d.o.CanDispatch(chat, SendFinal) {
		if d.now()-d.blockedAt[chat] >= ResultDispatchMs {
			reason := ResultWaitingForUser
			if d.o.DeferReason != nil {
				reason = d.o.DeferReason(chat, SendFinal)
			}
			if err := d.o.Service.blockChatDeliveries(chat, reason); err != nil {
				return err
			}
			d.blockedAt[chat] = d.now()
		}
		return nil
	}
	delete(d.blockedAt, chat)
	last, err := d.o.Store.LastDispatchAt(chat)
	if err != nil || d.now()-last < ResultDispatchMs {
		return err
	}
	list, err := d.o.Service.listDeliveries([]string{chat})
	if err != nil {
		return err
	}
	refs, err := d.o.Store.References()
	if err != nil {
		return err
	}
	for _, item := range selectResultDeliveries(list) {
		if item.ChatID != chat {
			return errors.New("Result dispatch chat identity mismatch")
		}
		known := false
		for _, r := range refs {
			known = known || r.ID == item.ID
		}
		ok, err := d.maySend(item)
		if err != nil {
			return err
		}
		if known || !ok {
			continue
		}
		ref := ResultReference{ID: item.ID, ChatID: chat, Identity: resultIdentity(item), Status: RefPending}
		if err := d.o.Store.Put(ref); err != nil {
			return err
		}
		if _, err := d.process(ref, item, summary); err != nil {
			return err
		}
	}
	return nil
}

func (d *ResultDispatcher) receipt(ref ResultReference, summary *ResultDispatchSummary) error {
	if ref.AttemptID == "" || ref.ReceiptStatus == "" {
		return errors.New("Missing result receipt intent")
	}
	item, err := d.o.Service.recordReceipt(ref.ID, ref.AttemptID, ref.ReceiptStatus)
	if err != nil {
		return err
	}
	if err := d.verify(ref, item); err != nil {
		return err
	}
	if err := d.o.Store.Remove(ref.ID); err != nil {
		return err
	}
	if item.Status == ResultAccepted {
		summary.Accepted++
	} else {
		summary.Uncertain++
	}
	return nil
}

func (d *ResultDispatcher) deferRef(ref ResultReference) error {
	if ref.AttemptID == "" || ref.DeferReason == "" {
		return errors.New("Missing result defer intent")
	}
	item, err := d.o.Service.deferDelivery(ref.ID, ref.AttemptID, ref.DeferReason, ref.SubmittedMessages)
	if err != nil {
		return err
	}
	if err := d.verify(ref, item); err != nil {
		return err
	}
	return d.finishDefer(ref)
}

// finishDefer keeps a partly accepted result's evidence pinned while it
// waits for a new window.
func (d *ResultDispatcher) finishDefer(ref ResultReference) error {
	if ref.SubmittedMessages > 0 {
		return d.o.Store.Update(ref.ID, func(r *ResultReference) { r.Status = RefWaiting })
	}
	return d.o.Store.Remove(ref.ID)
}

func (d *ResultDispatcher) persistReceipt(ref ResultReference, status string) (ResultReference, error) {
	if err := d.o.Store.Update(ref.ID, func(r *ResultReference) { r.Status, r.ReceiptStatus = RefReceipt, status }); err != nil {
		return ref, err
	}
	ref.Status, ref.ReceiptStatus = RefReceipt, status
	return ref, nil
}

func (d *ResultDispatcher) receiptNow(ref ResultReference, status string, summary *ResultDispatchSummary) error {
	ref, err := d.persistReceipt(ref, status)
	if err != nil {
		return err
	}
	return d.receipt(ref, summary)
}

func (d *ResultDispatcher) deferNow(ref ResultReference, reason string, submitted int) error {
	if err := d.o.Store.Update(ref.ID, func(r *ResultReference) {
		r.Status, r.DeferReason, r.SubmittedMessages = RefDefer, reason, submitted
	}); err != nil {
		return err
	}
	ref.Status, ref.DeferReason, ref.SubmittedMessages = RefDefer, reason, submitted
	return d.deferRef(ref)
}

// process moves one reference on; true when it used the chat's send turn.
func (d *ResultDispatcher) process(ref ResultReference, item ResultDelivery, summary *ResultDispatchSummary) (bool, error) {
	now := d.now()
	if item.Status == ResultAccepted {
		return false, d.o.Store.Remove(ref.ID)
	}
	if ref.Status == RefReceipt {
		if item.AttemptID != ref.AttemptID {
			return false, errors.New("Result receipt recovery attempt mismatch")
		}
		return false, d.receipt(ref, summary)
	}
	if (item.Status == ResultClaimed || item.Status == ResultUncertain) && item.AttemptID == ref.AttemptID && d.acknowledged(item) == d.totalSteps(item) {
		return false, d.receiptNow(ref, ResultAccepted, summary)
	}
	if item.Status == ResultUncertain || item.Status == ResultSuperseded || item.ExpiresAt <= now || item.RetainedUntil <= now {
		summary.Skipped++
		return false, d.o.Store.Remove(ref.ID)
	}
	if ref.Status == RefDefer {
		if (item.Status == ResultWaitingForUser || item.Status == ResultReady) && item.LastDeferredAttemptID == ref.AttemptID &&
			item.LastDeferReason == ref.DeferReason && item.SubmittedMessages >= ref.SubmittedMessages {
			return false, d.finishDefer(ref)
		}
		if item.Status == ResultClaimed && item.AttemptID == ref.AttemptID {
			return false, d.deferRef(ref)
		}
		return false, errors.New("Result defer recovery attempt mismatch")
	}
	if item.Status == ResultWaitingForUser {
		summary.Skipped++
		return false, d.finishDefer(ref)
	}
	if ref.Status == RefWaiting {
		if item.Status != ResultReady {
			summary.Skipped++
			return false, nil
		}
		if err := d.o.Store.Update(ref.ID, func(r *ResultReference) { r.Status, r.AttemptID, r.DeferReason = RefPending, "", "" }); err != nil {
			return false, err
		}
		ref.Status, ref.AttemptID, ref.DeferReason = RefPending, "", ""
	}
	if item.Status == ResultReady {
		ok, err := d.maySend(item)
		if err != nil || !ok {
			if err == nil {
				summary.Skipped++
			}
			return false, err
		}
		attemptID := ref.AttemptID
		if attemptID == "" {
			attemptID = newUUID()
		}
		if err := d.o.Store.Update(ref.ID, func(r *ResultReference) { r.Status, r.AttemptID = RefClaiming, attemptID }); err != nil {
			return false, err
		}
		if item, err = d.o.Service.claimDelivery(item.ID, attemptID); err != nil {
			return false, err
		}
		if err := d.verify(ref, item); err != nil {
			return false, err
		}
		ref.Status, ref.AttemptID = RefClaimed, attemptID
		if err := d.o.Store.Update(ref.ID, func(r *ResultReference) { r.Status = RefClaimed }); err != nil {
			return false, err
		}
	}
	if item.Status != ResultClaimed || item.AttemptID != ref.AttemptID {
		return false, errors.New("Result claim recovery attempt mismatch")
	}
	if d.acknowledged(item) == d.totalSteps(item) {
		return false, d.receiptNow(ref, ResultAccepted, summary)
	}
	for i := 0; i < d.totalSteps(item); i++ {
		if inspectResultStep(d.o.State, item, i) == OutcomeUncertain {
			return false, d.receiptNow(ref, ResultUncertain, summary)
		}
	}
	if item.AttemptExpiresAt <= d.now() {
		summary.Skipped++
		return false, d.o.Store.Remove(ref.ID)
	}
	ok, err := d.maySend(item)
	if err != nil {
		return false, err
	}
	if !ok {
		kind := d.kindOf(item)
		if d.o.IsReplyPending(item.ChatID) || !d.o.CanDispatch(item.ChatID, kind) {
			reason := "reply_pending"
			if !d.o.IsReplyPending(item.ChatID) {
				reason = ResultWaitingForUser
				if d.o.DeferReason != nil {
					reason = d.o.DeferReason(item.ChatID, kind)
				}
			}
			if err := d.deferNow(ref, reason, d.acknowledged(item)); err != nil {
				return false, err
			}
		} else if !d.active() {
			// Stopped after the claim: give the attempt back rather than let it lapse into uncertain.
			if err := d.deferNow(ref, DeferContextChanged, d.acknowledged(item)); err != nil {
				return false, err
			}
		}
		summary.Skipped++
		return false, nil
	}
	materialized, err := d.o.Service.materializeDelivery(item.ID, ref.AttemptID)
	if err != nil {
		return false, err
	}
	if err := d.verify(ref, materialized); err != nil {
		return false, err
	}
	// Saved before the send: a crash cannot bypass the per-chat interval.
	if err := d.o.Store.RecordDispatch(item.ChatID, d.now()); err != nil {
		return false, err
	}
	outcome, err := d.o.Runner.deliver(d.o.Ctx, materialized)
	if err != nil {
		return true, err
	}
	if outcome.Status == OutcomeDeferred {
		reason := outcome.Reason
		if reason == "" {
			reason = "not_submitted"
		}
		summary.Skipped++
		return true, d.deferNow(ref, reason, outcome.AcknowledgedSteps)
	}
	if err := d.receiptNow(ref, outcome.Status, summary); err != nil {
		return true, err
	}
	if outcome.Status == OutcomeUncertain && d.o.OnSendUnknown != nil {
		errorType := outcome.ErrorType
		if errorType == "" {
			errorType = "UncertainSend"
		}
		return true, d.o.OnSendUnknown(item.ID, ref.AttemptID, errorType)
	}
	if outcome.Status == OutcomeUncertain && d.o.OnUncertain != nil {
		return true, d.o.OnUncertain(item.ID, ref.AttemptID)
	}
	return true, nil
}
