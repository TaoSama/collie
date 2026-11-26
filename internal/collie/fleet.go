package collie

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// A Machine is one computer whose herdr Collie can drive: the hub's own
// machine, or a computer whose node has dialed in (see node.go).
type Machine struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	OS   string `json:"os,omitempty"`
	// Version of collie it runs; "" for nodes from before v0.2.5.
	Version string `json:"version,omitempty"`
	// Agents it has installed (herdr kinds); nil when not known.
	Agents []string  `json:"agents"`
	Home   string    `json:"home,omitempty"`
	Bin    string    `json:"-"`
	Local  bool      `json:"local"`
	Online bool      `json:"online"`
	Since  time.Time `json:"since,omitempty"`

	conn   *herdrConn
	socket string                                      // local only: herdr API socket path
	ext    func(ctx context.Context) (net.Conn, error) // remote only: an extension's stream (ext_proc.go)
	client *http.Client                                // remote only: the node's own API, through the tunnel
	snap   *listSnap                                   // remote only: the lists the node last sent (node_watch.go)
	ip     string                                      // remote only: where it connects from
	ui     string                                      // remote only: its local web UI (http://127.0.0.1:port), if any
	close  func()
}

// Herdr is the herdr API across every connected machine. It speaks herdr's
// own protocol, so callers never care where a pane lives:
//
//   - IDs on remote machines carry a suffix: pane "w1:p1" on machine "mac"
//     is "w1:p1@mac". IDs on the hub's own machine stay bare.
//   - Calls that name a pane, workspace or agent go to that machine, with the
//     suffix removed; IDs in the result get the suffix added back.
//   - List calls fan out to every machine and merge.
//   - workspace.create takes an extra "_machine" parameter.
type Herdr struct {
	mu       sync.RWMutex
	local    *Machine
	remote   map[string]*Machine
	names    map[string]string // agent name → its pane id, suffixed on remote machines
	onChange func()
	// logged finds a prompt in the agent's own log (Server.promptLogged);
	// nil leaves Prompt with the screen alone.
	logged func(ctx context.Context, pane, probe string, since int64) (size int64, found, ok bool)

	// named remembers the names Collie gave the agents it started. herdr 0.8
	// drops an agent's name once the launch is confirmed; newer versions keep
	// it, and their answer wins.
	named     map[string]string // pane id → name
	saveNames func(map[string]string)

	// runs: the model and effort Collie started an agent with ("" = the
	// agent's own default), by pane id. What it runs now may differ if the
	// human switched models inside the agent.
	runs     map[string][2]string
	saveRuns func(map[string][2]string)

	asks     map[string]askCheck  // pane → recent look at an "idle" screen
	unfolded map[string]time.Time // pane → when Codex's queued question was last opened

	listWake paneWake // the state streams, woken when a node's lists change
}

func newHerdr(socket, hostname, home string) *Herdr {
	return &Herdr{
		local:    &Machine{ID: "", Name: hostname, OS: runtime.GOOS + "/" + runtime.GOARCH, Home: home, Local: true, socket: socket, conn: &herdrConn{dial: unixDialer(socket)}},
		remote:   map[string]*Machine{},
		names:    map[string]string{},
		named:    map[string]string{},
		runs:     map[string][2]string{},
		asks:     map[string]askCheck{},
		unfolded: map[string]time.Time{},
	}
}

