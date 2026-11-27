package collie

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func stubHeadlessBin(t *testing.T, dir string) {
	t.Helper()
	orig := headlessBin
	t.Cleanup(func() { headlessBin = orig })
	headlessBin = func(name, _ string) string {
		p := filepath.Join(dir, name)
		if isFile(p) {
			return p
		}
		return ""
	}
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func TestSendExternalCodexQueueCLI(t *testing.T) {
	const thread = "11111111-1111-1111-1111-111111111111"
	var mu sync.Mutex
	var methods []string
	startFakeCodexDaemon(t, func(method string, _ json.RawMessage) (any, *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}) {
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
		switch method {
		case "thread/queue/add":
			return map[string]any{"queuedSubmission": map[string]any{"id": "q-cli"}}, nil
		case "thread/resume", "thread/queue/start", "thread/unsubscribe":
			return nil, nil
		default:
			t.Errorf("unexpected RPC: %s", method)
			return nil, nil
		}
	})

	origWake := codexWakeApp
	t.Cleanup(func() { codexWakeApp = origWake })
	codexWakeApp = func(context.Context, string) error {
		t.Error("codexWakeApp must not be called for queue-cli")
		return nil
	}

	s := &Server{home: t.TempDir()}
	externalCache.Lock()
	externalCache.at = time.Now()
	externalCache.list = []*External{{
		ID: "cx-" + thread, Kind: "codex", Cwd: "/work", Surface: surfaceCLI,
		Session: thread, CanSend: true, SendVia: "queue-cli", CanTakeOver: true, desktop: true,
	}}
	externalCache.Unlock()
	t.Cleanup(forgetExternal)

	r := httptest.NewRequest("POST", "/api/external/cx-"+thread+"/send", strings.NewReader(`{"text":"继续排查"}`))
	r.SetPathValue("id", "cx-"+thread)
	w := httptest.NewRecorder()
	s.sendExternal(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true || body["pane_id"] != nil {
		t.Fatalf("body = %+v, want ok=true and no pane_id", body)
	}
	want := []string{"thread/queue/add", "thread/resume", "thread/queue/start", "thread/unsubscribe"}
	if !slices.Equal(methods, want) {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
}

func TestSendExternalClaudeHeadlessInheritsCwdAndAlias(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "capture.txt")
	script := fmt.Sprintf(`#!/bin/sh
{
  printf 'PWD=%%s\n' "$PWD"
  printf 'BASE=%%s\n' "$ANTHROPIC_BASE_URL"
  printf 'CFG=%%s\n' "$CLAUDE_CONFIG_DIR"
  printf 'HERDR=%%s\n' "$HERDR_PANE_ID"
  printf 'CC=%%s\n' "$CLAUDECODE"
  for a in "$@"; do printf 'ARG=%%s\n' "$a"; done
} > %q
echo '{"type":"result","is_error":false,"result":"ok"}'
`, capture)
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	stubHeadlessBin(t, binDir)
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))

	const sid = "33333333-3333-3333-3333-333333333333"
	s := &Server{home: home}
	s.aliases.put("", aliasReport{
		Shell: "zsh",
		RC:    "~/.zshrc",
		Aliases: []LauncherAlias{{
			Name:   "ccx",
			Kind:   "claude",
			raw:    []string{"--model", "opus"},
			rawEnv: map[string]string{"ANTHROPIC_BASE_URL": "https://gw.example", "CLAUDE_CONFIG_DIR": "~/.claude-ccx"},
		}},
	}, nil)

	externalCache.Lock()
	externalCache.at = time.Now()
	externalCache.list = []*External{{
		ID: "cc-" + sid, Kind: "claude", Cwd: cwd, Surface: surfaceCLI, Alias: "ccx",
		Session: sid, CanSend: true, SendVia: "headless", CanTakeOver: true, desktop: true,
	}}
	externalCache.Unlock()
	t.Cleanup(forgetExternal)

	r := httptest.NewRequest("POST", "/api/external/cc-"+sid+"/send", strings.NewReader(`{"text":"继续修完"}`))
	r.SetPathValue("id", "cc-"+sid)
	w := httptest.NewRecorder()
	s.sendExternal(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["ok"] != true || body["pane_id"] != nil {
		t.Fatalf("body = %+v, want ok=true without pane_id", body)
	}

	waitUntil(t, 5*time.Second, func() bool { return !s.headless.running(sid) })
	got, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{
		"PWD=" + cwd + "\n",
		"BASE=https://gw.example\n",
		"CFG=" + filepath.Join(home, ".claude-ccx") + "\n",
		"HERDR=\n",
		"CC=\n",
		"ARG=--model\nARG=opus\nARG=-p\nARG=--resume\nARG=" + sid + "\nARG=--output-format\nARG=stream-json\nARG=--verbose\nARG=--\nARG=继续修完\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("capture missing %q\ngot:\n%s", want, text)
		}
	}
}

