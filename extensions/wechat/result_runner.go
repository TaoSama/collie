package wechat

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Result delivery outcomes.
const (
	OutcomeAccepted  = "accepted"
	OutcomeUncertain = "uncertain"
	OutcomeDeferred  = "deferred"
)

// ResultBudget is the shared ledger's answer for one chat.
type ResultBudget struct {
	Allowed   bool
	Remaining int64
	Reason    string
}

// ResultSendOptions shape one result step's send.
type ResultSendOptions struct {
	DeliveryKey    string
	Kind           string
	ShouldContinue func() bool
}

// ResultTransport sends one verbatim message or one file with the shared
// transport's durable per-message checkpoint.
type ResultTransport interface {
	SendText(ctx context.Context, chatID, text string, o ResultSendOptions) error
	SendFile(ctx context.Context, chatID string, artifact ResultArtifact, data []byte, o ResultSendOptions) error
}

// ResultDeliveryOutcome is how far one delivery got.
type ResultDeliveryOutcome struct {
	Status            string
	AcknowledgedSteps int
	TotalSteps        int
	Reason            string
	// ErrorType is the sanitized error of a send whose outcome is unknown.
	ErrorType string
}

// ResultRunnerOptions wire the runner to the shared state.
type ResultRunnerOptions struct {
	State              *StateManager
	Transport          ResultTransport
	DownloadArtifact   func(ctx context.Context, artifact ResultArtifact) ([]byte, error)
	Budget             func(chatID, kind string) ResultBudget
	ContextFingerprint func(chatID string) string
	IsReplyPending     func(chatID string) bool
	ClassifyError      func(error) string // not_submitted | rejected | unknown
	Now                func() int64
}

// resultStepKey is a step's reply plan key; a download link's key includes
// the link, which may be renewed after a definite non-submit.
func resultStepKey(plan ResultDelivery, index int) string {
	key := fmt.Sprintf("result:%s:step:%d", plan.ID, index)
	if plan.Mode == ModeDownload && index == 1 && len(plan.Messages) > 1 {
		key += ":" + hashJSON(plan.Messages[index])
	}
	return key
}

// inspectResultStep looks at every signature of a step for acceptance or
// ambiguity.
func inspectResultStep(s *StateManager, plan ResultDelivery, index int) string {
	prefix := fmt.Sprintf("result:%s:step:%d", plan.ID, index)
	accepted := false
	for key, item := range s.ReplyDeliveries {
		if key != prefix && !(plan.Mode == ModeDownload && index == 1 && strings.HasPrefix(key, prefix+":")) {
			continue
		}
		if item.InFlight {
			return OutcomeUncertain
		}
		if item.AcknowledgedChunks == item.TotalChunks && item.TotalChunks > 0 {
			accepted = true
		}
	}
	if accepted {
		return OutcomeAccepted
	}
	return "pending"
}

func resultTotalSteps(plan ResultDelivery) int {
	n := len(plan.Messages)
	if plan.Mode == ModeAttachment {
		n++
	}
	return n
}

func resultAcknowledgedSteps(s *StateManager, plan ResultDelivery) int {
	total, count := resultTotalSteps(plan), 0
	for count < total && inspectResultStep(s, plan, count) == OutcomeAccepted {
		count++
	}
	return count
}

func defaultClassification(err error) string {
	if IsRejectedSendReceipt(err) {
		return "rejected"
	}
	if _, ok := asDeferred(err); ok {
		return "not_submitted"
	}
	return "unknown"
}

// ResultDeliveryRunner only orchestrates: the transport owns the shared
// budget and every per-message checkpoint.
type ResultDeliveryRunner struct {
	o          ResultRunnerOptions
	inProgress map[string]*resultRun
	loop       *loop
}

type resultRun struct {
	done    chan struct{}
	outcome ResultDeliveryOutcome
	err     error
}

func NewResultDeliveryRunner(l *loop, o ResultRunnerOptions) *ResultDeliveryRunner {
	return &ResultDeliveryRunner{o: o, inProgress: map[string]*resultRun{}, loop: l}
}

func (r *ResultDeliveryRunner) now() int64 {
	if r.o.Now != nil {
		return r.o.Now()
	}
	return nowMs()
}

// deliver joins a delivery of the same plan already running.
func (r *ResultDeliveryRunner) deliver(ctx context.Context, plan ResultDelivery) (ResultDeliveryOutcome, error) {
	identity := plan.ChatID + ":" + plan.ID
	if running, ok := r.inProgress[identity]; ok {
		r.loop.wait(context.Background(), running.done)
		return running.outcome, running.err
	}
	run := &resultRun{done: make(chan struct{})}
	r.inProgress[identity] = run
	run.outcome, run.err = r.run(ctx, plan)
	delete(r.inProgress, identity)
	close(run.done)
	return run.outcome, run.err
}

