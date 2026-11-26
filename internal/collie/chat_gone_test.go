package collie

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// flakyDial fails the next `down` herdr calls, like a node link that stalls
// for a moment, and passes the rest to f.
type flakyDial struct {
	mu   sync.Mutex
	down int
	f    *fakeHerdr
}

func (d *flakyDial) dial(ctx context.Context) (net.Conn, error) {
	d.mu.Lock()
	if d.down > 0 {
		d.down--
		d.mu.Unlock()
		return nil, errors.New("stream open timeout")
	}
	d.mu.Unlock()
	return d.f.dial(ctx)
}

// paneChatEvents opens a pane's chat stream and returns the event names it sends
// within d; a meta event carries its data.
func paneChatEvents(t *testing.T, s *Server, pane string, d time.Duration) []string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/panes/{id}/chat", s.streamChat)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/panes/"+url.PathEscape(pane)+"/chat", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var evs []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if ev, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
			evs = append(evs, ev)
		} else if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok && evs[len(evs)-1] == "meta" {
			evs[len(evs)-1] += " " + data
		}
	}
	return evs
}

func chatServer(t *testing.T, down int, kind string) (*Server, *fakeHerdr, string) {
	f := newFakeHerdr(t)
	pane := f.add("w1", "worker", "worker", kind, "/tmp")
	f.panes[pane].screen = "$ "
	h := newHerdr("", "test", t.TempDir())
	h.local.conn.dial = (&flakyDial{down: down, f: f}).dial
	return &Server{herdr: h}, f, pane
}

func TestChatStreamSurvivesFailedReads(t *testing.T) {
	// agent.get and the pane probe both fail once: the pane is still there.
	s, _, pane := chatServer(t, 2, "")
	evs := paneChatEvents(t, s, pane, 1500*time.Millisecond)
	if strings.Contains(strings.Join(evs, ","), "gone") {
		t.Fatalf("a failed read closed the stream as gone: %v", evs)
	}
	if !strings.Contains(strings.Join(evs, ","), "log") {
		t.Fatalf("stream never recovered after the failed read: %v", evs)
	}
}

func TestChatStreamGoneWhenPaneClosed(t *testing.T) {
	s, f, pane := chatServer(t, 0, "")
	delete(f.panes, pane)
	evs := paneChatEvents(t, s, pane, 1500*time.Millisecond)
	if len(evs) != 1 || evs[0] != "gone" {
		t.Fatalf("closed pane: got %v, want [gone]", evs)
	}
}

func TestChatStreamKeepsAgentThroughFailedRead(t *testing.T) {
	// The agent lookup fails once: the header must not drop to a shell.
	s, f, pane := chatServer(t, 1, "claude")
	f.panes[pane].status = "working"
	evs := paneChatEvents(t, s, pane, 1500*time.Millisecond)
	all := strings.Join(evs, ",")
	if strings.Contains(all, "gone") || strings.Contains(all, `"shell"`) || !strings.Contains(all, `"status":"working"`) {
		t.Fatalf("failed agent lookup: got %v", evs)
	}
}

