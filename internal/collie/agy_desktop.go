package collie

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The Antigravity desktop app runs a language server on this computer. Its
// local API takes a message into a conversation the same way the app's own
// input box does (SendUserCascadeMessage), guarded by a CSRF token the app
// passes on the server's command line. Collie finds the server, its token
// and the port that speaks plain HTTP, and sends through it.
type agyServer struct {
	addr, csrf string
}

var agyServerCache struct {
	sync.Mutex
	at time.Time
	ls *agyServer
}

const (
	agyService       = "/exa.language_server_pb.LanguageServerService/"
	agyConversations = "/product.v1.ConversationService/"
)

// agyDesktopServer: the running app's language server, or nil.
func agyDesktopServer() *agyServer {
	agyServerCache.Lock()
	defer agyServerCache.Unlock()
	if time.Since(agyServerCache.at) < 20*time.Second {
		return agyServerCache.ls
	}
	agyServerCache.at, agyServerCache.ls = time.Now(), findAgyServer()
	return agyServerCache.ls
}

func forgetAgyServer() {
	agyServerCache.Lock()
	agyServerCache.at = time.Time{}
	agyServerCache.Unlock()
}

func findAgyServer() *agyServer {
	if ls := findAgyHub(); ls != nil {
		return ls
	}
	// Before 2.34 the token was on the server's command line.
	out, err := exec.Command("ps", "-axww", "-o", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.HasSuffix(f[1], "/language_server") {
			continue
		}
		args := map[string]string{}
		for i := 2; i+1 < len(f); i++ {
			if strings.HasPrefix(f[i], "--") {
				args[f[i]] = f[i+1]
			}
		}
		// The desktop app's server (the CLI keeps its own elsewhere).
		if args["--app_data_dir"] != "antigravity" || args["--csrf_token"] == "" {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		for _, port := range listenPorts(pid) {
			ls := &agyServer{addr: "127.0.0.1:" + port, csrf: args["--csrf_token"]}
			if ls.probe() {
				return ls
			}
		}
	}
	return nil
}

// findAgyHub reads the file the app (2.34 and later) writes for local
// clients: ~/.gemini/antigravity/daemon/hub_<port>.json with the server's
// pid, plain-HTTP port and token.
func findAgyHub() *agyServer {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(home, ".gemini", "antigravity", "daemon", "hub_*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var h struct {
			PID      int    `json:"pid"`
			HTTPPort int    `json:"httpPort"`
			CSRF     string `json:"csrfToken"`
		}
		if json.Unmarshal(b, &h) != nil || h.HTTPPort == 0 || h.CSRF == "" {
			continue
		}
		if h.PID > 0 && syscall.Kill(h.PID, 0) != nil {
			continue // left behind by a server that has quit
		}
		ls := &agyServer{addr: "127.0.0.1:" + strconv.Itoa(h.HTTPPort), csrf: h.CSRF}
		if ls.probe() {
			return ls
		}
	}
	return nil
}

// listenPorts: the loopback TCP ports a process listens on.
func listenPorts(pid int) []string {
	out, err := exec.Command("lsof", "-nP", "-a", "-p", strconv.Itoa(pid), "-iTCP", "-sTCP:LISTEN", "-Fn").Output()
	if err != nil {
		return nil
	}
	var ports []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "n") {
			if i := strings.LastIndex(l, ":"); i > 0 {
				ports = append(ports, l[i+1:])
			}
		}
	}
	return ports
}

// probe: this port answers the API in plain HTTP with this token.
func (ls *agyServer) probe() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := ls.call(ctx, "GetCascadeTrajectorySteps", map[string]any{"cascadeId": "00000000-0000-0000-0000-000000000000"})
	var api *agyError
	return err == nil || errors.As(err, &api)
}

type agyError struct{ Code, Message string }

func (e *agyError) Error() string { return e.Message }

func (ls *agyServer) call(ctx context.Context, method string, body any) (json.RawMessage, error) {
	return ls.post(ctx, agyService+method, body)
}

// post calls one of the server's services; each reads the token from a
// header of its own name, so both are sent.
func (ls *agyServer) post(ctx context.Context, path string, body any) (json.RawMessage, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", "http://"+ls.addr+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("x-codeium-csrf-token", ls.csrf)
	req.Header.Set("x-antigravity-csrf-token", ls.csrf)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		return nil, fmt.Errorf("Antigravity: HTTP %d", res.StatusCode)
	}
	if res.StatusCode != 200 {
		var e agyError
		json.Unmarshal(out, &e)
		if e.Message == "" {
			e.Message = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return nil, &e
	}
	return out, nil
}

// agyAppSend puts a message into an Antigravity desktop conversation, as if
// typed in the app.
func (s *Server) agyAppSend(ctx context.Context, conversation, text string) error {
	ls := agyDesktopServer()
	if ls == nil {
		return errors.New("Antigravity 没有打开：打开它之后就能从这里发消息")
	}
	_, err := ls.call(ctx, "SendUserCascadeMessage", map[string]any{
		"cascadeId": conversation,
		"items":     []map[string]string{{"text": text}},
	})
	if err != nil {
		var api *agyError
		if !errors.As(err, &api) {
			forgetAgyServer() // the app restarted: look again next time
		}
		return fmt.Errorf("Antigravity 没收下这条消息：%w", err)
	}
	forgetDesktopRows()
	return nil
}