func TestSendExternalClaudeHeadlessRejectsLivePID(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	sessDir := filepath.Join(claudeDir, "sessions")
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const sid = "44444444-4444-4444-4444-444444444444"
	reg, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "sessionId": sid, "status": "idle"})
	if err := os.WriteFile(filepath.Join(sessDir, fmt.Sprintf("%d.json", os.Getpid())), reg, 0o600); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	ran := filepath.Join(t.TempDir(), "ran")
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(fmt.Sprintf("#!/bin/sh\ntouch %q\n", ran)), 0o755); err != nil {
		t.Fatal(err)
	}
	stubHeadlessBin(t, binDir)

	s := &Server{home: home}
	externalCache.Lock()
	externalCache.at = time.Now()
	externalCache.list = []*External{{
		ID: "cc-" + sid, Kind: "claude", Cwd: home, Surface: surfaceCLI,
		Session: sid, CanSend: true, SendVia: "headless", CanTakeOver: true, desktop: true,
	}}
	externalCache.Unlock()
	t.Cleanup(forgetExternal)

	r := httptest.NewRequest("POST", "/api/external/cc-"+sid+"/send", strings.NewReader(`{"text":"并发测试"}`))
	r.SetPathValue("id", "cc-"+sid)
	w := httptest.NewRecorder()
	s.sendExternal(w, r)

	if w.Code != 409 {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("claude -p must never run while a live PID holds the session")
	}
}

