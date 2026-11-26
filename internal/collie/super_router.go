package collie

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func (sp *Super) routerDir() string { return filepath.Join(sp.dir, "router") }

func (sp *Super) ensureRouter(ctx context.Context) {
	if sp.s == nil || sp.s.herdr == nil {
		return
	}
	sp.mu.Lock()
	if sp.routerLaunching || time.Since(sp.routerLaunchAt) < 45*time.Second {
		sp.mu.Unlock()
		return
	}
	sp.routerLaunching, sp.routerLaunchAt = true, time.Now()
	kind := sp.st.Settings.RouterKind
	machine := sp.routerMachineLocked()
	args := sp.routerArgsLocked()
	aliasName := sp.st.Settings.RouterAlias
	prevRun, prevLaunch := sp.routerRunLocked(), sp.routerLaunchLocked()
	sp.setRouterRun([2]string{sp.st.Settings.RouterModel, sp.st.Settings.RouterEffort})
	sp.setRouterOn(machine, aliasName)
	sp.mu.Unlock()
	// keep: no new Router started; the record goes back to the running one's.
	keep := func(name string) {
		sp.mu.Lock()
		defer sp.mu.Unlock()
		sp.routerLaunchOKLocked()
		if prevRun != nil {
			sp.setRouterRun(*prevRun)
		}
		prevLaunch.Name = name
		sp.saveRouterLaunchLocked(prevLaunch)
	}

	go func() {
		defer func() { sp.mu.Lock(); sp.routerLaunching = false; sp.mu.Unlock() }()
		lctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// Another computer's paths go into ROUTER.md: learn them before writing it.
		err := sp.syncRouter(lctx)
		if err == nil {
			if err = sp.writeRouterFiles(); err != nil {
				log.Printf("router files: %v", err)
			}
			sp.routerReadsFresh()
			err = sp.syncRouter(lctx)
		}
		// The tick that sent us here may have listed agents while the Router's
		// computer was still reconnecting (a hub restart, a node update): a
		// sync went through, so it answers now. Is the Router there after all?
		if err == nil {
			if live, ok := sp.liveAgents(lctx); ok {
				if a, alive := live[routerName]; alive {
					log.Printf("router: %s is running after all; keeping it", a.Pane)
					keep(prevLaunch.Name)
					return
				}
			}
		}
		var name string // the herdr name the new Router got, if it started
		kept := false
		if err == nil {
			sp.closeStaleRouterWorkspaces(lctx)
			alias := sp.launchAlias(lctx, machine, aliasName, kinds[kind], routerName)
			sp.noteAliasFallback(machine, aliasName, alias)
			var pane, ws string
			pane, ws, err = sp.s.startAgentAs(lctx, machine, kinds[kind], routerName, sp.routerCwd(machine), routerLabel, alias, args)
			switch {
			case nameTaken(err) && pane != "":
				name, kept, err = sp.settleNameTaken(lctx, machine, pane, ws, kinds[kind], args)
			case pane != "":
				name = routerName
			}
		} else {
			err = fmt.Errorf("没能把 Collie 的目录同步到 %s：%w", machine, err)
		}
		if kept {
			keep("") // the Router that had the name all along carries on
			return
		}
		sp.mu.Lock()
		defer sp.mu.Unlock()
		if name != "" {
			sp.setRouterNameLocked(name)
		}
		var he *HerdrError
		if err != nil && !(errors.As(err, &he) && he.Code == "agent_not_ready") {
			if msg := sp.routerLaunchFailedLocked(machine, err); msg != "" {
				sp.addEntryLocked("event", mainThread, msg, routerName, "error")
			}
			return
		}
		sp.routerLaunchOKLocked()
		sp.routerFresh = true
		sp.addEntryLocked("event", mainThread, "Collie 已唤醒（"+kind+"）", routerName, "info")
	}()
}

func (sp *Super) RestartRouter(ctx context.Context) error {
	if live, ok := sp.liveAgents(ctx); ok {
		if a, ok := live[routerName]; ok {
			var p Pane
			if err := sp.s.herdr.Call(ctx, "pane.get", map[string]any{"pane_id": a.Pane}, &struct {
				Pane *Pane `json:"pane"`
			}{&p}); err == nil && p.WorkspaceID != "" {
				sp.s.herdr.Call(ctx, "workspace.close", map[string]any{"workspace_id": p.WorkspaceID}, nil)
			}
		}
	}
	sp.mu.Lock()
	sp.routerLaunchAt = time.Time{}
	sp.mu.Unlock()
	sp.ensureRouter(ctx)
	return nil
}

