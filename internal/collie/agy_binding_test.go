package collie

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestAgyPaneRestartDropsOldConversation(t *testing.T) {
	const (
		oldID   = "11111111-1111-4111-8111-111111111111"
		newID   = "22222222-2222-4222-8222-222222222222"
		otherID = "33333333-3333-4333-8333-333333333333"
	)
	now := time.Now().UTC().Truncate(time.Second)
	home, cwd := t.TempDir(), t.TempDir()
	root := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE conversation_summaries (
		conversation_id TEXT, workspace_uris TEXT, title TEXT, last_modified_time TEXT)`); err != nil {
		t.Fatal(err)
	}
	workspace, err := json.Marshal([]string{"file://" + cwd})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{oldID, newID, otherID} {
		if _, err := db.Exec(`INSERT INTO conversation_summaries VALUES (?, ?, ?, ?)`, id, string(workspace), id, now.Format(time.DateTime)); err != nil {
			t.Fatal(err)
		}
	}
	addLog := func(id string) string {
		log := filepath.Join(root, "brain", id, ".system_generated", "logs", "transcript.jsonl")
		if err := os.MkdirAll(filepath.Dir(log), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(log, nil, 0600); err != nil {
			t.Fatal(err)
		}
		return log
	}
	oldLog := addLog(oldID)
	const procStartFormat = "Mon Jan _2 15:04:05 2006"
	ps := filepath.Join(t.TempDir(), "ps")
	output := "#!/bin/sh\ncase \"$2\" in\n" +
		"101) printf '%s\\n' '" + now.Add(-time.Hour).Format(procStartFormat) + "';;\n" +
		"102|103) printf '%s\\n' '" + now.Format(procStartFormat) + "';;\n" +
		"esac\n"
	if err := os.WriteFile(ps, []byte(output), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(ps)+string(os.PathListSeparator)+os.Getenv("PATH"))
	// The herdr agent identity is unchanged when the CLI is restarted in the
	// same shell. Only the foreground process information shows the restart.
	f := newFakeHerdr(t)
	pane := f.add("w-agy-restart", "worker", "agy-restart", "agy", cwd)
	pid, argv := 101, []string{"agy"}
	h := newHerdr("", "test", t.TempDir())
	h.local.conn.dial = func(context.Context) (net.Conn, error) {
		client, server := net.Pipe()
		go func(pid int, args []string) {
			defer server.Close()
			var req struct {
				ID     string         `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := json.NewDecoder(server).Decode(&req); err != nil {
				t.Error(err)
				return
			}
			res, err := f.handle(req.Method, req.Params)
			if req.Method == "pane.process_info" {
				res, err = map[string]any{"process_info": map[string]any{
					"foreground_processes": []map[string]any{{"pid": pid, "name": "agy", "argv": args}},
				}}, nil
			}
			reply := map[string]any{"id": req.ID, "result": res}
			if err != nil {
				reply["error"] = err
			}
			if err := json.NewEncoder(server).Encode(reply); err != nil {
				t.Error(err)
			}
		}(pid, argv)
		return client, nil
	}
	s := &Server{home: home, herdr: h}
	kind := "agy"
	a := agentDetail{Agent: &kind, Cwd: &cwd}
	cache := map[string]*transcript{}
	check := func(label, wantPath, wantID string) {
		t.Helper()
		tr := s.transcriptFor(pane, pane, a, cache)
		if tr == nil && wantPath != "" || tr != nil && tr.path != wantPath {
			var got string
			if tr != nil {
				got = tr.path
			}
			t.Errorf("%s transcript = %q, want %q", label, got, wantPath)
		}
		if got := s.localSessions(t.Context())[pane].Session; got != wantID {
			t.Errorf("%s session exposed to title and actions = %s, want %s", label, strconv.Quote(got), strconv.Quote(wantID))
		}
	}
	check("original process", oldLog, oldID)
	pid = 102
	check("restarted before the new log exists", "", "")
	newLog := addLog(newID)
	check("restarted with one new log", newLog, newID)
	addLog(otherID)
	check("another candidate appeared after the initial match", "", "")
	pid = 103
	argv = []string{"agy", "--conversation", newID}
	check("explicitly resumed process", newLog, newID)
}
