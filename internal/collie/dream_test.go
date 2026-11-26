package collie

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFinishDreamSkillOptionsPendingDBAndLark(t *testing.T) {
	sent := fakeLark(t)
	sp, db, call := newPendingTestEnv(t)
	sp.bin = "collie"
	sp.st.Settings.NotifyLark = true
	sp.st.Settings.NotifyLevel = "quiet"
	sp.seenAt = time.Now().Add(-5 * time.Minute)
	sp.seenPage("", "http://hub:7788", false)
	sp.seenAt = time.Now().Add(-5 * time.Minute)

	at := time.Date(2026, 11, 24, 4, 5, 0, 0, time.Local)
	dreamsDir := filepath.Join(sp.routerDir(), "dreams")
	if err := os.MkdirAll(dreamsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	journal := strings.Join([]string{
		"精简了 instructions.md 并归档 2 个已完成任务。",
		"可做成 skill：Collie 双端同步发版、Dorado 草稿只读巡检。",
		"可改进 skill：feishu-todo（补齐群话题游标容错）。",
		"",
		"- 移除了已有活跃定时器覆盖的重复指令",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dreamsDir, journalName(at)+".md"), []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}

	sp.mu.Lock()
	sp.dream = &dreamRun{at: at, started: true, worked: true}
	sp.mu.Unlock()

	sp.finishDream(context.Background(), &dreamRun{at: at}, true)

	wantSkillOpts := []string{"做成个人 skill", "放到项目仓库", "先不用"}
	wantFixOpts := []string{"派人去修", "先不用"}

	snapRec := call("GET", "/api/super", nil)
	if snapRec.Code != 200 {
		t.Fatalf("GET /api/super = %d", snapRec.Code)
	}
	var snap superView
	if err := json.Unmarshal(snapRec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Entries) != 3 {
		t.Fatalf("entries len = %d, want 3: %+v", len(snap.Entries), snap.Entries)
	}
	ev, eSkill, eFix := snap.Entries[0], snap.Entries[1], snap.Entries[2]
	if ev.Role != "event" || !strings.Contains(ev.Text, "精简了 instructions.md") {
		t.Fatalf("unexpected dream wake event: %+v", ev)
	}
	if eSkill.Role != "collie" || !strings.Contains(eSkill.Text, "Collie 双端同步发版、Dorado 草稿只读巡检") || !slices.Equal(eSkill.Options, wantSkillOpts) {
		t.Fatalf("unexpected skills entry: %+v", eSkill)
	}
	if eFix.Role != "collie" || !strings.Contains(eFix.Text, "feishu-todo（补齐群话题游标容错）") || !slices.Equal(eFix.Options, wantFixOpts) {
		t.Fatalf("unexpected fixes entry: %+v", eFix)
	}

	// Both appear in Pending in order, with their options intact.
	if len(snap.Pending) != 2 || snap.Pending[0].ID != eSkill.ID || snap.Pending[1].ID != eFix.ID {
		t.Fatalf("pending = %+v, want [%s, %s]", snap.Pending, eSkill.ID, eFix.ID)
	}
	if !slices.Equal(snap.Pending[0].Options, wantSkillOpts) || !slices.Equal(snap.Pending[1].Options, wantFixOpts) {
		t.Fatalf("pending options mismatch: %+v", snap.Pending)
	}

	// Lark notification in quiet mode: both option messages notify as orange "Collie · 需要你" card.
	sp.notifyAt = time.Time{}
	sp.sendNotifications()
	larkRaw, err := os.ReadFile(sent)
	if err != nil {
		t.Fatalf("expected lark notification for dream skill options: %v", err)
	}
	larkStr := string(larkRaw)
	for _, want := range []string{
		`"template":"orange"`,
		"Collie · 需要你",
		"Collie 需要你选：做成个人 skill / 放到项目仓库 / 先不用",
		"Collie 需要你选：派人去修 / 先不用",
		"#/d/" + eSkill.ID,
		"#/d/" + eFix.ID,
	} {
		if !strings.Contains(larkStr, want) {
			t.Errorf("lark card missing %q in:\n%s", want, larkStr)
		}
	}

	// Persistence across SQLite flush + loadState.
	sp.flush()
	reloaded, err := loadState(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Entries) != 3 || !slices.Equal(reloaded.Entries[1].Options, wantSkillOpts) || !slices.Equal(reloaded.Entries[2].Options, wantFixOpts) {
		t.Fatalf("reloaded entries lost options: %+v", reloaded.Entries)
	}

	// Clicking option on eSkill (POST /api/super/say with re: eSkill.ID) resolves eSkill while keeping eFix pending.
	if rec := call("POST", "/api/super/say", map[string]any{"text": "做成个人 skill", "re": eSkill.ID}); rec.Code != 200 {
		t.Fatalf("say status = %d", rec.Code)
	}
	if got := pendingIDs(t, call); !slices.Equal(got, []string{eFix.ID}) {
		t.Fatalf("pending after answering eSkill = %v, want [%s]", got, eFix.ID)
	}

	// Clicking option on eFix resolves eFix too.
	if rec := call("POST", "/api/super/say", map[string]any{"text": "派人去修", "re": eFix.ID}); rec.Code != 200 {
		t.Fatalf("say status = %d", rec.Code)
	}
	if got := pendingIDs(t, call); len(got) != 0 {
		t.Fatalf("pending after answering eFix = %v, want empty", got)
	}
}

func TestMemoryUpFrontSplitAnd10KBBudget(t *testing.T) {
	sp, _, call := newPendingTestEnv(t)
	sp.bin = "collie"

	if upFrontBudget != 10<<10 {
		t.Fatalf("upFrontBudget = %d, want %d (10 KB)", upFrontBudget, 10<<10)
	}

	// Verify routerDoc and dreamPrompt carry 10 KB budget, have no hardcoded "6 KB",
	// and dreamPrompt explicitly allows removing recurring items covered by active timers + notes/.
	if err := sp.writeRouterFiles(); err != nil {
		t.Fatal(err)
	}
	routerBytes, err := os.ReadFile(filepath.Join(sp.routerDir(), "ROUTER.md"))
	if err != nil {
		t.Fatal(err)
	}
	routerStr := string(routerBytes)
	if !strings.Contains(routerStr, "~10 KB") || strings.Contains(routerStr, "6 KB") {
		t.Fatalf("ROUTER.md budget text unexpected:\n%s", routerStr)
	}
	if !strings.Contains(routerStr, "create a timer + a notes/ workflow directly,") || !strings.Contains(routerStr, "proactively\n  propose an actionable suggestion") {
		t.Fatalf("ROUTER.md missing timer-in-notes or proactive --opt guidance:\n%s", routerStr)
	}
	renderedDream := strings.ReplaceAll(dreamPrompt, "{{BUDGET}}", "10")
	if !strings.Contains(renderedDream, "~10 KB") || strings.Contains(renderedDream, "6 KB") {
		t.Fatalf("dreamPrompt budget text unexpected:\n%s", renderedDream)
	}
	if !strings.Contains(dreamPrompt, "exempt from rule 6") || !strings.Contains(dreamPrompt, "active timer") {
		t.Fatalf("dreamPrompt missing active-timer deduplication rule")
	}

	// Seed controllable up-front files (~5.5 KB) + large hub-written messages.md (~6 KB)
	// + non-upfront notes/topic.md & tasks/done/old.md (~8 KB).
	memDir := sp.memDir()
	insBody := "# 指令\n" + strings.Repeat("中文规则测试行\n", 120)        // ~2.6 KB
	idxBody := "# 笔记索引\n" + strings.Repeat("- topic.md：说明\n", 60) // ~1.5 KB
	taskBody := "# 进行中任务\n" + strings.Repeat("步骤详情\n", 90)        // ~1.2 KB
	doneBody := "# 已归档任务\n" + strings.Repeat("历史记录\n", 300)       // not counted in upFront
	noteBody := "# 专题笔记\n" + strings.Repeat("长篇复盘经验\n", 300)      // not counted in upFront
	msgBody := "# 近期消息\n" + strings.Repeat("- 11-24 08:00 [chat:main] 最近回复内容\n", 120)

	mustWrite := func(rel, content string) int64 {
		p := filepath.Join(memDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return int64(len(content))
	}
	wantUpFront := mustWrite("instructions.md", insBody) +
		mustWrite("notes/index.md", idxBody) +
		mustWrite("tasks/active.md", taskBody)
	mustWrite("tasks/done/old.md", doneBody)
	mustWrite("notes/topic.md", noteBody)
	wantMsgs := mustWrite("messages.md", msgBody)

	if got := sp.upFrontSize(); got != wantUpFront {
		t.Fatalf("upFrontSize() = %d, want %d", got, wantUpFront)
	}
	if got := sp.messagesSize(); got != wantMsgs {
		t.Fatalf("messagesSize() = %d, want %d", got, wantMsgs)
	}

	rec := call("GET", "/api/super/memory", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /api/super/memory = %d: %s", rec.Code, rec.Body.String())
	}
	var apiOut struct {
		UpFront  int64 `json:"up_front"`
		Messages int64 `json:"messages"`
		Loaded   int64 `json:"loaded"`
		Budget   int64 `json:"budget"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &apiOut); err != nil {
		t.Fatal(err)
	}
	if apiOut.UpFront != wantUpFront || apiOut.Messages != wantMsgs || apiOut.Loaded != wantUpFront+wantMsgs || apiOut.Budget != 10<<10 {
		t.Fatalf("/api/super/memory = %+v, want up_front=%d messages=%d loaded=%d budget=%d",
			apiOut, wantUpFront, wantMsgs, wantUpFront+wantMsgs, 10<<10)
	}
	// Even though loaded (up_front + messages) > 10 KB, up_front itself is well under 10 KB!
	if apiOut.Loaded <= apiOut.Budget || apiOut.UpFront >= apiOut.Budget {
		t.Fatalf("expected loaded (%d) > budget (%d) while up_front (%d) < budget", apiOut.Loaded, apiOut.Budget, apiOut.UpFront)
	}

	// sessionStart only warns when upFrontSize > upFrontBudget*2 (20 KB).
	if s := sp.sessionStart(); strings.Contains(s, "a lot for every session") {
		t.Fatalf("unexpected sessionStart bloat warning below 20 KB:\n%s", s)
	}
	mustWrite("tasks/huge.md", strings.Repeat("超长活跃任务内容\n", 900)) // pushes upFrontSize > 20 KB
	if s := sp.sessionStart(); !strings.Contains(s, "bring them to ~10 KB") {
		t.Fatalf("expected ~10 KB sessionStart warning above 20 KB, got:\n%s", s)
	}
}

// 11-26: Collie ran on another computer, and the clean-up only looked at the
// hub's herdr. The last dream's session there kept the name collie-dream, so
// every new dream's agent failed to be named, and each failure left its
// workspace behind.
func TestOldDreamsClosedOnCollieComputer(t *testing.T) {
	hub, mac := newFakeHerdr(t), newFakeHerdr(t)
	sp := newRouterTestSuper(t, hub)
	h := sp.s.herdr
	h.mu.Lock()
	h.remote["mac"] = &Machine{ID: "mac", Name: "mac", Online: true, conn: &herdrConn{dial: mac.dial}}
	h.mu.Unlock()
	hub.add("w1", dreamLabel, "", "", "/hub") // the hub's own: not this computer's to clear
	mac.add("w21", dreamLabel, dreamName, "claude", "/router")
	mac.add("w22", "renamed by hand", dreamName+"-x", "claude", "/router")
	mac.add("w23", "renamed by hand", "", "claude", "/router")
	mac.panes["w23:p1"].name = dreamName
	delete(mac.panes, "w21:p1") // a workspace whose agent is gone
	mac.add("w5", "work", "worker-1", "claude", "/work")

	sp.closeOldDreams(context.Background(), "mac")

	if _, ok := mac.ws["w21"]; ok {
		t.Error("the last dream's workspace on Collie's computer stayed open")
	}
	if _, ok := mac.ws["w23"]; ok {
		t.Error("the agent holding the dream's name stayed")
	}
	for _, ws := range []string{"w22", "w5"} {
		if _, ok := mac.ws[ws]; !ok {
			t.Errorf("%s closed, but it is not a dream", ws)
		}
	}
	if _, ok := hub.ws["w1"]; !ok {
		t.Error("closed the hub's workspace while clearing mac")
	}
}

// A dream whose agent came up but could not start closes its workspace.
func TestDreamFailedStartClosesWorkspace(t *testing.T) {
	f := newFakeHerdr(t)
	sp := newRouterTestSuper(t, f)
	// Holds the name but isn't listed as an agent, so nothing clears it first.
	f.add("w9", "shell", dreamName, "", "/x")
	if err := sp.StartDream(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	for until := time.Now().Add(10 * time.Second); sp.dreaming(); {
		if time.Now().After(until) {
			t.Fatal("the dream neither started nor failed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, label := range f.ws {
		if label == dreamLabel {
			t.Errorf("failed dream left its workspace %s", id)
		}
	}
	if _, ok := f.ws["w9"]; !ok {
		t.Error("closed a workspace that is not a dream's")
	}
}
