package collie

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgyDesktopProbeRequiresExpectedResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"success", http.StatusOK, `{}`, true},
		{"malformed success", http.StatusOK, `{`, false},
		{"missing probe conversation", http.StatusNotFound, `{"code":"not_found"}`, true},
		{"rejected token", http.StatusUnauthorized, `{"code":"unauthenticated"}`, false},
		{"forbidden token", http.StatusForbidden, `{"code":"permission_denied"}`, false},
		{"unexpected API error", http.StatusInternalServerError, `{"code":"internal"}`, false},
		{"unstructured 404", http.StatusNotFound, `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != agyService+"GetCascadeTrajectorySteps" {
					t.Errorf("unexpected probe path %q", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if _, err := io.WriteString(w, tc.body); err != nil {
					t.Error(err)
				}
			}))
			defer app.Close()
			ls := &agyServer{addr: strings.TrimPrefix(app.URL, "http://"), csrf: "test-token"}
			if got := ls.probe(); got != tc.want {
				t.Fatalf("probe() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestAgyDesktopWritesRediscoverAfterAuthFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		write  func(*Server, context.Context) error
	}{
		{
			"send", http.StatusForbidden, `{"code":"permission_denied","message":"stale token"}`,
			func(s *Server, ctx context.Context) error { return s.agyAppSend(ctx, "test-session", "hello") },
		},
		{
			"annotate", http.StatusUnauthorized, `{"message":"stale token"}`,
			func(s *Server, ctx context.Context) error {
				return s.agyAnnotate(ctx, "test-session", map[string]any{"title": "new title"})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				body := `{}`
				if r.Header.Get("x-antigravity-csrf-token") != "fresh-token" {
					w.WriteHeader(tc.status)
					body = tc.body
				}
				if _, err := io.WriteString(w, body); err != nil {
					t.Error(err)
				}
			}))
			defer app.Close()
			dir := filepath.Join(home, ".gemini", "antigravity", "daemon")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			port := app.Listener.Addr().(*net.TCPAddr).Port
			for _, candidate := range []struct{ name, token string }{
				{"hub_1.json", "stale-token"},
				{"hub_2.json", "fresh-token"},
			} {
				body := fmt.Sprintf(`{"httpPort":%d,"csrfToken":%q}`, port, candidate.token)
				if err := os.WriteFile(filepath.Join(dir, candidate.name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			agyServerCache.Lock()
			agyServerCache.at = time.Now()
			agyServerCache.ls = &agyServer{addr: strings.TrimPrefix(app.URL, "http://"), csrf: "stale-token"}
			agyServerCache.Unlock()
			t.Cleanup(forgetAgyServer)

			s := &Server{home: home}
			if err := tc.write(s, t.Context()); err == nil || !strings.Contains(err.Error(), "stale token") {
				t.Fatalf("first write error = %v, want the original token rejection", err)
			}
			if err := tc.write(s, t.Context()); err != nil {
				t.Fatalf("second write should rediscover the valid candidate: %v", err)
			}
		})
	}
}
