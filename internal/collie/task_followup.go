package collie

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	taskFollowupSource = "task-followup"
	taskDefaultReview  = 2 * time.Hour
	taskFirstLongCheck = 30 * time.Minute
	taskLongReview     = time.Hour
	taskOfflineReview  = 15 * time.Minute
	taskReviewRetry    = 15 * time.Minute
)

var (
	errTaskConflict = errors.New("task follow-up changed; refresh it first")
	errTaskMissing  = errors.New("no open task follow-up")
	errTaskInvalid  = errors.New("invalid task follow-up")
)

// TaskFollowup holds the schedule independently of the Router's session and
// the task's Markdown. Path is the stable name of that open task file.
type TaskFollowup struct {
	Path           string    `json:"path"`
	Worker         string    `json:"worker,omitempty"`
	WorkerCreated  time.Time `json:"worker_created,omitempty"`
	Long           bool      `json:"long,omitempty"`
	Waiting        bool      `json:"waiting,omitempty"`
	Decision       string    `json:"decision,omitempty"`
	LastDecision   string    `json:"last_decision,omitempty"`
	PastDecisions  []string  `json:"past_decisions,omitempty"`
	Owner          string    `json:"owner,omitempty"`
	Status         string    `json:"status,omitempty"`
	Detail         string    `json:"detail,omitempty"`
	LastProgressAt time.Time `json:"last_progress_at,omitempty"`
	Evidence       string    `json:"evidence,omitempty"`
	NextAt         time.Time `json:"next_at"`
	Manual         bool      `json:"manual,omitempty"`
	ClosedAt       time.Time `json:"closed_at,omitempty"`
	Version        uint64    `json:"version"`
	OfflineAt      time.Time `json:"offline_at,omitempty"`
	Issue          string    `json:"worker_issue,omitempty"`
	Missed         int       `json:"missed_reviews,omitempty"`
	Review         string    `json:"review,omitempty"`
	ReviewKind     string    `json:"review_kind,omitempty"`
	ReviewAt       time.Time `json:"review_at,omitempty"`
}

