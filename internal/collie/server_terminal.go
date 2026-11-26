package collie

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
)

// streamPane mirrors one pane over SSE.
//
//	mode=screen: recent output with ANSI styling, for a scrollable terminal render,
//	             the cursor marked in it (markScreenCursor).
//	mode=log:    recent output with soft wraps joined, for reading on narrow screens.
//
// screenLines is how far back screen mode can scroll: herdr's pane.read
// returns at most 1000 lines however many you ask for.
const screenLines = 1000

// paneWheel reports whether screen mode should hand the wheel to the program
// in the pane instead of scrolling the page: an agent drawing fullscreen
// (Claude Code with tui=fullscreen, Codex without --no-alt-screen, agy with
// altScreenMode=always) keeps its history to itself, so herdr has no
// scrollback for it, and scrolls on mouse wheel reports. All three ignore
// the reports when inline, so a short inline history (no scrollback yet)
// gets them harmlessly. Shells and other programs keep the page's own
// scrolling: a shell would print the reports.
func paneWheel(p Pane) bool {
	return p.Agent != nil && slices.Contains(agentKinds, *p.Agent) && p.Scroll != nil && p.Scroll.MaxOffset == 0
}

func (s *Server) streamPane(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	mode := r.URL.Query().Get("mode")
	sse, ok := newSSE(w)
	if !ok {
		return
	}
	rates := pollRates{normal: 200 * time.Millisecond, quick: 50 * time.Millisecond, burst: 10 * time.Millisecond}
	if mode == "log" {
		rates = pollRates{normal: 700 * time.Millisecond, quick: 100 * time.Millisecond}
	}
	var woke time.Time
	wake, unsubscribe := s.wakes.subscribe(id)
	defer unsubscribe()
	var lastText string
	var lastSize Rect
	var wheel bool
	var kind string // the pane's agent, "" for a shell
	var sizeAt time.Time
	beat := time.Now()
	for {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		var (
			res ReadResult
			err error
		)
		if mode == "log" {
			res, err = s.herdr.Read(ctx, id, "recent_unwrapped", "text", 600)
		} else {
			if time.Since(sizeAt) > 2*time.Second {
				if sz, e := s.herdr.PaneSize(ctx, id); e == nil {
					lastSize = sz
				}
				if p, e := s.herdr.Pane(ctx, id); e == nil {
					wheel = paneWheel(p)
					kind = ""
					if p.Agent != nil {
						kind = *p.Agent
					}
				}
				sizeAt = time.Now()
			}
			res, err = s.herdr.Read(ctx, id, "recent", "ansi", screenLines)
		}
		cancel()
		if err != nil {
			if notFound(err) {
				b, _ := json.Marshal(map[string]any{"error": err.Error()})
				sse.send("gone", b)
				return
			}
		} else {
			if res.Text != lastText {
				lastText = res.Text
				text, cursor := res.Text, false
				if mode != "log" {
					text, cursor = markScreenCursor(text, lastSize.Height, kind)
					text = trimScreenBlankTail(text)
				}
				b, _ := json.Marshal(map[string]any{
					"text": text, "revision": res.Revision, "truncated": res.Truncated,
					"cols": lastSize.Width, "rows": lastSize.Height, "cursor": cursor, "wheel": wheel,
				})
				if sse.send("frame", b) != nil {
					return
				}
				beat = time.Now()
			}
		}
		if time.Since(beat) > 15*time.Second {
			sse.comment()
			beat = time.Now()
		}
		if !pollAfter(r.Context().Done(), wake, rates, &woke) {
			return
		}
	}
}

// trimScreenBlankTail drops unpainted blank rows at the bottom of an ANSI
// snapshot while keeping internal blank rows and background-styled bars.
func trimScreenBlankTail(s string) string {
	for len(s) > 0 {
		i := strings.LastIndexByte(s, '\n')
		line := s[i+1:]
		if !isUnpaintedBlankRow(line) {
			break
		}
		if i < 0 {
			return ""
		}
		if i > 0 && s[i-1] == '\r' {
			i--
		}
		s = s[:i]
	}
	return s
}

func isUnpaintedBlankRow(line string) bool {
	rest := strings.ReplaceAll(line, "\x1b[0m", "")
	rest = strings.ReplaceAll(rest, "\x1b[m", "")
	for i := 0; i < len(rest); i++ {
		if rest[i] != ' ' && rest[i] != '\r' {
			return false
		}
	}
	return true
}