func (r *ResultDeliveryRunner) run(ctx context.Context, plan ResultDelivery) (ResultDeliveryOutcome, error) {
	if err := ValidateResultDelivery(plan, true); err != nil {
		return ResultDeliveryOutcome{}, err
	}
	if plan.Status != ResultClaimed {
		return ResultDeliveryOutcome{}, errors.New("Result must be claimed before delivery")
	}
	if plan.Kind == KindProgressResult && (plan.Mode != ModeText || len(plan.Messages) != 1) {
		return ResultDeliveryOutcome{}, errors.New("Progress must occupy one message")
	}
	s, total := r.o.State, resultTotalSteps(plan)
	account, context0 := s.LoginFingerprint(), r.o.ContextFingerprint(plan.ChatID)
	kind := SendFinal
	if plan.Kind == KindProgressResult {
		kind = SendProgress
	}
	outcome := func(status, reason string) ResultDeliveryOutcome {
		return ResultDeliveryOutcome{Status: status, AcknowledgedSteps: resultAcknowledgedSteps(s, plan), TotalSteps: total, Reason: reason}
	}
	admissible := func(checkBudget bool) (string, error) {
		now := r.now()
		if ctx.Err() != nil || account == "" || context0 == "" || account != s.LoginFingerprint() || context0 != r.o.ContextFingerprint(plan.ChatID) {
			return DeferContextChanged, nil
		}
		if plan.ExpiresAt <= now || plan.RetainedUntil <= now {
			return reasonExpired, nil
		}
		if plan.AttemptExpiresAt <= now {
			return "attempt_expired", nil
		}
		if r.o.IsReplyPending(plan.ChatID) {
			return "reply_pending", nil
		}
		if checkBudget {
			b := r.o.Budget(plan.ChatID, kind)
			if !b.Allowed {
				if b.Reason != "" {
					return b.Reason, nil
				}
				return ResultWaitingForUser, nil
			}
			if b.Remaining < 0 {
				return "", errors.New("Invalid shared result budget")
			}
			if b.Remaining < int64(total-resultAcknowledgedSteps(s, plan)) {
				return DeferBudgetExhausted, nil
			}
		}
		return "", nil
	}
	classify := r.o.ClassifyError
	if classify == nil {
		classify = defaultClassification
	}
	for index := 0; index < total; index++ {
		switch inspectResultStep(s, plan, index) {
		case OutcomeUncertain:
			return outcome(OutcomeUncertain, "previous_send_unknown"), nil
		case OutcomeAccepted:
			continue
		}
		reason, err := admissible(true)
		if err != nil || reason != "" {
			return outcome(OutcomeDeferred, reason), err
		}
		fileStep := index == len(plan.Messages)
		var data []byte
		if fileStep {
			if data, err = r.o.DownloadArtifact(ctx, plan.Artifact); err != nil {
				if ctx.Err() != nil {
					return outcome(OutcomeDeferred, DeferContextChanged), nil
				}
				return outcome(OutcomeDeferred, "artifact_unavailable"), nil
			}
			if reason, err := admissible(true); err != nil || reason != "" {
				return outcome(OutcomeDeferred, reason), err
			}
		}
		o := ResultSendOptions{DeliveryKey: resultStepKey(plan, index), Kind: kind, ShouldContinue: func() bool {
			reason, err := admissible(false)
			return err == nil && reason == ""
		}}
		if fileStep {
			err = r.o.Transport.SendFile(ctx, plan.ChatID, plan.Artifact, data, o)
		} else {
			err = r.o.Transport.SendText(ctx, plan.ChatID, plan.Messages[index], o)
		}
		if err != nil {
			switch inspectResultStep(s, plan, index) {
			case OutcomeUncertain:
				unknown := outcome(OutcomeUncertain, reasonSendUnknown)
				unknown.ErrorType = SanitizeErrorType(err)
				return unknown, nil
			case OutcomeAccepted:
				continue
			}
			// The transport persists intent before sendmessage: without it, nothing unknown went out.
			if classify(err) == "rejected" {
				return outcome(OutcomeDeferred, "rejected"), nil
			}
			reason, aerr := admissible(true)
			if aerr != nil {
				return outcome(OutcomeDeferred, ""), aerr
			}
			if reason == "" {
				reason = "not_submitted"
			}
			return outcome(OutcomeDeferred, reason), nil
		}
		if inspectResultStep(s, plan, index) != OutcomeAccepted {
			return ResultDeliveryOutcome{}, errors.New("Result transport returned without durable acceptance evidence")
		}
	}
	return outcome(OutcomeAccepted, ""), nil
}
