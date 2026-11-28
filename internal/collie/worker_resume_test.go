package collie

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestClassifyStop(t *testing.T) {
	cases := []struct {
		line string
		want agentStop
	}{
		{"API Error: Connection dropped unexpectedly", stopBlip},
		{"API Error: 500 Internal Server Error", stopBlip},
		{"API Error: Overloaded", stopBlip},
		{"request failed: read ECONNRESET", stopBlip},
		{"connect ECONNREFUSED 127.0.0.1:8080", stopBlip},
		{"stream disconnected before completion: error sending request", stopBlip},
		{"unexpected status 503 Service Unavailable", stopBlip},
		{"API Error: 429 Too Many Requests", stopQuota},
		{"You've hit your session limit · resets 3:45pm", stopQuota},
		{"You've hit your usage limit. Upgrade to Pro or try again at 3:05 PM.", stopQuota},
		{"Claude AI usage limit reached|1749470400", stopQuota},
		{"API Error: 401 {\"type\":\"authentication_error\"}", stopHandOff},
		{"Invalid API key · Please run /login", stopHandOff},
		{"Your credit balance is too low to access the Anthropic API", stopHandOff},
		{"API Error: 403 Request not allowed", stopHandOff},
	}
	for _, c := range cases {
		if !workerStopRe.MatchString(c.line) {
			t.Errorf("%q: not seen as an agent stopping", c.line)
		}
		if got := classifyStop(c.line); got != c.want {
			t.Errorf("%q: got %d, want %d", c.line, got, c.want)
		}
	}
}

func TestResetTime(t *testing.T) {
	sh, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip(err)
	}
	now := time.Date(2026, 11, 26, 14, 0, 0, 0, sh) // a Thursday
	at := func(mo time.Month, d, h, m int) time.Time { return time.Date(2026, mo, d, h, m, 0, 0, sh) }
	cases := []struct {
		line string
		want time.Time
	}{
		{"You've hit your session limit · resets 3:45pm", at(11, 26, 15, 45)},
		{"5-hour limit reached ∙ resets 3pm (Asia/Shanghai)", at(11, 26, 15, 0)},
		{"Claude usage limit reached. Your limit will reset at 1pm (Asia/Shanghai).", at(11, 27, 13, 0)}, // passed today
		{"You've hit your weekly limit · resets Mon 12:00am", at(11, 30, 0, 0)},
		{"try again at Apr 13th, 2027 12:46 AM", time.Date(2027, 4, 13, 0, 46, 0, 0, sh)},
		{"Upgrade your plan or add credits to continue, or try again at 28 Nov 2026, 16:29.", at(11, 28, 16, 29)},
		{"You've hit your usage limit. try again in 2 hours 15 minutes.", now.Add(2*time.Hour + 15*time.Minute)},
		{"Claude AI usage limit reached|1764140400", time.Unix(1764140400, 0)},
		{"resets 9am (UTC)", time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)}, // 17:00 in Shanghai: still today
		{"resets 5am (UTC)", time.Date(2026, 11, 27, 5, 0, 0, 0, time.UTC)}, // 13:00 in Shanghai: tomorrow's
	}
	for _, c := range cases {
		got, ok := resetTime(c.line, now)
		if !ok || !got.Equal(c.want) {
			t.Errorf("%q: got %v %v, want %v", c.line, got, ok, c.want)
		}
	}
	for _, line := range []string{"API Error: 429 Too Many Requests", "usage limit reached, try again later"} {
		if got, ok := resetTime(line, now); ok {
			t.Errorf("%q: no time in it, got %v", line, got)
		}
	}
}

func TestAgentStopLineAfterResume(t *testing.T) {
	screen := "⎿  API Error: Connection dropped\n> " + workerResumeText + "\n⏺ 接着把测试补完了\n> "
	if got := agentStopLine(screen, "接着把测试补完了"); got != "" {
		t.Errorf("the error before Collie's carry-on was handled already: %q", got)
	}
	screen += "\n⎿  API Error: Overloaded\n"
	if got := agentStopLine(screen, ""); got != "API Error: Overloaded" {
		t.Errorf("a new error after it: %q", got)
	}
}

