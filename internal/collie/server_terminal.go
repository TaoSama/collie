package collie

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type paneScreenMeta struct {
	size     Rect
	wheel    bool
	kind     string
	lastText string
	at       time.Time
}

type screenMetaCache struct {
	mu sync.RWMutex
	m  map[string]paneScreenMeta
}

func (c *screenMetaCache) get(id string) (paneScreenMeta, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[id]
	return v, ok
}

func (c *screenMetaCache) set(id string, v paneScreenMeta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]paneScreenMeta{}
	}
	v.at = time.Now()
	c.m[id] = v
}

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
	useDiff := mode != "log" && r.URL.Query().Get("diff") == "1"
	sse, ok := newSSE(w)
	if !ok {
		return
	}
	rates := pollRates{normal: 200 * time.Millisecond, quick: 50 * time.Millisecond, burst: 10 * time.Millisecond, snap: 2 * time.Millisecond}
	if mode == "log" {
		rates = pollRates{normal: 700 * time.Millisecond, quick: 100 * time.Millisecond}
	}
	var woke time.Time
	wake, unsubscribe := s.wakes.subscribe(id)
	defer unsubscribe()
	var lastText string
	var lastSentLines []string
	var lastSize Rect
	var wheel bool
	var kind string // the pane's agent, "" for a shell
	var sizeAt time.Time
	var histAt time.Time
	var histLines atomic.Int64
	var histBusy atomic.Bool
	var lastSentHist int
	beat := time.Now()
	for {
		active := !woke.IsZero() && time.Since(woke) < wakeFast
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		var (
			res ReadResult
			err error
		)
		if mode == "log" {
			res, err = s.herdr.Read(ctx, id, "recent_unwrapped", "text", 600)
		} else {
			// Refresh pane dimensions/metadata on the first frame, and while
			// idle — never mid-scroll, where two extra RPCs add tail latency.
			if sizeAt.IsZero() || (!active && time.Since(sizeAt) > 3*time.Second) {
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
			// Estimate total history lines asynchronously when idle so the
			// screen scrollbar reflects real transcript proportion from frame 0.
			if wheel && s.home != "" && !active && time.Since(histAt) > 4*time.Second && histBusy.CompareAndSwap(false, true) {
				histAt = time.Now()
				cols := lastSize.Width
				go func(reqCtx context.Context) {
					defer histBusy.Store(false)
					if n := s.estimatePaneHistoryLines(reqCtx, id, cols); n > 0 {
						histLines.Store(int64(n))
					}
				}(r.Context())
			}
			hedge := time.Duration(0)
			if active {
				hedge = 6 * time.Millisecond
			}
			res, err = s.herdr.ReadFast(ctx, id, "recent", "ansi", screenLines, hedge)
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
				frame := map[string]any{
					"revision": res.Revision, "truncated": res.Truncated,
					"cols": lastSize.Width, "rows": lastSize.Height, "cursor": cursor, "wheel": wheel, "kind": kind,
				}
				if h := int(histLines.Load()); h > 0 && wheel {
					frame["hist"] = h
					lastSentHist = h
				}
				sentDiff := false
				if useDiff {
					curLines := splitScreenLines(text)
					if len(lastSentLines) > 0 {
						shift, patch, patchBytes := computeScreenDiff(lastSentLines, curLines)
						if len(patch) < len(curLines) && patchBytes*10 < len(text)*7 {
							frame["shift"] = shift
							frame["patch"] = patch
							frame["n"] = len(curLines)
							sentDiff = true
						}
					}
					lastSentLines = curLines
				}
				if !sentDiff {
					frame["text"] = text
				}
				b, _ := json.Marshal(frame)
				if sse.send("frame", b) != nil {
					return
				}
				if mode != "log" {
					s.screens.set(id, paneScreenMeta{size: lastSize, wheel: wheel, kind: kind, lastText: lastText})
				}
				beat = time.Now()
			} else if wheel && mode != "log" {
				if h := int(histLines.Load()); h > 0 && h != lastSentHist {
					lastSentHist = h
					b, _ := json.Marshal(map[string]any{"hist": h})
					if sse.send("hist", b) != nil {
						return
					}
					beat = time.Now()
				}
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

func splitScreenLines(text string) []string {
	raw := strings.ReplaceAll(text, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "")
	if raw == "" {
		return []string{""}
	}
	return strings.Split(raw, "\n")
}

// computeScreenDiff finds the vertical row shift between prev and cur that
// maximizes exact line matches and returns only the differing [row, text] pairs.
func computeScreenDiff(prev, cur []string) (bestShift int, patch [][2]any, patchBytes int) {
	maxShift := len(cur) - 1
	if maxShift > 32 {
		maxShift = 32
	}
	bestMatches := -1
	score := func(shift int) int {
		m := 0
		for r, line := range cur {
			pr := r + shift
			if pr >= 0 && pr < len(prev) && prev[pr] == line {
				m++
			}
		}
		return m
	}
	bestMatches = score(0)
	for d := 1; d <= maxShift; d++ {
		if m := score(-d); m > bestMatches {
			bestMatches, bestShift = m, -d
		}
		if m := score(d); m > bestMatches {
			bestMatches, bestShift = m, d
		}
	}
	for r, line := range cur {
		pr := r + bestShift
		if pr < 0 || pr >= len(prev) || prev[pr] != line {
			patch = append(patch, [2]any{r, line})
			patchBytes += len(line) + 12
		}
	}
	return bestShift, patch, patchBytes
}

// estimatePaneHistoryLines approximates the rendered line count of an agent's
// transcript at cols width, for sizing the fullscreen screen-mode scrollbar
// before the user has scrolled all the way up.
func (s *Server) estimatePaneHistoryLines(parent context.Context, id string, cols int) int {
	if cols <= 0 {
		cols = 120
	}
	ctx, cancel := context.WithTimeout(parent, 1500*time.Millisecond)
	defer cancel()
	var info struct {
		Agent agentDetail `json:"agent"`
	}
	if err := s.herdr.Call(ctx, "agent.get", map[string]any{"target": id}, &info); err != nil || info.Agent.Agent == nil {
		return 0
	}
	cache := map[string]*transcript{}
	tr := s.transcriptFor(id, id, info.Agent, cache)
	if tr == nil {
		return 0
	}
	tr.update()
	msgs := tr.view()
	if len(msgs) == 0 {
		return 0
	}
	wrap := max(cols-6, 40)
	countText := func(s string) int {
		if s == "" {
			return 0
		}
		rows := 0
		for _, line := range strings.Split(s, "\n") {
			n := len([]rune(line))
			if n <= wrap {
				rows++
			} else {
				rows += (n + wrap - 1) / wrap
			}
		}
		return rows
	}
	total := 6 // startup banner / header margin
	for _, m := range msgs {
		total += 2
		if m.Kind == "tool" {
			total += 1 + min(countText(m.Output), 8)
		} else {
			total += countText(m.Text)
		}
	}
	return total
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
		Wait  bool   `json:"wait"`
	}
	if !decode(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	var beforeMeta paneScreenMeta
	hasMeta := false
	if body.Wait {
		beforeMeta, hasMeta = s.screens.get(id)
		if !hasMeta || beforeMeta.lastText == "" {
			if rr, err := s.herdr.ReadFast(r.Context(), id, "recent", "ansi", screenLines, 6*time.Millisecond); err == nil {
				beforeMeta.lastText = rr.Text
			}
		}
	}
	if err := s.writePaneText(r, id, body.Text, body.Enter); err != nil {
		fail(w, err)
		return
	}
	s.wakes.wake(id)
	resp := map[string]any{"ok": true}
	if body.Wait {
		if frame := s.waitForScreenFrame(r.Context(), id, beforeMeta, hasMeta); frame != nil {
			resp["frame"] = frame
		}
	}
	writeJSON(w, 200, resp)
}

// waitForScreenFrame briefly polls local herdr right after a wheel write so a
// remote browser gets the updated frame in the same HTTP round trip instead of
// waiting for a separate SSE delivery. Bounded to 32ms so it never stalls.
func (s *Server) waitForScreenFrame(parent context.Context, id string, meta paneScreenMeta, hasMeta bool) map[string]any {
	ctx, cancel := context.WithTimeout(parent, 55*time.Millisecond)
	defer cancel()
	deadline := time.Now().Add(50 * time.Millisecond)
	for {
		time.Sleep(2 * time.Millisecond)
		res, err := s.herdr.ReadFast(ctx, id, "recent", "ansi", screenLines, 6*time.Millisecond)
		if err != nil {
			return nil
		}
		if res.Text != meta.lastText {
			if !hasMeta || meta.size.Width == 0 {
				if sz, e := s.herdr.PaneSize(ctx, id); e == nil {
					meta.size = sz
				}
				if p, e := s.herdr.Pane(ctx, id); e == nil {
					meta.wheel = paneWheel(p)
					if p.Agent != nil {
						meta.kind = *p.Agent
					}
				}
			}
			text, cursor := markScreenCursor(res.Text, meta.size.Height, meta.kind)
			text = trimScreenBlankTail(text)
			meta.lastText = res.Text
			s.screens.set(id, meta)
			return map[string]any{
				"text": text, "revision": res.Revision, "truncated": res.Truncated,
				"cols": meta.size.Width, "rows": meta.size.Height, "cursor": cursor,
				"wheel": meta.wheel, "kind": meta.kind,
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil
		}
	}
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