// followLocked is the soonest one-off timer Collie set itself ("in 30m",
// "at …"): a follow-up. Routines like a weekday check are not one.
func (sp *Super) followLocked() *followView {
	var f *followView
	for _, t := range sp.st.Timers {
		if t.Enabled && t.Owner != "you" && (strings.HasPrefix(t.Spec, "in ") || strings.HasPrefix(t.Spec, "at ")) && t.NextRun.After(time.Now()) {
			if f == nil || t.NextRun.Before(f.Next) {
				f = &followView{Next: t.NextRun, NextText: clip(firstLine(t.Text), 80)}
			}
		}
	}
	return f
}

// routerReadFile keeps a copy of the ROUTER.md the running Collie read.
const routerReadFile = ".router-read.md"

// routerDocChanged: ROUTER.md was just written (a new build, new settings).
// The running Collie read it when its session began; if it now says
// something else, have it read it again.
func (sp *Super) routerDocChanged(dir string, doc []byte) {
	read, err := os.ReadFile(filepath.Join(dir, routerReadFile))
	if err != nil || bytes.Equal(read, doc) {
		return // no session has read it yet, or nothing changed
	}
	os.WriteFile(filepath.Join(dir, routerReadFile), doc, 0o600)
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if t := sp.rereadTrig; t != nil && t.Status == "queued" {
		return // already asked
	}
	sp.rereadTrig = sp.ingestLocked("system", mainThread, "collie", "ROUTER.md changed (a new Collie build or new settings). Re-read it now and work by it from here on. Nothing to tell the human.", "", "", "")
}

// routerReadsFresh: a new session is starting and reads ROUTER.md as it is.
func (sp *Super) routerReadsFresh() {
	dir := sp.routerDir()
	if b, err := os.ReadFile(filepath.Join(dir, "ROUTER.md")); err == nil {
		os.WriteFile(filepath.Join(dir, routerReadFile), b, 0o600)
	}
	sp.mu.Lock()
	if t := sp.rereadTrig; t != nil && t.Status == "queued" {
		t.Status = "ignored" // it reads them anyway
	}
	sp.rereadTrig = nil
	sp.mu.Unlock()
}

func (sp *Super) writeRouterFiles() error {
	dir := sp.routerDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sp.mu.Lock()
	words := sp.st.Settings.ReplyChars
	wkind := sp.st.Settings.WorkerKind
	wcwd := sp.s.tilde(sp.st.Settings.WorkerCwd)
	machine := sp.routerOnLocked()
	sp.mu.Unlock()
	tdir, tbin := sp.routerTarget(machine)
	if tdir == "" { // that computer's folder is not known yet: the first sync writes these
		sp.ensureMemory()
		return nil
	}
	layout := "   " + strings.ReplaceAll(memoryLayout, "\n", "\n   ")
	wbDoc := ""
	if sp.s.workbenchOn() {
		wbDoc = routerWBDoc
	}
	doc := strings.NewReplacer("{{LAYOUT}}", layout, "{{BIN}}", tbin, "{{WORDS}}", fmt.Sprint(words), "{{WKIND}}", wkind, "{{WCWD}}", wcwd, "{{BUDGET}}", fmt.Sprint(upFrontBudget>>10)).Replace(strings.Replace(routerDoc, "{{WB}}", wbDoc, 1))
	for _, name := range []string{"ROUTER.md", "CLAUDE.md", "AGENTS.md", "GEMINI.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(doc), 0o600); err != nil {
			return err
		}
	}
	sp.routerDocChanged(dir, []byte(doc))
	// Claude Code asks before every shell command; the router's whole job is
	// running the collie CLI and keeping memory/, so allow exactly that.
	perms, _ := json.MarshalIndent(map[string]any{"permissions": map[string]any{
		"allow": collieAllow(tbin, append(memoryRules(tdir),
			// Read-only peeks at a worker's terminal.
			"Bash(herdr agent read:*)", "Bash(herdr agent list:*)", "Bash(herdr agent get:*)", "Bash(herdr pane read:*)")...),
	}}, "", "  ")
	os.MkdirAll(filepath.Join(dir, ".claude"), 0o700)
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), perms, 0o600); err != nil {
		return err
	}
	sp.ensureMemory()
	sp.syncRouterNow()
	return nil
}

