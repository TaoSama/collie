package collie

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The tokens this computer's agents ran, read from what Claude Code and
// Codex write as they go: each sample reads what their transcripts gained
// and adds every reply to the minute it came in. Antigravity keeps no counts
// to read. All tokens count: input, cache writes, cache reads and output.
//
// A reply counts once: Claude Code writes one line per content block, all
// with the message's id and usage, and a fork copies the history it starts
// from into a new file; Codex repeats its token_count when nothing changed.

const (
	tokenKeep = 26 * time.Hour // a day and the hour drawn before it
	// tokenWalk: how often the folders are looked through for new
	// transcripts; between, only the ones known are read again.
	tokenWalk = time.Minute
	// tokenRecent: the minutes the card draws.
	tokenRecent = 60
)

type tokenReport struct {
	PerMinute int64   `json:"per_minute"` // over the last minutes
	Today     int64   `json:"today"`      // since local midnight
	Recent    []int64 `json:"recent"`     // per minute, oldest first, the last ending now
	Minute    int64   `json:"minute"`     // unix time of Recent's last minute
}

// tokenDayStats summarizes today's local transcript token & cache activity.
type tokenDayStats struct {
	Tokens     int64 `json:"tokens,omitempty"`
	Requests   int64 `json:"requests,omitempty"`
	CacheRead  int64 `json:"cache_read,omitempty"`
	CacheWrite int64 `json:"cache_write,omitempty"`
	CacheInput int64 `json:"cache_input,omitempty"`
}

type minuteStats struct {
	tokens     int64
	requests   int64
	cacheRead  int64
	cacheWrite int64
	cacheInput int64
}

type seenUsage struct {
	tokens     int64
	cacheRead  int64
	cacheWrite int64
	cacheInput int64
}

// A lineParser counts one transcript line through add.
type lineParser func(m *tokenMeter, line []byte, f *meterFile, from time.Time)

type meterFile struct {
	parse lineParser
	off   int64
	mod   time.Time
	total int64 // Codex: the running total last counted
}

// files and walked belong to the scan (one goroutine); mu guards the counts
// report reads, and is held only to change them, never across file reads.
type tokenMeter struct {
	home   string
	files  map[string]*meterFile
	walked time.Time

	mu      sync.Mutex
	minutes map[int64]int64       // unix minute → tokens
	mstats  map[int64]minuteStats // unix minute → detailed stats
	seen    map[string]seenUsage  // reply → usage counted so far
	seenAt  map[string]int64      // reply → its unix time, to forget it
}

func newTokenMeter(home string) *tokenMeter {
	return &tokenMeter{
		home:    home,
		files:   map[string]*meterFile{},
		minutes: map[int64]int64{},
		mstats:  map[int64]minuteStats{},
		seen:    map[string]seenUsage{},
		seenAt:  map[string]int64{},
	}
}

// scan reads what changed since the last scan. The first one reads the
// transcripts written to within tokenKeep from their start.
func (m *tokenMeter) scan(now time.Time) {
	from := now.Add(-tokenKeep)
	if now.Sub(m.walked) >= tokenWalk {
		m.walked = now
		m.walk(filepath.Join(claudeRoot(m.home), "projects"), from, (*tokenMeter).claudeLine)
		// Codex files by the day they started: a session from yesterday
		// may still be going.
		codex := filepath.Join(codexRoot(m.home), "sessions")
		y, mo, d := from.Date()
		for day := time.Date(y, mo, d, 0, 0, 0, 0, now.Location()); !day.After(now); day = day.AddDate(0, 0, 1) {
			m.walk(filepath.Join(codex, day.Format("2006/01/02")), from, (*tokenMeter).codexLine)
		}
	} else {
		for p, f := range m.files {
			if fi, err := os.Stat(p); err == nil {
				m.update(p, f, fi, from)
			}
		}
	}
	m.forget(from)
}

func (m *tokenMeter) walk(root string, from time.Time, parse lineParser) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		fi, err := d.Info()
		if err != nil || fi.ModTime().Before(from) {
			return nil
		}
		f := m.files[p]
		if f == nil {
			f = &meterFile{parse: parse}
			m.files[p] = f
		}
		m.update(p, f, fi, from)
		return nil
	})
}

func (m *tokenMeter) update(p string, f *meterFile, fi fs.FileInfo, from time.Time) {
	f.mod = fi.ModTime()
	if fi.Size() < f.off { // rewritten
		f.off, f.total = 0, 0
	}
	if fi.Size() > f.off {
		m.read(p, f, from)
	}
}

// read counts the whole lines added to p.
func (m *tokenMeter) read(p string, f *meterFile, from time.Time) {
	fh, err := os.Open(p)
	if err != nil {
		log.Printf("tokens: %v", err)
		return
	}
	defer fh.Close()
	if _, err := fh.Seek(f.off, io.SeekStart); err != nil {
		log.Printf("tokens: %v", err)
		return
	}
	// Line by line: a first read can be a long day's transcript. A line not
	// finished yet is read again next time.
	r := bufio.NewReaderSize(fh, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if err != io.EOF {
				log.Printf("tokens: %s: %v", p, err)
			}
			return
		}
		f.off += int64(len(line))
		f.parse(m, line, f, from)
	}
}

