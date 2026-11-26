package collie

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
)

// An agent logs what it was sent the moment it takes it: Claude writes the
// user line, or a queue-operation line while busy; Codex its rollout.

const (
	promptLogMax  = 8 << 20 // the most of a log one look reads
	promptLogTail = 4 << 20 // how far back a look after a restart reads
)

// promptLogged finds probe in the log of the agent in pane, in what was
// written past offset since (negative: that many bytes from the end). size
// is where the log ends now; ok is false when there is no log to read.
func (s *Server) promptLogged(ctx context.Context, pane, probe string, since int64) (size int64, found, ok bool) {
	if local, machine := split(pane); machine != "" {
		return s.nodeLogged(ctx, machine, local, probe, since)
	}
	var info struct {
		Agent agentDetail `json:"agent"`
	}
	if s.herdr.Call(ctx, "agent.get", map[string]any{"target": pane}, &info) != nil || deref(info.Agent.Agent) == "agy" {
		return 0, false, false // agy's log isn't the JSON it was sent
	}
	tr := s.transcriptFor("logged:"+pane, pane, info.Agent, map[string]*transcript{})
	if tr == nil {
		return 0, false, false
	}
	return logHas(tr.path, probe, since)
}

// nodeLogged asks the node of machine, where that agent's log lives.
func (s *Server) nodeLogged(ctx context.Context, machine, pane, probe string, since int64) (int64, bool, bool) {
	c, err := s.herdr.Client(machine)
	if err != nil {
		return 0, false, false
	}
	var out struct {
		Size  int64 `json:"size"`
		Found bool  `json:"found"`
	}
	p := "/api/panes/" + url.PathEscape(pane) + "/logged?since=" + strconv.FormatInt(since, 10) + "&probe=" + url.QueryEscape(probe)
	if nodeJSON(ctx, c, "GET", p, nil, &out) != nil {
		return 0, false, false
	}
	return out.Size, out.Found, true
}

// paneLogged serves promptLogged for this machine's panes.
func (s *Server) paneLogged(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	size, found, ok := s.promptLogged(r.Context(), r.PathValue("id"), q.Get("probe"), since)
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "no log"})
		return
	}
	writeJSON(w, 200, map[string]any{"size": size, "found": found})
}

// logHas reads path from since (see promptLogged) and looks for probe as a
// JSON log writes it.
func logHas(path, probe string, since int64) (size int64, found, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, false, false
	}
	size = fi.Size()
	if probe == "" {
		return size, false, true
	}
	switch {
	case since < 0:
		since = max(0, size+since)
	case since > size: // rewritten since: read it all
		since = 0
	}
	if _, err := f.Seek(since, io.SeekStart); err != nil {
		return size, false, false
	}
	b, err := io.ReadAll(io.LimitReader(f, promptLogMax))
	if err != nil {
		return size, false, false
	}
	return size, bytes.Contains(b, []byte(jsonInner(probe))), true
}
