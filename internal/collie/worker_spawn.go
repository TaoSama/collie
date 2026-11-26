package collie

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"
)

var workerNameRe = regexp.MustCompile(`[^a-z0-9_-]+`)

func cleanWorkerName(name string) string {
	return strings.Trim(workerNameRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

// Spawn starts a worker session asynchronously (start() returns at once; the
// outcome arrives later as a trigger — the async ReAct pattern).
// model and effort are launch choices for this worker; "" falls back to the
// default worker settings when kind is the default kind, else the agent's own.
// resumeFrom is what `spawn --resume` carries over from a stopped worker:
// where it ran and the conversation to open again.
func (sp *Super) resumeFrom(name string) (*Worker, error) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	w := sp.st.Workers[name]
	switch {
	case w == nil:
		return nil, fmt.Errorf("no worker named %s", name)
	case !w.Ended:
		return nil, fmt.Errorf("%s is still running: `send` it instead", name)
	case w.Session == "":
		return nil, fmt.Errorf("%s left no conversation to resume; spawn a fresh worker", name)
	}
	c := *w
	return &c, nil
}

// SpawnSpec is what a worker starts with; "" takes the settings' default.
type SpawnSpec struct {
	Kind, Cwd, Task, Thread, Name, By, Machine, Model, Effort string
	// Alias launches it through one of the computer's shell aliases: nil for
	// the default worker alias (which goes with the default kind), "" for none.
	Alias     *string
	Subscribe bool
	Resume    string
	Conv      string // extension conversation whose first request is already queued
}

func (sp *Super) Spawn(spec SpawnSpec) (*Worker, <-chan error, error) {
	kind, cwd, task, thread, name, by, machine, model, effort := spec.Kind, spec.Cwd, spec.Task, spec.Thread, spec.Name, spec.By, spec.Machine, spec.Model, spec.Effort
	subscribe, resume := spec.Subscribe, spec.Resume
	m, err := sp.s.herdr.machine(machine)
	if err != nil {
		if spec.Conv != "" {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("machine %q is not connected", machine)
	}
	if cwd == "" {
		if st := sp.settings(); m.ID == st.WorkerMachine || (m.Local && st.WorkerMachine == "") {
			cwd = st.WorkerCwd
		}
	}
	// Checked where the worker runs: herdr would start a missing directory's
	// shell in that computer's home without a word.
	dir, err := sp.s.resolveDirOn(m, cwd)
	dir, fallback := orHome(m.Home, dir, err)
	sp.mu.Lock()
	// takenLocked: herdr wants each agent's name its own, so one another
	// agent has (a session of the human's, an old worker's pane) goes to a
	// spare name. Called with sp.mu held.
	takenLocked := func(n string) bool {
		w := sp.st.Workers[n]
		return (w != nil && !w.Ended) || sp.s.herdr.hasName(n) || n == routerName || n == sp.routerAgentLocked()
	}
	if kind == "" {
		kind = sp.st.Settings.WorkerKind
	}
	if _, ok := kinds[kind]; !ok {
		sp.mu.Unlock()
		return nil, nil, fmt.Errorf("unknown kind %q (claude, codex, antigravity)", kind)
	}
	// Say so now, while the router can still pick another computer or kind.
	have := m.Agents
	if m.Local {
		have = localAgents(sp.s.home, false)
	}
	if have != nil && !slices.Contains(have, kinds[kind]) {
		sp.mu.Unlock()
		return nil, nil, fmt.Errorf("%s has no %s installed (it has: %s); pick another computer or kind", m.Name, kind, strings.Join(append(have, "none")[:max(len(have), 1)], ", "))
	}
	if thread == "" {
		thread = mainThread
	}
	if name == "" {
		base := kind
		if base == "antigravity" {
			base = "agy"
		}
		for i := 1; ; i++ {
			if n := fmt.Sprintf("%s-%d", base, i); !takenLocked(n) {
				name = n
				break
			}
		}
	}
	name = cleanWorkerName(name)
	if !nameRe.MatchString(name) || name == routerName {
		sp.mu.Unlock()
		return nil, nil, fmt.Errorf("invalid worker name %q", name)
	}
	if w := sp.st.Workers[name]; w != nil && !w.Ended {
		sp.mu.Unlock()
		return nil, nil, fmt.Errorf("worker %s is still running: `collie send %s …` to give it more, or spawn under another name such as %s", name, name, spareName(name, takenLocked))
	}
	if takenLocked(name) {
		n := spareName(name, takenLocked)
		log.Printf("spawn: %q is taken in herdr; the worker is %q", name, n)
		name = n
	}
	w := &Worker{Name: name, Kind: kind, Model: model, Effort: effort, Machine: m.ID, Cwd: dir, Task: task, Thread: thread, CreatedBy: by, CreatedAt: time.Now(), Status: "starting", Session: resume}
	w.Conv, w.cwdFallback = spec.Conv, fallback
	sp.st.Workers[name] = w
	if subscribe {
		th := sp.threadLocked(thread)
		if !slices.Contains(th.Subscribers, name) {
			th.Subscribers = append(th.Subscribers, name)
		}
	}
	where := ""
	if !m.Local {
		where = " @ " + m.Name
	}
	verb := "派出"
	if resume != "" {
		verb = "续上"
	}
	if fallback != "" {
		where += "，目录不存在，在主目录"
	}
	sp.addEntryLocked("event", thread, fmt.Sprintf("%s %s（%s%s）：%s", verb, name, kind, where, clip(firstLine(task), 120)), name, "spawn")
	sp.mu.Unlock()

	// The default alias, model and effort go with the default kind. The
	// default model only with the default alias: settings take it from that
	// alias, and another one (or none) brings (or leaves) its own.
	sp.mu.Lock()
	def, aliasName := kinds[kind] == kinds[sp.st.Settings.WorkerKind], ""
	if def {
		aliasName = sp.st.Settings.WorkerAlias
	}
	if spec.Alias != nil {
		aliasName = *spec.Alias
	}
	if def && aliasName == sp.st.Settings.WorkerAlias {
		if model == "" {
			model = sp.st.Settings.WorkerModel
		}
		if effort == "" {
			effort = sp.st.Settings.WorkerEffort
		}
	}
	runModel := model
	if aliasName != "" {
		runModel = "" // the alias's own (aliasArgs drops --model); a plain fallback keeps it
	}
	w.Run, w.Alias = &[2]string{runModel, effort}, &aliasName
	ask := sp.st.Settings.AskPermissions
	sp.mu.Unlock()
	wargs := withBypass(modelArgs(kind, model, effort), kind, ask)

	// started answers whoever is still waiting on the spawn (superSpawn waits
	// a few seconds): a failure it hears it can say at once, as the spawn's
	// own error, instead of as an event after it already said "started".
	started := make(chan error)
	if spec.Conv != "" {
		started = make(chan error, 1)
	}
	tell := func(err error) bool {
		select {
		case started <- err:
			return true
		default:
			return false
		}
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		bin := sp.machineBin(m)
		// codex's resume is a subcommand: it goes first, the flags after it.
		args := append(resumeArgs(kinds[kind], resume), wargs...)
		if kinds[kind] == "claude" {
			// Reporting back is part of the job; don't make it a permission prompt.
			args = append(args, "--allowedTools", strings.Join(collieAllow(bin), ","))
		}
		alias := sp.launchAlias(ctx, m.ID, aliasName, kinds[kind], name)
		pane, ws, err := sp.s.startAgentAs(ctx, m.ID, kinds[kind], name, dir, name, alias, args)
		if pane != "" {
			sp.s.herdr.RememberRun(pane, aliasModel(alias, model), effort)
		}
		var he *HerdrError
		blocked := err != nil && errors.As(err, &he) && he.Code == "agent_not_ready"
		failed, msg := err != nil && !blocked, ""
		if failed {
			if nameTaken(err) {
				sp.mu.Lock()
				err = fmt.Errorf("%w (spawn again under another name such as %s)", err, spareName(name, takenLocked))
				sp.mu.Unlock()
			}
			// The start may have used up ctx; cleaning up gets its own time.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			// What its terminal said goes with the error; the terminal itself
			// is closed, not left behind as an empty shell.
			msg = err.Error()
			if ws != "" {
				if why := deathReason(sp.paneTail(ctx, pane)); why != "" && !strings.Contains(msg, why) {
					msg += "（终端里：" + why + "）"
				}
				sp.closeWorkspace(ctx, ws)
			}
		}
		sp.mu.Lock()
		w.PaneID, w.WorkspaceID = pane, ws
		if failed {
			// The Router hears it (as the spawn's result, or as an event) and
			// retries, or tells the human; the spawn line shows it ended.
			log.Printf("worker %s failed to start: %s", name, msg)
			w.Status, w.Ended, w.Note = "failed", true, msg
			sp.changed()
			sp.mu.Unlock()
			if !tell(err) {
				sp.Ingest("worker", thread, name, fmt.Sprintf("worker %s failed to start: %s", name, msg), "", "", name)
			}
			return
		}
		w.Status = "idle"
		if blocked {
			w.Status = "blocked"
		}
		sp.changed()
		sp.mu.Unlock()
		tell(nil)
		// The task brief is delivered like any other trigger, so it waits
		// politely if the worker is stuck on a startup dialog.
		if spec.Conv == "" {
			sp.Ingest("system", thread, routerName, workerBrief(bin, w), "", name, name)
		}
	}()
	return w, started, nil
}

func workerBrief(bin string, w *Worker) string {
	return fmt.Sprintf(`You are worker %q, managed by Collie (thread %s). Your task:

%s

How this works:
- Collie relays between you and the human, through %s report "…".
- When the task is done, report the outcome: what you found or changed, and where the details are.
- When you need what only the human can give (a decision, access), or the same failure keeps coming back after a few different approaches, report "BLOCKED: <what you need> · tried: <what you tried> · I'd pick: <your choice>". Collie decides or asks them, and tells you.
- Follow-ups for your thread arrive as messages starting with [collie].`, w.Name, w.Thread, strings.TrimSpace(w.Task), bin)
}

func (sp *Super) Report(name, text string) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	w := sp.st.Workers[name]
	if w == nil {
		return fmt.Errorf("%w %s", errUnknownWorker, name)
	}
	updated := *w
	updated.LastReport, updated.ReportAt = text, time.Now()
	t, th := sp.prepareTriggerLocked("worker", w.Thread, name, fmt.Sprintf("report from %s:\n%s", name, text), "", "", name)
	e := &Entry{ID: sp.st.nextID("e"), Role: "event", Thread: w.Thread, Text: name + " 回报：" + clip(text, 160), At: updated.ReportAt, Ref: name, Tone: "done"}
	if err := sp.commitTriggerLocked(t, th, e, &updated); err != nil {
		return err
	}
	w.LastReport, w.ReportAt = updated.LastReport, updated.ReportAt
	return nil
}

func (sp *Super) workerByPane(pane string) string {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.workerByPaneLocked(pane)
}
