package collie

import (
	"fmt"
	"log"
	"time"
)

// Workers on an offline computer can still be running. Keep the bound
// instance until its machine answers so the follow-up can reconcile it.
func (sp *Super) taskWorkerOfflineLocked(w *Worker, answered map[string]bool) bool {
	if answered[w.Machine] {
		return false
	}
	for _, task := range sp.st.TaskFollowups {
		if task.ClosedAt.IsZero() && task.Worker == w.Name && task.WorkerCreated.Equal(w.CreatedAt) {
			return true
		}
	}
	return false
}

func (sp *Super) taskTriggerCurrentLocked(trigger *Trigger) bool {
	if trigger.Source != taskFollowupSource {
		return true
	}
	task := sp.st.TaskFollowups[trigger.Ref]
	return task != nil && task.Review == trigger.ID && task.ClosedAt.IsZero()
}

func (sp *Super) tickTaskFollowupsLocked(now time.Time, answered map[string]bool) {
	for _, current := range sp.st.TaskFollowups {
		if !current.ClosedAt.IsZero() {
			continue
		}
		task := *current
		var dialog *Entry
		if task.Waiting {
			if entry, _ := sp.entryLocked(task.Decision); entry != nil && entry.Ask != nil {
				dialog = entry
			}
		}
		settled := task.resumeAfterDialog(dialog, now)
		reason := sp.taskReviewReasonLocked(&task, now, answered, dialog != nil)
		if reason == "" && !settled && task.OfflineAt.Equal(current.OfflineAt) {
			continue
		}

		// Observations and a failed-delivery retry don't invalidate the
		// version a Router already received. A new check or settled dialog does.
		if settled || reason != "" && !task.ReviewRetried {
			task.Version++
		}
		var event *Trigger
		var thread *Thread
		if reason != "" {
			text := fmt.Sprintf("Internal follow-up for %s (version %d): %s. Inspect the current state and write back your finding using collie task with --version %d.",
				task.Path, task.Version, taskReviewText(reason), task.Version)
			event, thread = sp.prepareTriggerLocked(taskFollowupSource, mainThread, routerName, text, "", routerName, task.Path)
			task.Review, task.ReviewKind, task.ReviewAt = event.ID, reason, now
		}
		if !settled {
			dialog = nil
		}
		if err := sp.saveTaskLocked(&task, event, thread, dialog); err != nil {
			log.Printf("task follow-up %s: %v", task.Path, err)
		}
	}
}

func (sp *Super) taskReviewReasonLocked(task *TaskFollowup, now time.Time, answered map[string]bool, nativeDialog bool) string {
	worker := sp.taskWorkerLocked(task)
	waitingChat := task.Waiting && !nativeDialog
	if answered != nil && worker != nil {
		if answered[worker.Machine] {
			if !task.OfflineAt.IsZero() {
				task.OfflineAt = time.Time{}
				if !waitingChat && sp.taskShouldReportReconnectLocked(task) {
					task.clearReview()
					return "reconnected"
				}
			}
		} else if since := sp.unanswered[worker.Machine]; !waitingChat && !since.IsZero() && now.Sub(since) >= awayGrace &&
			task.OfflineAt.IsZero() && !worker.Ended {
			task.OfflineAt = since.Add(taskOfflineReview)
		}
	}

	if task.Review != "" {
		if answered == nil && (task.ReviewKind == "offline" || task.ReviewKind == "reconnected") {
			return ""
		}
		if !task.ReviewRetried && !task.ReviewAt.Add(taskReviewRetry).After(now) {
			trigger, err := sp.deliveryTriggerLocked(task.Review)
			if err != nil {
				log.Printf("task follow-up %s: load review %s: %v", task.Path, task.Review, err)
			} else if trigger != nil && trigger.Status == "failed" {
				task.ReviewRetried = true
				return task.ReviewKind
			}
		}
		return ""
	}
	if task.Waiting && sp.taskDecisionLocked(task.Decision, now) != nil {
		return ""
	}
	if !waitingChat && !task.OfflineAt.IsZero() {
		if answered != nil && !task.OfflineAt.After(now) {
			return "offline"
		}
		return ""
	}
	if task.NextAt.After(now) {
		return ""
	}
	if task.Waiting {
		return "decision"
	}
	// Blocked/failed/exited workers already have immediate lifecycle events.
	// In particular, wait for a transient directory trust prompt to clear.
	if worker != nil && worker.Status == "blocked" {
		return ""
	}
	return "due"
}

// An offline check already delivered (or in flight) keeps its version and
// lets that investigation finish. The Router can verify the machine again.
func (sp *Super) taskShouldReportReconnectLocked(task *TaskFollowup) bool {
	if task.Review == "" {
		return true
	}
	if task.ReviewKind != "offline" {
		return false
	}
	trigger := sp.st.trigger(task.Review)
	return trigger != nil && trigger.Status == "queued" && len(trigger.Deliveries) == 0
}

func taskReviewText(kind string) string {
	switch kind {
	case "offline":
		return "the executing machine is still offline; preserve the last result and check recovery or delivery impact"
	case "reconnected":
		return "the executing machine reconnected; reconcile the original session and any new results"
	case "decision":
		return "the business decision remains open but its old message is no longer actionable; verify the answer or whether a new question is needed"
	default:
		return "the next progress check is due; look for new results or credible ongoing activity"
	}
}