// closeStaleRouterWorkspaces removes router workspaces whose agent has exited,
// so a crashed router does not leave an orphan shell behind.
func (sp *Super) closeStaleRouterWorkspaces(ctx context.Context) {
	wss, err := sp.s.herdr.Workspaces(ctx)
	if err != nil {
		return
	}
	for _, ws := range wss {
		if ws.Label == routerLabel { // wherever it ran: there is one Collie at a time
			sp.s.herdr.Call(ctx, "workspace.close", map[string]any{"workspace_id": ws.ID}, nil)
		}
	}
}

// agentErrRe matches what an agent CLI prints when it can't run (the API
// refused, the login expired, the quota ran out), not Collie's own words about
// a task: "the backup failed twice" and "its error output" are work, not errors.
var agentErrRe = regexp.MustCompile(`(?i)(api error|please run /login|invalid api key|quota exceeded|daily limit|rate.?limit(ed)?\b|overloaded|unauthori[sz]ed|credit balance|unexpected status \d{3}|^[\W]*(⚠|■|error\b|error:))`)

// routerSettled runs once after the router finishes a turn that we prompted.
// If it went quiet without a `collie reply` and its screen shows an error
// (bad credentials, region block, quota…), surface that in the chat instead of
// leaving the human staring at silence.
func (sp *Super) routerSettled(ctx context.Context, pane string) {
	sp.mu.Lock()
	asked := sp.routerAskedAt
	if asked.IsZero() || time.Since(asked) < 4*time.Second {
		sp.mu.Unlock()
		return
	}
	sp.routerAskedAt = time.Time{}
	for i := len(sp.st.Entries) - 1; i >= 0; i-- {
		e := sp.st.Entries[i]
		if e.At.Before(asked) {
			break
		}
		if e.Role == "collie" {
			sp.routerRetries = 0
			sp.mu.Unlock()
			return
		}
	}
	sp.mu.Unlock()

	rr, err := sp.s.herdr.Read(ctx, pane, "recent", "text", 60)
	if err != nil {
		return
	}
	lines := strings.Split(rr.Text, "\n")
	// Only look at what the agent printed after our prompt, never at the event text itself.
	start := 0
	for i, l := range lines {
		if strings.Contains(l, "collie reply") || strings.Contains(l, "yourself.") || strings.Contains(l, routerRetryMark) {
			start = i + 1
		}
	}
	for i := start; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		if l == "" || !agentErrRe.MatchString(l) {
			continue
		}
		// Messages wrap in the terminal: keep following lines of the same block.
		msg := l
		for j := i + 1; j < len(lines) && j <= i+3; j++ {
			next := strings.TrimSpace(lines[j])
			if next == "" || strings.HasPrefix(next, "─") || strings.HasPrefix(next, "✻") || strings.HasPrefix(next, "Error ID") {
				break
			}
			msg += " " + next
		}
		if r := []rune(msg); len(r) > 240 {
			msg = string(r[:240]) + "…"
		}
		sp.mu.Lock()
		// Most of these pass (a region check, a busy API, a dropped
		// connection): try again before calling it an error.
		if n := sp.routerRetries; n < len(routerRetryWait) {
			sp.routerRetries++
			wait := routerRetryWait[n]
			sp.addEntryLocked("event", mainThread, fmt.Sprintf("Collie 遇到 API 错误（%s），%s后自动重试", clip(msg, 80), waitText(wait)), "", "info")
			sp.mu.Unlock()
			go sp.retryRouter(wait, msg)
			return
		}
		sp.routerRetries = 0
		text := "Collie 出错了：" + msg + "（自动重试了 " + fmt.Sprint(len(routerRetryWait)) + " 次都没好；可以在终端里让它继续，或在 自动化 → 设置 里给 Collie 换一个 agent）"
		for i := len(sp.st.Entries) - 1; i >= 0 && i >= len(sp.st.Entries)-6; i-- {
			if e := sp.st.Entries[i]; e.Text == text && time.Since(e.At) < 10*time.Minute {
				sp.mu.Unlock()
				return // same failure again; one notice is enough
			}
		}
		sp.addEntryLocked("event", mainThread, text, routerName, "error")
		sp.mu.Unlock()
		return
	}
	sp.mu.Lock()
	sp.routerRetries = 0 // a quiet turn, no error: it is working again
	sp.mu.Unlock()
}

