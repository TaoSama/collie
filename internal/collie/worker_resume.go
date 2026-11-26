package collie

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// A worker's agent can stop mid-job on an error that isn't the job's: the
// connection dropped, the API was busy, a usage limit ran out. Most of these
// pass. Collie tells the worker to carry on by itself, so neither the Router
// nor the human has to; only what needs someone (a login, a key, credits),
// or an error that keeps coming back, goes to the Router.

type agentStop int

const (
	stopHandOff agentStop = iota // a login, a key, credits: someone has to act
	stopQuota                    // a usage limit: passes when it resets
	stopBlip                     // the connection or the API: passes in a moment
)

const (
	handOffPat = `please run /login|not logged in|log ?in again|invalid api key|unauthori[sz]ed|authentication|\b40[13]\b|forbidden|token (has )?expired|credit balance|out of credits`
	quotaPat   = `usage limit|limit reached|hit your( \w+)? limit|reached your( \w+)? limit|quota exceeded|exceeded your( current)? quota|resource.?exhausted|daily limit|rate.?limit(ed)?\b|\b429\b|too many requests|try again (at|in) `
	blipPat    = `api error|overloaded|unexpected status \d{3}|econnreset|econnrefused|etimedout|socket hang up|connection (dropped|reset|refused|closed|error)|stream (error|disconnected)|fetch failed|internal server error|bad gateway|service unavailable|gateway timeout`
)

var (
	agentHandOffRe = regexp.MustCompile(`(?i)(` + handOffPat + `)`)
	agentQuotaRe   = regexp.MustCompile(`(?i)(` + quotaPat + `)`)
	// workerStopRe is an agent CLI saying it can't go on. Narrower than
	// agentErrRe: a worker's screen is full of its work, and "error:" there
	// is mostly the work's own.
	workerStopRe = regexp.MustCompile(`(?i)(` + handOffPat + `|` + quotaPat + `|` + blipPat + `)`)
)

// classifyStop sorts the line an agent stopped on. "API Error: 401" is a
// login and "API Error: 429" a limit, so those are looked for first.
func classifyStop(line string) agentStop {
	switch {
	case agentHandOffRe.MatchString(line):
		return stopHandOff
	case agentQuotaRe.MatchString(line):
		return stopQuota
	}
	return stopBlip
}

// workerResumeMark starts the message that tells a worker to carry on. The
// error scan reads the screen from the last one: the error above it is the
// one already handled.
const workerResumeMark = "[collie] 继续"

const workerResumeText = workerResumeMark + "：你上一轮被模型 API 的错误打断了（不是任务本身的问题），接着做没做完的事。"

const (
	// resumeAfterReset: a limit's reset time is to the minute; give it one.
	resumeAfterReset = time.Minute
	// maxQuotaWait: past a weekly limit's reset, waiting isn't the answer.
	maxQuotaWait = 8 * 24 * time.Hour
)

// workerStoppedLocked handles a worker whose turn ended on msg, its agent's
// error: plan a "carry on" when it passes by itself, otherwise hand the job
// to the Router as before.
func (sp *Super) workerStoppedLocked(w *Worker, msg, summary string, now time.Time) {
	kind, n := classifyStop(msg), w.Resumed
	if kind != stopHandOff && n < len(routerRetryWait) {
		at, quota := now.Add(routerRetryWait[n]), false
		if kind == stopQuota {
			if t, ok := resetTime(msg, now); ok && t.Sub(now) < maxQuotaWait {
				at, quota = later(t, now).Add(resumeAfterReset), true
			}
		}
		w.Resumed, w.ResumeAt = n+1, at
		sp.changed()
		log.Printf("worker %s stopped on an agent error (%s); carry on at %s (%d/%d)", w.Name, msg, at.Format(time.DateTime), w.Resumed, len(routerRetryWait))
		if quota {
			// Waiting may be too slow for the job: the Router can decide.
			sp.ingestLocked("worker", w.Thread, w.Name, fmt.Sprintf(
				"worker %s hit a usage limit: %s\nCollie will tell it to carry on at %s, after the limit resets (auto-resume %d of %d). Nothing to do if the job can wait. If it can't: `collie stop %s` first, so the two don't both carry on, then hand the job to another kind of agent.",
				w.Name, msg, at.Format("01-02 15:04"), w.Resumed, len(routerRetryWait), w.Name), "", "", w.Name)
			sp.addEntryLocked("event", w.Thread, fmt.Sprintf("%s 用完了额度，%s 额度重置后 Collie 让它自动继续", w.Name, at.Format("01-02 15:04")), w.Name, "info")
			return
		}
		sp.addEntryLocked("event", w.Thread, fmt.Sprintf("%s 遇到模型 API 错误（%s），%s后自动继续（第 %d/%d 次）",
			w.Name, clip(msg, 80), waitText(routerRetryWait[n]), w.Resumed, len(routerRetryWait)), w.Name, "info")
		return
	}
	again := ""
	if n > 0 {
		again = fmt.Sprintf("Collie already told it to carry on %d times; the error keeps coming back.\n", n)
	}
	w.clearResume()
	sp.ingestLocked("worker", w.Thread, w.Name,
		fmt.Sprintf("worker %s stopped on an error from its agent, not because it was done:\n%s\n%sLast message before it:\n%s\nIf this is a usage limit, a login or an outage, it won't get better by itself soon: hand the job to another kind of agent (spawn a fresh one with the brief and what this one got done; --resume only works with the same agent), then tell the human in one line what happened and who carries on.",
			w.Name, msg, again, summary), "", "", w.Name)
	log.Printf("worker %s stopped on an agent error: %s", w.Name, msg)
}

