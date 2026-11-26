package collie

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeRenameAppendsCustomTitle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	dir := filepath.Join(home, ".claude", "projects", "-repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "abc.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{home: home}
	if err := s.claudeRename("abc", "新名字 <b>"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if want := `{"type":"custom-title","customTitle":"新名字 <b>","sessionId":"abc"}`; len(lines) != 2 || lines[1] != want {
		t.Fatalf("transcript = %q, want last line %s", b, want)
	}
	if err := s.claudeRename("missing", "x"); err == nil {
		t.Fatal("renaming a session that isn't there should fail")
	}
	if err := s.claudeRename("../abc", "x"); err == nil {
		t.Fatal("a session id with a path in it should be refused")
	}
}

func TestMarksSetAndClear(t *testing.T) {
	marks.Lock()
	marks.m, marks.loaded = nil, false
	marks.Unlock()
	s := &Server{}
	post := func(body string) map[string]*sessionMark {
		w := httptest.NewRecorder()
		s.setMark(w, httptest.NewRequest("POST", "/api/marks", bytes.NewBufferString(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
		var out struct{ Marks map[string]*sessionMark }
		json.Unmarshal(w.Body.Bytes(), &out)
		return out.Marks
	}
	m := post(`{"key":"mac|claude|s1","unread":true}`)
	if !m["mac|claude|s1"].Unread {
		t.Fatalf("unread not kept: %+v", m)
	}
	m = post(`{"key":"mac|claude|s1","archived":true}`)
	if k := m["mac|claude|s1"]; !k.Unread || !k.Archived {
		t.Fatalf("a mark set later should keep the earlier one: %+v", k)
	}
	post(`{"key":"mac|claude|s1","unread":false}`)
	m = post(`{"key":"mac|claude|s1","archived":false}`)
	if _, ok := m["mac|claude|s1"]; ok {
		t.Fatalf("a mark with nothing left should go: %+v", m)
	}
}

func TestSessionUsageReadsLatestReply(t *testing.T) {
	t.Setenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "")
	t.Setenv("CLAUDE_CODE_MAX_CONTEXT_TOKENS", "")
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	dir := filepath.Join(home, ".claude", "projects", "-repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	log := `{"type":"assistant","requestedModel":"fable","effort":"high","message":{"model":"claude-fable-5-1","usage":{"input_tokens":1}}}
{"type":"user","message":{"content":"/model"}}
{"type":"assistant","requestedModel":"claude-opus-5-5[1m]","effort":"medium","message":{"model":"claude-opus-5-5","usage":{"input_tokens":1}}}
{"type":"assistant","isSidechain":true,"requestedModel":"haiku","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":1}}}
{"type":"assistant","message":{"model":"<synthetic>","usage":{"input_tokens":0}}}
{"type":"user"}
`
	if err := os.WriteFile(filepath.Join(dir, "abc.jsonl"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{home: home}
	got := s.sessionUsage("claude", "abc")
	if got == nil || got.Model != "claude-opus-5-5[1m]" || got.Effort != "medium" || got.Window != 1000000 {
		t.Fatalf("sessionUsage = %+v, want the latest real reply's model and effort, and its 1M window", got)
	}
	if got := s.sessionUsage("claude", "missing"); got != nil {
		t.Fatalf("no log should give nothing, got %v", got)
	}
}

func TestAgyAnnotateSaysWhenTheAppCannotLoadIt(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		w.Write([]byte(`{"code":"not_found","message":"conversation b600 not found"}`))
	}))
	defer app.Close()
	agyServerCache.Lock()
	agyServerCache.at, agyServerCache.ls = time.Now(), &agyServer{addr: strings.TrimPrefix(app.URL, "http://")}
	agyServerCache.Unlock()
	defer forgetAgyServer()
	err := (&Server{home: t.TempDir()}).agyAnnotate(t.Context(), "b600", map[string]any{"title": "new title"})
	if err == nil || !strings.Contains(err.Error(), "无法打开这个会话，没能改名") {
		t.Fatalf("err = %v, want it to say the app can't load the conversation", err)
	}
}

func TestCollieArchivePersistsAndCanBeCleared(t *testing.T) {
	resetMarks := func() {
		marks.Lock()
		defer marks.Unlock()
		marks.m, marks.loaded = nil, false
	}
	resetMarks()
	t.Cleanup(resetMarks)
	db, err := openDB(filepath.Join(t.TempDir(), "herd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	s := &Server{sup: &Super{db: db}}
	const key = "macpro|agy|one"
	post := func(archived bool) {
		t.Helper()
		body, err := json.Marshal(map[string]any{"key": key, "collie_archived": archived})
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		s.setMark(w, httptest.NewRequest("POST", "/api/marks", bytes.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("archive %t: %d %s", archived, w.Code, w.Body)
		}
	}
	reload := func() map[string]*sessionMark {
		t.Helper()
		resetMarks()
		w := httptest.NewRecorder()
		s.listMarks(w, httptest.NewRequest("GET", "/api/marks", nil))
		var response struct{ Marks map[string]*sessionMark }
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.Marks
	}

	post(true)
	if m := reload()[key]; m == nil || !m.CollieArchived {
		t.Fatalf("archive missing after reload: %+v", m)
	}
	post(false)
	if m := reload()[key]; m != nil {
		t.Fatalf("archive still present after reload: %+v", m)
	}
}

func TestCollieArchiveReportsStorageFailure(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "herd.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := &Server{sup: &Super{db: db}}
	const key = "macpro|agy|failed"
	w := httptest.NewRecorder()
	s.setMark(w, httptest.NewRequest("POST", "/api/marks", strings.NewReader(`{"key":"`+key+`","collie_archived":true}`)))
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "没能保存这次更改") {
		t.Fatalf("archive: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	s.listMarks(w, httptest.NewRequest("GET", "/api/marks", nil))
	if strings.Contains(w.Body.String(), key) {
		t.Fatalf("failed write leaked to the list: %s", w.Body)
	}
}
