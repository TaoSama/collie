package collie

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testDialog = "╭────╮\n│ Bash command\n│   git push origin main\n│ Do you want to proceed?\n│ ❯ 1. Yes\n│   2. No, and tell Claude what to do differently (esc)\n╰────╯"

// webhookSink records what the hub posts to its notification webhook.
func webhookSink(t *testing.T) (url string, got func() []string) {
	t.Helper()
	ch := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- string(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		var out []string
		for {
			select {
			case s := <-ch:
				out = append(out, s)
			default:
				return out
			}
		}
	}
}

func decisionTestSuper(t *testing.T) (*Super, *fakeHerdr, *Worker, func() []string) {
	t.Helper()
	f := newFakeHerdr(t)
	sp := newRouterTestSuper(t, f)
	pane := f.add("w1", "readme", "collie-readme", "claude", "/")
	f.panes[pane].screen, f.panes[pane].status = testDialog, "blocked"
	hook, got := webhookSink(t)
	sp.s.publicURLs = []string{"https://c.example"}
	sp.st.Settings.NotifyWebhook = hook
	sp.seenAt = time.Now().Add(-5 * time.Minute)
	w := &Worker{Name: "collie-readme", Kind: "claude", Thread: mainThread, PaneID: pane, Status: "blocked"}
	sp.st.Workers[w.Name] = w
	return sp, f, w, got
}

func decide(t *testing.T, sp *Super, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	sp.s.superDecide(rec, httptest.NewRequest("POST", "/api/super/decide", bytes.NewReader(b)))
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func decisionOf(t *testing.T, sp *Super, q string) Decision {
	t.Helper()
	rec := httptest.NewRecorder()
	sp.s.superDecision(rec, httptest.NewRequest("GET", "/api/super/decision?"+q, nil))
	var d Decision
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &d) != nil {
		t.Fatalf("decision %s: %d %s", q, rec.Code, rec.Body)
	}
	return d
}

func TestDialogDecisionNotifiesOnceAndLinks(t *testing.T) {
	sp, _, w, got := decisionTestSuper(t)
	ctx := context.Background()
	sp.workerSettled(ctx, w, true)
	e := sp.st.Entries[len(sp.st.Entries)-1]
	if e.Ask == nil || e.Ask.Pane != w.PaneID || len(e.Ask.Choices) != 2 {
		t.Fatalf("no dialog decision recorded: %+v", e)
	}
	sp.sendNotifications()
	sent := got()
	if len(sent) != 1 {
		t.Fatalf("want one webhook post, got %d", len(sent))
	}
	var body struct {
		Text string `json:"text"`
		Link string `json:"link"`
	}
	json.Unmarshal([]byte(sent[0]), &body)
	if want := "https://c.example/#/d/" + e.ID; body.Link != want || !strings.Contains(body.Text, want) {
		t.Errorf("link = %q, text %q; want %s", body.Link, body.Text, want)
	}
	if !strings.Contains(body.Text, "collie-readme 需要你选：Yes / No, and tell Claude what") {
		t.Errorf("text doesn't say whose it is and what it needs: %q", body.Text)
	}
	if strings.Contains(body.Text, "git push") || strings.Contains(body.Text, "Bash command") {
		t.Errorf("the screen leaked into the notification: %q", body.Text)
	}

	// The same dialog showing again is the same decision: no new line, no second push.
	n := len(sp.st.Entries)
	sp.mu.Lock()
	sp.settleAskLocked(w.PaneID, "", "") // a status flicker
	sp.mu.Unlock()
	sp.workerSettled(ctx, w, true)
	sp.notifyAt = time.Time{}
	sp.sendNotifications()
	if len(sp.st.Entries) != n || len(got()) != 0 {
		t.Fatalf("pushed the same decision again (%d → %d entries)", n, len(sp.st.Entries))
	}
	if d := decisionOf(t, sp, "id="+e.ID); d.Status != "open" || d.Kind != "dialog" || d.Task != "collie-readme" {
		t.Fatalf("reopened decision: %+v", d)
	}
}

func TestDecideDialogFromNotification(t *testing.T) {
	sp, f, w, _ := decisionTestSuper(t)
	sp.workerSettled(context.Background(), w, true)
	id := sp.st.Entries[len(sp.st.Entries)-1].ID

	if code, _ := decide(t, sp, map[string]any{"id": id, "choice": "Maybe"}); code != 409 {
		t.Errorf("a choice the dialog doesn't have: %d", code)
	}
	// Another dialog took its place: the button for the old one must not answer it.
	f.panes[w.PaneID].screen = strings.Replace(testDialog, "git push origin main", "rm -rf build", 1)
	f.panes[w.PaneID].screen = strings.Replace(f.panes[w.PaneID].screen, "Bash command", "Bash command (destructive)", 1)
	if code, _ := decide(t, sp, map[string]any{"id": id, "choice": "Yes"}); code != 409 {
		t.Errorf("answered a different dialog: %d", code)
	}
	f.panes[w.PaneID].screen = testDialog
	if code, out := decide(t, sp, map[string]any{"id": id, "choice": "Yes"}); code != 200 || out["picked"] != "Yes" {
		t.Fatalf("decide = %d %v", code, out)
	}
	d := decisionOf(t, sp, "id="+id)
	if d.Status != "done" || d.Answer == nil || d.Answer.Choice != "Yes" || d.Answer.By != "你" {
		t.Fatalf("after deciding: %+v %+v", d, d.Answer)
	}
	if d2 := decisionOf(t, sp, "pane="+w.PaneID); d2.ID != id {
		t.Errorf("by pane: %+v", d2)
	}
	if code, _ := decide(t, sp, map[string]any{"id": id, "choice": "Yes"}); code != 409 {
		t.Errorf("a settled decision answered twice: %d", code)
	}
}