func resumeTestSuper() (*Super, *Worker) {
	w := &Worker{Name: "w1", Kind: "claude", Thread: mainThread, Status: "idle", PaneID: "wA:p1"}
	sp := &Super{st: &State{Workers: map[string]*Worker{"w1": w}, Threads: map[string]*Thread{}}, subs: map[chan struct{}]bool{}}
	return sp, w
}

// routerHeard is what Collie queued for the Router about w1.
func routerHeard(sp *Super) []string {
	var out []string
	for _, t := range sp.st.Triggers {
		if t.Source == "worker" && t.From == "w1" {
			out = append(out, t.Text)
		}
	}
	return out
}

func TestWorkerStoppedBacksOff(t *testing.T) {
	sp, w := resumeTestSuper()
	now := time.Now()
	for i, wait := range routerRetryWait {
		sp.workerStoppedLocked(w, "API Error: Connection dropped", "", now)
		if !w.ResumeAt.Equal(now.Add(wait)) || w.Resumed != i+1 {
			t.Fatalf("stop %d: plan %v resumed %d, want in %v", i+1, w.ResumeAt, w.Resumed, wait)
		}
	}
	if got := routerHeard(sp); len(got) != 0 {
		t.Fatalf("the Router is bothered while Collie retries: %q", got)
	}
	if ev := chatEvents(sp); !strings.Contains(ev, "20 秒后自动继续（第 1/3 次）") || !strings.Contains(ev, "3 分钟后自动继续（第 3/3 次）") {
		t.Errorf("chat events: %s", ev)
	}
	sp.workerStoppedLocked(w, "API Error: Connection dropped", "half done", now) // a fourth time
	got := routerHeard(sp)
	if len(got) != 1 || !strings.Contains(got[0], "carry on 3 times") || !strings.Contains(got[0], "hand the job to another kind of agent") {
		t.Fatalf("after three tries the Router hears: %q", got)
	}
	if !w.ResumeAt.IsZero() || w.Resumed != 0 {
		t.Errorf("handed off, yet still planned: %v %d", w.ResumeAt, w.Resumed)
	}
}

func TestWorkerStoppedOnQuotaWaitsForReset(t *testing.T) {
	sp, w := resumeTestSuper()
	now := time.Now()
	sp.workerStoppedLocked(w, "You've hit your usage limit. try again in 2 hours.", "", now)
	if !w.ResumeAt.Equal(now.Add(2*time.Hour + resumeAfterReset)) {
		t.Fatalf("plan: %v", w.ResumeAt)
	}
	got := routerHeard(sp)
	if len(got) != 1 || !strings.Contains(got[0], "after the limit resets (auto-resume 1 of 3)") || !strings.Contains(got[0], "collie stop w1") {
		t.Fatalf("the Router hears: %q", got)
	}
	// No time to read: retried like a blip, the Router not bothered.
	sp2, w2 := resumeTestSuper()
	sp2.workerStoppedLocked(w2, "API Error: 429 Too Many Requests", "", now)
	if !w2.ResumeAt.Equal(now.Add(routerRetryWait[0])) || len(routerHeard(sp2)) != 0 {
		t.Fatalf("429 without a reset: %v %q", w2.ResumeAt, routerHeard(sp2))
	}
}

func TestWorkerStoppedOnLoginHandsOff(t *testing.T) {
	sp, w := resumeTestSuper()
	sp.workerStoppedLocked(w, "Invalid API key · Please run /login", "", time.Now())
	if !w.ResumeAt.IsZero() {
		t.Fatalf("a login doesn't pass by itself: %v", w.ResumeAt)
	}
	if got := routerHeard(sp); len(got) != 1 || !strings.Contains(got[0], "stopped on an error from its agent") {
		t.Fatalf("the Router hears: %q", got)
	}
}

func TestResumeDue(t *testing.T) {
	sp, w := resumeTestSuper()
	now := time.Now()
	if sp.resumeDueLocked(w, "idle", now) {
		t.Fatal("nothing planned")
	}
	w.ResumeAt = now.Add(time.Minute)
	if sp.resumeDueLocked(w, "idle", now) {
		t.Fatal("not yet")
	}
	w.ResumeAt = now.Add(-time.Second)
	for _, status := range []string{"working", "blocked", "unknown"} {
		if sp.resumeDueLocked(w, status, now) {
			t.Errorf("%s: only an idle worker is told to carry on", status)
		}
	}
	if !sp.resumeDueLocked(w, "done", now) {
		t.Fatal("due")
	}
	sp.Ingest("router", mainThread, routerName, "do this instead", "", "w1", "")
	if sp.resumeDueLocked(w, "idle", now) {
		t.Error("a new message for it carries it on instead")
	}
	sp.st.Triggers = nil
	w.Ended, w.Status = true, "gone" // stopped by the human or the Router
	if sp.resumeDueLocked(w, "idle", now) {
		t.Error("an ended worker is left alone")
	}
}

