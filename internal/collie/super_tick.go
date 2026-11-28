package collie

import (
	"context"
	"crypto/sha1"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

type liveAgent struct {
	Pane          string
	Status        string
	Ready         bool
	Kind          string
	Session       string
	CompletionSeq uint64
}

var uuidRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// sessionID is what an agent's conversation is resumed by. herdr gives some
// (Codex) as the path of their log, whose name ends in the id.
func sessionID(kind, value string) string {
	if kind != "path" {
		return value
	}
	ids := uuidRe.FindAllString(filepath.Base(value), -1)
	if len(ids) == 0 {
		return ""
	}
	return ids[len(ids)-1]
}

func (sp *Super) liveAgents(ctx context.Context) (map[string]liveAgent, bool) {
	list, ok := sp.agentList(ctx)
	if !ok {
		return nil, false
	}
	return sp.byName(list), true
}

func (sp *Super) agentList(ctx context.Context) ([]Agent, bool) {
	if sp.s == nil || sp.s.herdr == nil {
		return nil, false
	}
	list, err := sp.s.herdr.Agents(ctx)
	return list, err == nil
}

// byName is the named agents in list: Collie's workers, and the Router as routerName.
func (sp *Super) byName(list []Agent) map[string]liveAgent {
	m := map[string]liveAgent{}
	router := sp.routerAgent()
	machine := sp.routerOn()
	cwd := sp.routerCwd(machine)
	for _, a := range list {
		if a.Name == nil {
			continue
		}
		name := *a.Name
		switch name {
		case router:
			if !isRouterAgent(a, router, machine, cwd) {
				continue
			}
			name = routerName
		case routerName:
			continue // another agent took the name the Router could not have
		}
		la := liveAgent{Pane: a.PaneID, Status: a.AgentStatus, Ready: a.Ready, Kind: deref(a.Agent), CompletionSeq: a.CompletionSeq}
		if a.Session != nil {
			la.Session = sessionID(a.Session.Kind, a.Session.Value)
		}
		m[name] = la
	}
	return m
}

func (sp *Super) tick(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	if sp.s.workbenchOn() {
		sp.wb.snapshotDue() // feeds are read over the web: herdr need not be up
	}
	// Listing every computer can take seconds. A worker spawned or done
	// starting meanwhile is missing from a list taken before it was named:
	// it isn't gone, and its brief must not go to the Router instead. Only
	// the workers already up before the list can be missing from it.
	sp.mu.Lock()
	settled := map[string]*Worker{}
	for name, w := range sp.st.Workers {
		if w.Status != "starting" {
			settled[name] = w
		}
	}
	sp.mu.Unlock()
	// One computer that hangs must not use up the tick: it is left out of
	// this list, and what is on it waits for it (away).
	listCtx, cancelList := context.WithTimeout(ctx, tickListWait)
	list, answered, ok := sp.agentsListed(listCtx)
	cancelList()
	if !ok {
		// The follow-up clock and the daily batch do not need a live pane.
		now := time.Now()
		sp.mu.Lock()
		sp.tickTaskFollowupsLocked(now, nil)
		sp.remindPendingLocked(now)
		sp.mu.Unlock()
		return
	}
	sp.push.watch(list)
	live := sp.byName(list)
	now := time.Now()
	away := sp.awayMachines(answered, now)
	sp.recoverDeliveries(ctx, away, live)

	type finished struct {
		w           *Worker
		blocked     bool
		draft       string
		interrupted bool
	}
	var fin []finished
	var dead []*Worker // the agent never started, or quit, leaving a shell
	var resumes []*Worker
	deliveries := map[string][]*Trigger{}

	sp.mu.Lock()
	paused := sp.st.Settings.Paused
	// 1. Timers → triggers.
	for _, t := range sp.st.Timers {
		if t.Enabled && !t.NextRun.After(now) {
			at := now
			t.LastRun = &at
			t.advance(now)
			sp.ingestLocked("timer", t.Thread, t.ID, t.Text, "", t.Target, t.ID)
		}
	}
	sp.remindPendingLocked(now)
	// 2. Worker lifecycle → triggers for the router.
	for _, w := range sp.st.Workers {
		if w.Ended {
			// herdr restored its pane (a herdr restart or update): it's back.
			if a, ok := live[w.Name]; ok && w.Status == "gone" && w.PaneID != "" && a.Pane == w.PaneID {
				next := *w
				next.Ended, next.Status = false, a.Status
				// A completion while the pane was gone belongs to the old turn.
				if a.CompletionSeq > 0 {
					next.CompletionSeq = a.CompletionSeq
				}
				switch {
				case a.Status == "done" || a.Status == "idle":
					if !sp.clearDraftTurnLocked(w, next) {
						continue
					}
				case w.DraftTrigger == "" || a.Status == "working" || a.Status == "blocked":
					*w = next
				default:
					continue // wait until herdr knows whether the draft turn is still running
				}
				sp.ingestLocked("worker", w.Thread, w.Name, fmt.Sprintf("worker %s is back: herdr restored its pane (status %s).", w.Name, a.Status), "", "", w.Name)
				sp.addEntryLocked("event", w.Thread, w.Name+" 回来了（herdr 恢复了它的窗格）", w.Name, "info")
				sp.changed()
			}
			continue
		}
		a, alive := live[w.Name]
		if !alive && (away[w.Machine] || sp.taskWorkerOfflineLocked(w, answered)) {
			continue // its computer didn't answer: it may well be running
		}
		if !alive {
			if w.Status != "starting" && w.Status != "failed" && settled[w.Name] == w {
				w.Ended, w.Status = true, "gone"
				w.Background = nil
				sp.ingestLocked("worker", w.Thread, w.Name, fmt.Sprintf("worker %s exited (its herdr pane is gone).", w.Name), "", "", w.Name)
				sp.addEntryLocked("event", w.Thread, w.Name+" 已退出", w.Name, "info")
			}
			continue
		}
		// No agent in the pane for a while: it failed to start (a broken
		// install, a bad flag) or quit. Messages for it would wait forever.
		if a.Kind == "" {
			since, seen := sp.noAgent[w.Name]
			if !seen {
				sp.noAgent[w.Name], since = now, now
			}
			if now.Sub(since) > 30*time.Second && now.Sub(w.CreatedAt) > 45*time.Second {
				delete(sp.noAgent, w.Name)
				w.Ended, w.Status, w.PaneID = true, "failed", a.Pane
				w.Background = nil
				for _, t := range sp.st.Triggers {
					if t.Status == "queued" && len(t.Deliveries) == 0 && len(t.Targets) == 1 && t.Targets[0] == w.Name {
						t.Status = "ignored"
					}
				}
				dead = append(dead, w)
				sp.changed()
			}
			continue
		}
		delete(sp.noAgent, w.Name)
		if a.Session != "" && a.Session != w.Session {
			w.Session = a.Session
			sp.dirty = true
		}
		prev, prevComp := w.Status, w.CompletionSeq
		if a.CompletionSeq > 0 && a.CompletionSeq != w.CompletionSeq {
			w.CompletionSeq = a.CompletionSeq
			sp.dirty = true
		}
		if a.Status == "blocked" && now.Sub(sp.lastAnswer[a.Pane]) < 2*time.Second {
			continue // give a just-answered dialog a moment to clear (or show the next one)
		}
		if a.Status != prev {
			w.Status = a.Status
			w.PaneID = a.Pane
			sp.changed()
			if prev == "blocked" {
				sp.settleAskLocked(a.Pane, "", "") // answered in its terminal, or it moved on
			}
			switch a.Status {
			case "blocked":
				w.Background = nil
				fin = append(fin, finished{w: w, blocked: true})
			case "working":
				w.Background = nil
				w.ResumeAt = time.Time{} // it got going again: a message, the human, or Collie's own nudge
			}
		}
		if a.Status == "done" || a.Status == "idle" {
			draft := w.DraftTrigger
			completed := prev == "working" || (a.Status == "done" && prev != "blocked" &&
				(prev != "done" || (prevComp > 0 && a.CompletionSeq > prevComp)))
			switch {
			case draft != "" && (completed || prev == "blocked" || a.CompletionSeq > prevComp):
				if !sp.clearDraftTurnLocked(w, *w) {
					w.Status, w.CompletionSeq = prev, prevComp
					continue
				}
				fin = append(fin, finished{w: w, draft: draft, interrupted: prev == "blocked" && a.CompletionSeq <= prevComp})
			case draft == "" && w.Background != nil:
				if prevComp > 0 && a.CompletionSeq > prevComp {
					w.Background = &workerBackgroundWait{Since: now, clearAt: now}
				}
				fin = append(fin, finished{w: w})
			case draft == "" && completed && now.Sub(w.ReportAt) > workerReportGrace:
				if w.Conv == "" || sp.s.ext == nil {
					// herdr can report completion before the agent paints its footer.
					w.Background = &workerBackgroundWait{Since: now, clearAt: now}
					sp.dirty = true
				} else {
					fin = append(fin, finished{w: w})
				}
			}
		}
		if !paused && sp.resumeDueLocked(w, a.Status, now) {
			w.ResumeAt = time.Time{}
			resumes = append(resumes, w)
			sp.changed()
		}
	}
	sp.tickTaskFollowupsLocked(now, answered)
	// 3. Route queued triggers to sessions: subscribed workers first (fast path), router otherwise.
	for _, t := range sp.st.Triggers {
		if t.Status != "queued" {
			continue
		}
		if !sp.taskTriggerCurrentLocked(t) {
			t.Status = "ignored"
			sp.changed()
			continue
		}
		if _, ok := sp.larkDeliveryLocked(t); !ok {
			sp.cancelLarkDeliveryLocked(t)
			continue
		}
		if !sp.routeExtensionLocked(t, live, away, paused) {
			continue
		}
		if len(t.Targets) == 0 {
			if t.Source != "worker" && t.Source != "chat" {
				if th := sp.st.Threads[t.Thread]; th != nil {
					for _, sub := range th.Subscribers {
						if w := sp.st.Workers[sub]; w != nil && !w.Ended && sub != t.From {
							t.Targets = append(t.Targets, sub)
						}
					}
				}
			}
			if len(t.Targets) == 0 {
				t.Targets = []string{routerName}
			}
		}
		for _, target := range t.Targets {
			if _, pending := t.Deliveries[target]; !pending {
				deliveries[target] = append(deliveries[target], t)
			}
		}
	}
	// Persist the state that produced these triggers before sending them.
	// The delivery journal alone cannot preserve an advanced timer or the ID sequence.
	if !paused && len(deliveries) > 0 {
		if err := sp.flushLocked(); err != nil {
			log.Printf("save delivery queue: %v", err)
			clear(deliveries)
		}
	}
	routerAlive := false
	_, routerAlive = live[routerName]
	sp.mu.Unlock()

	// Worker completion summaries need file/screen IO: do it outside the lock.
	for _, f := range fin {
		if f.draft != "" {
			sp.finishDraftTurn(ctx, f.w, f.draft, f.interrupted)
		} else {
			sp.workerSettled(ctx, f.w, f.blocked)
		}
	}
	for _, w := range dead {
		sp.workerDead(ctx, w)
	}

	if paused {
		return
	}
	var resuming sync.WaitGroup
	for _, w := range resumes {
		resuming.Go(func() { sp.resumeWorker(ctx, w) })
	}
	resuming.Wait()

	// 4. Deliver batches to free sessions, side by side: each one is a send
	// and a read back across the internet, with its own time.
	var sending sync.WaitGroup
	for target, list := range deliveries {
		if target == routerName && (sp.handingOver() || sp.dreaming()) {
			continue // events wait for the fresh session
		}
		a, alive := live[target]
		if !alive {
			if away[sp.targetMachine(target)] {
				continue // delivered as soon as its computer answers again
			}
			if target == routerName {
				sp.ensureRouter(ctx)
			} else {
				sp.rerouteToRouter(target, list)
			}
			continue
		}
		if target == routerName && a.Status == "blocked" {
			sp.trustOwnDir(ctx, a.Pane)
		}
		// Nothing is typed over a dialog or into an agent that is still
		// starting. Otherwise a message goes in mid-turn: Claude, Codex and agy
		// queue typed input and pick it up between steps, so corrections and
		// questions arrive now rather than after the current step. Collie's
		// other events (reports, timers) batch up while it works; the human's
		// own messages don't wait.
		if a.Status == "blocked" || (!a.Ready && a.Status == "unknown") {
			continue
		}
		if a.Status == "working" {
			if w := settled[target]; w != nil && strings.HasPrefix(w.Conv, convID(larkxID, "")) {
				continue // a dedicated Feishu agent finishes its turn before the next request
			}
			if target == routerName {
				list = fromHuman(list)
			} else {
				list = withoutLarkRequests(list)
			}
			if len(list) == 0 {
				continue
			}
		}
		sending.Add(1)
		go func() {
			defer sending.Done()
			dctx, cancel := context.WithTimeout(parent, deliverWait)
			defer cancel()
			sp.deliver(dctx, target, list)
		}()
	}
	sending.Wait()

	if a, ok := live[routerName]; ok && (a.Status == "done" || a.Status == "idle") {
		sp.routerSettled(ctx, a.Pane)
	}
	sp.checkHandover(ctx, live, len(deliveries[routerName]) > 0)
	sp.checkDream(ctx, live, len(deliveries[routerName]) > 0)

	// 5. Watchdog.
	sp.watchdog(ctx, live, routerAlive)
}

const (
	tickListWait = 3 * time.Second
	deliverWait  = 30 * time.Second // a send, read back, and its retries
	// awayGrace: how long a computer that doesn't answer (reconnecting,
	// hanging) keeps its workers. Past it they count as gone, as before.
	awayGrace = 2 * time.Minute
)

// agentsListed is the agent list with the computers that answered.
func (sp *Super) agentsListed(ctx context.Context) ([]Agent, map[string]bool, bool) {
	if sp.s == nil || sp.s.herdr == nil {
		return nil, nil, false
	}
	list, answered, err := sp.s.herdr.AgentsListed(ctx)
	return list, answered, err == nil
}

// awayMachines is the computers that didn't answer this list (or aren't
// connected, as after a hub restart), for less than awayGrace so far.
func (sp *Super) awayMachines(answered map[string]bool, now time.Time) map[string]bool {
	ids := map[string]bool{}
	for _, m := range sp.s.herdr.Machines() {
		ids[m.ID] = true
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	for _, w := range sp.st.Workers {
		if !w.Ended {
			ids[w.Machine] = true
		}
	}
	ids[sp.routerMachineLocked()] = true
	away := map[string]bool{}
	for id := range sp.unanswered {
		if !ids[id] || answered[id] {
			delete(sp.unanswered, id)
		}
	}
	for id := range ids {
		if answered[id] {
			continue
		}
		since, ok := sp.unanswered[id]
		if !ok {
			sp.unanswered[id], since = now, now
		}
		away[id] = now.Sub(since) < awayGrace
	}
	return away
}

// watchdog is the external control loop: it restarts the router when work is
// waiting and flags workers that look stuck (screen unchanged while working).
func (sp *Super) watchdog(ctx context.Context, live map[string]liveAgent, routerAlive bool) {
	sp.mu.Lock()
	stall := time.Duration(sp.st.Settings.StallMinutes) * time.Minute
	var watch []*Worker
	for _, w := range sp.st.Workers {
		if !w.Ended && w.Status == "working" {
			watch = append(watch, w)
		}
	}
	sp.mu.Unlock()
	for _, w := range watch {
		a, ok := live[w.Name]
		if !ok {
			continue
		}
		res, err := sp.s.herdr.Read(ctx, a.Pane, "detection", "text", 0)
		if err != nil {
			continue
		}
		h := progressHash(res.Text)
		m := sp.lastScreen[w.Name]
		if m.hash != h {
			sp.lastScreen[w.Name] = screenMark{hash: h, since: time.Now()}
			continue
		}
		if !m.flag && stall > 0 && time.Since(m.since) > stall {
			m.flag = true
			sp.lastScreen[w.Name] = m
			sp.Ingest("system", w.Thread, "watchdog",
				fmt.Sprintf("watchdog: worker %s has shown no visible progress for %d min while 'working'. Inspect with `collie show %s`; send guidance, stop it, or tell the human.", w.Name, int(stall.Minutes()), w.Name), "", "", w.Name)
		}
	}
	sp.watchRouter(ctx, live, stall)
}

// watchRouter catches Collie itself hanging mid-turn (a stream that never
// ends, a tool that never returns): nothing but the timer moves on its screen.
// The first time it interrupts and tells Collie to carry on; if that turn
// hangs again, it tells the human instead of looping.
func (sp *Super) watchRouter(ctx context.Context, live map[string]liveAgent, stall time.Duration) {
	a, ok := live[routerName]
	if !ok || stall <= 0 || a.Status != "working" {
		sp.mu.Lock()
		delete(sp.lastScreen, routerName)
		if ok && (a.Status == "done" || a.Status == "idle") {
			sp.routerNudged = false
		}
		sp.mu.Unlock()
		return
	}
	res, err := sp.s.herdr.Read(ctx, a.Pane, "detection", "text", 0)
	if err != nil {
		return
	}
	h := progressHash(res.Text)
	sp.mu.Lock()
	m := sp.lastScreen[routerName]
	if m.hash != h {
		sp.lastScreen[routerName] = screenMark{hash: h, since: time.Now()}
		sp.mu.Unlock()
		return
	}
	if m.flag || time.Since(m.since) < stall {
		sp.mu.Unlock()
		return
	}
	m.flag = true
	sp.lastScreen[routerName] = m
	mins := int(time.Since(m.since).Minutes())
	if sp.routerNudged {
		sp.addEntryLocked("event", mainThread, fmt.Sprintf("Collie 又卡住了（%d 分钟没有动静，自动打断过一次没好）；可以在终端里看看它，或在 自动化 → 设置 里给 Collie 换一个 agent", mins), routerName, "error")
		sp.mu.Unlock()
		return
	}
	sp.routerNudged = true
	sp.addEntryLocked("event", mainThread, fmt.Sprintf("Collie 卡住了 %d 分钟没有动静，已自动打断让它继续", mins), "", "info")
	sp.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		sp.s.herdr.SendKeys(ctx, a.Pane, "esc")
		time.Sleep(2 * time.Second)
		if err := sp.s.herdr.Prompt(ctx, sp.routerAgent(), fmt.Sprintf("%s: nothing on your screen changed for %d min, so Collie interrupted it. Carry on with what you were doing; the events you had are still yours to handle. A command that hangs is a job for a worker: hand it over.", routerRetryMark, mins)); err != nil {
			log.Printf("router nudge: %v", err)
			return
		}
		sp.mu.Lock()
		if sp.routerAskedAt.IsZero() {
			sp.routerAskedAt = time.Now()
		}
		sp.mu.Unlock()
	}()
}

// progressHash fingerprints a pane's screen for the watchdog, leaving out what
// moves on its own while an agent waits: the spinner line ("✻ Thinking… (3m
// 21s · esc to interrupt)", "Working (12s • esc to interrupt)") and numbers.
func progressHash(screen string) string {
	var b strings.Builder
	for _, l := range strings.Split(screen, "\n") {
		if strings.Contains(strings.ToLower(l), "esc to interrupt") || strings.Contains(strings.ToLower(l), "esc to cancel") {
			continue
		}
		b.WriteString(strings.Map(func(r rune) rune {
			if unicode.IsDigit(r) {
				return -1
			}
			return r
		}, l))
		b.WriteByte('\n')
	}
	return fmt.Sprintf("%x", sha1.Sum([]byte(b.String())))
}
