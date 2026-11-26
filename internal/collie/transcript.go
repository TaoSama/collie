package collie

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// transcript incrementally parses an agent's JSONL session log.
type transcript struct {
	generation string
	path       string
	format     string // claude | codex | agy
	offset     int64
	msgs       []*Message
	tools      map[string]*Message
	pending    []*Message // agy: tool calls waiting for their output, in order
	lastID     string
	dropped    int       // messages trimmed off the front, so positions stay stable
	partial    bool      // offset may be mid-line: skip to the next line first
	began      int64     // where reading started (0: the top of the log)
	lineAt     int64     // where the line being parsed starts
	asked      time.Time // agy: when the turn being read began
	limit      int64     // stop reading here (0: at the end)
}

const (
	maxMessages = 60 // what a viewer gets first; older ones load on demand
	maxOutput   = 1600
	maxInput    = 400
)

// Resolve the file before opening it or comparing it with a cached transcript.
// Antigravity keeps complete messages next to its truncated transcript.jsonl.
func transcriptPath(path, format string) string {
	if format == "agy" && filepath.Base(path) == "transcript.jsonl" {
		if full := filepath.Join(filepath.Dir(path), "transcript_full.jsonl"); isFile(full) {
			return full
		}
	}
	return path
}

// A byte cursor belongs to one source file. Appending to it keeps the same
// generation. Replacing it or switching between short and full logs does not.
func transcriptGeneration(path string, fi os.FileInfo) string {
	st := fi.Sys().(*syscall.Stat_t)
	key := fmt.Sprintf("%s:%d:%d", path, st.Dev, st.Ino)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
}

func sourceGeneration(path, format string) string {
	path = transcriptPath(path, format)
	if format == "codex" {
		path = codexRollout(strings.TrimSuffix(path, codexZstSuffix))
	}
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return transcriptGeneration(path, fi)
}

func newTranscript(path, format string) *transcript {
	return &transcript{path: path, format: format, tools: map[string]*Message{}}
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 { // stay on a UTF-8 boundary
		cut--
	}
	return s[:cut] + " …"
}

func (m *Message) setToolResult(output string, failed bool) {
	m.Output = clip(output, maxOutput)
	m.Error = failed
	m.ToolDone = m.Kind == "tool"
}

// update reads any complete lines appended since the last call. It reports
// whether the conversation changed.
func (t *transcript) update() bool {
	if compressedCodex(t.path, t.format) {
		plain := strings.TrimSuffix(t.path, codexZstSuffix)
		if !isFile(plain) {
			return false
		}
		t.path = plain
	}
	f, err := os.Open(t.path)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	generation := transcriptGeneration(t.path, fi)
	replaced := t.generation != "" && t.generation != generation || fi.Size() < t.offset
	if replaced {
		*t = *newTranscript(t.path, t.format)
	}
	t.generation = generation
	if !replaced && fi.Size() == t.offset {
		return false
	}
	return t.updateFile(f, fi.Size()) || replaced
}

func (t *transcript) updateFile(f io.ReaderAt, size int64) bool {
	r := bufio.NewReaderSize(io.NewSectionReader(f, t.offset, max(0, size-t.offset)), 1<<20)
	if t.partial {
		skip, err := r.ReadBytes('\n')
		if err != nil {
			return false
		}
		t.offset += int64(len(skip))
		t.partial = false
		t.began = t.offset
	}
	changed := false
	for t.limit == 0 || t.offset < t.limit {
		line, err := r.ReadBytes('\n')
		if err != nil { // incomplete trailing line: retry next tick
			break
		}
		t.lineAt = t.offset
		t.offset += int64(len(line))
		changed = t.line(line) || changed
	}
	if len(t.msgs) > maxMessages*2 && t.limit == 0 { // a bounded read keeps all it reads
		t.dropped += len(t.msgs) - maxMessages
		t.msgs = append([]*Message(nil), t.msgs[len(t.msgs)-maxMessages:]...)
	}
	return changed
}

// openTail reads only the end of a long log: enough of it to fill the view,
// growing the window when the last few megabytes hold too few messages. A
// 40 MB Claude session otherwise takes most of a second to open.
func (t *transcript) openTailFile(f *os.File, size int64) {
	for win := int64(1 << 20); ; win *= 4 {
		*t = *newTranscript(t.path, t.format)
		if win >= size {
			t.updateFile(f, size)
			return
		}
		t.offset, t.partial = size-win, true
		t.updateFile(f, size)
		if len(t.msgs) >= maxMessages/2 {
			return
		}
	}
}

