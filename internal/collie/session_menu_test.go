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

func TestClaudeRanReadsLatestReply(t *testing.T) {
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
	if got := s.claudeRan("abc"); got == nil || *got != [2]string{"claude-opus-5-5[1m]", "medium"} {
		t.Fatalf("claudeRan = %v, want the latest real reply's model and effort", got)
	}
	if got := s.claudeRan("missing"); got != nil {
		t.Fatalf("no log should give nothing, got %v", got)
	}
}