func TestSendExternalAgyHeadlessAndLockGuard(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "agy.txt")
	script := fmt.Sprintf(`#!/bin/sh
{
  printf 'PWD=%%s\n' "$PWD"
  for a in "$@"; do printf 'ARG=%%s\n' "$a"; done
} > %q
echo '{"type":"result","status":"SUCCESS"}'
`, capture)
	if err := os.WriteFile(filepath.Join(binDir, "agy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	stubHeadlessBin(t, binDir)

	const sid = "55555555-5555-5555-5555-555555555555"
	presDir := filepath.Join(home, ".gemini", "antigravity-cli", "presence")
	if err := os.MkdirAll(presDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lf, err := os.OpenFile(filepath.Join(presDir, sid+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lf.Close()
		t.Fatal(err)
	}

	s := &Server{home: home}
	setCache := func() {
		externalCache.Lock()
		externalCache.at = time.Now()
		externalCache.list = []*External{{
			ID: "ag-" + sid, Kind: "agy", Cwd: cwd, Surface: surfaceCLI,
			Session: sid, CanSend: true, SendVia: "headless", CanTakeOver: true, desktop: true,
		}}
		externalCache.Unlock()
	}
	setCache()
	t.Cleanup(forgetExternal)

	// While presence lock is held: hard reject with 409 and never run agy -p.
	r := httptest.NewRequest("POST", "/api/external/ag-"+sid+"/send", strings.NewReader(`{"text":"-hello"}`))
	r.SetPathValue("id", "ag-"+sid)
	w := httptest.NewRecorder()
	s.sendExternal(w, r)
	if w.Code != 409 {
		lf.Close()
		t.Fatalf("locked agy status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(capture); err == nil {
		lf.Close()
		t.Fatal("agy -p must never run while presence lock is held")
	}

	// Release lock: now agy -p succeeds with -p=<text>.
	syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	lf.Close()
	setCache()

	r = httptest.NewRequest("POST", "/api/external/ag-"+sid+"/send", strings.NewReader(`{"text":"-hello"}`))
	r.SetPathValue("id", "ag-"+sid)
	w = httptest.NewRecorder()
	s.sendExternal(w, r)
	if w.Code != 200 {
		t.Fatalf("unlocked agy status = %d, body = %s", w.Code, w.Body.String())
	}
	waitUntil(t, 5*time.Second, func() bool { return !s.headless.running(sid) })
	got, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	want := "PWD=" + cwd + "\nARG=--conversation\nARG=" + sid + "\nARG=--output-format\nARG=stream-json\nARG=-p=-hello\n"
	if string(got) != want {
		t.Fatalf("agy capture =\n%s\nwant:\n%s", string(got), want)
	}
}

func TestHeadlessSerialQueueAndStop(t *testing.T) {
	home := t.TempDir()
	binDir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "order.log")
	script := fmt.Sprintf(`#!/bin/sh
echo "start:$*" >> %q
sleep 0.25
echo "end:$*" >> %q
echo '{"type":"result","status":"SUCCESS"}'
`, logFile, logFile)
	if err := os.WriteFile(filepath.Join(binDir, "agy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	stubHeadlessBin(t, binDir)

	const sid = "66666666-6666-6666-6666-666666666666"
	s := &Server{home: home}
	e := &External{ID: "ag-" + sid, Kind: "agy", Cwd: home, Surface: surfaceCLI, Session: sid, CanSend: true, SendVia: "headless", desktop: true}
	if _, err := s.sendHeadless(context.Background(), e, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sendHeadless(context.Background(), e, "second"); err != nil {
		t.Fatal(err)
	}
	if !s.headless.running(sid) {
		t.Fatal("expected session to report running while queued")
	}
	waitUntil(t, 5*time.Second, func() bool { return !s.headless.running(sid) })
	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "start:") || !strings.HasPrefix(lines[1], "end:") ||
		!strings.HasPrefix(lines[2], "start:") || !strings.HasPrefix(lines[3], "end:") ||
		!strings.Contains(lines[0], "-p=first") || !strings.Contains(lines[2], "-p=second") {
		t.Fatalf("serial execution log = %v, want strictly sequential first then second", lines)
	}

	// Now test cancellation via POST /api/external/{id}/stop.
	if err := os.WriteFile(filepath.Join(binDir, "agy"), []byte("#!/bin/sh\ntrap 'exit 130' INT\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sendHeadless(context.Background(), e, "long-turn"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sendHeadless(context.Background(), e, "queued-after"); err != nil {
		t.Fatal(err)
	}
	externalCache.Lock()
	externalCache.at = time.Now()
	externalCache.list = []*External{e}
	externalCache.Unlock()
	t.Cleanup(forgetExternal)

	r := httptest.NewRequest("POST", "/api/external/ag-"+sid+"/stop", nil)
	r.SetPathValue("id", "ag-"+sid)
	w := httptest.NewRecorder()
	s.stopExternal(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"stopped":true`) {
		t.Fatalf("stopExternal = %d %s, want 200 stopped:true", w.Code, w.Body.String())
	}
	waitUntil(t, 5*time.Second, func() bool { return !s.headless.running(sid) })
}

func TestHeadlessStreamErrorPublished(t *testing.T) {
	home := t.TempDir()
	binDir := t.TempDir()
	script := `#!/bin/sh
echo '{"type":"result","is_error":true,"result":"API Error: 409 route mismatch"}'
exit 1
`
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	stubHeadlessBin(t, binDir)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))

	const sid = "77777777-7777-7777-7777-777777777777"
	s := &Server{home: home}
	ch, unsub := s.headless.subscribeErr(sid)
	defer unsub()
	e := &External{ID: "cc-" + sid, Kind: "claude", Cwd: home, Surface: surfaceCLI, Session: sid, CanSend: true, SendVia: "headless", desktop: true}
	if _, err := s.sendHeadless(context.Background(), e, "hi"); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		if !strings.Contains(ev.msg, "409 route mismatch") {
			t.Fatalf("published error = %q, want 409 route mismatch", ev.msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for published headless error")
	}
	waitUntil(t, 5*time.Second, func() bool { return !s.headless.running(sid) })
}
