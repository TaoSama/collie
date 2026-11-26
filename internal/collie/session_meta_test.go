package collie

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexScheduledAndPinnedReadback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	if err := os.MkdirAll(filepath.Join(home, ".codex", "automations", "a1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "automations", "a1", "automation.toml"),
		[]byte("status = \"ACTIVE\"\ntarget_thread_id = \"t-sched\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, ".codex", "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE threads (id TEXT, name TEXT, title TEXT, first_user_message TEXT, cwd TEXT, rollout_path TEXT,
		created_at INTEGER, created_at_ms INTEGER, updated_at INTEGER, updated_at_ms INTEGER, originator TEXT, archived INTEGER, source TEXT, thread_section_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)
	insert := func(id string, at time.Time, archived int, section string) {
		if _, err := db.Exec(`INSERT INTO threads VALUES (?, '', ?, ?, '/root', '', ?, ?, ?, ?, '', ?, 'vscode', ?)`,
			id, id, id, at.Unix(), at.UnixMilli(), at.Unix(), at.UnixMilli(), archived, section); err != nil {
			t.Fatal(err)
		}
	}
	insert("t-pinned-old", old, 0, codexPinnedSectionID)
	insert("t-pinned-archived", now, 1, codexPinnedSectionID)
	insert("t-sched", now, 0, "")
	for i := range codexMax {
		insert(fmt.Sprintf("t-normal-%d", i), now.Add(-time.Duration(i)*time.Second), 0, "")
	}

	forgetPinned()
	t.Cleanup(forgetPinned)
	s := &Server{home: home}
	pinned := s.codexPinnedIDs()
	if !pinned["t-pinned-old"] || pinned["t-pinned-archived"] || pinned["t-sched"] {
		t.Fatalf("codexPinnedIDs = %v, want only unarchived pinned thread", pinned)
	}
	rows := s.codexAppRows()
	byID := map[string]desktopRow{}
	for _, r := range rows {
		byID[r.session] = r
	}
	if !byID["t-pinned-old"].pinned || byID["t-pinned-old"].scheduled {
		t.Fatalf("pinned old row = %+v, want pinned=true scheduled=false", byID["t-pinned-old"])
	}
	if !byID["t-sched"].scheduled || byID["t-sched"].pinned {
		t.Fatalf("scheduled row = %+v, want scheduled=true pinned=false", byID["t-sched"])
	}
}

func TestAgyScheduledAndPinned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	root := filepath.Join(home, ".gemini", "antigravity")
	if err := os.MkdirAll(filepath.Join(root, "annotations"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE conversation_summaries (conversation_id TEXT, title TEXT, preview TEXT, workspace_uris TEXT, status TEXT,
		not_fully_idle NUMERIC, last_modified_time DATETIME, last_user_input_time DATETIME, nesting_depth INTEGER, killed NUMERIC, source TEXT)`); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Format("2006-01-02 15:04:05")
	for _, c := range []struct{ id, src string }{
		{"11111111-1111-1111-1111-111111111111", "CORTEX_TRAJECTORY_SOURCE_VSCODE"},
		{"22222222-2222-2222-2222-222222222222", "CORTEX_TRAJECTORY_SOURCE_VSCODE"},
		{"33333333-3333-3333-3333-333333333333", "CORTEX_TRAJECTORY_SOURCE_CLI"},
	} {
		if _, err := db.Exec(`INSERT INTO conversation_summaries VALUES (?, '对话', '', '["file:///root"]', '', 0, ?, ?, 0, 0, ?)`, c.id, at, at, c.src); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "annotations", "11111111-1111-1111-1111-111111111111.pbtxt"), []byte("pinned: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var pinCalls []map[string]any
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case agyService + "ListBackgroundTasks":
			w.Write([]byte(`{"tasks":[
				{"conversationId":"11111111-1111-1111-1111-111111111111","task":{"toolName":"schedule"}},
				{"conversationId":"22222222-2222-2222-2222-222222222222","task":{"toolName":"run_command"}},
				{"conversationId":"33333333-3333-3333-3333-333333333333","task":{"toolName":"schedule"}}
			]}`))
		case agyConversations + "UpdateConversation":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			pinCalls = append(pinCalls, body)
			w.Write([]byte(`{}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"code":"not_found"}`))
		}
	}))
	defer app.Close()

	agyServerCache.Lock()
	agyServerCache.at, agyServerCache.ls = time.Now(), &agyServer{addr: strings.TrimPrefix(app.URL, "http://"), csrf: "tok"}
	agyServerCache.Unlock()
	forgetAgyScheduled()
	t.Cleanup(func() { forgetAgyServer(); forgetAgyScheduled() })

	s := &Server{home: home}
	rows := s.agyDesktopRows()
	byID := map[string]desktopRow{}
	for _, r := range rows {
		byID[r.session] = r
	}
	if !byID["11111111-1111-1111-1111-111111111111"].scheduled || !byID["11111111-1111-1111-1111-111111111111"].pinned {
		t.Fatalf("app sched+pinned row = %+v", byID["11111111-1111-1111-1111-111111111111"])
	}
	if byID["22222222-2222-2222-2222-222222222222"].scheduled || byID["22222222-2222-2222-2222-222222222222"].pinned {
		t.Fatalf("app non-schedule row = %+v", byID["22222222-2222-2222-2222-222222222222"])
	}
	if byID["33333333-3333-3333-3333-333333333333"].scheduled || byID["33333333-3333-3333-3333-333333333333"].pinned {
		t.Fatalf("cli row should not be marked scheduled/pinned: %+v", byID["33333333-3333-3333-3333-333333333333"])
	}

	// Desktop pin hits UpdateConversation; CLI pin does not.
	req := httptest.NewRequest("POST", "/api/sessions/11111111-1111-1111-1111-111111111111/pin",
		strings.NewReader(`{"kind":"agy","surface":"","pinned":true}`))
	req.SetPathValue("id", "11111111-1111-1111-1111-111111111111")
	w := httptest.NewRecorder()
	s.pinSession(w, req)
	if w.Code != 200 || len(pinCalls) != 1 {
		t.Fatalf("desktop pin = %d %s, calls=%v", w.Code, w.Body, pinCalls)
	}
	ann, _ := pinCalls[0]["annotations"].(map[string]any)
	if pinCalls[0]["conversationId"] != "11111111-1111-1111-1111-111111111111" || ann["pinned"] != true {
		t.Fatalf("unexpected UpdateConversation payload: %v", pinCalls[0])
	}

	reqCLI := httptest.NewRequest("POST", "/api/sessions/33333333-3333-3333-3333-333333333333/pin",
		strings.NewReader(`{"kind":"agy","surface":"cli","pinned":true}`))
	reqCLI.SetPathValue("id", "33333333-3333-3333-3333-333333333333")
	wCLI := httptest.NewRecorder()
	s.pinSession(wCLI, reqCLI)
	if wCLI.Code != 200 || len(pinCalls) != 1 {
		t.Fatalf("cli pin should be no-op on agy desktop RPC: code=%d calls=%d", wCLI.Code, len(pinCalls))
	}
}

func TestClaudeScheduledDetection(t *testing.T) {
	// Durable CLI scheduled_tasks.json
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".claude", "scheduled_tasks.json"),
		[]byte(`{"tasks":[{"id":"1","cron":"0 9 * * *","prompt":"hi","createdAt":1,"createdBySessionId":"sess-a"},{"id":"2","cron":"0 9 * * *","prompt":"hi","createdAt":2}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ids := claudeScheduledIn(repo)
	if !ids["sess-a"] || len(ids) != 1 {
		t.Fatalf("claudeScheduledIn = %v, want only sess-a", ids)
	}

	// Claude desktop scheduledTaskId
	home := t.TempDir()
	app := filepath.Join(home, "Library", "Application Support", "Claude")
	dir := filepath.Join(app, "claude-code-sessions", "a", "b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for _, item := range []map[string]any{
		{"sessionId": "local_s1", "cliSessionId": "c1", "cwd": "/root", "title": "定时会话", "scheduledTaskId": "task-1", "createdAt": now, "lastActivityAt": now},
		{"sessionId": "local_s2", "cliSessionId": "c2", "cwd": "/root", "title": "普通会话", "createdAt": now, "lastActivityAt": now - 1000},
	} {
		b, _ := json.Marshal(item)
		if err := os.WriteFile(filepath.Join(dir, item["sessionId"].(string)+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rows := (&Server{home: home}).claudeDesktopRows([]string{app})
	byID := map[string]desktopRow{}
	for _, r := range rows {
		byID[r.session] = r
	}
	if !byID["c1"].scheduled || byID["c2"].scheduled {
		t.Fatalf("claude desktop scheduled flags = c1:%v c2:%v, want true/false", byID["c1"].scheduled, byID["c2"].scheduled)
	}
}

func TestCodexPinCallsSectionMove(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(home, "rpc.log")
	script := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
  printf '%%s\n' "$line" >> %q
  case "$line" in
    *'"id":1'*) printf '{"id":1,"result":{}}\n' ;;
    *'"id":2'*) printf '{"id":2,"result":{}}\n'; exit 0 ;;
  esac
done
`, logFile)
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	binCache.Delete("codex")
	t.Cleanup(func() { binCache.Delete("codex") })

	s := &Server{home: home}
	for _, pinned := range []bool{true, false} {
		body, _ := json.Marshal(map[string]any{"kind": "codex", "pinned": pinned})
		req := httptest.NewRequest("POST", "/api/sessions/thread-1/pin", bytes.NewReader(body))
		req.SetPathValue("id", "thread-1")
		w := httptest.NewRecorder()
		s.pinSession(w, req)
		if w.Code != 200 {
			t.Fatalf("pin(%t) = %d %s", pinned, w.Code, w.Body)
		}
	}
	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var moves []string
	for _, l := range lines {
		if strings.Contains(l, `"method":"thread/section/move"`) {
			moves = append(moves, l)
		}
	}
	if len(moves) != 2 {
		t.Fatalf("expected 2 thread/section/move calls, got %v", lines)
	}
	if !strings.Contains(moves[0], `"sectionId":"`+codexPinnedSectionID+`"`) || !strings.Contains(moves[0], `"threadId":"thread-1"`) {
		t.Fatalf("pin call = %s", moves[0])
	}
	if !strings.Contains(moves[1], `"sectionId":null`) || !strings.Contains(moves[1], `"threadId":"thread-1"`) {
		t.Fatalf("unpin call = %s", moves[1])
	}
}

func TestMarksPinnedSetAndClear(t *testing.T) {
	marks.Lock()
	marks.m, marks.loaded = nil, false
	marks.Unlock()
	t.Cleanup(func() {
		marks.Lock()
		marks.m, marks.loaded = nil, false
		marks.Unlock()
	})
	s := &Server{}
	post := func(body string) map[string]*sessionMark {
		w := httptest.NewRecorder()
		s.setMark(w, httptest.NewRequest("POST", "/api/marks", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
		var out struct{ Marks map[string]*sessionMark }
		json.Unmarshal(w.Body.Bytes(), &out)
		return out.Marks
	}
	m := post(`{"key":"devbox|claude|s1","pinned":true}`)
	if m["devbox|claude|s1"] == nil || !m["devbox|claude|s1"].Pinned {
		t.Fatalf("pinned mark not saved: %+v", m)
	}
	m = post(`{"key":"devbox|claude|s1","pinned":false}`)
	if m["devbox|claude|s1"] != nil {
		t.Fatalf("clearing pinned should remove empty mark: %+v", m)
	}
}