// Through the tick: the turn ends on an API error, Collie waits, tells the
// worker to carry on once, and the Router hears it with the finished turn.
func TestTickResumesWorkerOnce(t *testing.T) {
	_, sp, p := simAgent(t)
	sp.mu.Lock()
	sp.st.Settings.Paused = false
	sp.st.Workers["worker-1"] = &Worker{Name: "worker-1", Kind: "claude", Thread: mainThread, Status: "working", PaneID: "wA:p1", CreatedAt: time.Now().Add(-time.Hour)}
	sp.mu.Unlock()
	p.said = []string{"⎿  API Error: Connection dropped (ECONNRESET)"}
	p.detection = "⏺ 正在改代码" // its last message, without the error
	ctx := context.Background()
	sp.tick(ctx) // working → idle, on the error
	w := sp.st.Workers["worker-1"]
	w.Background.clearAt = time.Now().Add(-backgroundFooterGrace)
	sp.tick(ctx) // confirm the footer before settling the turn
	sp.mu.Lock()
	planned := !w.ResumeAt.IsZero()
	sp.mu.Unlock()
	if !planned {
		t.Fatalf("no carry-on planned: %+v", w)
	}
	sp.tick(ctx)
	if len(p.said) != 1 {
		t.Fatalf("told to carry on before it was due: %q", p.said)
	}
	sp.mu.Lock()
	w.ResumeAt = time.Now().Add(-time.Second)
	sp.mu.Unlock()
	sp.tick(ctx)
	if len(p.said) != 2 || p.said[1] != workerResumeText {
		t.Fatalf("said: %q", p.said)
	}
	sp.tick(ctx) // it works on: nothing more is sent
	if len(p.said) != 2 {
		t.Fatalf("told twice: %q", p.said)
	}
	p.status = "" // its turn ends, no error after the carry-on
	sp.tick(ctx)
	w.Background.clearAt = time.Now().Add(-backgroundFooterGrace)
	sp.tick(ctx)
	sp.mu.Lock()
	defer sp.mu.Unlock()
	var heard string
	for _, tr := range sp.st.Triggers {
		if tr.Source == "worker" && tr.From == "worker-1" {
			heard += tr.Text
		}
	}
	if !strings.Contains(heard, "finished a turn. (Collie auto-resumed it 1 time(s)") {
		t.Errorf("the Router hears: %q", heard)
	}
	if w.Resumed != 0 || !w.ResumeAt.IsZero() {
		t.Errorf("the count starts again: %d %v", w.Resumed, w.ResumeAt)
	}
}

// A carry-on that can't be sent counts as a try; out of tries, the Router hears.
func TestResumeSendFails(t *testing.T) {
	_, sp, _ := simAgent(t)
	w := &Worker{Name: "w1", Kind: "claude", Thread: mainThread, Status: "idle", Resumed: 1}
	sp.mu.Lock()
	sp.st.Workers["w1"] = w
	sp.mu.Unlock()
	sp.resumeWorker(context.Background(), w) // no pane named w1
	if w.Resumed != 2 || w.ResumeAt.IsZero() {
		t.Fatalf("after a failed send: %d %v", w.Resumed, w.ResumeAt)
	}
	w.ResumeAt, w.Resumed = time.Time{}, len(routerRetryWait)
	sp.resumeWorker(context.Background(), w)
	if got := routerHeard(sp); len(got) != 1 || !strings.Contains(got[0], "could not tell it to carry on") {
		t.Fatalf("the Router hears: %q", got)
	}
	w.Ended = true
	w.clearResume()
	sp.resumeWorker(context.Background(), w)
	if !w.ResumeAt.IsZero() || len(routerHeard(sp)) != 1 {
		t.Error("an ended worker is left alone")
	}
}
