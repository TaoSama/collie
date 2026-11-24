package collie

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The Router is in the middle of a turn (status working) while the tick
// can't see it. Whether the launch's second look finds it or herdr
// refuses the name, it is kept, nothing new runs and the chat stays quiet.
func TestEnsureRouterKeepsWorkingRouter(t *testing.T) {
	for _, hidden := range []int{0, 2} {
		f := newFakeHerdr(t)
		sp := newRouterTestSuper(t, f)
		old := f.add("wA", routerLabel, routerName, "claude", sp.routerCwd(""))
		f.panes[old].status = "working"
		f.hidden = hidden
		launchRouter(t, sp)
		if len(f.panes) != 1 || f.panes[old] == nil {
			t.Errorf("hidden=%d: panes left = %v, want only the working Router %s", hidden, f.panes, old)
		}
		if ev := chatEvents(sp); ev != "" {
			t.Errorf("hidden=%d: chat got %q, want nothing", hidden, ev)
		}
	}
}

// The Router's computer is offline (a node update, a network blip): Collie
// keeps retrying quietly and tells the human once, after a few failures.
func TestEnsureRouterTellsOnceAfterRetries(t *testing.T) {
	f := newFakeHerdr(t)
	sp := newRouterTestSuper(t, f)
	sp.st.Settings.RouterMachine = "macpro" // not connected
	for i := 1; i <= routerFailsToTell+2; i++ {
		sp.mu.Lock()
		sp.routerLaunchAt = time.Time{}
		sp.mu.Unlock()
		launchRouter(t, sp)
		ev := chatEvents(sp)
		n := strings.Count(ev, "\n")
		switch {
		case i < routerFailsToTell && n != 0:
			t.Fatalf("failure %d: chat got %q, want nothing yet", i, ev)
		case i >= routerFailsToTell && n != 1:
			t.Fatalf("failure %d: chat got %q, want one notice", i, ev)
		case i == routerFailsToTell && !strings.Contains(ev, "macpro 一直没连上"):
			t.Errorf("notice = %q, want it to say macpro is offline", ev)
		}
	}
	// It comes back: the next run of failures is counted afresh.
	sp.st.Settings.RouterMachine = ""
	sp.mu.Lock()
	sp.routerLaunchAt = time.Time{}
	sp.mu.Unlock()
	launchRouter(t, sp)
	sp.mu.Lock()
	fails, told := sp.routerFails, sp.routerFailTold
	sp.mu.Unlock()
	if fails != 0 || told {
		t.Errorf("after a launch: fails=%d told=%t, want 0 false", fails, told)
	}
}

func spawnTest(t *testing.T, sp *Super, name string) (*Worker, error) {
	t.Helper()
	w, started, err := sp.Spawn(SpawnSpec{Kind: "claude", Cwd: t.TempDir(), Task: "do it", Name: name, Alias: new(string)})
	if err != nil {
		return nil, err
	}
	select {
	case err = <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("spawn did not finish")
	}
	return w, err
}

func errorEntries(sp *Super) []string {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	var out []string
	for _, e := range sp.st.Entries {
		if e.Tone == "error" {
			out = append(out, e.Text)
		}
	}
	return out
}

// A name another herdr agent has (one of the human's sessions) goes to a
// spare one; the spawn says which.
func TestSpawnTakesSpareName(t *testing.T) {
	f := newFakeHerdr(t)
	sp := newRouterTestSuper(t, f)
	f.add("wH", "mine", "fix-bug", "claude", "/work")
	f.add("wJ", "x", "fix-bug-2", "codex", "/work")
	sp.liveAgents(context.Background()) // the tick's look
	w, err := spawnTest(t, sp, "fix-bug")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if w.Name != "fix-bug-3" {
		t.Errorf("worker = %q, want fix-bug-3", w.Name)
	}
	if f.panes[w.PaneID] == nil || f.panes[w.PaneID].name != "fix-bug-3" {
		t.Errorf("herdr pane %s = %+v, want named fix-bug-3", w.PaneID, f.panes[w.PaneID])
	}
	// Generated names skip herdr's too.
	f.add("wK", "y", "claude-1", "claude", "/work")
	sp.liveAgents(context.Background())
	if w, err := spawnTest(t, sp, ""); err != nil || w.Name != "claude-2" {
		t.Errorf("generated worker = %v, %v; want claude-2", w, err)
	}
	if e := errorEntries(sp); len(e) != 0 {
		t.Errorf("chat errors = %q, want none", e)
	}
}

// A running worker's name is the Router's to sort out: the error says how.
func TestSpawnRunningWorkerSuggests(t *testing.T) {
	f := newFakeHerdr(t)
	sp := newRouterTestSuper(t, f)
	if _, err := spawnTest(t, sp, "job"); err != nil {
		t.Fatal(err)
	}
	_, err := spawnTest(t, sp, "job")
	if err == nil || !strings.Contains(err.Error(), "collie send job") || !strings.Contains(err.Error(), "job-2") {
		t.Errorf("err = %v, want a hint to send or use job-2", err)
	}
}

// The name went between the look and the start: the Router hears it with a
// name to use, the human's chat gets no error.
func TestSpawnStartFailureGoesToRouter(t *testing.T) {
	f := newFakeHerdr(t)
	sp := newRouterTestSuper(t, f)
	f.add("wH", "mine", "job", "claude", "/work")
	// No tick has listed it yet.
	_, err := spawnTest(t, sp, "job")
	if !nameTaken(err) || !strings.Contains(err.Error(), "job-2") {
		t.Errorf("err = %v, want agent_name_taken with job-2 suggested", err)
	}
	if e := errorEntries(sp); len(e) != 0 {
		t.Errorf("chat errors = %q, want none", e)
	}
}
