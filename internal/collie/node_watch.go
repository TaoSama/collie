package collie

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

// Each node watches its own herdr, where asking costs nothing, and sends
// its workspaces, panes and agents to the hub as they change. What the hub
// shows is then at hand, instead of a trip across the internet to every
// computer for each list, and a change reaches the browser at once.

const (
	watchEvery = 150 * time.Millisecond // how often a node looks at its herdr
	watchBeat  = 10 * time.Second       // resent unchanged this often, to show it is current
	watchStale = 3 * watchBeat / 2      // a snapshot older than this is not used
)

// watchLists (node): the three lists as one JSON line now, and again on each
// change (or beat). {"error": …} when herdr can't be read.
func (s *Server) watchLists(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(200)
	f.Flush()
	var (
		last []byte
		sent time.Time
	)
	for {
		line := s.herdr.listsLine(r.Context())
		if !bytes.Equal(line, last) || time.Since(sent) >= watchBeat {
			if _, err := w.Write(line); err != nil {
				return
			}
			f.Flush()
			last, sent = line, time.Now()
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(watchEvery):
		}
	}
}

// listsLine reads the lists, keeping only what the hub shows: a pane's
// output revision, scrollback and focus change all the time and would make
// every look a change to send.
func (h *Herdr) listsLine(ctx context.Context) []byte {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	fail := func(err error) []byte {
		b, _ := json.Marshal(map[string]string{"error": err.Error()})
		return append(b, '\n')
	}
	ws, err := h.Workspaces(ctx)
	if err != nil {
		return fail(err)
	}
	panes, err := h.Panes(ctx)
	if err != nil {
		return fail(err)
	}
	agents, err := h.Agents(ctx)
	if err != nil {
		return fail(err)
	}
	for i := range ws {
		ws[i].Focused = false
	}
	for i := range panes {
		panes[i].Revision, panes[i].Focused, panes[i].Scroll = 0, false, nil
	}
	out := map[string]any{
		"workspace.list": map[string]any{"workspaces": ws},
		"pane.list":      map[string]any{"panes": panes},
		"agent.list":     map[string]any{"agents": agents},
	}
	b, _ := json.Marshal(out)
	return append(b, '\n')
}

// listSnap is the latest lists a node sent, with its ids suffixed.
type listSnap struct {
	mu    sync.Mutex
	at    time.Time
	lists map[string]json.RawMessage
}

func (s *listSnap) get(method string) (json.RawMessage, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.lists[method]
	return raw, ok && time.Since(s.at) < watchStale
}

// set stores a line and reports whether the lists changed.
func (s *listSnap) set(lists map[string]json.RawMessage) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := len(lists) != len(s.lists)
	for k, v := range lists {
		changed = changed || !bytes.Equal(v, s.lists[k])
	}
	s.lists, s.at = lists, time.Now()
	return changed
}

var errNoWatch = errors.New("node does not send its lists")

// watch follows a node's lists until ctx ends (it disconnects). A node from
// before this stops it; the sidebar then asks that node as it always did.
func (h *Herdr) watch(ctx context.Context, m *Machine) {
	for {
		err := h.watchOnce(ctx, m)
		m.snap.set(nil)
		h.listsChanged()
		if errors.Is(err, errNoWatch) || !h.attached(m) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (h *Herdr) attached(m *Machine) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.remote[m.ID] == m && m.Online
}

func (h *Herdr) watchOnce(ctx context.Context, m *Machine) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://node/api/watch", nil)
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errNoWatch
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var lists map[string]json.RawMessage
		if json.Unmarshal(sc.Bytes(), &lists) != nil || lists["error"] != nil {
			if m.snap.set(nil) {
				h.listsChanged()
			}
			continue
		}
		for k, v := range lists {
			lists[k] = suffixIDs(v, m.ID)
		}
		if m.snap.set(lists) {
			h.listsChanged()
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("watch ended")
}

// listsChanged tells the state streams that a computer's lists changed.
func (h *Herdr) listsChanged() { h.listWake.wake("") }

// listsUpdates gets a value when any computer's lists change; stop ends it.
func (h *Herdr) listsUpdates() (updates <-chan struct{}, stop func()) {
	return h.listWake.subscribe("")
}

type cachedListsKey struct{}

// withCachedLists lets list calls under ctx use the lists nodes sent instead
// of asking them: for showing state, where a quarter second old is fine. Code
// that acts on what it finds (starting the Router, naming agents) asks.
func withCachedLists(ctx context.Context) context.Context {
	return context.WithValue(ctx, cachedListsKey{}, true)
}

func cachedLists(ctx context.Context) bool {
	v, _ := ctx.Value(cachedListsKey{}).(bool)
	return v
}