func (w *Worker) clearResume() {
	w.ResumeAt, w.Resumed = time.Time{}, 0
}

// resumedNote is what the Router hears with a finished turn when Collie had
// to tell the worker to carry on n times.
func resumedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (Collie auto-resumed it %d time(s) after its agent stopped on an API error or a usage limit.)", n)
}

// resumeDueLocked says whether to send w its "carry on" now: it's due, w is
// still running and idle, and nothing new waits for it.
func (sp *Super) resumeDueLocked(w *Worker, status string, now time.Time) bool {
	if w.ResumeAt.IsZero() || w.Ended || now.Before(w.ResumeAt) || (status != "idle" && status != "done") {
		return false
	}
	for _, t := range sp.st.Triggers {
		if t.Status == "queued" && contains(t.Targets, w.Name) {
			return false // a new message for it: that carries it on
		}
	}
	return true
}

// resumeWorker tells w to carry on. A send that fails counts as a try and
// is tried again in a moment.
func (sp *Super) resumeWorker(ctx context.Context, w *Worker) {
	sp.mu.Lock()
	ended := w.Ended // stopped since the tick looked
	sp.mu.Unlock()
	if ended {
		return
	}
	h := sp.s.herdr
	pane := h.paneOfName(ctx, sp.herdrTarget(w.Name))
	err := errors.New("no pane")
	if pane != "" {
		err = h.Prompt(ctx, pane, workerResumeText)
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if err == nil {
		log.Printf("worker %s: told it to carry on (%d/%d)", w.Name, w.Resumed, len(routerRetryWait))
		return
	}
	log.Printf("worker %s: carry on: %v", w.Name, err)
	switch {
	case !w.ResumeAt.IsZero() || w.Ended:
	case w.Resumed < len(routerRetryWait):
		w.Resumed++
		w.ResumeAt = time.Now().Add(routerRetryWait[0])
		sp.changed()
	default: // out of tries: the Router takes it from here
		sp.workerStoppedLocked(w, "Collie could not tell it to carry on: "+err.Error(), "", time.Now())
	}
}

// later is t, or now if t has passed: a reset read a little wrong still waits.
func later(t, now time.Time) time.Time {
	if t.Before(now) {
		return now
	}
	return t
}

var (
	resetUnixRe  = regexp.MustCompile(`\|(\d{10})\b`)
	resetInRe    = regexp.MustCompile(`(?i)try again in\s+(.+)`)
	resetDurRe   = regexp.MustCompile(`(?i)(\d+)\s*(days?|d|hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)\b`)
	resetAtRe    = regexp.MustCompile(`(?i)(?:resets?(?: at| on)?|try again at|continuing automatically at)\s+(.+)`)
	resetZoneRe  = regexp.MustCompile(`\(([A-Za-z]+(?:/[A-Za-z_]+)+|UTC|GMT)\)`)
	resetClockRe = regexp.MustCompile(`(?i)\b(\d{1,2})(?::(\d{2}))?\s*(am|pm)\b|\b(\d{1,2}):(\d{2})\b`)
	resetMDRe    = regexp.MustCompile(`(?i)\b(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?\b(?:,?\s+(\d{4})\b)?`)
	resetDMRe    = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?\s+(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?(?:,?\s+(\d{4})\b)?`)
	resetDayRe   = regexp.MustCompile(`(?i)\b(sun|mon|tue|wed|thu|fri|sat)[a-z]*\b`)
)

var resetMonths = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}

// resetTime reads when a usage limit resets from the line an agent stopped
// on: "resets 3:45pm (Asia/Shanghai)", "resets Mon 12:00am", "try again at
// Apr 13th, 2026 12:46 AM", "try again at 20 Aug 2026, 16:29", "try again in
// 2 hours 15 minutes", "…limit reached|1749470400". A time without a zone is
// the hub's local time.
func resetTime(line string, now time.Time) (time.Time, bool) {
	if m := resetUnixRe.FindStringSubmatch(line); m != nil {
		sec, _ := strconv.ParseInt(m[1], 10, 64)
		return time.Unix(sec, 0), true
	}
	if m := resetInRe.FindStringSubmatch(line); m != nil {
		var d time.Duration
		for _, p := range resetDurRe.FindAllStringSubmatch(m[1], -1) {
			v, _ := strconv.Atoi(p[1])
			unit := map[byte]time.Duration{'d': 24 * time.Hour, 'h': time.Hour, 'm': time.Minute, 's': time.Second}[strings.ToLower(p[2])[0]]
			d += time.Duration(v) * unit
		}
		return now.Add(d), d > 0
	}
	m := resetAtRe.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}, false
	}
	rest, loc := m[1], now.Location()
	if z := resetZoneRe.FindStringSubmatch(rest); z != nil {
		if l, err := time.LoadLocation(z[1]); err == nil {
			loc = l
		}
		rest = strings.Replace(rest, z[0], " ", 1)
	}
	c := resetClockRe.FindStringSubmatchIndex(rest)
	if c == nil {
		return time.Time{}, false
	}
	group := func(i int) string {
		if c[2*i] < 0 {
			return ""
		}
		return rest[c[2*i]:c[2*i+1]]
	}
	hh, mm, ampm := group(1), group(2), strings.ToLower(group(3))
	if hh == "" {
		hh, mm = group(4), group(5)
	}
	hour, _ := strconv.Atoi(hh)
	minute, _ := strconv.Atoi(mm)
	if ampm != "" {
		hour %= 12
		if ampm == "pm" {
			hour += 12
		}
	}
	if hour > 23 || minute > 59 {
		return time.Time{}, false
	}
	date := rest[:c[0]] + " " + rest[c[1]:] // the clock taken out
	local := now.In(loc)
	year, month, day := local.Date()
	explicit, hasYear := false, false
	if d := resetMDRe.FindStringSubmatch(date); d != nil {
		month, day, explicit = time.Month(slices.Index(resetMonths, strings.ToLower(d[1][:3]))+1), atoi(d[2]), true
		year, hasYear = cmpYear(d[3], year)
	} else if d := resetDMRe.FindStringSubmatch(date); d != nil {
		month, day, explicit = time.Month(slices.Index(resetMonths, strings.ToLower(d[2][:3]))+1), atoi(d[1]), true
		year, hasYear = cmpYear(d[3], year)
	}
	t := time.Date(year, month, day, hour, minute, 0, 0, loc)
	switch {
	case explicit:
		if !hasYear && t.Before(now.Add(-24*time.Hour)) {
			t = t.AddDate(1, 0, 0) // "Jan 2" read on Dec 31
		}
	case resetDayRe.MatchString(date): // "Mon 12:00am": the next one
		d := resetDayRe.FindStringSubmatch(date)
		t = t.AddDate(0, 0, int(weekdayNames[strings.ToLower(d[1][:3])]-t.Weekday()+7)%7)
		if !t.After(now) {
			t = t.AddDate(0, 0, 7)
		}
	case !t.After(now): // "3pm" said at 4pm: tomorrow's
		t = t.AddDate(0, 0, 1)
	}
	return t, true
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// cmpYear is the year written, or this year when none is.
func cmpYear(s string, this int) (int, bool) {
	if s == "" {
		return this, false
	}
	return atoi(s), true
}
