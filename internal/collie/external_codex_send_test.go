package collie

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestCodexThreadReadFailure(t *testing.T) {
	// Keep the unix socket path short even when the system temp path is long.
	home, err := os.MkdirTemp("/tmp", "collie-codex-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(home); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("CODEX_HOME", home)
	control := filepath.Join(home, "app-server-control")
	if err := os.Mkdir(control, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(control, "app-server-control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	const thread, readError = "test-thread", "cannot load thread metadata"
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer ws.CloseNow()
		for {
			_, data, err := ws.Read(r.Context())
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				return
			}
			if err != nil {
				t.Error(err)
				return
			}
			var request struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			if err := json.Unmarshal(data, &request); err != nil {
				t.Error(err)
				return
			}
			reply := map[string]any{"id": request.ID, "result": map[string]any{}}
			switch request.Method {
			case "initialized":
				continue
			case "initialize", "thread/resume":
			case "thread/loaded/list":
				reply["result"] = map[string]any{"data": []string{thread}}
			case "thread/read":
				delete(reply, "result")
				reply["error"] = map[string]any{"code": -32603, "message": readError}
			default:
				t.Errorf("unexpected RPC after failed thread read: %s", request.Method)
			}
			if err := ws.Write(r.Context(), websocket.MessageText, jsonNoEscape(reply)); err != nil {
				t.Error(err)
				return
			}
		}
	})}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	go func() {
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = (&Server{}).codexSend(ctx, thread, "hello")
	if err == nil || !strings.Contains(err.Error(), readError) {
		t.Fatalf("send error = %v, want the thread/read error %q", err, readError)
	}
}
