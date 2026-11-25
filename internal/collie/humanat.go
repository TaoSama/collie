package collie

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// humanAt is when the human last wrote in a session, from its log: their own
// message, not one Collie delivered (its events and worker briefs). A session
// they never wrote in gives when it started. The sidebar orders projects by
// it, so an agent busy on its own doesn't move them.
//
// A log is read once from its end backwards until their latest message; after
// that only what it grows by.
func humanAt(path, format string) time.Time {
	if path == "" {
		return time.Time{}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	size := fi.Size()
	humanLogs.Lock()
	h, ok := humanLogs.m[path]
	humanLogs.Unlock()
	switch {
	case ok && h.size == size:
	case ok && h.size < size:
		if at, end := lastHuman(path, format, h.size, size); end > h.size {
			if !at.IsZero() {
				h.last = at
			}
			h.size = end
		}
	default:
		h = humanLog{started: logStart(path)}
		for win := int64(1 << 20); ; win *= 4 {
			from := max(size-win, 0)
			at, end := lastHuman(path, format, from, size)
			h.last, h.size = at, end
			if !at.IsZero() || from == 0 {
				break
			}
		}
	}
	humanLogs.Lock()
	if len(humanLogs.m) > 2000 {
		humanLogs.m = map[string]humanLog{}
	}
	humanLogs.m[path] = h
	humanLogs.Unlock()
	if h.last.IsZero() {
		return h.started
	}
	return h.last
}

type humanLog struct {
	size          int64 // read up to here: the end of its last whole line
	last, started time.Time
}

var humanLogs = struct {
	sync.Mutex
	m map[string]humanLog
}{m: map[string]humanLog{}}

// lastHuman reads the whole lines of path between from (the next line, when
// mid-line) and to: the time of the human's last message there, and where
// the last whole line ends.
func lastHuman(path, format string, from, to int64) (time.Time, int64) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, from
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return time.Time{}, from
	}
	r := bufio.NewReaderSize(io.LimitReader(f, to-from), 1<<20)
	at, end := from, from
	if from > 0 {
		// Mid-line unless the byte before is a newline: skip to the next line.
		prev := make([]byte, 1)
		if _, err := f.ReadAt(prev, from-1); err != nil || prev[0] != '\n' {
			skip, err := r.ReadBytes('\n')
			if err != nil {
				return time.Time{}, from
			}
			at += int64(len(skip))
			end = at
		}
	}
	tr := &transcript{path: path, format: format, tools: map[string]*Message{}}
	var last time.Time
	for {
		line, err := r.ReadBytes('\n')
		if err != nil { // an unfinished last line is read when it is whole
			break
		}
		at += int64(len(line))
		end = at
		tr.line(line)
		for _, m := range tr.msgs {
			if m.Kind == "user" && !collieSession(m.Text) {
				if t := lineTime(line); !t.IsZero() {
					last = t
				}
			}
		}
		tr.msgs = tr.msgs[:0]
	}
	return last, end
}

// logStart is the time on the first lines of a log that carry one.
func logStart(path string) time.Time {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for i := 0; i < 50 && sc.Scan(); i++ {
		if t := lineTime(sc.Bytes()); !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

// lineTime: Claude and Codex stamp each line "timestamp", Antigravity "created_at".
func lineTime(line []byte) time.Time {
	if !bytes.Contains(line, []byte(`"timestamp"`)) && !bytes.Contains(line, []byte(`"created_at"`)) {
		return time.Time{}
	}
	var v struct {
		Timestamp time.Time `json:"timestamp"`
		CreatedAt time.Time `json:"created_at"`
	}
	if json.Unmarshal(line, &v) != nil {
		return time.Time{}
	}
	if v.Timestamp.IsZero() {
		return v.CreatedAt
	}
	return v.Timestamp
}