// logInterrupting notes input from a page that can stop an agent's turn: Esc,
// ctrl+c, or escape sequences (one split in two reads as Esc). herdr logs only
// failed sends, so without this an unexplained "Interrupted" can't be traced
// to a page. Only the shape of the text is logged, never what was typed.
func (s *Server) logInterrupting(r *http.Request, pane, what string) {
	who := "the master token"
	if _, d := s.identity(r); d != nil {
		who = d.Name
	}
	log.Printf("pane %s: %s from %s", pane, what, who)
}

// escChunkMax bounds one write of text holding escape sequences.
const escChunkMax = 256

// escChunks cuts text holding escape sequences into writes of about max bytes,
// each cut just before an ESC. An agent that reads an ESC with nothing after
// it yet takes it for the Esc key and stops its turn, and a terminal passes
// a long write on in pieces (macOS holds 1 KB): a cut can land right after
// an ESC. A short write that ends where a sequence does can't leave one
// hanging. Text without ESC, or short, goes whole.
func escChunks(text string, max int) []string {
	if len(text) <= max || !strings.Contains(text, "\x1b") {
		if text == "" {
			return nil
		}
		return []string{text}
	}
	var out []string
	for len(text) > max {
		cut := strings.LastIndexByte(text[1:max+1], 0x1b) + 1
		if cut <= 0 {
			// No ESC to cut at in reach: up to the next one, whole.
			if next := strings.IndexByte(text[1:], 0x1b); next >= 0 {
				cut = next + 1
			} else {
				break
			}
		}
		out = append(out, text[:cut])
		text = text[cut:]
	}
	return append(out, text)
}

func (s *Server) sendText(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text  string `json:"text"`
		Enter bool   `json:"enter"`
	}
	if !decode(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	if err := s.writePaneText(r, id, body.Text, body.Enter); err != nil {
		fail(w, err)
		return
	}
	s.wakes.wake(id)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) writePaneText(r *http.Request, id, text string, enter bool) error {
	if n := strings.Count(text, "\x1b"); n > 0 {
		s.logInterrupting(r, id, fmt.Sprintf("%d bytes holding %d Esc", len(text), n))
	}
	ctx := r.Context()
	for _, part := range escChunks(text, escChunkMax) {
		if err := s.herdr.SendText(ctx, id, part); err != nil {
			return err
		}
	}
	if enter {
		return s.herdr.SendKeys(ctx, id, "enter")
	}
	return nil
}

const paneSubmitRoute = "POST /api/panes/{id}/submit"

// submitPane is the chat composer entry point. The executing server checks
// the actual pane because the browser's list can lag behind a restart.
func (s *Server) submitPane(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeJSON(w, 400, map[string]any{"error": "empty prompt"})
		return
	}
	id := r.PathValue("id")
	ctx := r.Context()
	pane, err := s.herdr.Pane(ctx, id)
	if err != nil {
		fail(w, err)
		return
	}
	switch {
	case pane.ID == id && deref(pane.Agent) != "":
		err = s.herdr.Prompt(ctx, id, body.Text)
	case pane.ID == id && pane.Agent == nil && pane.DisplayAgent == nil && pane.AgentStatus == "unknown":
		// herdr represents a plain terminal with no agent and status unknown.
		err = s.writePaneText(r, id, body.Text, true)
	default:
		writeJSON(w, 409, map[string]any{"error": "无法确认这个终端的类型，请刷新会话"})
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.wakes.wake(id)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) sendKeys(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Keys []string `json:"keys"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.Keys) == 0 || len(body.Keys) > 64 {
		writeJSON(w, 400, map[string]any{"error": "keys required"})
		return
	}
	if slices.ContainsFunc(body.Keys, func(k string) bool { return k == "esc" || k == "ctrl+c" }) {
		s.logInterrupting(r, r.PathValue("id"), "keys "+strings.Join(body.Keys, " "))
	}
	if err := s.herdr.SendKeys(r.Context(), r.PathValue("id"), body.Keys...); err != nil {
		fail(w, err)
		return
	}
	s.wakes.wake(r.PathValue("id"))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) promptAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeJSON(w, 400, map[string]any{"error": "empty prompt"})
		return
	}
	if err := s.herdr.Prompt(r.Context(), r.PathValue("target"), body.Text); err != nil {
		fail(w, err)
		return
	}
	s.wakes.wake(r.PathValue("target"))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// closePane closes one terminal; herdr closes its workspace with its last pane.
func (s *Server) closePane(w http.ResponseWriter, r *http.Request) {
	if err := s.herdr.Call(r.Context(), "pane.close", map[string]any{"pane_id": r.PathValue("id")}, nil); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) closeWorkspace(w http.ResponseWriter, r *http.Request) {
	if err := s.herdr.Call(r.Context(), "workspace.close", map[string]any{"workspace_id": r.PathValue("id")}, nil); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
