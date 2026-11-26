package collie

import (
	"context"
	"net/http"
	"sort"
	"time"
)

type AgentView struct {
	PaneID         string     `json:"pane_id"`
	WorkspaceID    string     `json:"workspace_id"`
	WorkspaceLabel string     `json:"workspace_label"`
	Name           string     `json:"name"`
	Kind           string     `json:"kind"`
	Status         string     `json:"status"`
	Cwd            string     `json:"cwd"`
	Title          string     `json:"title"`
	Topic          string     `json:"topic,omitempty"`   // the session's own title
	Session        string     `json:"session,omitempty"` // id to resume it by
	IsAgent        bool       `json:"is_agent"`
	IsRouter       bool       `json:"is_router"`
	Machine        string     `json:"machine"`
	Run            *[2]string `json:"run,omitempty"` // model, effort: its latest reply's, else what Collie started it with
	Context        *ctxUsage  `json:"context,omitempty"`
	HumanAt        time.Time  `json:"human_at,omitzero"`   // the human's last message in it, else its start
	Archived       bool       `json:"archived,omitempty"`  // its agent's own archive has it (archived.go)
	Scheduled      bool       `json:"scheduled,omitempty"` // has an active scheduled task / automation
	Pinned         bool       `json:"pinned,omitempty"`    // pinned in its agent's own sidebar
}

type PaneState struct {
	OK       bool        `json:"ok"`
	Error    string      `json:"error,omitempty"`
	Agents   []AgentView `json:"agents"`
	Shells   []AgentView `json:"shells"`
	Machines []Machine   `json:"machines"`
}

func (s *Server) state(ctx context.Context) PaneState {
	ctx, cancel := context.WithTimeout(withCachedLists(ctx), 5*time.Second)
	defer cancel()
	machines := s.herdr.Machines()
	ws, err := s.herdr.Workspaces(ctx)
	if err != nil {
		return PaneState{Error: err.Error(), Agents: []AgentView{}, Shells: []AgentView{}, Machines: machines}
	}
	panes, err := s.herdr.Panes(ctx)
	if err != nil {
		return PaneState{Error: err.Error(), Agents: []AgentView{}, Shells: []AgentView{}, Machines: machines}
	}
	agents, _ := s.herdr.Agents(ctx)

	labels := map[string]string{}
	order := map[string]int{}
	for _, w := range ws {
		labels[w.ID] = w.Label
		order[w.ID] = w.Number
	}
	names := map[string]string{}
	var routerPane string
	router := routerName
	if s.sup != nil {
		router = s.sup.routerAgent()
	}
	for _, a := range agents {
		names[a.PaneID] = deref(a.Name)
		if names[a.PaneID] == router {
			routerPane = a.PaneID
		}
	}

	st := PaneState{OK: true, Agents: []AgentView{}, Shells: []AgentView{}, Machines: machines}
	topics := map[string]map[string]paneSession{"": s.titlesFor("", ctx)}
	for _, m := range machines {
		if !m.Local && m.Online {
			topics[m.ID] = s.titlesFor(m.ID, ctx)
		}
	}
	for _, p := range panes {
		cwd := deref(p.ForegroundCwd)
		if cwd == "" {
			cwd = deref(p.Cwd)
		}
		v := AgentView{
			PaneID: p.ID, WorkspaceID: p.WorkspaceID, WorkspaceLabel: labels[p.WorkspaceID],
			Kind: deref(p.Agent), Status: p.AgentStatus, Cwd: cwd, Title: deref(p.Title),
			Name: names[p.ID], IsAgent: p.Agent != nil, IsRouter: p.ID == routerPane && p.Agent != nil,
		}
		var local string
		local, v.Machine = split(p.ID)
		if v.IsAgent {
			ps := topics[v.Machine][local]
			v.Topic, v.Session, v.Context, v.HumanAt, v.Archived, v.Scheduled, v.Pinned = ps.Title, ps.Session, ps.Context, ps.HumanAt, ps.Archived, ps.Scheduled, ps.Pinned
			if ps.Ran != nil {
				v.Run = ps.Ran // what it last replied with
			} else if r, ok := s.herdr.Run(p.ID); ok {
				v.Run = r // started from here
			} else {
				v.Run = ps.Run // started by that machine's own node
			}
		}
		if v.Name == "" {
			v.Name = v.WorkspaceLabel
		}
		if v.IsAgent {
			v.Status = s.seen.status(p.ID, v.Status)
		}
		if v.IsAgent && s.waiting(p.ID, v.Status) {
			v.Status = "waiting"
		}
		if v.IsAgent {
			st.Agents = append(st.Agents, v)
		} else {
			v.Status = "shell"
			st.Shells = append(st.Shells, v)
		}
	}
	less := func(list []AgentView) func(i, j int) bool {
		return func(i, j int) bool {
			if list[i].Machine != list[j].Machine {
				return list[i].Machine < list[j].Machine
			}
			if order[list[i].WorkspaceID] != order[list[j].WorkspaceID] {
				return order[list[i].WorkspaceID] < order[list[j].WorkspaceID]
			}
			return list[i].PaneID < list[j].PaneID
		}
	}
	alive := map[string]bool{}
	for _, p := range panes { // shells too: an agent may not show as one yet
		alive[p.ID] = true
	}
	online := map[string]bool{"": true}
	for _, m := range machines {
		online[m.ID] = m.Local || m.Online
	}
	s.herdr.pruneRuns(alive, func(m string) bool { return online[m] })
	sort.SliceStable(st.Agents, less(st.Agents))
	sort.SliceStable(st.Shells, less(st.Shells))
	return st
}

func (s *Server) getState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.state(r.Context()))
}

// streamState pushes the herdr session state and the super-agent view over one
// SSE connection. Browsers allow ~6 HTTP/1.1 connections per origin, so every
// stream a tab opens counts.
func (s *Server) streamState(w http.ResponseWriter, r *http.Request) {
	sse, ok := newSSE(w)
	if !ok {
		return
	}
	var ch chan struct{}
	if s.sup != nil {
		c, done := s.sup.subscribe()
		defer done()
		ch = c
	}
	var lastState, lastSuper []byte
	listsChanged, stopLists := s.herdr.listsUpdates()
	defer stopLists()
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	beat := time.NewTicker(15 * time.Second)
	defer beat.Stop()
	for {
		// Collie's chat first: it is quick, and a message just sent should
		// show before the slower look across every computer's sessions.
		if s.sup != nil {
			if _, err := sse.sendChangedJSON("super", s.superState(r.Context()), &lastSuper); err != nil {
				return
			}
		}
		if _, err := sse.sendChangedJSON("state", s.state(r.Context()), &lastState); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			sse.comment()
		case <-ch:
		case <-listsChanged:
		case <-tick.C:
		}
	}
}
