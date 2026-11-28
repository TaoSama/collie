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
		reason := sp.taskReviewReasonLocked(&task, now, answered)
		if reason == "" && task.OfflineAt.Equal(current.OfflineAt) && task.Issue == current.Issue {
			continue
		}
		task.Version++
		var event *Trigger
		var thread *Thread
		if reason != "" {
			text := fmt.Sprintf("Internal follow-up for %s (version %d): %s. Inspect the current state and write back your finding using collie task with --version %d.",
				task.Path, task.Version, taskReviewText(reason, task.Issue), task.Version)
			event, thread = sp.prepareTriggerLocked(taskFollowupSource, mainThread, routerName, text, "", routerName, task.Path)
			task.Review, task.ReviewKind, task.ReviewAt = event.ID, reason, now
		}
		if err := sp.saveTaskLocked(&task, event, thread, nil); err != nil {
			log.Printf("task follow-up %s: %v", task.Path, err)
		}
	}
}

func (sp *Super) taskReviewReasonLocked(task *TaskFollowup, now time.Time, answered map[string]bool) string {
	worker := sp.taskWorkerLocked(task)
	if answered != nil && worker != nil {
		if !answered[worker.Machine] {
			if task.OfflineAt.IsZero() && !worker.Ended {
				task.OfflineAt = now.Add(taskOfflineReview)
				task.Missed = 0
				task.clearReview()
			}
		} else if !task.OfflineAt.IsZero() {
			task.OfflineAt = time.Time{}
			return "reconnected"
		}
	}

	if answered != nil {
		issue := taskWorkerIssue(task, worker, answered)
		if issue != "" && issue != task.Issue {
			task.Issue = issue
			return "worker"
		}
		if task.Review == "" {
			task.Issue = issue
		}
	}
	if task.Review != "" {
		trigger := sp.st.trigger(task.Review)
		if (trigger != nil && trigger.Status == "queued") || task.ReviewAt.Add(taskReviewRetry).After(now) {
			return ""
		}
		return task.ReviewKind
	}
	if task.Waiting && sp.taskDecisionLocked(task.Decision, now) != nil {
		return ""
	}
	if !task.OfflineAt.IsZero() {
		if !task.OfflineAt.After(now) {
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
	return "due"
}

func taskWorkerIssue(task *TaskFollowup, worker *Worker, answered map[string]bool) string {
	if task.Worker == "" {
		return ""
	}
	if worker == nil {
		return "the bound worker instance is gone"
	}
	if worker.Ended {
		return "the worker exited"
	}
	if !answered[worker.Machine] {
		return ""
	}
	switch worker.Status {
	case "gone":
		return "the worker exited"
	case "blocked":
		return "the worker is waiting on a dialog"
	case "failed", "error":
		return "the worker reported an error"
	default:
		return ""
	}
}

func taskReviewText(kind, issue string) string {
	switch kind {
	case "offline":
		return "the executing machine is still offline; preserve the last result and check recovery or delivery impact"
	case "reconnected":
		return "the executing machine reconnected; reconcile the original session and any new results"
	case "worker":
		return issue
	case "decision":
		return "the business decision remains open but its old message is no longer actionable; verify the answer or whether a new question is needed"
	default:
		return "the next progress check is due; look for new results or credible ongoing activity"
	}
}
