package collie

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestAskOf(t *testing.T) {
	m := askOf("AskUserQuestion", json.RawMessage(`{"questions":[{"question":"Which?","header":"H","options":[{"label":"A"},{"label":"B"}],"multiSelect":false}]}`))
	if m == nil || m.Kind != "ask" || m.Text != "Which?" || len(m.Options) != 2 {
		t.Fatalf("AskUserQuestion: %+v", m)
	}
	m = askOf("mcp__ccd_turn__wrap_up", json.RawMessage(`{"recap":"R","ask":"Go?","actions":[{"label":"Yes"},{"action":"commit","label":"x"}]}`))
	if m == nil || m.Name != "wrap_up" || m.Text != "Go?" || m.Input != "R" || len(m.Options) != 1 || m.Options[0] != "Yes" {
		t.Fatalf("wrap_up: %+v", m)
	}
	if m := askOf("mcp__ccd_turn__wrap_up", json.RawMessage(`{"recap":"R"}`)); m == nil || m.Name != "recap" || m.Input != "R" {
		t.Fatalf("recap only: %+v", m)
	}
}

func TestQueuedCommand(t *testing.T) {
	tr := newTranscript("", "claude")
	if !tr.claudeLine([]byte(`{"type":"attachment","attachment":{"type":"queued_command","prompt":"还有些消息看不到","origin":{"kind":"human"}}}`)) {
		t.Fatal("human queued command not taken")
	}
	tr.claudeLine([]byte(`{"type":"attachment","attachment":{"type":"queued_command","prompt":[{"type":"image","source":{}},{"type":"text","text":"看图"}],"origin":{"kind":"human"}}}`))
	if tr.claudeLine([]byte(`{"type":"attachment","attachment":{"type":"queued_command","prompt":"x","origin":{"kind":"task-notification"}}}`)) {
		t.Fatal("non-human queued command shown")
	}
	if len(tr.msgs) != 2 || tr.msgs[0].Text != "还有些消息看不到" || tr.msgs[1].Text != "[image]\n看图" {
		t.Fatalf("%+v %+v", tr.msgs[0], tr.msgs[len(tr.msgs)-1])
	}
}

// Real Codex 0.172 screens: a request_user_input question asked mid-turn,
// folded under "Queued follow-up inputs", then opened with ⇧←.
func TestCodexQueuedQuestion(t *testing.T) {
	queued, _ := os.ReadFile("testdata/codex-question-queued.txt")
	opened, _ := os.ReadFile("testdata/codex-question-expanded.txt")
	if !codexQueuedAsk(string(queued)) || codexQueuedAsk(string(opened)) {
		t.Fatal("folded question not told from the opened one")
	}
	if screenAsks(string(queued)) {
		t.Error("the folded question has no choices to show")
	}
	a := parseApproval(string(opened))
	var labels []string
	for _, c := range a.Choices {
		labels = append(labels, c.Label)
	}
	if !screenAsks(string(opened)) || strings.TrimSpace(a.Question) != "Which color do you prefer?" || strings.Join(labels, ",") != "Red,Green,Blue,Other" {
		t.Fatalf("opened: %q %v", a.Question, labels)
	}
	if k := a.Choices[2].Keys; strings.Join(k, " ") != "down down enter" {
		t.Errorf("Blue: %v", k)
	}

	f := newFakeHerdr(t)
	sp := newRouterTestSuper(t, f)
	pane := f.add("w1", "cx", "codex-1", "codex", "/")
	f.panes[pane].screen, f.panes[pane].unfolded = string(queued), string(opened)
	got, err := sp.s.herdr.ReadAsk(context.Background(), pane)
	if err != nil || got != string(opened) || f.count("pane.send_keys") != 1 {
		t.Fatalf("ReadAsk did not open it: %v, %d key sends", err, f.count("pane.send_keys"))
	}
	again, err := sp.s.herdr.ReadAsk(context.Background(), pane)
	if err != nil {
		t.Fatal(err)
	}
	if a := parseApproval(again); !strings.Contains(choicesText(&a), "4. Other") {
		t.Error("the router isn't told the choices")
	}
	if f.count("pane.send_keys") != 1 {
		t.Error("an opened question is not pressed again")
	}
}

// Codex's update prompt at launch. Real 0.167 screens: npm and standalone
// installs, wide and narrow. The standalone install's option 1 spells out the
// whole install command, longer than a line of prose in a list.
func TestCodexUpdatePrompt(t *testing.T) {
	screens := map[string]string{}
	for _, name := range []string{"codex-update-npm.txt", "codex-update-narrow.txt", "codex-update-curl.txt"} {
		screen, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		screens[name] = string(screen)
		a := parseApproval(string(screen))
		var labels []string
		for _, c := range a.Choices {
			labels = append(labels, c.Text+"."+strings.SplitN(c.Label, " ", 2)[0])
		}
		if !screenAsks(string(screen)) || strings.Join(labels, ",") != "1.Update,2.Skip,3.Skip" || !a.Choices[0].Current ||
			!strings.HasPrefix(strings.TrimSpace(a.Question), "Update available") {
			t.Errorf("%s: %q %v", name, a.Question, labels)
		}
	}
	a := parseApproval(screens["codex-update-curl.txt"])
	if want := "Update now (runs `sh -c 's=$(curl -fsSL https://chatgpt.com/codex/install.sh) && CODEX_NON_INTERACTIVE=1 eval \"$s\"'`)"; len(a.Choices) == 0 || a.Choices[0].Label != want {
		t.Errorf("option 1: %+v", a.Choices)
	}
}