// msgSync sends a transcript to one viewer as patches: the first carries the
// whole view, later ones only the messages from the first one that changed
// (a tool call gets its output, a new message lands), so a long conversation
// is not sent again on every line.
type msgSync struct {
	sent       map[int]string // position → the message as last sent
	generation string
}

type msgPatch struct {
	Generation string     `json:"generation,omitempty"`
	Base       int        `json:"base"` // position of the first message in the view
	From       int        `json:"from"` // position of msgs[0]
	Msgs       []*Message `json:"msgs"`
	Earlier    int64      `json:"earlier,omitempty"` // cursor for older messages (see earlier)
}

// patch: what changed since the last call, or nil.
func (m *msgSync) patch(t *transcript) *msgPatch {
	if m.generation != t.generation {
		m.sent = nil
		m.generation = t.generation
	}
	view := t.view()
	base := t.dropped + len(t.msgs) - len(view)
	enc := make([]string, len(view))
	for i, msg := range view {
		b, _ := json.Marshal(msg)
		enc[i] = string(b)
	}
	from := base + len(view)
	if m.sent == nil {
		from = base
	} else {
		for i, s := range enc {
			if m.sent[base+i] != s {
				from = base + i
				break
			}
		}
		if from == base+len(view) && len(m.sent) == len(view) {
			if _, ok := m.sent[base]; ok {
				return nil
			}
		}
	}
	m.sent = make(map[int]string, len(enc))
	for i, s := range enc {
		m.sent[base+i] = s
	}
	return &msgPatch{Generation: t.generation, Base: base, From: from, Msgs: append([]*Message{}, view[from-base:]...), Earlier: t.earlierCursor()}
}

func (t *transcript) add(m *Message) { m.at = t.lineAt; t.msgs = append(t.msgs, m) }

// earlierCursor: where the messages before the view end, or 0 when the view
// starts at the top of the log.
func (t *transcript) earlierCursor() int64 {
	view := t.view()
	if len(view) == 0 || (t.began == 0 && len(view) == len(t.msgs) && t.dropped == 0) {
		return 0
	}
	return view[0].at
}

// earlierFile returns the last want messages before the byte cursor. It reads
// past the cursor so a tool call immediately before it can get its output.
func earlierFile(f *os.File, format string, size, before int64, want int) ([]*Message, int64) {
	before = min(before, size)
	if before <= 0 {
		return nil, 0
	}
	end := before + min(size-before, 1<<20)
	for win := int64(4 << 20); ; win *= 4 {
		t := newTranscript("", format)
		if start := before - win; start > 0 {
			t.offset, t.partial = start, true
		}
		t.limit = end
		t.updateFile(f, size)
		var keep []*Message
		for _, m := range t.msgs {
			if m.at < before {
				keep = append(keep, m)
			}
		}
		// Read from the top of the log: nothing is older than what we have.
		top := t.began == 0
		if len(keep) > want {
			keep = keep[len(keep)-want:]
			return keep, keep[0].at
		}
		if top {
			if t.dropped > 0 && len(keep) > 0 {
				return keep, keep[0].at
			}
			return keep, 0
		}
	}
}

// openTranscript holds an open descriptor for the entire random-access read.
func openTranscript(ctx context.Context, path, format string, progress func(transcriptProgress)) (*os.File, func(), error) {
	path = transcriptPath(path, format)
	if format == "codex" {
		if current := codexRollout(strings.TrimSuffix(path, codexZstSuffix)); current != "" {
			path = current
		}
	}
	if compressedCodex(path, format) {
		return openCodexReplay(ctx, path, progress, codexCacheBudget)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { closeCodexFile(f) }, nil
}

func (t *transcript) load(ctx context.Context, progress func(transcriptProgress), tail bool) error {
	generation := sourceGeneration(t.path, t.format)
	f, release, err := openTranscript(ctx, t.path, t.format, progress)
	if err != nil {
		return err
	}
	defer release()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !compressedCodex(t.path, t.format) {
		generation = transcriptGeneration(t.path, fi)
	}
	if tail {
		t.openTailFile(f, fi.Size())
	} else {
		t.updateFile(f, fi.Size())
	}
	t.generation = generation
	return nil
}

func readEarlier(ctx context.Context, path, format string, before int64, want int) ([]*Message, int64, error) {
	if before <= 0 {
		return nil, 0, nil
	}
	f, release, err := openTranscript(ctx, path, format, nil)
	if err != nil {
		return nil, 0, err
	}
	defer release()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	msgs, cursor := earlierFile(f, format, fi.Size(), before, want)
	return msgs, cursor, nil
}