type TaskUpdate struct {
	Action   string    `json:"action"`
	Path     string    `json:"path"`
	Worker   string    `json:"worker,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	Decision string    `json:"decision,omitempty"`
	Long     bool      `json:"long,omitempty"`
	Activity bool      `json:"activity,omitempty"`
	At       time.Time `json:"at,omitzero"`
	Version  uint64    `json:"version,omitempty"`
}

func validTaskPath(path string) bool {
	return filepath.ToSlash(filepath.Dir(path)) == "tasks" &&
		path == "tasks/"+filepath.Base(path) && strings.HasSuffix(path, ".md") &&
		filepath.Base(path) != ".md" && !strings.ContainsAny(path, "\\\r\n\x00")
}

func taskInvalid(reason string) error { return fmt.Errorf("%w: %s", errTaskInvalid, reason) }

func (task *TaskFollowup) interval() time.Duration {
	if task.Long {
		return taskLongReview
	}
	return taskDefaultReview
}

func (task *TaskFollowup) schedule(now, at time.Time, previous *TaskFollowup) {
	if previous.Manual && previous.NextAt.After(now) {
		return
	}
	if at.IsZero() {
		at = now.Add(task.interval())
	}
	task.NextAt, task.Manual = at, false
}

func (task *TaskFollowup) clearReview() {
	task.Review, task.ReviewKind, task.ReviewAt = "", "", time.Time{}
}

func dailyTaskTime(after time.Time) time.Time {
	y, m, d := after.In(time.Local).Date()
	return time.Date(y, m, d, remindHour, 0, 0, 0, time.Local)
}

func taskDecisionTime(entry *Entry) time.Time {
	eligible := entry.At.Add(remindAge)
	if morning := dailyTaskTime(eligible); morning.After(eligible) {
		return morning
	}
	return eligible
}

func (sp *Super) taskDecisionLocked(id string, now time.Time) *Entry {
	if id == "" {
		return nil
	}
	entry, index := sp.entryLocked(id)
	if entry == nil {
		return nil
	}
	if entry.Ask != nil {
		worker := sp.st.Workers[entry.Ref]
		if entry.Ask.Done == nil && worker != nil && !worker.Ended &&
			worker.Status == "blocked" && worker.PaneID == entry.Ask.Pane && sp.unanswered[worker.Machine].IsZero() {
			return entry
		}
		return nil
	}
	if !askEntry(entry) {
		return nil
	}
	if index >= 0 && sp.waitsLocked(index, now) ||
		index < 0 && sp.heldLocked(id) && sp.heldWaitsLocked(entry, now, sp.quotesLocked()) {
		return entry
	}
	return nil
}

func (sp *Super) taskWorkerLocked(task *TaskFollowup) *Worker {
	if w := sp.st.Workers[task.Worker]; w != nil && w.CreatedAt.Equal(task.WorkerCreated) {
		return w
	}
	return nil
}

// API callers see a snapshot. A settled or expired message stops providing
// a link while the business decision remains in the follow-up record.
func (sp *Super) taskViewLocked(task *TaskFollowup, now time.Time) TaskFollowup {
	view := *task
	view.Owner = task.Worker
	view.Status = "active"
	view.PastDecisions = nil
	view.Decision = ""
	switch {
	case !task.ClosedAt.IsZero():
		view.Status = "closed"
	case task.Waiting:
		view.Owner, view.Status = "", "waiting"
		if sp.taskDecisionLocked(task.Decision, now) == nil {
			view.LastDecision = task.Decision
			if !task.OfflineAt.IsZero() {
				view.Status, view.NextAt = "offline", task.OfflineAt
			} else if task.Review != "" {
				view.Status = "review"
			}
		} else {
			view.Decision = task.Decision
			if !sp.st.RemindedAt.IsZero() && !view.NextAt.After(sp.st.RemindedAt) {
				view.NextAt = dailyTaskTime(sp.st.RemindedAt).AddDate(0, 0, 1)
			} else if morning := dailyTaskTime(view.NextAt); morning.After(view.NextAt) {
				view.NextAt = morning
			}
		}
	case !task.OfflineAt.IsZero():
		view.Owner, view.Status, view.NextAt = "", "offline", task.OfflineAt
	case task.Missed >= 2:
		view.Owner, view.Status = "", "stalled"
	case task.Review != "" || task.Issue != "":
		view.Owner, view.Status = "", "review"
	}
	if task.Review != "" && view.Decision == "" {
		view.NextAt = task.ReviewAt.Add(taskReviewRetry)
	}
	if !view.NextAt.IsZero() {
		view.NextAt = view.NextAt.In(time.Local)
	}
	return view
}

func (sp *Super) taskFollowups() map[string]TaskFollowup {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	now := time.Now()
	result := make(map[string]TaskFollowup, len(sp.st.TaskFollowups))
	for path, task := range sp.st.TaskFollowups {
		if task.ClosedAt.IsZero() {
			result[path] = sp.taskViewLocked(task, now)
		}
	}
	return result
}

func (sp *Super) updateTask(update TaskUpdate) (*TaskFollowup, error) {
	return sp.updateTaskAt(update, time.Now())
}

func (sp *Super) updateTaskAt(update TaskUpdate, now time.Time) (*TaskFollowup, error) {
	if !validTaskPath(update.Path) {
		return nil, taskInvalid("expected tasks/<name>.md")
	}
	if !update.At.IsZero() && !update.At.After(now) {
		return nil, taskInvalid("time must be in the future")
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()

	current := sp.st.TaskFollowups[update.Path]
	if current == nil {
		if update.Action != "start" {
			return nil, errTaskMissing
		}
		current = &TaskFollowup{Path: update.Path}
	} else if !current.ClosedAt.IsZero() {
		return nil, errTaskMissing
	}
	if (update.Version != 0 || update.Action == "remind") && current.Version != update.Version {
		return nil, errTaskConflict
	}
	next := *current
	if update.Worker != "" {
		worker := sp.st.Workers[update.Worker]
		if worker == nil || worker.Ended {
			return nil, taskInvalid("worker is not active")
		}
		if !worker.CreatedAt.Equal(next.WorkerCreated) || next.Worker != worker.Name {
			next.Worker, next.WorkerCreated = worker.Name, worker.CreatedAt
			next.OfflineAt, next.Issue, next.Missed = time.Time{}, "", 0
		}
	}

	detail := clip(oneLine(update.Detail), 180)
	var decision *Entry
	switch update.Action {
	case "start":
		if current.Version != 0 {
			return nil, errTaskConflict
		}
		next.Long, next.NextAt = update.Long, now.Add(taskDefaultReview)
		if next.Long {
			next.NextAt = now.Add(taskFirstLongCheck)
		}
		if !update.At.IsZero() {
			next.NextAt = update.At
		}
	case "progress", "review":
		if update.Action == "progress" {
			if detail == "" {
				return nil, taskInvalid("verified progress needs a detail")
			}
			if current.Evidence == detail && next.Worker == current.Worker && next.WorkerCreated.Equal(current.WorkerCreated) {
				view := sp.taskViewLocked(current, now)
				return &view, nil
			}
			next.LastProgressAt, next.Missed, next.Evidence = now, 0, detail
		} else if update.Activity {
			next.Missed = 0
		} else if current.ReviewKind == "due" {
			next.Missed++
		}
		next.clearReview()
		if (!next.Waiting || sp.taskDecisionLocked(next.Decision, now) == nil) &&
			(update.Action == "progress" || current.Review != "" || !update.At.IsZero() || !current.NextAt.After(now)) {
			next.schedule(now, update.At, current)
		}
		if !next.OfflineAt.IsZero() && current.Review != "" &&
			(!current.Manual || !current.OfflineAt.After(now)) {
			next.OfflineAt = now.Add(taskOfflineReview)
			if !update.At.IsZero() {
				next.OfflineAt = update.At
			}
		}
	case "wait":
		decision = sp.taskDecisionLocked(update.Decision, now)
		if decision == nil {
			return nil, taskInvalid("decision must be a live option, review or dialog")
		}
		for path, task := range sp.st.TaskFollowups {
			if path != update.Path && (task.Decision == decision.ID || slices.Contains(task.PastDecisions, decision.ID)) {
				return nil, taskInvalid("decision already belongs to another task")
			}
		}
		if slices.Contains(next.PastDecisions, decision.ID) || !next.Waiting && next.Decision == decision.ID {
			return nil, taskInvalid("decision already settled for this task")
		}
		if next.Decision != decision.ID && next.Decision != "" {
			if next.Waiting && sp.taskDecisionLocked(next.Decision, now) != nil {
				return nil, taskInvalid("settle the original question before replacing it")
			}
			next.PastDecisions = append(slices.Clone(next.PastDecisions), next.Decision)
		}
		next.Waiting, next.Decision, next.Missed = true, decision.ID, 0
		next.clearReview()
		if !current.Waiting || !current.Manual || !current.NextAt.After(now) {
			next.NextAt, next.Manual = taskDecisionTime(decision), false
			if !update.At.IsZero() {
				next.NextAt, next.Manual = update.At, true
			}
		}
	case "resolve":
		if !next.Waiting || detail == "" {
			return nil, taskInvalid("resolving needs a waiting task and the confirmed outcome")
		}
		next.Waiting, next.Missed, next.Manual = false, 0, false
		next.clearReview()
		next.schedule(now, update.At, &next)
	case "close":
		if detail == "" {
			return nil, taskInvalid("closing needs a result or a reason")
		}
		next.ClosedAt, next.NextAt, next.OfflineAt = now, time.Time{}, time.Time{}
		next.clearReview()
	case "remind":
		if update.At.IsZero() {
			return nil, taskInvalid("a new time is required")
		}
		next.NextAt, next.Manual = update.At, true
		if !next.OfflineAt.IsZero() && !next.Waiting {
			next.OfflineAt = update.At
		}
		next.clearReview()
	default:
		return nil, taskInvalid("unknown action")
	}
	if detail != "" {
		next.Detail = detail
	}
	next.Version++
	if err := sp.saveTaskLocked(&next, nil, nil, decision); err != nil {
		return nil, err
	}
	if next.Waiting || current.Waiting {
		sp.remindLookAt = time.Time{}
	}
	view := sp.taskViewLocked(&next, now)
	return &view, nil
}

// A closed task leaves the panel as soon as the operation commits. The
// Router moves its Markdown into done/ separately, including on a remote host.
func (sp *Super) taskPanel() ([]*memFile, map[string]TaskFollowup) {
	files := sp.taskMemFiles()
	sp.mu.Lock()
	defer sp.mu.Unlock()
	open := files[:0]
	followup := map[string]TaskFollowup{}
	if sp.st == nil {
		return files, followup
	}
	now := time.Now()
	for _, file := range files {
		if task := sp.st.TaskFollowups[file.Name]; task != nil {
			if !task.ClosedAt.IsZero() {
				continue
			}
			followup[file.Name] = sp.taskViewLocked(task, now)
		}
		open = append(open, file)
	}
	return open, followup
}
