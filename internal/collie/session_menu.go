package collie

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// A session's menu renames or archives it the way its own agent does:
// Codex through its app-server (thread/name/set, thread/archive), Antigravity
// through its local server (UpdateConversation), Claude Code by the line its
// /rename writes. Claude Code has no archive of its own: the hub hides those
// (see marks.go). A running agent in herdr is renamed by typing /rename into
// it, and is closed before it is archived; the page does both.

// renameSession: POST /api/sessions/{id}/rename {kind, name}.
func (s *Server) renameSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	id, name := r.PathValue("id"), strings.TrimSpace(body.Name)
	if id == "" || name == "" {
		writeJSON(w, 400, map[string]any{"error": "缺少会话或名字"})
		return
	}
	var err error
	switch externalKinds[body.Kind] {
	case "codex":
		err = s.codexThread(r.Context(), "thread/name/set", map[string]any{"threadId": id, "name": name})
	case "agy":
		err = s.agyAnnotate(r.Context(), id, map[string]any{"title": name})
	case "claude":
		err = s.claudeRename(id, name)
	default:
		err = fmt.Errorf("不认识的 agent：%s", body.Kind)
	}
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// archiveSession: POST /api/sessions/{id}/archive {kind}.
func (s *Server) archiveSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind string `json:"kind"`
		// Its program was just closed: Codex may hold the thread a moment more.
		JustClosed bool `json:"just_closed"`
	}
	if !decode(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	var err error
	switch externalKinds[body.Kind] {
	case "codex":
		err = s.codexArchive(r.Context(), id, body.JustClosed)
	case "agy":
		err = s.agyAnnotate(r.Context(), id, map[string]any{"archived": true})
	default:
		err = fmt.Errorf("%s 没有自己的归档", body.Kind)
	}
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	forgetDesktopRows()
	forgetArchived()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// codexArchiveWait: a thread whose program was just closed may still be
// held for a moment; Codex refuses to archive it until let go.
const codexArchiveWait = 8 * time.Second

func (s *Server) codexArchive(ctx context.Context, thread string, justClosed bool) error {
	deadline := time.Now().Add(codexArchiveWait)
	for {
		err := s.codexThread(ctx, "thread/archive", map[string]any{"threadId": thread})
		if err == nil || !justClosed || time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		time.Sleep(time.Second)
	}
}

// codexThread asks Codex to change a thread: through the shared daemon when
// one runs, so the App and the terminals on it see the change at once, else
// through a short-lived app-server of its own.
func (s *Server) codexThread(ctx context.Context, method string, params any) error {
	if sock := s.codexDaemonSock(); sock != "" {
		if c, err := dialCodex(ctx, sock); err == nil {
			defer c.close()
			return c.call(method, params, nil)
		}
	}
	bin := findBin("codex", s.home)
	if bin == "" {
		return errors.New("这台电脑上找不到 codex")
	}
	_, err := codexOnce(ctx, bin, method, params)
	return err
}

// codexOnce asks a short-lived `codex app-server` (stdio, JSON lines) one thing.
func codexOnce(ctx context.Context, bin, method string, params any) (json.RawMessage, error) {
	cmd := exec.CommandContext(ctx, bin, "app-server")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 codex app-server：%w", err)
	}
	defer func() { in.Close(); cmd.Process.Kill(); cmd.Wait() }()
	send := func(v any) error { _, err := in.Write(append(jsonNoEscape(v), '\n')); return err }
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	wait := func(id int) (json.RawMessage, error) {
		for sc.Scan() {
			var m struct {
				ID     *int            `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == nil || *m.ID != id || m.Method != "" {
				continue
			}
			if m.Error != nil {
				return nil, fmt.Errorf("Codex：%s (%d)", m.Error.Message, m.Error.Code)
			}
			return m.Result, nil
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("codex app-server 没有回答就退出了")
	}
	if err := send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "collie", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}}); err != nil {
		return nil, err
	}
	if _, err := wait(1); err != nil {
		return nil, err
	}
	if err := send(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	if err := send(map[string]any{"id": 2, "method": method, "params": params}); err != nil {
		return nil, err
	}
	return wait(2)
}

// agyAnnotate changes what you can edit on an Antigravity conversation
// (title, archived) through the app's local server.
func (s *Server) agyAnnotate(ctx context.Context, conversation string, annotations map[string]any) error {
	ls := agyDesktopServer()
	if ls == nil {
		return errors.New("Antigravity 没有打开：打开它之后才能改这个会话")
	}
	_, err := ls.post(ctx, agyConversations+"UpdateConversation", map[string]any{"conversationId": conversation, "annotations": annotations})
	if err != nil {
		var api *agyError
		if !errors.As(err, &api) || api.authFailed() {
			forgetAgyServer()
		} else if api.Code == "not_found" || strings.HasSuffix(api.Message, "not found") {
			return errors.New("Antigravity 加载不到这个会话（可能是没建成的空会话），改不了它")
		}
		return fmt.Errorf("Antigravity 没有改成：%w", err)
	}
	forgetDesktopRows()
	return nil
}

// claudeRename adds the line Claude Code's /rename writes to the end of the
// session's transcript: the last one names it.
func (s *Server) claudeRename(session, name string) error {
	if strings.ContainsAny(session, `/\`) {
		return errors.New("会话 id 不对")
	}
	path, _ := s.findTranscript("claude", "", session, "", "")
	if path == "" {
		return errors.New("这台电脑上找不到这个 Claude 会话的记录")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	line := jsonNoEscape(struct {
		Type        string `json:"type"`
		CustomTitle string `json:"customTitle"`
		SessionID   string `json:"sessionId"`
	}{"custom-title", name, session})
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
