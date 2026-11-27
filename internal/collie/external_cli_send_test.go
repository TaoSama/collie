package collie

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeHerdrResume struct {
	mu          sync.Mutex
	startErr    *HerdrError
	startKind   string
	startArgs   []string
	startCwd    string
	promptText  string
	promptPane  string
	paneStatus  string
	paneScreen  string
	next        int
}

func (f *fakeHerdrResume) dial(context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		var req struct {
			ID     string         `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(server).Decode(&req); err != nil {
			return
		}
		str := func(k string) string { s, _ := req.Params[k].(string); return s }
		f.mu.Lock()
		var out any
		var herr *HerdrError
		switch req.Method {
		case "workspace.create":
			f.next++
			ws := fmt.Sprintf("wR%d", f.next)
			f.startCwd = str("cwd")
			out = map[string]any{
				"workspace": map[string]any{"workspace_id": ws},
				"root_pane": map[string]any{"pane_id": ws + ":p1"},
			}
		case "agent.start":
			f.startKind = str("kind")
			f.startArgs = nil
			if raw, ok := req.Params["args"].([]any); ok {
				for _, a := range raw {
					f.startArgs = append(f.startArgs, fmt.Sprint(a))
				}
			}
			if f.startErr != nil {
				herr = f.startErr
			} else {
				f.paneStatus = "idle"
				out = map[string]any{}
			}
		case "agent.prompt":
			f.promptPane = str("target")
			f.promptText = str("text")
			f.paneStatus = "working"
			f.paneScreen = f.promptText
			out = map[string]any{}
		case "pane.get":
			out = map[string]any{"pane": map[string]any{
				"pane_id": str("pane_id"), "agent": f.startKind,
				"agent_status": f.paneStatus, "interactive_ready": true,
			}}
		case "pane.read":
			out = map[string]any{"read": map[string]any{"text": f.paneScreen}}
		default:
			herr = &HerdrError{Code: "unexpected_method", Message: req.Method}
		}
		f.mu.Unlock()
		resp := map[string]any{"id": req.ID}
		if herr != nil {
			resp["error"] = herr
		} else {
			resp["result"] = out
		}
		json.NewEncoder(server).Encode(resp)
	}()
	return client, nil
}

func fakeAgentBins(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"claude", "codex", "agy"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestSendExternalViaHerdrResumesAndPrompts(t *testing.T) {
	fakeAgentBins(t)
	cases := []struct {
		id, kind, session string
		wantArgs          []string
	}{
		{"ag-s1", "agy", "11111111-1111-1111-1111-111111111111", []string{"--conversation", "11111111-1111-1111-1111-111111111111"}},
		{"cx-s2", "codex", "22222222-2222-2222-2222-222222222222", []string{"resume", "22222222-2222-2222-2222-222222222222"}},
		{"cc-s3", "claude", "33333333-3333-3333-3333-333333333333", []string{"--resume", "33333333-3333-3333-3333-333333333333"}},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			fh := &fakeHerdrResume{}
			h := newHerdr("", "test", t.TempDir())
			h.local.conn.dial = fh.dial
			s := &Server{home: t.TempDir(), herdr: h}

			externalCache.Lock()
			externalCache.at = time.Now()
			externalCache.list = []*External{{
				ID: tc.id, Kind: tc.kind, Cwd: "/proj/demo", Surface: surfaceCLI,
				Session: tc.session, CanSend: true, SendVia: "herdr", CanTakeOver: true, desktop: true,
			}}
			externalCache.Unlock()
			t.Cleanup(forgetExternal)

			r := httptest.NewRequest("POST", "/api/external/"+tc.id+"/send", strings.NewReader(`{"text":"继续修完"}`))
			r.SetPathValue("id", tc.id)
			w := httptest.NewRecorder()
			s.sendExternal(w, r)

			if w.Code != 200 {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			var body struct {
				OK          bool   `json:"ok"`
				PaneID      string `json:"pane_id"`
				WorkspaceID string `json:"workspace_id"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if !body.OK || body.PaneID != "wR1:p1" || body.WorkspaceID != "wR1" {
				t.Fatalf("body = %+v", body)
			}
			if fh.startKind != tc.kind || fh.startCwd != "/proj/demo" || !slices.Equal(fh.startArgs, tc.wantArgs) {
				t.Fatalf("start = (%q, %q, %v), want (%q, /proj/demo, %v)", fh.startKind, fh.startCwd, fh.startArgs, tc.kind, tc.wantArgs)
			}
			if fh.promptPane != "wR1:p1" || fh.promptText != "继续修完" {
				t.Fatalf("prompt = (%q, %q), want (wR1:p1, 继续修完)", fh.promptPane, fh.promptText)
			}
		})
	}
}

func TestSendExternalViaHerdrNotReadyReturns202WithPane(t *testing.T) {
	fakeAgentBins(t)
	fh := &fakeHerdrResume{startErr: &HerdrError{Code: "agent_not_ready", Message: "dialog"}}
	h := newHerdr("", "test", t.TempDir())
	h.local.conn.dial = fh.dial
	s := &Server{home: t.TempDir(), herdr: h}

	externalCache.Lock()
	externalCache.at = time.Now()
	externalCache.list = []*External{{
		ID: "ag-s1", Kind: "agy", Cwd: "/proj/demo", Surface: surfaceCLI,
		Session: "11111111-1111-1111-1111-111111111111", CanSend: true, SendVia: "herdr", CanTakeOver: true, desktop: true,
	}}
	externalCache.Unlock()
	t.Cleanup(forgetExternal)

	r := httptest.NewRequest("POST", "/api/external/ag-s1/send", strings.NewReader(`{"text":"继续"}`))
	r.SetPathValue("id", "ag-s1")
	w := httptest.NewRecorder()
	s.sendExternal(w, r)

	if w.Code != 202 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var body struct {
		OK     bool   `json:"ok"`
		PaneID string `json:"pane_id"`
		Note   string `json:"note"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || body.PaneID != "wR1:p1" || body.Note == "" {
		t.Fatalf("body = %+v, want ok + pane_id + note", body)
	}
}