func TestDecisionSettledBeforeSendingIsDropped(t *testing.T) {
	sp, _, w, got := decisionTestSuper(t)
	sp.notifyAt = time.Now() // the last batch just went: this one waits
	sp.workerSettled(context.Background(), w, true)
	sp.mu.Lock()
	sp.settleAskLocked(w.PaneID, "Yes", "Collie") // collie answer, within the 20 s
	sp.mu.Unlock()
	sp.notifyAt = time.Time{}
	sp.sendNotifications()
	if s := got(); len(s) != 0 {
		t.Fatalf("pushed a decision Collie already answered: %v", s)
	}
	d := decisionOf(t, sp, "pane="+w.PaneID)
	if d.Status != "done" || d.Answer.By != "Collie" || d.Answer.Choice != "Yes" {
		t.Fatalf("%+v %+v", d, d.Answer)
	}
	// The page then says what it clicked: an answer already known stays.
	sp.mu.Lock()
	sp.settleAskLocked(w.PaneID, "No", "你")
	sp.mu.Unlock()
	if d := decisionOf(t, sp, "pane="+w.PaneID); d.Answer.Choice != "Yes" {
		t.Errorf("a known answer was overwritten: %+v", d.Answer)
	}
}

func TestEntryDecisions(t *testing.T) {
	sp, _, _, got := decisionTestSuper(t)
	qe, _ := sp.Reply(mainThread, "README 里的授权怎么写？", "", []string{"授权一并配置", "由其他 worker 配置"})
	check, _ := sp.Reply(mainThread, "部署完了，看一眼线上", "", nil, true)
	q := qe.ID

	sp.sendNotifications()
	sent := strings.Join(got(), "\n")
	if !strings.Contains(sent, "Collie 需要你选：授权一并配置 / 由其他 worker 配置") || !strings.Contains(sent, "#/d/"+q) {
		t.Errorf("options notice: %s", sent)
	}

	if d := decisionOf(t, sp, "id="+q); d.Kind != "options" || d.Status != "open" {
		t.Fatalf("%+v", d)
	}
	if code, _ := decide(t, sp, map[string]any{"id": q, "choice": "随便"}); code != 400 {
		t.Errorf("an option it doesn't offer: %d", code)
	}
	if code, _ := decide(t, sp, map[string]any{"id": q, "choice": "授权一并配置"}); code != 200 {
		t.Fatalf("decide option: %d", code)
	}
	d := decisionOf(t, sp, "id="+q)
	if d.Status != "done" || d.Answer == nil || d.Answer.Choice != "授权一并配置" {
		t.Fatalf("answered option: %+v %+v", d, d.Answer)
	}
	if code, out := decide(t, sp, map[string]any{"id": check.ID}); code != 200 || out["ok"] != true {
		t.Fatalf("check: %d %v", code, out)
	}
	if d := decisionOf(t, sp, "id="+check.ID); d.Kind != "check" || d.Status != "done" || !d.Answer.Dismissed {
		t.Fatalf("check after: %+v %+v", d, d.Answer)
	}
}

// A dialog answered from its card on the page: the page names the choice,
// the hub counts it answered only once the dialog is gone.
func TestPageNamesTheChoiceItClicked(t *testing.T) {
	sp, _, w, _ := decisionTestSuper(t)
	sp.workerSettled(context.Background(), w, true)
	body, _ := json.Marshal(map[string]string{"pane": w.PaneID, "choice": "No,  and tell Claude what to do differently (esc)"})
	sp.s.superDecided(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/super/decided", bytes.NewReader(body)))
	if d := decisionOf(t, sp, "pane="+w.PaneID); d.Status != "open" {
		t.Fatalf("settled before the dialog went away: %+v", d)
	}
	sp.mu.Lock()
	sp.settleAskLocked(w.PaneID, "", "") // the tick sees it leave "blocked"
	sp.mu.Unlock()
	d := decisionOf(t, sp, "pane="+w.PaneID)
	if d.Status != "done" || d.Answer.By != "你" || d.Answer.Choice != "No, and tell Claude what to do differently (esc)" {
		t.Fatalf("%+v %+v", d, d.Answer)
	}
}

// Collie stuck on a confirm while dreaming still notifies; it is not a
// decision the page can open, so it links to the chat as before.
func TestDreamBlockedStillNotifies(t *testing.T) {
	sp, _, _, got := decisionTestSuper(t)
	sp.mu.Lock()
	sp.addEntryLocked("event", mainThread, "Collie 整理记忆时停在一个确认上，点这里看看", dreamName, "blocked")
	sp.mu.Unlock()
	sp.sendNotifications()
	sent := strings.Join(got(), "\n")
	if !strings.Contains(sent, "整理记忆时停在一个确认上") || strings.Contains(sent, "#/d/") {
		t.Fatalf("dream notice: %q", sent)
	}
}