// LoadNames restores remembered agent names and sets how to persist them.
func (h *Herdr) LoadNames(m map[string]string, save func(map[string]string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, v := range m {
		h.named[k] = v
	}
	h.saveNames = save
}

// Remember records the name of an agent Collie started in pane.
func (h *Herdr) Remember(pane, name string) {
	if pane == "" || name == "" {
		return
	}
	h.mu.Lock()
	h.named[pane] = name
	h.names[name] = pane
	h.persistNamesLocked()
	h.mu.Unlock()
}

// LoadRuns restores the recorded launch models and sets how to persist them.
func (h *Herdr) LoadRuns(m map[string][2]string, save func(map[string][2]string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, v := range m {
		h.runs[k] = v
	}
	h.saveRuns = save
}

// RememberRun records what model and effort an agent in pane was started with.
func (h *Herdr) RememberRun(pane, model, effort string) {
	if pane == "" {
		return
	}
	h.mu.Lock()
	h.runs[pane] = [2]string{model, effort}
	h.persistRunsLocked()
	h.mu.Unlock()
}

// Run returns what RememberRun recorded for pane.
func (h *Herdr) Run(pane string) (*[2]string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	r, ok := h.runs[pane]
	if !ok {
		return nil, false
	}
	return &r, true
}

// pruneRuns forgets panes that are gone (herdr reuses pane ids), on the
// machines it has a full list from.
func (h *Herdr) pruneRuns(alive map[string]bool, listed func(machine string) bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := len(h.runs)
	for p := range h.runs {
		if _, m := split(p); !alive[p] && listed(m) {
			delete(h.runs, p)
		}
	}
	if len(h.runs) != n {
		h.persistRunsLocked()
	}
}

func (h *Herdr) persistRunsLocked() {
	if h.saveRuns == nil {
		return
	}
	cp := make(map[string][2]string, len(h.runs))
	for k, v := range h.runs {
		cp[k] = v
	}
	h.saveRuns(cp)
}

func (h *Herdr) persistNamesLocked() {
	if h.saveNames == nil {
		return
	}
	cp := make(map[string]string, len(h.named))
	for k, v := range h.named {
		cp[k] = v
	}
	h.saveNames(cp)
}

// split turns "w1:p1@mac" into ("w1:p1", "mac").
func split(id string) (local, machine string) {
	if i := strings.LastIndexByte(id, '@'); i >= 0 {
		return id[:i], id[i+1:]
	}
	return id, ""
}

func join(id, machine string) string {
	if machine == "" || id == "" {
		return id
	}
	return id + "@" + machine
}

func (h *Herdr) machine(id string) (*Machine, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if id == "" {
		return h.local, nil
	}
	m := h.remote[id]
	if m == nil || !m.Online {
		return nil, &HerdrError{Code: "machine_offline", Message: fmt.Sprintf("machine %q is not connected", id)}
	}
	return m, nil
}

// Machines lists the hub's machine first, then the others by name.
func (h *Herdr) Machines() []Machine {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := []Machine{*h.local}
	out[0].Agents = localAgents(h.local.Home, false)
	var rs []Machine
	for _, m := range h.remote {
		rs = append(rs, *m)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Name < rs[j].Name })
	return append(out, rs...)
}

func (h *Herdr) attach(m *Machine) {
	h.mu.Lock()
	if old := h.remote[m.ID]; old != nil && old.close != nil {
		old.close()
	}
	h.remote[m.ID] = m
	cb := h.onChange
	h.mu.Unlock()
	if cb != nil {
		cb()
	}
}

func (h *Herdr) detach(id string, m *Machine) {
	h.mu.Lock()
	if cur := h.remote[id]; cur == m || m == nil {
		if cur != nil {
			cur.Online = false
			if cur.close != nil && m == nil {
				cur.close()
			}
		}
		if m == nil {
			delete(h.remote, id)
		}
	}
	cb := h.onChange
	h.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// Client returns the HTTP client for a remote machine's node API.
func (h *Herdr) Client(machine string) (*http.Client, error) {
	m, err := h.machine(machine)
	if err != nil {
		return nil, err
	}
	if m.client == nil {
		return nil, fmt.Errorf("machine %q has no node API", machine)
	}
	return m.client, nil
}

var routeKeys = []string{"pane_id", "workspace_id", "tab_id", "target"}

// Call routes one herdr request and decodes its result into out.
func (h *Herdr) Call(ctx context.Context, method string, params any, out any) error {
	raw, err := h.call(ctx, method, params)
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (h *Herdr) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	switch method {
	case "workspace.list", "pane.list", "agent.list":
		return h.fanOut(ctx, method)
	}
	p := map[string]any{}
	if params != nil {
		b, _ := json.Marshal(params)
		json.Unmarshal(b, &p)
	}
	target, found := "", false
	if v, ok := p["_machine"].(string); ok {
		target, found = v, true
		delete(p, "_machine")
	}
	for _, k := range routeKeys {
		v, ok := p[k].(string)
		if !ok || v == "" {
			continue
		}
		if k == "target" && !strings.Contains(v, ":") {
			// An agent name. Older herdr (0.8) only resolves pane ids, and
			// the name may live on another machine: resolve it here.
			if pane := h.paneOfName(ctx, v); pane != "" {
				v = pane
			}
		}
		id, m := split(v)
		if found && m != target {
			return nil, &HerdrError{Code: "cross_machine", Message: "ids from different machines in one call"}
		}
		target, found = m, true
		p[k] = id
	}
	mc, err := h.machine(target)
	if err != nil {
		return nil, err
	}
	raw, err := mc.conn.call(ctx, method, p)
	if err != nil {
		return raw, err
	}
	if method == herdrPaneSendKeys || method == herdrPaneSendText {
		if id, _ := p["pane_id"].(string); id != "" {
			h.mu.Lock()
			delete(h.asks, id)
			delete(h.asks, join(id, target))
			h.mu.Unlock()
		}
	}
	if target != "" {
		raw = suffixIDs(raw, target)
	}
	if method == "agent.get" || method == "pane.get" {
		key := map[string]string{"agent.get": "agent", "pane.get": "pane"}[method]
		var body map[string]map[string]any
		if json.Unmarshal(raw, &body) == nil && body[key] != nil {
			changed := h.fixStatus(ctx, body[key])
			if pane, _ := body[key]["pane_id"].(string); body[key]["name"] == nil && h.namedFor(pane) != "" {
				body[key]["name"], changed = h.namedFor(pane), true
			}
			if changed {
				raw, _ = json.Marshal(body)
			}
		}
	}
	return raw, nil
}

func (h *Herdr) namedFor(pane string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.named[pane]
}

// hasName: some agent has name, as of the last agent.list (the tick lists
// every couple of seconds).
func (h *Herdr) hasName(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.names[name]
	return ok
}

func (h *Herdr) paneOfName(ctx context.Context, name string) string {
	h.mu.RLock()
	p, ok := h.names[name]
	h.mu.RUnlock()
	if ok {
		return p
	}
	h.Agents(ctx) // refreshes the name index
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.names[name]
}

func machineOf(id string) string { _, m := split(id); return m }

// suffixIDs appends "@machine" to every id-like string in a herdr result.
func suffixIDs(raw json.RawMessage, machine string) json.RawMessage {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case map[string]any:
			for k, val := range t {
				if s, ok := val.(string); ok && s != "" && (k == "pane_id" || k == "workspace_id" || k == "tab_id") {
					t[k] = join(s, machine)
				} else {
					t[k] = walk(val)
				}
			}
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
		}
		return x
	}
	b, _ := json.Marshal(walk(v))
	return b
}
