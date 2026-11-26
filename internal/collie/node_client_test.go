package collie

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestNodeJSONCallerStatusErrors(t *testing.T) {
	const machine = "mac"
	const notFound = "404 Not Found"
	diagnostic := " \n" + strings.Repeat("x", 512)
	cases := []struct {
		method, path, body, want string
		call                     func(context.Context, *Server) error
	}{
		{
			method: http.MethodGet,
			path:   "/api/ext-local",
			want:   notFound,
			call: func(ctx context.Context, s *Server) error {
				_, err := (&extHost{s: s}).remoteLocal(ctx, machine)
				return err
			},
		},
		{
			method: http.MethodPost,
			path:   "/api/trust",
			body:   `{"dir":"/tmp/project","kind":"agy"}`,
			want:   "node: " + notFound,
			call: func(ctx context.Context, s *Server) error {
				return s.trustRemote(ctx, machine, "/tmp/project")
			},
		},
		{
			method: http.MethodPost,
			path:   "/api/codex/interrupt",
			body:   `{"thread":"test-thread"}`,
			want:   "interrupt: " + notFound + " " + diagnostic[:500],
			call: func(ctx context.Context, s *Server) error {
				return s.stopCodexTurn(ctx, machine, "test-thread")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tc.method || r.URL.Path != tc.path {
					t.Errorf("request = %s %s, want %s %s", r.Method, r.URL.Path, tc.method, tc.path)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != tc.body {
					t.Errorf("request body = %q, error = %v, want %q", body, err, tc.body)
				}
				if tc.body != "" && r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
				}
				w.WriteHeader(http.StatusNotFound)
				if _, err := io.WriteString(w, diagnostic); err != nil {
					t.Errorf("write status diagnostic: %v", err)
				}
			})
			hub, _ := remoteNode(t, nil, api, false)
			if err := tc.call(t.Context(), &Server{herdr: hub}); err == nil || err.Error() != tc.want {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
