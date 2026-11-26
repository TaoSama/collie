package collie

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const archiveTestID = "3af54475-70ea-4809-91b6-20baeb4f8640"

func stubAgyArchive(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	app := httptest.NewServer(handler)
	t.Cleanup(app.Close)
	agyServerCache.Lock()
	agyServerCache.at = time.Now()
	agyServerCache.ls = &agyServer{addr: strings.TrimPrefix(app.URL, "http://")}
	agyServerCache.Unlock()
	t.Cleanup(forgetAgyServer)
}

func TestAgyNativeArchiveAndUnarchive(t *testing.T) {
	changes := make(chan *bool, 2)
	stubAgyArchive(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			CascadeID        string                   `json:"cascadeId"`
			MergeAnnotations bool                     `json:"mergeAnnotations"`
			Annotations      struct{ Archived *bool } `json:"annotations"`
		}
		if r.URL.Path != agyService+"UpdateConversationAnnotations" || json.NewDecoder(r.Body).Decode(&body) != nil || body.CascadeID != archiveTestID || !body.MergeAnnotations {
			t.Error("unexpected request to Antigravity")
		}
		changes <- body.Annotations.Archived
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, "{}"); err != nil {
			t.Error(err)
		}
	})
	s := &Server{home: t.TempDir()}
	for _, archived := range []bool{true, false} {
		path := "/api/sessions/" + archiveTestID + "/"
		if archived {
			path += "archive"
		} else {
			path += "unarchive"
		}
		resp := httptest.NewRecorder()
		s.NodeRoutes().ServeHTTP(resp, httptest.NewRequest("POST", path, strings.NewReader(`{"kind":"agy"}`)))
		if resp.Code != 200 {
			t.Fatalf("%s: %d %s", path, resp.Code, resp.Body)
		}
		if got := <-changes; got == nil || *got != archived {
			t.Fatalf("explicit archived field missing or incorrect: %v", got)
		}
	}
}

func TestAgyNativeArchiveBacksUpAndChangesCLIStore(t *testing.T) {
	home, data := t.TempDir(), t.TempDir()
	t.Setenv("COLLIE_HOME", data)
	root := filepath.Join(home, ".gemini", "antigravity-cli")
	index := filepath.Join(root, "conversation_summaries.db")
	add := agyIndexFixture(t, root)
	add(archiveTestID, time.Now(), false)
	add("another-session", time.Now(), true)
	conversation := filepath.Join(root, "conversations", archiveTestID+".db")
	annotation := filepath.Join(root, "annotations", archiveTestID+".pbtxt")
	const transcript = "session database was left alone"
	const title = "title: \"CLI archive test\"\n"
	for path, contents := range map[string]string{conversation: transcript, annotation: title} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stubAgyArchive(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		if _, err := io.WriteString(w, `{"code":"not_found"}`); err != nil {
			t.Error(err)
		}
	})
	s := &Server{home: home}
	lock, err := lockAgyArchive(root, archiveTestID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.agyArchive(t.Context(), archiveTestID, true); err == nil || !strings.Contains(err.Error(), "还在 Antigravity CLI") {
		t.Fatalf("an open conversation was not refused: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	for _, archived := range []bool{true, true, false} {
		if err := s.agyArchive(t.Context(), archiveTestID, archived); err != nil {
			t.Fatal(err)
		}
		flag := "false"
		if archived {
			flag = "true"
		}
		got, err := os.ReadFile(annotation)
		if err != nil || !strings.HasPrefix(string(got), title+"archived: "+flag+"\n") || len(agyArchiveTimeField.FindAll(got, -1)) != 1 {
			t.Fatalf("annotation = %q, %v", got, err)
		}
		found := sqliteColumn(index, "conversations_fts", agyArchivedSQL)
		hasTarget, hasOther := false, false
		for _, id := range found {
			hasTarget = hasTarget || id == archiveTestID
			hasOther = hasOther || id == "another-session"
		}
		if hasTarget != archived || !hasOther {
			t.Fatalf("archived IDs = %v, target should be archived: %t", found, archived)
		}
	}
	backups, err := filepath.Glob(filepath.Join(data, "archive-backups", "*.json"))
	if err != nil || len(backups) != 2 {
		t.Fatalf("expected one backup per change: %v, %v", backups, err)
	}
	var flags [2]bool
	for _, p := range backups {
		var backup struct {
			Entry struct {
				ID       string `json:"conversation_id"`
				Archived int
			}
			Annotation []byte `json:"annotation_before"`
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &backup); err != nil {
			t.Fatal(err)
		}
		if backup.Entry.ID != archiveTestID || !strings.HasPrefix(string(backup.Annotation), title) || backup.Entry.Archived < 0 || backup.Entry.Archived > 1 {
			t.Fatalf("invalid index backup: %+v", backup)
		}
		flags[backup.Entry.Archived] = true
	}
	if flags != [2]bool{true, true} {
		t.Fatalf("backups must include both prior states: %v", flags)
	}
	if got, err := os.ReadFile(conversation); err != nil || string(got) != transcript {
		t.Fatalf("conversation database was changed: %v", err)
	}
}

func TestClaudeNativeArchiveOnlyChangesTheMatchingIndex(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "claude-code-sessions", "organization", "account")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "local_session.json")
	if err := os.WriteFile(path, []byte(`{"cliSessionId":"cli-session","title":"Sample","keptDirtyWorktree":true,"extra":{"some":"value"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if found, err := claudeArchiveIndex([]string{base}, "cli-session"); found != path || err != nil {
		t.Fatalf("Claude archive index = %q, %v", found, err)
	}
	for _, archived := range []bool{true, false} {
		if err := setClaudeArchiveIndex(path, "cli-session", archived); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			IsArchived             bool
			AutoArchiveExempt      bool
			UnarchivedCliSessionID string
			KeptDirtyWorktree      bool
			Extra                  map[string]string
		}
		if err := json.Unmarshal(b, &got); err != nil || got.IsArchived != archived || got.Extra["some"] != "value" {
			t.Fatalf("Claude index changed unexpectedly: %s, %v", b, err)
		}
		if !archived && (!got.AutoArchiveExempt || got.UnarchivedCliSessionID != "cli-session" || got.KeptDirtyWorktree) {
			t.Fatalf("Claude recovery fields are missing: %s", b)
		}
	}
	if note, err := (&Server{home: t.TempDir()}).claudeArchive(t.Context(), "cli-session", true); note == "" || err != nil {
		t.Fatalf("CLI archive note = %q, %v", note, err)
	}
}

func TestCodexArchiveMatchesOnlyKnownState(t *testing.T) {
	home, add := codexStateFixture(t)
	s := &Server{home: home}
	add("test-thread", "Verification", time.Now())
	if !s.codexArchiveMatches(t.Context(), "test-thread", false) || s.codexArchiveMatches(t.Context(), "test-thread", true) {
		t.Fatal("a known active thread should only match the unarchived state")
	}
	archiveCodexThread(t, home, "test-thread")
	if !s.codexArchiveMatches(t.Context(), "test-thread", true) || s.codexArchiveMatches(t.Context(), "missing", false) {
		t.Fatal("the archived state should match, while missing threads should not")
	}
}
