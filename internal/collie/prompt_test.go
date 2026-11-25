package collie

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func simAgent(t *testing.T) (*fakeHerdr, *Super, *fakePane) {
	t.Helper()
	wait := promptConfirmWait
	promptConfirmWait = time.Second
	t.Cleanup(func() { promptConfirmWait = wait })
	f := newFakeHerdr(t)
	sp := newRouterTestSuper(t, f)
	f.add("wA", "w", "worker-1", "claude", "/")
	p := f.panes["wA:p1"]
	p.sim = true
	return f, sp, p
}

func promptOnce(t *testing.T, sp *Super, text string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return sp.s.herdr.Prompt(ctx, "worker-1", text)
}

const brief = "[collie] 1 new event\n\n#t7 · chat:main · system\n  You are worker \"worker-1\"\n  do the thing"

func TestPromptTakenFirstTime(t *testing.T) {
	f, sp, p := simAgent(t)
	if err := promptOnce(t, sp, brief); err != nil {
		t.Fatal(err)
	}
	if len(p.said) != 1 || f.count("agent.prompt") != 1 || f.count("pane.send_text") != 0 {
		t.Errorf("said %d times, %d prompts, %d pastes", len(p.said), f.count("agent.prompt"), f.count("pane.send_text"))
	}
}

// The paste arrived but its Enter was lost: Enter again, never the text.
func TestPromptPressesEnterForTextLeftInTheBox(t *testing.T) {
	f, sp, p := simAgent(t)
	p.dropEnters = 1
	if err := promptOnce(t, sp, brief); err != nil {
		t.Fatal(err)
	}
	if len(p.said) != 1 || p.said[0] != brief || f.count("pane.send_text") != 0 {
		t.Errorf("said %q with %d pastes", p.said, f.count("pane.send_text"))
	}
}

// herdr said ok but nothing reached the agent (it wasn't reading yet): sent
// once more.
func TestPromptResendsALostMessageOnce(t *testing.T) {
	f, sp, p := simAgent(t)
	p.dropPrompts = 1
	if err := promptOnce(t, sp, brief); err != nil {
		t.Fatal(err)
	}
	if len(p.said) != 1 || f.count("pane.send_text") != 1 {
		t.Errorf("said %d times with %d pastes", len(p.said), f.count("pane.send_text"))
	}
}

// Taken without a change of state (a slash command, an agent herdr can't
// track): the words on screen show it went in, so it isn't sent again.
func TestPromptSeesQuietTakeOnScreen(t *testing.T) {
	f, sp, p := simAgent(t)
	p.quiet = true
	if err := promptOnce(t, sp, "/model gpt-5.5"); err != nil {
		t.Fatal(err)
	}
	if len(p.said) != 1 || f.count("pane.send_text") != 0 {
		t.Errorf("said %d times with %d pastes", len(p.said), f.count("pane.send_text"))
	}
}

// Every batch starts with the same header: the words to look for are the
// first ones not on screen yet, or an old batch passes for the new one.
func TestPromptProbeSkipsWordsAlreadyOnScreen(t *testing.T) {
	f, sp, p := simAgent(t)
	p.quiet, p.said = true, []string{"[collie] 1 new event\n\n#t5 · old"}
	if err := promptOnce(t, sp, "[collie] 1 new event\n\n#t6 · new"); err != nil {
		t.Fatal(err)
	}
	if len(p.said) != 2 || f.count("pane.send_text") != 0 {
		t.Errorf("said %d times with %d pastes", len(p.said), f.count("pane.send_text"))
	}
	if got := promptProbe("[collie] 1 new event\n\n#t6 · new", "❯ [collie] 1 new event"); got != "#t6 · new" {
		t.Errorf("probe = %q", got)
	}
}

// The node dropped after typing it: what shows on screen settles it, and
// the caller is not told to send it again.
func TestPromptTransportErrorButTaken(t *testing.T) {
	f, sp, p := simAgent(t)
	p.hangUp = true
	if err := promptOnce(t, sp, brief); err != nil {
		t.Fatalf("err = %v, want it confirmed from the screen", err)
	}
	if len(p.said) != 1 || f.count("pane.send_text") != 0 {
		t.Errorf("said %d times with %d pastes", len(p.said), f.count("pane.send_text"))
	}
}

// Mid-turn nothing tells whether a queued message went in: no resend, and
// no error either.
func TestPromptMidTurnIsNotResent(t *testing.T) {
	f, sp, p := simAgent(t)
	p.status, p.dropPrompts = "working", 1
	if err := promptOnce(t, sp, brief); err != nil {
		t.Fatal(err)
	}
	if f.count("pane.send_text") != 0 {
		t.Errorf("resent mid-turn: %d pastes", f.count("pane.send_text"))
	}
}

func TestPromptGivesUpWhenNothingTakes(t *testing.T) {
	f, sp, p := simAgent(t)
	p.dropPrompts, p.dropEnters = 1, 10
	err := promptOnce(t, sp, brief)
	var he *HerdrError
	if !errors.As(err, &he) || he.Code != "prompt_unconfirmed" {
		t.Fatalf("err = %v, want prompt_unconfirmed", err)
	}
	if len(p.said) != 0 || f.count("pane.send_text") != 1 {
		t.Errorf("said %d times with %d pastes, want one resend", len(p.said), f.count("pane.send_text"))
	}
}