func TestAgyDualTranscriptStaysIdle(t *testing.T) {
	s, f, pane := chatServer(t, 0, "agy")
	s.home = t.TempDir()
	const id = "11111111-1111-4111-8111-111111111111"
	short := filepath.Join(s.agyCLIHome(), "brain", id, agyTranscriptRelPath)
	if err := os.MkdirAll(filepath.Dir(short), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(short, []byte(`{"type":"USER_INPUT","content":"short"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(filepath.Dir(short), "transcript_full.jsonl")
	if err := os.WriteFile(full, []byte(`{"type":"USER_INPUT","content":"full"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.panes[pane].log = id

	events := paneChatEvents(t, s, pane, 2*time.Second)
	if polls := f.count("agent.get"); polls < 2 {
		t.Fatalf("need at least two idle polls, got %d", polls)
	}
	var historyEvents []string
	for _, event := range events {
		if event == "reset" || event == "patch" {
			historyEvents = append(historyEvents, event)
		}
	}
	if !slices.Equal(historyEvents, []string{"reset", "patch"}) {
		t.Fatalf("idle conversation sent history events %v, want one reset and one patch", historyEvents)
	}
}

func TestPaneEarlierRejectsReplacedTranscript(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		t.Run("in_place="+strconv.FormatBool(rewrite), func(t *testing.T) {
			paneEarlierRejectsReplacement(t, rewrite)
		})
	}
}

func paneEarlierRejectsReplacement(t *testing.T, rewrite bool) {
	t.Helper()
	s, f, pane := chatServer(t, 0, "claude")
	s.home = t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(s.home, ".claude"))
	logPath := filepath.Join(claudeRoot(s.home), "projects", "test", "id.jsonl")
	must(t, os.MkdirAll(filepath.Dir(logPath), 0o700))
	log := `{"type":"user","message":{"content":"same"}}` + "\n"
	must(t, os.WriteFile(logPath, []byte(log), 0o600))
	f.panes[pane].log = logPath

	tr := newTranscript(logPath, "claude")
	must(t, tr.load(t.Context(), nil, true))
	var sync msgSync
	first := sync.patch(tr)
	if first.Generation == "" {
		t.Fatal("live snapshot has no transcript generation")
	}
	page := func(generation string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/?before=40&generation="+generation, nil)
		req.SetPathValue("id", pane)
		w := httptest.NewRecorder()
		s.paneEarlier(w, req)
		return w
	}
	w := page(first.Generation)
	var body transcriptPage
	must(t, json.Unmarshal(w.Body.Bytes(), &body))
	if w.Code != 200 || body.Generation != first.Generation {
		t.Fatalf("page = %d %s", w.Code, w.Body)
	}
	appendFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	must(t, err)
	_, err = appendFile.WriteString(log)
	closeErr := appendFile.Close()
	must(t, err)
	must(t, closeErr)
	if got := page(first.Generation).Code; got != 200 {
		t.Fatalf("page after append = %d, want 200", got)
	}

	if rewrite {
		// WriteFile truncates the existing file without replacing its inode.
		log = strings.Replace(log, "same", "new", 1)
	} else {
		must(t, os.Rename(logPath, logPath+".old"))
	}
	must(t, os.WriteFile(logPath, []byte(log), 0o600))
	if got := page(first.Generation).Code; got != 409 {
		t.Fatalf("stale page status = %d, want 409", got)
	}
	if !tr.update() {
		t.Fatal("replacing the log did not mark it changed")
	}
	if next := sync.patch(tr); next == nil || next.Generation == first.Generation || next.From != next.Base {
		t.Fatalf("replacement patch = %+v", next)
	}
}

func TestCodexLastReplyNeedsReportedSession(t *testing.T) {
	s, f, pane := chatServer(t, 0, "codex")
	s.home = t.TempDir()
	t.Setenv("CODEX_HOME", s.home)
	log := strings.Replace(zstRollout, `"payload":{}`, `"payload":{"cwd":"/tmp"}`, 1)
	writeFiles(t, s.home, map[string]string{
		"session_index.jsonl":                       `{"id":"id","thread_name":"Topic"}` + "\n",
		"sessions/2026/11/26/rollout-date-id.jsonl": log,
	})
	f.panes[pane].title = "Topic | me"
	w := &Worker{Name: "worker", PaneID: pane}
	sp := &Super{s: s, transcripts: map[string]*transcript{}}
	remoteStatus := func() int {
		req := httptest.NewRequest("GET", "/?screen=0", nil)
		req.SetPathValue("id", pane)
		out := httptest.NewRecorder()
		s.lastMessage(out, req)
		return out.Code
	}
	if got := remoteStatus(); got != 404 {
		t.Fatalf("title-only remote reply status = %d, want 404", got)
	}
	if got := sp.lastReply(t.Context(), w, 0, false); got != "" {
		t.Fatalf("title-only local reply = %q", got)
	}
	f.panes[pane].log = "id.jsonl"
	if got := remoteStatus(); got != 200 {
		t.Fatalf("reported-session remote reply status = %d, want 200", got)
	}
	if got := sp.lastReply(t.Context(), w, 0, false); got != "Two files." {
		t.Fatalf("reported-session local reply = %q", got)
	}
}