func (m *tokenMeter) add(key string, at time.Time, tokens, cacheRead, cacheWrite, cacheInput int64, from time.Time) {
	if tokens <= 0 || at.Before(from) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	minKey := at.Unix() / 60 * 60
	ms := m.mstats[minKey]
	if prev, ok := m.seen[key]; ok {
		if tokens <= prev.tokens {
			return
		}
		dt := tokens - prev.tokens
		dcr := max(int64(0), cacheRead-prev.cacheRead)
		dcw := max(int64(0), cacheWrite-prev.cacheWrite)
		dci := max(int64(0), cacheInput-prev.cacheInput)
		m.seen[key] = seenUsage{
			tokens:     tokens,
			cacheRead:  max(prev.cacheRead, cacheRead),
			cacheWrite: max(prev.cacheWrite, cacheWrite),
			cacheInput: max(prev.cacheInput, cacheInput),
		}
		m.minutes[minKey] += dt
		ms.tokens += dt
		ms.cacheRead += dcr
		ms.cacheWrite += dcw
		ms.cacheInput += dci
	} else {
		m.seen[key] = seenUsage{tokens: tokens, cacheRead: cacheRead, cacheWrite: cacheWrite, cacheInput: cacheInput}
		m.minutes[minKey] += tokens
		ms.tokens += tokens
		ms.requests++
		ms.cacheRead += cacheRead
		ms.cacheWrite += cacheWrite
		ms.cacheInput += cacheInput
	}
	m.mstats[minKey] = ms
	m.seenAt[key] = at.Unix()
}

// claudeLine: an assistant line's usage, the most its message's lines say.
func (m *tokenMeter) claudeLine(line []byte, _ *meterFile, from time.Time) {
	if !bytes.Contains(line, []byte(`"usage"`)) {
		return
	}
	var v struct {
		Type      string    `json:"type"`
		Timestamp time.Time `json:"timestamp"`
		IsError   bool      `json:"isApiErrorMessage"`
		Message   struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage struct {
				Input      int64 `json:"input_tokens"`
				CacheWrite int64 `json:"cache_creation_input_tokens"`
				CacheRead  int64 `json:"cache_read_input_tokens"`
				Output     int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &v) != nil || v.Type != "assistant" || v.IsError || v.Message.Model == "<synthetic>" || v.Message.ID == "" {
		return
	}
	u := v.Message.Usage
	total := u.Input + u.CacheWrite + u.CacheRead + u.Output
	cacheIn := u.Input + u.CacheWrite + u.CacheRead
	m.add("c:"+v.Message.ID, v.Timestamp, total, u.CacheRead, u.CacheWrite, cacheIn, from)
}

// codexLine: a token_count whose running total moved counts its last reply.
// Codex's input already holds what came from the cache.
func (m *tokenMeter) codexLine(line []byte, f *meterFile, from time.Time) {
	if !bytes.Contains(line, []byte(`"token_count"`)) {
		return
	}
	var v struct {
		Timestamp time.Time `json:"timestamp"`
		Payload   struct {
			Type string `json:"type"`
			Info *struct {
				Total struct {
					Tokens int64 `json:"total_tokens"`
				} `json:"total_token_usage"`
				Last struct {
					Input      int64 `json:"input_tokens"`
					CacheRead  int64 `json:"cached_input_tokens"`
					CacheWrite int64 `json:"cache_write_input_tokens"`
					Tokens     int64 `json:"total_tokens"`
				} `json:"last_token_usage"`
			} `json:"info"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &v) != nil || v.Payload.Type != "token_count" || v.Payload.Info == nil {
		return
	}
	total := v.Payload.Info.Total.Tokens
	if total == f.total {
		return
	}
	f.total = total
	last := v.Payload.Info.Last
	m.add("x:"+v.Timestamp.Format(time.RFC3339Nano), v.Timestamp, last.Tokens, last.CacheRead, last.CacheWrite, last.Input, from)
}

// forget drops minutes and replies older than from, and files not written
// to since.
func (m *tokenMeter) forget(from time.Time) {
	for p, f := range m.files {
		if f.mod.Before(from) {
			delete(m.files, p)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cut := from.Unix()
	for t := range m.minutes {
		if t < cut-60 {
			delete(m.minutes, t)
			delete(m.mstats, t)
		}
	}
	for k, t := range m.seenAt {
		if t < cut {
			delete(m.seenAt, k)
			delete(m.seen, k)
		}
	}
}

func (m *tokenMeter) todayDetail(now time.Time) tokenDayStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	y, mo, d := now.Date()
	midnight := time.Date(y, mo, d, 0, 0, 0, 0, now.Location()).Unix()
	var out tokenDayStats
	for t, ms := range m.mstats {
		if t >= midnight {
			out.Tokens += ms.tokens
			out.Requests += ms.requests
			out.CacheRead += ms.cacheRead
			out.CacheWrite += ms.cacheWrite
			out.CacheInput += ms.cacheInput
		}
	}
	return out
}

func (m *tokenMeter) report(now time.Time) tokenReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := now.Unix() / 60 * 60
	r := tokenReport{Minute: cur, Recent: make([]int64, tokenRecent)}
	for i := range r.Recent {
		r.Recent[i] = m.minutes[cur-int64(tokenRecent-1-i)*60]
	}
	y, mo, d := now.Date()
	midnight := time.Date(y, mo, d, 0, 0, 0, 0, now.Location()).Unix()
	for t, n := range m.minutes {
		if t >= midnight {
			r.Today += n
		}
	}
	// The last two whole minutes and the one going on, by the time they cover.
	part := float64(now.Unix()-cur) / 60
	sum := m.minutes[cur] + m.minutes[cur-60] + m.minutes[cur-120]
	r.PerMinute = int64(float64(sum) / (2 + part))
	return r
}