// A delivery that can't be confirmed stops (no second copy next tick) and
// shows in the chat and the inbox.
func TestDeliverShowsUnconfirmedMessage(t *testing.T) {
	_, sp, p := simAgent(t)
	p.dropPrompts, p.dropEnters = 1, 10
	sp.mu.Lock()
	tr := sp.ingestLocked("router", mainThread, routerName, "look again", "", "worker-1", "")
	sp.mu.Unlock()
	sp.deliver(context.Background(), "worker-1", []*Trigger{tr})
	if tr.Status != "failed" || len(tr.Targets) != 0 {
		t.Errorf("trigger %s → %v, want failed", tr.Status, tr.Targets)
	}
	if ev := chatEvents(sp); !strings.Contains(ev, "没能把消息送进 worker-1 的终端（#"+tr.ID+"）") {
		t.Errorf("no error in the chat:\n%s", ev)
	}
}

// The tick lists every computer, which can take seconds; a worker spawned
// or done starting meanwhile isn't in that list. It must not be taken for
// gone, and its brief must wait for it rather than go to the Router.
func TestTickKeepsWorkerThatStartedDuringTheList(t *testing.T) {
	for _, spawnedDuring := range []bool{false, true} {
		f, sp, _ := simAgent(t)
		delete(f.panes, "wA:p1")
		w := &Worker{Name: "worker-1", Kind: "claude", Thread: mainThread, CreatedAt: time.Now(), Status: "starting"}
		// A stopped worker of the same name, which a resume replaces.
		old := &Worker{Name: w.Name, Kind: "claude", Thread: mainThread, Status: "gone", Ended: true}
		sp.mu.Lock()
		sp.st.Workers[w.Name] = w
		if spawnedDuring {
			sp.st.Workers[w.Name] = old
		}
		sp.mu.Unlock()
		var brief *Trigger
		f.afterList = func() {
			f.afterList = nil
			// What Spawn does once the agent is up, while the tick still waits.
			f.add("wA", "w", "worker-1", "claude", "/")
			f.panes["wA:p1"].sim = true
			sp.mu.Lock()
			sp.st.Workers[w.Name] = w
			w.Status, w.PaneID = "idle", "wA:p1"
			brief = sp.ingestLocked("system", mainThread, routerName, "You are worker", "", "worker-1", "worker-1")
			sp.mu.Unlock()
		}
		sp.tick(context.Background())
		if w.Ended || w.Status == "gone" {
			t.Fatalf("spawned during the list %v: worker taken for gone", spawnedDuring)
		}
		if len(brief.Targets) != 1 || brief.Targets[0] != "worker-1" {
			t.Fatalf("spawned during the list %v: brief retargeted to %v", spawnedDuring, brief.Targets)
		}
		sp.tick(context.Background())
		if said := f.panes["wA:p1"].said; brief.Status != "delivered" || len(said) != 1 || !strings.Contains(said[0], "You are worker") {
			t.Errorf("spawned during the list %v: brief %s, worker got %q", spawnedDuring, brief.Status, said)
		}
		if strings.Contains(chatEvents(sp), "已退出") {
			t.Errorf("spawned during the list %v: chat says the worker exited:\n%s", spawnedDuring, chatEvents(sp))
		}
	}
}

// A computer that hangs (half-dropped, its node stuck) used to eat the whole
// tick: the list waited out the tick's time and every delivery then failed
// on it. Now it is left out of the list, the rest is delivered at once, and
// the workers on it keep their events until it answers.
func TestTickDeliversPastAHangingComputer(t *testing.T) {
	_, sp, p := simAgent(t)
	hang := func(context.Context) (net.Conn, error) {
		c, _ := net.Pipe() // nobody answers
		return c, nil
	}
	h := sp.s.herdr
	h.mu.Lock()
	h.remote["slow"] = &Machine{ID: "slow", Name: "slow", Online: true, conn: &herdrConn{dial: hang}}
	h.mu.Unlock()
	sp.mu.Lock()
	sp.st.Workers["worker-1"] = &Worker{Name: "worker-1", Kind: "claude", Thread: mainThread, Status: "idle", PaneID: "wA:p1"}
	far := &Worker{Name: "far", Kind: "claude", Machine: "slow", Thread: mainThread, Status: "idle", PaneID: "wB:p1@slow"}
	sp.st.Workers[far.Name] = far
	near := sp.ingestLocked("chat", mainThread, "you", "hello near", "", "worker-1", "")
	toFar := sp.ingestLocked("chat", mainThread, "you", "hello far", "", "far", "")
	sp.mu.Unlock()
	start := time.Now()
	sp.tick(context.Background())
	if d := time.Since(start); d > tickListWait+3*time.Second {
		t.Errorf("tick took %v", d)
	}
	if near.Status != "delivered" || len(p.said) != 1 {
		t.Errorf("near: %s, said %q", near.Status, p.said)
	}
	if far.Ended || toFar.Status != "queued" || len(toFar.Targets) != 1 || toFar.Targets[0] != "far" {
		t.Errorf("far worker ended=%v, its message %s → %v", far.Ended, toFar.Status, toFar.Targets)
	}
}
