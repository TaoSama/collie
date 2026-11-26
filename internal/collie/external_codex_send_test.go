package collie

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func startFakeCodexDaemon(t *testing.T, handle func(method string, params json.RawMessage) (any, *struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
})) string {
	t.Helper()
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
	if err := os.Mkdir(control, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(control, "app-server-control.sock"))
	if err != nil {
		t.Fatal(err)
	}
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
				ID     int64           `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(data, &request); err != nil {
				t.Error(err)
				return
			}
			if request.Method == "initialized" {
				continue
			}
			reply := map[string]any{"id": request.ID}
			if request.Method == "initialize" {
				reply["result"] = map[string]any{}
			} else {
				res, rpcErr := handle(request.Method, request.Params)
				if rpcErr != nil {
					reply["error"] = rpcErr
				} else {
					if res == nil {
						res = map[string]any{}
					}
					reply["result"] = res
				}
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
	return home
}

func TestCodexThreadReadFailure(t *testing.T) {
	const thread, readError = "test-thread", "cannot load thread metadata"
	startFakeCodexDaemon(t, func(method string, _ json.RawMessage) (any, *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}) {
		switch method {
		case "thread/resume":
			return nil, nil
		case "thread/loaded/list":
			return map[string]any{"data": []string{thread}}, nil
		case "thread/read":
			return nil, &struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}{Code: -32603, Message: readError}
		default:
			t.Errorf("unexpected RPC after failed thread read: %s", method)
			return nil, nil
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := (&Server{}).codexSend(ctx, thread, "hello")
	if err == nil || !strings.Contains(err.Error(), readError) {
		t.Fatalf("send error = %v, want the thread/read error %q", err, readError)
	}
}

func holdCodexThreadLock(t *testing.T, home, thread string) {
	t.Helper()
	dir := filepath.Join(home, "thread-writer-locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, thread+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	})
}

func TestCodexQueueSendLockedSkipsWakeAndResume(t *testing.T) {
	const thread = "locked-thread"
	var mu sync.Mutex
	var methods []string
	home := startFakeCodexDaemon(t, func(method string, _ json.RawMessage) (any, *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}) {
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
		if method == "thread/queue/add" {
			return map[string]any{"queuedSubmission": map[string]any{"id": "q1"}}, nil
		}
		t.Errorf("unexpected RPC for locked thread: %s", method)
		return nil, nil
	})
	holdCodexThreadLock(t, home, thread)

	origWake := codexWakeApp
	t.Cleanup(func() { codexWakeApp = origWake })
	codexWakeApp = func(context.Context, string) error {
		t.Error("codexWakeApp should not be called when thread is already locked")
		return nil
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	note, err := (&Server{}).codexQueueSend(ctx, thread, "hello")
	if err != nil || note != "" {
		t.Fatalf("codexQueueSend = (%q, %v), want empty note and nil error", note, err)
	}
	if !slices.Equal(methods, []string{"thread/queue/add"}) {
		t.Fatalf("methods = %v, want [thread/queue/add]", methods)
	}
}

func TestCodexQueueSendUnloadedWakesApp(t *testing.T) {
	const thread = "unloaded-wake"
	var mu sync.Mutex
	var methods []string
	drained := false
	startFakeCodexDaemon(t, func(method string, _ json.RawMessage) (any, *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}) {
		mu.Lock()
		methods = append(methods, method)
		isDrained := drained
		mu.Unlock()
		switch method {
		case "thread/queue/add":
			return map[string]any{"queuedSubmission": map[string]any{"id": "q2"}}, nil
		case "thread/queue/list":
			if isDrained {
				return map[string]any{"data": []any{}}, nil
			}
			return map[string]any{"data": []map[string]any{{"id": "q2"}}}, nil
		default:
			t.Errorf("unexpected RPC when app wake drains queue: %s", method)
			return nil, nil
		}
	})

	origWake, origWait := codexWakeApp, codexWakeWait
	t.Cleanup(func() { codexWakeApp, codexWakeWait = origWake, origWait })
	codexWakeWait = time.Second
	woke := ""
	codexWakeApp = func(_ context.Context, id string) error {
		woke = id
		mu.Lock()
		drained = true
		mu.Unlock()
		return nil
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	note, err := (&Server{}).codexQueueSend(ctx, thread, "hello")
	if err != nil || note != "" {
		t.Fatalf("codexQueueSend = (%q, %v), want empty note and nil error", note, err)
	}
	if woke != thread {
		t.Fatalf("woke = %q, want %q", woke, thread)
	}
	if !slices.Equal(methods, []string{"thread/queue/add", "thread/queue/list"}) {
		t.Fatalf("methods = %v, want [thread/queue/add thread/queue/list]", methods)
	}
}

func TestCodexQueueSendUnloadedFallbackToDaemon(t *testing.T) {
	const thread = "unloaded-fallback"
	var mu sync.Mutex
	var methods []string
	var resumePolicy string
	startFakeCodexDaemon(t, func(method string, params json.RawMessage) (any, *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}) {
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
		switch method {
		case "thread/queue/add":
			return map[string]any{"queuedSubmission": map[string]any{"id": "q3"}}, nil
		case "thread/resume":
			var p struct {
				ApprovalPolicy string `json:"approvalPolicy"`
			}
			json.Unmarshal(params, &p)
			resumePolicy = p.ApprovalPolicy
			return nil, nil
		case "thread/queue/start", "thread/unsubscribe":
			return nil, nil
		default:
			t.Errorf("unexpected RPC: %s", method)
			return nil, nil
		}
	})

	origWake := codexWakeApp
	t.Cleanup(func() { codexWakeApp = origWake })
	codexWakeApp = func(context.Context, string) error { return errors.New("unavailable") }

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	note, err := (&Server{}).codexQueueSend(ctx, thread, "hello")
	if err != nil || note != codexBgNote {
		t.Fatalf("codexQueueSend = (%q, %v), want (%q, nil)", note, err, codexBgNote)
	}
	if resumePolicy != "never" {
		t.Fatalf("resume approvalPolicy = %q, want never", resumePolicy)
	}
	want := []string{"thread/queue/add", "thread/resume", "thread/queue/start", "thread/unsubscribe"}
	if !slices.Equal(methods, want) {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
}

func TestCodexQueueSendFallbackActiveWriterIsOK(t *testing.T) {
	const thread = "raced-writer"
	startFakeCodexDaemon(t, func(method string, _ json.RawMessage) (any, *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}) {
		switch method {
		case "thread/queue/add":
			return map[string]any{"queuedSubmission": map[string]any{"id": "q4"}}, nil
		case "thread/resume":
			return nil, &struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}{Code: -32600, Message: "thread raced-writer already has an active writer"}
		default:
			t.Errorf("unexpected RPC: %s", method)
			return nil, nil
		}
	})

	origWake := codexWakeApp
	t.Cleanup(func() { codexWakeApp = origWake })
	codexWakeApp = func(context.Context, string) error { return errors.New("unavailable") }

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	note, err := (&Server{}).codexQueueSend(ctx, thread, "hello")
	if err != nil || note != "" {
		t.Fatalf("codexQueueSend = (%q, %v), want empty note and nil error when App grabbed lock", note, err)
	}
}
