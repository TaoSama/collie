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

func TestResumeFrom(t *testing.T) {
	sp := &Super{st: &State{Workers: map[string]*Worker{
		"done":  {Name: "done", Kind: "codex", Cwd: "/w", Machine: "m1", Ended: true, Session: "s-1"},
		"live":  {Name: "live", Session: "s-2"},
		"blank": {Name: "blank", Ended: true},
	}}}
	w, err := sp.resumeFrom("done")
	if err != nil || w.Session != "s-1" || w.Machine != "m1" || w.Cwd != "/w" {
		t.Fatalf("done: %+v %v", w, err)
	}
	for _, n := range []string{"live", "blank", "nope"} {
		if _, err := sp.resumeFrom(n); err == nil {
			t.Errorf("%s: want an error", n)
		}
	}
	if got := resumeArgs(kinds["antigravity"], "c"); !slices.Equal(got, []string{"--conversation", "c"}) {
		t.Errorf("agy: %v", got)
	}
	if got := resumeArgs("claude", "c"); !slices.Equal(got, []string{"--resume", "c"}) {
		t.Errorf("claude: %v", got)
	}
}

func TestRouterDocChanged(t *testing.T) {
	dir := t.TempDir()
	sp := &Super{st: &State{Workers: map[string]*Worker{}, Threads: map[string]*Thread{}}, subs: map[chan struct{}]bool{}}
	asks := func() int {
		n := 0
		for _, tr := range sp.st.Triggers {
			if tr.Source == "system" && tr.Status == "queued" {
				n++
			}
		}
		return n
	}
	sp.routerDocChanged(dir, []byte("v1")) // no session has read one yet
	if asks() != 0 {
		t.Fatal("asked before any session read it")
	}
	os.WriteFile(filepath.Join(dir, routerReadFile), []byte("v1"), 0o600)
	sp.routerDocChanged(dir, []byte("v1"))
	if asks() != 0 {
		t.Fatal("asked though nothing changed")
	}
	sp.routerDocChanged(dir, []byte("v2"))
	sp.routerDocChanged(dir, []byte("v3")) // still waiting on the first ask
	if asks() != 1 {
		t.Fatalf("want one ask, got %d", asks())
	}
}

func TestAgentStopLine(t *testing.T) {
	cases := []struct{ screen, summary, want string }{
		{"⏺ 正在读文件\n\n  ⎿  You've hit your usage limit · resets 5am\n\n> ", "正在读文件", "You've hit your usage limit · resets 5am"},
		{"■ stream error: unexpected status 429 Too Many Requests\n", "", "stream error: unexpected status 429 Too Many Requests"},
		{"⏺ 加了 rate limit 中间件，测试通过\n> ", "加了 rate limit 中间件，测试通过", ""}, // the work talking
		{"  ⎿  Error: ENOENT no such file\n⏺ 改好了\n> ", "改好了", ""},      // a tool's error, not the agent's
	}
	for _, c := range cases {
		if got := agentStopLine(c.screen, c.summary); got != c.want {
			t.Errorf("%q: got %q, want %q", c.screen, got, c.want)
		}
	}
}

func TestDreamSkills(t *testing.T) {
	p := filepath.Join(t.TempDir(), "d.md")
	os.WriteFile(p, []byte("归档了两个任务。\n可做成 skill：Dorado 只读查表、Spark 草稿读 HDFS。\n\n# 记录\n"), 0o600)
	if got := dreamLine(p, dreamSkillsPrefix); got != "Dorado 只读查表、Spark 草稿读 HDFS" {
		t.Errorf("skills: %q", got)
	}
	if got := dreamSummary(p); got != "归档了两个任务。" {
		t.Errorf("summary: %q", got)
	}
	os.WriteFile(p, []byte("归档了两个任务。\n"), 0o600)
	if dreamLine(p, dreamSkillsPrefix) != "" {
		t.Error("no line, no skills")
	}
}