// routerRetryWait: how long to wait before each retry of a turn that ended
// on an API error.
var routerRetryWait = []time.Duration{20 * time.Second, time.Minute, 3 * time.Minute}

// routerRetryMark starts the retry prompt; the error scan reads from there.
const routerRetryMark = "[collie] Your last turn stopped"

func waitText(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	return fmt.Sprintf("%d 分钟", int(d.Minutes()))
}

// retryRouter nudges Collie to carry on after an API error, unless something
// else has got it going meanwhile (your next message, a worker's event).
func (sp *Super) retryRouter(wait time.Duration, msg string) {
	time.Sleep(wait)
	sp.mu.Lock()
	busy := !sp.routerAskedAt.IsZero() || sp.routerLaunching
	sp.mu.Unlock()
	if busy {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sp.s.herdr.Prompt(ctx, sp.routerAgent(), routerRetryMark+" on an error from your model's API ("+msg+"). It may have passed: carry on with what you were doing; the events you had are still yours to handle."); err != nil {
		log.Printf("router retry: %v", err)
		return
	}
	sp.mu.Lock()
	if sp.routerAskedAt.IsZero() {
		sp.routerAskedAt = time.Now() // so this turn is checked like any other
	}
	sp.mu.Unlock()
}

// routerArgsLocked: the extra launch flags plus the chosen model and effort.
func (sp *Super) routerArgsLocked() []string {
	st := sp.st.Settings
	args := append(append([]string{}, st.RouterArgs...), modelArgs(st.RouterKind, st.RouterModel, st.RouterEffort)...)
	return withBypass(args, st.RouterKind, st.AskPermissions)
}

// The model and effort the router was launched with, kept across hub restarts.
func (sp *Super) setRouterRun(run [2]string) {
	sp.routerRun = &run
	b, _ := json.Marshal(run)
	sp.db.Exec(`INSERT INTO kv(k,v) VALUES('router_run',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, string(b))
}

// routerLaunch: where and through which alias the router was last started,
// kept across hub restarts. Older hubs stored the bare machine id.
type routerLaunch struct {
	Machine string `json:"machine"`
	Alias   string `json:"alias,omitempty"`
	Name    string `json:"name,omitempty"` // its herdr name when not routerName
}

// setRouterOn keeps the Router's herdr name: the running one is still known
// by it until a new launch has named its own.
func (sp *Super) setRouterOn(machine, alias string) {
	sp.saveRouterLaunchLocked(routerLaunch{Machine: machine, Alias: alias, Name: sp.routerLaunchLocked().Name})
}

func (sp *Super) saveRouterLaunchLocked(l routerLaunch) {
	sp.routerLaunched = &l
	if sp.db == nil {
		return
	}
	b, err := json.Marshal(sp.routerLaunched)
	if err == nil {
		_, err = sp.db.Exec(`INSERT INTO kv(k,v) VALUES('router_on',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, string(b))
	}
	if err != nil {
		log.Printf("router_on: %v", err)
	}
}

func (sp *Super) routerLaunchLocked() routerLaunch {
	if sp.routerLaunched == nil {
		var l routerLaunch
		var raw string
		if sp.db != nil {
			if err := sp.db.QueryRow(`SELECT v FROM kv WHERE k='router_on'`).Scan(&raw); err != nil && !errors.Is(err, sql.ErrNoRows) {
				log.Printf("router_on: %v", err)
			}
		}
		if json.Unmarshal([]byte(raw), &l) != nil {
			l.Machine = raw
		}
		sp.routerLaunched = &l
	}
	return *sp.routerLaunched
}

func (sp *Super) routerOnLocked() string { return sp.routerLaunchLocked().Machine }

// noteAliasFallback: the alias asked for could not be used and the agent
// starts plainly; the record says so, so Collie shows as waiting for a restart.
func (sp *Super) noteAliasFallback(machine, asked string, used *LauncherAlias) {
	if asked == "" || used != nil {
		return
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.setRouterOn(machine, "")
}

func (sp *Super) routerRunLocked() *[2]string {
	if sp.routerRun == nil {
		var raw string
		var run [2]string
		if sp.db.QueryRow(`SELECT v FROM kv WHERE k='router_run'`).Scan(&raw) == nil && json.Unmarshal([]byte(raw), &run) == nil {
			sp.routerRun = &run
		}
	}
	return sp.routerRun
}
