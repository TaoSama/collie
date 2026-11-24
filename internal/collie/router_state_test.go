package collie

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStateDistinguishesRouterFromWorkspaceNamedCollie(t *testing.T) {
	const projectPane = "w1:p1"
	const routerPane = "w2:p1"
	claude, router := "claude", "collie"
	replies := map[string]any{
		"workspace.list": map[string][]Workspace{
			"workspaces": {
				{ID: "w1", Label: "collie", Number: 1},
				{ID: "w2", Label: "collie · router", Number: 2},
			},
		},
		"pane.list": map[string][]Pane{
			"panes": {
				{ID: projectPane, WorkspaceID: "w1", Agent: &claude, AgentStatus: "working"},
				{ID: routerPane, WorkspaceID: "w2", Agent: &claude, AgentStatus: "working"},
			},
		},
		"agent.list": map[string][]Agent{
			"agents": {
				{PaneID: projectPane, WorkspaceID: "w1", Agent: &claude, AgentStatus: "working"},
				{PaneID: routerPane, WorkspaceID: "w2", Name: &router, Agent: &claude, AgentStatus: "working"},
			},
		},
	}

	herdr := newHerdr("", "test", t.TempDir())
	herdr.local.conn.dial = func(context.Context) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			var request struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			if err := json.NewDecoder(server).Decode(&request); err != nil {
				t.Errorf("read mock herdr request: %v", err)
				return
			}
			response := map[string]any{"id": request.ID}
			if reply, ok := replies[request.Method]; ok {
				response["result"] = reply
			} else {
				response["error"] = HerdrError{Code: "unexpected_method", Message: request.Method}
			}
			if err := json.NewEncoder(server).Encode(response); err != nil {
				t.Errorf("write mock herdr response: %v", err)
			}
		}()
		return client, nil
	}
	s := &Server{herdr: herdr, home: herdr.local.Home, node: true}
	body := httptest.NewRecorder()
	s.NodeRoutes().ServeHTTP(body, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if body.Code != http.StatusOK {
		t.Fatalf("GET /api/state = %d: %s", body.Code, body.Body)
	}
	var state struct {
		OK     bool `json:"ok"`
		Agents []struct {
			PaneID   string `json:"pane_id"`
			Name     string `json:"name"`
			IsRouter *bool  `json:"is_router"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(body.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if !state.OK || len(state.Agents) != 2 {
		t.Fatalf("expected two agents in state: %s", body.Body)
	}
	want := map[string]bool{projectPane: false, routerPane: true}
	for _, agent := range state.Agents {
		isRouter, ok := want[agent.PaneID]
		if !ok {
			t.Errorf("unexpected or duplicate pane %q", agent.PaneID)
			continue
		}
		if agent.Name != router {
			t.Errorf("pane %q name = %q, want %q", agent.PaneID, agent.Name, router)
		}
		if agent.IsRouter == nil {
			t.Errorf("pane %q is missing is_router", agent.PaneID)
		} else if *agent.IsRouter != isRouter {
			t.Errorf("pane %q is_router = %t, want %t", agent.PaneID, *agent.IsRouter, isRouter)
		}
		delete(want, agent.PaneID)
	}
}

func TestEndedRouterRole(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want bool
	}{
		{"ordinary session in workspace named collie", `{"name":"collie","is_router":false}`, false},
		{"router with explicit role", `{"name":"collie","is_router":true}`, true},
		{"router from older history", `{"name":"collie"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ended Ended
			if err := json.Unmarshal([]byte(tc.data), &ended); err != nil {
				t.Fatal(err)
			}
			if got := ended.isRouter(); got != tc.want {
				t.Errorf("isRouter() = %t, want %t", got, tc.want)
			}
		})
	}
}