func TestAgyFullTranscript(t *testing.T) {
	dir := t.TempDir()
	short, full := filepath.Join(dir, "transcript.jsonl"), filepath.Join(dir, "transcript_full.jsonl")
	if err := os.WriteFile(short, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if path := transcriptPath(short, "agy"); path != short {
		t.Errorf("no full one: got %s", path)
	}
	if err := os.WriteFile(full, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if path := transcriptPath(short, "agy"); path != full {
		t.Errorf("full one: got %s", path)
	}
	if path := transcriptPath(short, "claude"); path != short {
		t.Errorf("not agy: got %s", path)
	}
}

func TestSessionID(t *testing.T) {
	if got := sessionID("path", "/u/.codex/sessions/2026/11/21/rollout-2026-11-21T03-21-45-01a1fa68-5cc0-7561-9652-081fb93ad0f9.jsonl"); got != "01a1fa68-5cc0-7561-9652-081fb93ad0f9" {
		t.Errorf("codex path: %q", got)
	}
	if got := sessionID("id", "abc"); got != "abc" {
		t.Errorf("id: %q", got)
	}
	if got := sessionID("path", "/tmp/x.jsonl"); got != "" {
		t.Errorf("no id: %q", got)
	}
}

func TestFollow(t *testing.T) {
	soon, later := time.Now().Add(time.Hour), time.Now().Add(20*time.Hour)
	sp := &Super{st: &State{Timers: []*Timer{
		{Spec: "weekdays 10:00", Text: "飞书 todo 例行", NextRun: soon.Add(-30 * time.Minute), Enabled: true, Owner: "collie"},
		{Spec: "at 2026-11-22 09:00", Text: "复查 paper_df\n细节", NextRun: later, Enabled: true, Owner: "collie"},
		{Spec: "in 30m", Text: "看一眼进度", NextRun: soon, Enabled: false, Owner: "collie"},
	}}}
	f := sp.followLocked()
	if f == nil || f.NextText != "复查 paper_df" || !f.Next.Equal(later) {
		t.Fatalf("got %+v", f)
	}
	sp.st.Timers = sp.st.Timers[:1]
	if sp.followLocked() != nil {
		t.Error("a routine is not a follow-up")
	}
}

func TestSortedTimers(t *testing.T) {
	base := time.Date(2026, 11, 23, 14, 0, 0, 0, time.UTC)
	t1 := base.Add(10 * time.Minute)
	t2 := base.Add(2 * time.Hour)
	past1 := base.Add(-2 * time.Hour)
	past2 := base.Add(-10 * time.Minute)
	orig := []*Timer{
		{ID: "m2", Enabled: false, NextRun: past1},
		{ID: "m10", Enabled: true, NextRun: t2},
		{ID: "m4", Enabled: false, NextRun: past2},
		{ID: "m11", Enabled: false, LastRun: &past2, NextRun: past1},
		{ID: "m3", Enabled: true, NextRun: t1},
		{ID: "m1", Enabled: true, NextRun: t2},
	}
	got := sortedTimers(orig)
	var ids []string
	for _, x := range got {
		ids = append(ids, x.ID)
	}
	want := []string{"m3", "m1", "m10", "m11", "m4", "m2"}
	if !slices.Equal(ids, want) {
		t.Fatalf("got %v, want %v", ids, want)
	}
	if orig[0].ID != "m2" || orig[1].ID != "m10" {
		t.Fatal("sortedTimers mutated input slice")
	}
}

func TestProgressHash(t *testing.T) {
	a := progressHash("⏺ Bash(collie show t12)\n✻ Thinking… (3m 21s · ↓ 1.2k tokens · esc to interrupt)\n> ")
	b := progressHash("⏺ Bash(collie show t12)\n✶ Pondering… (4m 02s · ↓ 1.2k tokens · esc to interrupt)\n> ")
	if a != b {
		t.Error("the spinner line alone moving is not progress")
	}
	if c := progressHash("⏺ Bash(collie show t12)\n⏺ Read(memory/notes/index.md)\n> "); c == a {
		t.Error("a new step is progress")
	}
	if progressHash("Working (12s • esc to interrupt)\nran 3 tests") != progressHash("Working (95s • esc to interrupt)\nran 3 tests") {
		t.Error("codex's timer is not progress")
	}
}

func TestDreamFixLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "j.md")
	os.WriteFile(p, []byte("精简了指令。\n可改进 skill：feishu-todo（漏读话题回复）\n\n- 细节\n"), 0o600)
	if got := dreamLine(p, dreamFixPrefix); got != "feishu-todo（漏读话题回复）" {
		t.Errorf("fix line: %q", got)
	}
	if dreamLine(p, dreamSkillsPrefix) != "" || dreamSummary(p) != "精简了指令。" {
		t.Error("the fix line is neither a new skill nor the summary")
	}
	if journalName(time.Date(2026, 11, 21, 18, 32, 0, 0, time.Local)) != "2026-11-21-1832" {
		t.Error("journal name")
	}
}

func TestResumeCarriesAlias(t *testing.T) {
	ccx, none := "ccx", ""
	old := &Worker{Name: "w", Kind: "claude", Cwd: "/w", Machine: "m1", Model: "opus", Thread: "t1", Session: "s-1", Alias: &ccx}
	b := spawnBody{Task: "go on"}
	if s := b.carryOver(old); s != "s-1" || b.Alias == nil || *b.Alias != "ccx" || b.Kind != "claude" || b.Model != "opus" || *b.Machine != "m1" || b.Name != "w" || b.Thread != "t1" {
		t.Fatalf("carried: %q %+v", s, b)
	}
	if j, _ := json.Marshal(old); !strings.Contains(string(j), `"alias":"ccx"`) {
		t.Errorf("the page isn't told its alias: %s", j)
	}
	b = spawnBody{Task: "go on", Alias: &none, Model: "sonnet"}
	b.carryOver(old)
	if *b.Alias != "" || b.Model != "sonnet" {
		t.Errorf("asked for no alias and another model: %+v", b)
	}
	b = spawnBody{Task: "go on"}
	if b.carryOver(&Worker{Kind: "claude", Session: "s-2"}); b.Alias != nil {
		t.Errorf("a worker from before keeps the default alias: %v", *b.Alias)
	}
}

func TestFailedWorkerLeavesNoTerminal(t *testing.T) {
	f := newFakeHerdr(t)
	sp := newRouterTestSuper(t, f)
	sp.st.Settings.WorkerKind, sp.st.Settings.WorkerAlias = "claude", ""
	// Another pane holds the name: the start fails after its workspace exists.
	f.add("w0", "other", "dup", "claude", "/")
	none := ""
	w, started, err := sp.Spawn(SpawnSpec{Kind: "claude", Cwd: "/", Task: "x", Name: "dup", Alias: &none})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-started; err == nil {
		t.Fatal("want a start error")
	}
	if _, ok := f.ws["wN1"]; ok || f.count("workspace.close") != 1 {
		t.Errorf("its workspace is left: %v", f.ws)
	}
	if w.Alias == nil || *w.Alias != "" {
		t.Errorf("launch alias not recorded: %v", w.Alias)
	}

	// An agent that quit back to its shell: the shell goes too.
	f.add("w9", "quit", "", "", "/")
	sp.workerDead(context.Background(), &Worker{Name: "q", Kind: "claude", PaneID: "w9:p1", WorkspaceID: "w9", Thread: mainThread})
	if _, ok := f.ws["w9"]; ok {
		t.Error("the quit worker's shell is left")
	}
}
