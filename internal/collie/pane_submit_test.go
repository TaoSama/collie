package collie

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPaneSubmitUsesFreshPaneClassification(t *testing.T) {
	for _, tc := range []struct {
		name, kind, status   string
		down                 int
		code, prompts, texts int
		enters               int
	}{
		{name: "agent", kind: "claude", status: "idle", code: 200, prompts: 1},
		{name: "shell", status: "unknown", code: 200, texts: 1, enters: 1},
		{name: "lookup failed", down: 1, code: 502},
		{name: "unclassified", status: "working", code: 409},
		{name: "gone", code: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, pane := chatServer(t, tc.down, tc.kind)
			p := f.panes[pane]
			p.status, p.sim = tc.status, true
			if tc.name == "gone" {
				delete(f.panes, pane)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/panes/"+pane+"/submit", strings.NewReader(`{"text":"hello"}`))
			got := httptest.NewRecorder()
			s.NodeRoutes().ServeHTTP(got, req)
			if got.Code != tc.code {
				t.Errorf("status %d, want %d: %s", got.Code, tc.code, got.Body.String())
			}
			if n := f.count("pane.get"); n != 1 && tc.down == 0 {
				t.Errorf("pane.get calls = %d, want 1", n)
			}
			if n := f.count("agent.prompt"); n != tc.prompts {
				t.Errorf("prompt calls = %d, want %d", n, tc.prompts)
			}
			if n := f.count("pane.send_text"); n != tc.texts {
				t.Errorf("text calls = %d, want %d", n, tc.texts)
			}
			if n := f.count("pane.send_keys"); n != tc.enters {
				t.Errorf("key calls = %d, want %d", n, tc.enters)
			}
			if tc.code == 200 && (len(p.said) != 1 || p.said[0] != "hello") {
				t.Errorf("submitted messages = %q, want a single hello", p.said)
			}
		})
	}
}

func TestRemotePaneSubmitDoesNotRepeatAnUncertainWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		sent bool
	}{
		{name: "new node", code: 200, sent: true},
		{name: "node without route", code: 200, sent: true},
		{name: "pane gone", code: 404},
		{name: "response lost", code: 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, f, pane := chatServer(t, 0, "")
			p := f.panes[pane]
			p.status, p.sim = "unknown", true

			api := node.NodeRoutes()
			switch tc.name {
			case "node without route":
				api = http.NewServeMux()
			case "pane gone":
				delete(f.panes, pane)
			case "response lost":
				api = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Error(w, "response lost", http.StatusBadGateway)
				})
			}
			h, _ := remoteNode(t, f, api, tc.name != "new node")
			hub := &Server{herdr: h}
			mux := http.NewServeMux()
			hub.machineRoutes(mux, func(h http.HandlerFunc) http.Handler { return hub.routed(h) })
			req := httptest.NewRequest(http.MethodPost, "/api/panes/"+pane+"@mac/submit", strings.NewReader(`{"text":"hello"}`))
			got := httptest.NewRecorder()
			mux.ServeHTTP(got, req)
			if got.Code != tc.code {
				t.Errorf("status %d, want %d: %s", got.Code, tc.code, got.Body.String())
			}
			wantWrites := 0
			if tc.sent {
				wantWrites = 1
			}
			if len(p.said) != wantWrites || f.count("pane.send_text") != wantWrites {
				t.Errorf("submitted %q with %d writes, want %d writes", p.said, f.count("pane.send_text"), wantWrites)
			}
			wantLookups := 1
			if tc.name == "response lost" {
				wantLookups = 0
			}
			if n := f.count("pane.get"); n != wantLookups {
				t.Errorf("pane.get calls = %d, want %d", n, wantLookups)
			}
		})
	}
}
