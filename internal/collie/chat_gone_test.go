package collie

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
