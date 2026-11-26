package collie

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The session menu changes native session state on the agent's computer.
// The hub also keeps a Collie archive entry (marks.go).

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

func (s *Server) archiveSession(w http.ResponseWriter, r *http.Request) {
	s.setNativeArchive(w, r, true)
}

func (s *Server) unarchiveSession(w http.ResponseWriter, r *http.Request) {
	s.setNativeArchive(w, r, false)
}

// POST /api/sessions/{id}/pin {kind, surface?, pinned}.
func (s *Server) pinSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind    string `json:"kind"`
		Surface string `json:"surface"`
		Pinned  bool   `json:"pinned"`
	}
	if !decode(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, 400, map[string]any{"error": "缺少会话"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var err error
	switch externalKinds[body.Kind] {
	case "codex":
		err = s.codexPin(ctx, id, body.Pinned)
	case "agy":
		if body.Surface != surfaceCLI {
			err = s.agyPin(ctx, id, body.Pinned)
		}
	case "claude":
	default:
		writeJSON(w, 400, map[string]any{"error": "无法识别这个会话的 Agent"})
		return
	}
	if err != nil {
		if ctx.Err() != nil {
			err = errors.New("等待电脑响应超时，请重试")
		}
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	forgetDesktopRows()
	forgetExternal()
	forgetPinned()
	forgetLocalTitles()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) codexPin(ctx context.Context, thread string, pinned bool) error {
	var section any
	if pinned {
		section = codexPinnedSectionID
	}
	return s.codexThread(ctx, "thread/section/move", map[string]any{"threadId": thread, "sectionId": section})
}

func (s *Server) agyPin(ctx context.Context, conversation string, pinned bool) error {
	err := postAgyApp(ctx, agyConversations+"UpdateConversation", map[string]any{
		"conversationId": conversation,
		"annotations":    map[string]any{"pinned": pinned},
	})
	if agyConversationMissing(err) {
		return errors.New("Antigravity 无法打开这个会话，没能更新置顶状态")
	}
	if err != nil {
		return fmt.Errorf("Antigravity 没能更新置顶状态：%w", err)
	}
	return nil
}

// POST /api/sessions/{id}/archive or /unarchive {kind, surface?, just_closed?}.
func (s *Server) setNativeArchive(w http.ResponseWriter, r *http.Request, archived bool) {
	var body struct {
		Kind       string `json:"kind"`
		Surface    string `json:"surface"`
		JustClosed bool   `json:"just_closed"`
	}
	if !decode(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var note string
	var err error
	switch externalKinds[body.Kind] {
	case "codex":
		if archived {
			err = s.codexArchive(ctx, id, body.JustClosed)
		} else {
			err = s.codexThread(ctx, "thread/unarchive", map[string]any{"threadId": id})
		}
		// An archived thread could have been restored in Codex already, or
		// its original archive may only have succeeded in Collie.
		if err != nil && s.codexArchiveMatches(ctx, id, archived) {
			err = nil
		}
	case "agy":
		err = s.agyArchive(ctx, id, archived)
	case "claude":
		note, err = s.claudeArchive(ctx, id, archived)
		if note != "" && body.Surface == surfaceDesktop {
			err = errors.New("找不到 Claude 桌面版的这条会话记录")
		}
	default:
		writeJSON(w, 400, map[string]any{"error": "无法识别这个会话的 Agent"})
		return
	}
	if err != nil {
		if ctx.Err() != nil {
			err = errors.New("等待电脑响应超时，请重试")
		}
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	forgetDesktopRows()
	forgetArchived()
	writeJSON(w, 200, map[string]any{"ok": true, "note": note})
}

func (s *Server) codexArchiveMatches(ctx context.Context, id string, archived bool) bool {
	path := s.codexStateDB()
	if path == "" {
		return false
	}
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(2000)"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return false
	}
	defer db.Close()
	var value int
	err = db.QueryRowContext(ctx, `SELECT archived FROM threads WHERE id = ?`, id).Scan(&value)
	return err == nil && (value == 1 && archived || value == 0 && !archived)
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

var errAgyAppClosed = errors.New("Antigravity 桌面版没有打开")

func agyConversationMissing(err error) bool {
	var api *agyError
	return errors.As(err, &api) && (api.Code == "not_found" || api.status == http.StatusNotFound || strings.HasSuffix(api.Message, " not found"))
}

// agyAnnotate changes a conversation through the Antigravity app.
func (s *Server) agyAnnotate(ctx context.Context, conversation string, annotations map[string]any) error {
	err := postAgyApp(ctx, agyConversations+"UpdateConversation", map[string]any{"conversationId": conversation, "annotations": annotations})
	if agyConversationMissing(err) {
		return errors.New("Antigravity 无法打开这个会话，没能改名")
	}
	if err != nil {
		return fmt.Errorf("Antigravity 没能改名：%w", err)
	}
	forgetDesktopRows()
	return nil
}

func postAgyApp(ctx context.Context, path string, body any) error {
	ls := agyDesktopServer()
	if ls == nil {
		return errAgyAppClosed
	}
	_, err := ls.post(ctx, path, body)
	if err != nil {
		var api *agyError
		if !errors.As(err, &api) || api.authFailed() {
			forgetAgyServer()
		}
	}
	return err
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
