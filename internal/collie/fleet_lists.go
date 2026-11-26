package collie

import (
	"context"
	"encoding/json"
)

// fanOut runs a list call on every online machine and merges the results.
// The hub's machine failing is reported only when nothing else answered.
func (h *Herdr) fanOut(ctx context.Context, method string) (json.RawMessage, error) {
	raw, _, err := h.fanOutFrom(ctx, method)
	return raw, err
}

// AgentsListed is Agents with the machines that answered: an agent missing
// from the list may still run on the others.
func (h *Herdr) AgentsListed(ctx context.Context) ([]Agent, map[string]bool, error) {
	raw, answered, err := h.fanOutFrom(ctx, "agent.list")
	if err != nil {
		return nil, nil, err
	}
	var r struct {
		Agents []Agent `json:"agents"`
	}
	err = json.Unmarshal(raw, &r)
	return r.Agents, answered, err
}

func (h *Herdr) fanOutFrom(ctx context.Context, method string) (json.RawMessage, map[string]bool, error) {
	key := map[string]string{"workspace.list": "workspaces", "pane.list": "panes", "agent.list": "agents"}[method]
	ms := h.Machines()
	type res struct {
		id    string
		items []json.RawMessage
		err   error
	}
	ch := make(chan res, len(ms))
	n := 0
	for _, m := range ms {
		if !m.Local && !m.Online {
			continue
		}
		n++
		go func(m Machine) {
			mc, err := h.machine(m.ID)
			if err != nil {
				ch <- res{id: m.ID, err: err}
				return
			}
			// A node's own lists already carry the corrected statuses.
			raw, fresh := json.RawMessage(nil), false
			if cachedLists(ctx) {
				raw, fresh = mc.snap.get(method)
			}
			if !fresh {
				if raw, err = mc.conn.call(ctx, method, nil); err != nil {
					ch <- res{id: m.ID, err: err}
					return
				}
				if m.ID != "" {
					raw = suffixIDs(raw, m.ID)
				}
			}
			var body map[string][]json.RawMessage
			json.Unmarshal(raw, &body)
			items := body[key]
			if !fresh && (method == "agent.list" || method == "pane.list") {
				for i, it := range items {
					var a map[string]any
					if json.Unmarshal(it, &a) == nil && h.fixStatus(ctx, a) {
						items[i], _ = json.Marshal(a)
					}
				}
			}
			ch <- res{id: m.ID, items: items}
		}(m)
	}
	merged := []json.RawMessage{}
	var localErr error
	ok := 0
	byMachine := map[string][]json.RawMessage{}
	for i := 0; i < n; i++ {
		r := <-ch
		if r.err != nil {
			if r.id == "" {
				localErr = r.err
			}
			continue
		}
		ok++
		byMachine[r.id] = r.items
	}
	h.mu.Lock()
	h.local.Online = localErr == nil
	h.mu.Unlock()
	if ok == 0 && localErr != nil {
		return nil, nil, localErr
	}
	for _, m := range ms { // stable order: hub machine first
		merged = append(merged, byMachine[m.ID]...)
	}
	if method == "agent.list" {
		names := map[string]string{}
		h.mu.Lock()
		for i, it := range merged {
			var a map[string]any
			if json.Unmarshal(it, &a) != nil {
				continue
			}
			pane, _ := a["pane_id"].(string)
			name, _ := a["name"].(string)
			if name == "" && h.named[pane] != "" {
				name = h.named[pane]
				a["name"] = name
				merged[i], _ = json.Marshal(a)
			}
			if name != "" && pane != "" {
				names[name] = pane
			}
		}
		h.names = names
		h.mu.Unlock()
	}
	if method == "pane.list" {
		// Forget names of panes that are gone, on machines that answered.
		live := map[string]bool{}
		for _, it := range merged {
			var p struct {
				ID string `json:"pane_id"`
			}
			if json.Unmarshal(it, &p) == nil {
				live[p.ID] = true
			}
		}
		h.mu.Lock()
		changed := false
		for pane := range h.named {
			if _, ok := byMachine[machineOf(pane)]; ok && !live[pane] {
				delete(h.named, pane)
				changed = true
			}
		}
		if changed {
			h.persistNamesLocked()
		}
		h.mu.Unlock()
	}
	answered := make(map[string]bool, len(byMachine))
	for id := range byMachine {
		answered[id] = true
	}
	raw, err := json.Marshal(map[string]any{key: merged})
	return raw, answered, err
}
