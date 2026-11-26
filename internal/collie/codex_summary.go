package collie

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

type codexReplaySummary struct {
	humanAt time.Time
	title   string
	usage   ctxUsage
	running bool
}

type codexSummaryEntry struct {
	sync.Once
	summary codexReplaySummary
}

var codexSummaries = struct {
	sync.Mutex
	byFile map[string]*codexSummaryEntry
}{byFile: map[string]*codexSummaryEntry{}}

// The sidebar and pane metadata share one scan of an immutable compressed
// source. Only the resulting summary is cached, with its source identity.
func codexSummary(path string) codexReplaySummary {
	fi, err := os.Stat(path)
	if err != nil {
		return codexReplaySummary{}
	}
	key := codexFingerprint(path, fi)
	codexSummaries.Lock()
	entry := codexSummaries.byFile[key]
	if entry == nil {
		if len(codexSummaries.byFile) >= 2000 {
			codexSummaries.byFile = map[string]*codexSummaryEntry{}
		}
		entry = &codexSummaryEntry{}
		codexSummaries.byFile[key] = entry
	}
	codexSummaries.Unlock()
	entry.Do(func() {
		summary, err := scanCodexSummary(path, time.Since(fi.ModTime()) <= codexRunningAge)
		if err != nil {
			log.Printf("codex: summarize replay %s: %v", path, err)
			return
		}
		entry.summary = summary
	})
	return entry.summary
}

func scanCodexSummary(path string, checkTurn bool) (codexReplaySummary, error) {
	var summary codexReplaySummary
	f, err := os.Open(path)
	if err != nil {
		return summary, err
	}
	defer closeCodexFile(f)
	dec, err := openCodexDecoder(f)
	if err != nil {
		return summary, err
	}
	defer dec.Close()
	r := bufio.NewReaderSize(dec, 1<<20)
	tr := newTranscript(path, "codex")
	var started time.Time
	var offset, modelAt, tokensAt int64
	for i := 0; ; i++ {
		line, err := r.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			prefix := append([]byte(nil), line...)
			line, err = r.ReadBytes('\n')
			line = append(prefix, line...)
		}
		if err != nil && err != io.EOF {
			return summary, err
		}
		if i < 50 && started.IsZero() {
			started = lineTime(line)
		}
		if checkTurn {
			if start, end := codexTurnMarkers(line); start >= 0 || end >= 0 {
				summary.running = start > end
			}
		}
		var usage ctxUsage
		readCodexUsage(line, &usage)
		if usage.Model != "" {
			summary.usage.Model, summary.usage.Effort, modelAt = usage.Model, usage.Effort, offset
		}
		if usage.Used > 0 {
			summary.usage.Used, summary.usage.Window, tokensAt = usage.Used, usage.Window, offset
		}
		if err == nil && bytes.Contains(line, []byte(`"role"`)) && bytes.Contains(line, []byte(`"user"`)) {
			tr.codexLine(line)
			for _, m := range tr.msgs {
				if m.Kind != "user" {
					continue
				}
				if summary.title == "" && offset < titleScanBytes && !strings.HasPrefix(m.Text, "[collie]") {
					summary.title = messageTitle(m.Text)
				}
				if !collieSession(m.Text) {
					if at := lineTime(line); !at.IsZero() {
						summary.humanAt = at
					}
				}
			}
			tr.msgs = tr.msgs[:0]
			clear(tr.tools)
		}
		offset += int64(len(line))
		if err == io.EOF {
			break
		}
	}
	if summary.humanAt.IsZero() {
		summary.humanAt = started
	}
	if modelAt < offset-usageTail {
		summary.usage.Model, summary.usage.Effort = "", ""
	}
	if tokensAt < offset-usageTail {
		summary.usage.Used, summary.usage.Window = 0, 0
	}
	return summary, nil
}

// codexRolloutCwd reads only the session metadata, including from cold rollouts.
func codexRolloutCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer closeCodexFile(f)
	var r io.Reader = f
	if compressedCodex(path, "codex") {
		dec, err := openCodexDecoder(f)
		if err != nil {
			return ""
		}
		defer dec.Close()
		r = dec
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 1<<20)
	var header struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if sc.Scan() && json.Unmarshal(sc.Bytes(), &header) == nil && header.Type == "session_meta" {
		return header.Payload.Cwd
	}
	return ""
}
