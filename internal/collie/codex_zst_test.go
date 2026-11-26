package collie

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

const zstRollout = `{"type":"session_meta","payload":{}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Two files."}]}}
`

// compressRollout leaves only path.zst, the way Codex compresses a cold
// rollout, dated over a week back.
func compressRollout(t *testing.T, path, content string) time.Time {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+codexZstSuffix, enc.EncodeAll([]byte(content), nil), 0o644); err != nil {
		t.Fatal(err)
	}
	mod := time.Now().Add(-8 * 24 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path+codexZstSuffix, mod, mod); err != nil {
		t.Fatal(err)
	}
	return mod
}

func codexCacheFixture(t *testing.T) {
	t.Helper()
	t.Setenv("COLLIE_HOME", t.TempDir())
}

func codexCacheBytes(t *testing.T) int64 {
	t.Helper()
	var size int64
	err := filepath.WalkDir(codexCacheDir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		fi, err := d.Info()
		if err == nil {
			size += fi.Size()
		}
		return err
	})
	if !os.IsNotExist(err) {
		must(t, err)
	}
	return size
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestCodexReplayConcurrentReaders(t *testing.T) {
	codexCacheFixture(t)
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	compressRollout(t, path, zstRollout)
	path += codexZstSuffix
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		file    *os.File
		release func()
		err     error
	}
	results := make(chan result, 2)
	joined := make(chan struct{}, 2)
	// Stop the decoder until two clients have joined the same job. A zero
	// budget keeps the descriptor anonymous, so a cache hit cannot mask a
	// duplicate decode.
	codexDecodeSlot <- struct{}{}
	for range 2 {
		go func() {
			f, release, err := openCodexReplay(ctx, path, func(p transcriptProgress) {
				if p.Total == 0 {
					joined <- struct{}{}
				}
			}, 0)
			results <- result{f, release, err}
		}()
	}
	for range 2 {
		select {
		case <-joined:
		case <-ctx.Done():
			<-codexDecodeSlot
			t.Fatal(ctx.Err())
		}
	}
	<-codexDecodeSlot
	r1, r2 := <-results, <-results
	must(t, r1.err)
	defer r1.release()
	must(t, r2.err)
	defer r2.release()
	if r1.file != r2.file {
		t.Fatal("concurrent readers decoded the replay independently")
	}
	for _, r := range []result{r1, r2} {
		got, err := io.ReadAll(io.NewSectionReader(r.file, 0, int64(len(zstRollout))))
		must(t, err)
		if string(got) != zstRollout {
			t.Fatalf("decoded replay = %q", got)
		}
	}
	if size := codexCacheBytes(t); size != 0 {
		t.Fatalf("oversized replay left %d bytes", size)
	}
}

func TestCodexAppRowsReadCompressedRollout(t *testing.T) {
	codexCacheFixture(t)
	home, _ := codexStateFixture(t)
	rollout := filepath.Join(home, ".codex", "sessions", "2026", "11", "10", "rollout-2026-11-10T19-08-26-cold.jsonl")
	source := strings.Replace(zstRollout, `{"type":"response_item"`, `{"timestamp":"2026-11-10T20:00:00Z","type":"response_item"`, 1)
	compressRollout(t, rollout, source)
	db, err := sql.Open("sqlite", filepath.Join(home, ".codex", "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	if _, err := db.Exec(`INSERT INTO threads VALUES ('cold', '', 'list files', 'list files', '/root', ?, ?, ?, ?, ?, '', 0, 'vscode')`,
		rollout, now.Unix(), now.UnixMilli(), now.Unix(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	rows := (&Server{home: home}).codexAppRows()
	if len(rows) != 1 || rows[0].path == "" {
		t.Fatalf("rows = %+v, want the thread with its log", rows)
	}
	if got := humanAt(rows[0].path, rows[0].format); !got.Equal(time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)) {
		t.Fatalf("latest human message = %v", got)
	}
	if got := firstUserLine(rows[0].path, rows[0].format); got != "list files" {
		t.Fatalf("first user line = %q", got)
	}
	if size := codexCacheBytes(t); size != 0 {
		t.Fatalf("listing and metadata left %d replay bytes", size)
	}
	tr := newTranscript(rows[0].path, rows[0].format)
	must(t, tr.load(context.Background(), nil, true))
	if len(tr.msgs) != 2 || tr.msgs[0].Text != "list files" || tr.msgs[1].Text != "Two files." {
		t.Fatalf("msgs = %+v", tr.msgs)
	}
}

func TestFindTranscriptCodexCompressed(t *testing.T) {
	codexCacheFixture(t)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	id := "01a1c525-1e13-7ac3-9fed-a5d89c584ea9"
	rollout := filepath.Join(home, ".codex", "sessions", "2026", "11", "10", "rollout-2026-11-10T19-08-26-"+id+".jsonl")
	compressRollout(t, rollout, zstRollout)
	path, format := (&Server{home: home}).findTranscript("codex", "id", id, "", "")
	if format != "codex" || path != rollout+codexZstSuffix {
		t.Fatalf("found %q (%s), want the source rollout", path, format)
	}
	if got := firstUserLine(path, format); got != "list files" {
		t.Fatalf("first user line = %q", got)
	}
}

func TestCodexReplayBudgetAndEviction(t *testing.T) {
	codexCacheFixture(t)
	dir := codexCacheDir()
	legacy := filepath.Join(dir, t.TempDir(), "rollout-legacy.jsonl")
	must(t, os.MkdirAll(filepath.Dir(legacy), 0o700))
	must(t, os.WriteFile(legacy, make([]byte, 4096), 0o600))
	// Startup cleanup reclaims old copies even with the source intact.
	source := strings.TrimPrefix(legacy, dir)
	compressRollout(t, source, zstRollout)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	maintainCodexCache(ctx)
	if isFile(legacy) || !isFile(source+codexZstSuffix) {
		t.Fatal("expected startup to remove the legacy cache and keep the source")
	}
	budget := int64(len(zstRollout))
	open := func(name string) (*os.File, func()) {
		path := filepath.Join(t.TempDir(), name+".jsonl")
		compressRollout(t, path, zstRollout)
		f, release, err := openCodexReplay(context.Background(), path+codexZstSuffix, nil, budget)
		must(t, err)
		return f, release
	}
	first, release := open("first")
	defer release()
	_, releaseNext := open("next")
	defer releaseNext()
	if size := codexCacheBytes(t); size != budget {
		t.Fatalf("cache = %d bytes, want %d", size, budget)
	}
	got, err := io.ReadAll(io.NewSectionReader(first, 0, budget))
	must(t, err)
	if string(got) != zstRollout {
		t.Fatal("eviction interrupted an active reader")
	}
}

func TestCodexOversizedReplayMatchesPlainPages(t *testing.T) {
	codexCacheFixture(t)
	var rollout strings.Builder
	for i := range 380 {
		fmt.Fprintf(&rollout, `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"message %d"}]}}`+"\n", i)
	}
	plain := filepath.Join(t.TempDir(), "plain.jsonl")
	must(t, os.WriteFile(plain, []byte(rollout.String()), 0o600))
	want := newTranscript(plain, "codex")
	must(t, want.load(context.Background(), nil, true))
	path := filepath.Join(t.TempDir(), "compressed.jsonl")
	compressRollout(t, path, rollout.String())
	f, release, err := openCodexReplay(context.Background(), path+codexZstSuffix, nil, 1024)
	must(t, err)
	defer release()
	fi, err := f.Stat()
	must(t, err)
	got := newTranscript(path, "codex")
	got.openTailFile(f, fi.Size())
	if !reflect.DeepEqual(got.view(), want.view()) || got.earlierCursor() != want.earlierCursor() {
		t.Fatal("compressed snapshot differs from plain JSONL")
	}
	for cursor := got.earlierCursor(); cursor > 0; {
		msgs, next := earlierFile(f, "codex", fi.Size(), cursor, earlierPage)
		expected, expectedNext, err := readEarlier(context.Background(), plain, "codex", cursor, earlierPage)
		must(t, err)
		if !reflect.DeepEqual(msgs, expected) || next != expectedNext || next >= cursor {
			t.Fatalf("page before %d differs from plain JSONL", cursor)
		}
		cursor = next
	}
	if size := codexCacheBytes(t); size != 0 {
		t.Fatalf("oversized replay left %d bytes", size)
	}
	// Plain logs remain live and keep using the same uncompressed offsets.
	out, err := os.OpenFile(plain, os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = out.WriteString(zstRollout)
	must(t, err)
	must(t, out.Close())
	if !want.update() || want.view()[len(want.view())-1].Text != "Two files." {
		t.Fatal("plain log did not pick up an appended reply")
	}
}

func TestCodexReplayCorrupt(t *testing.T) {
	codexCacheFixture(t)
	path := filepath.Join(t.TempDir(), "rollout-bad.jsonl.zst")
	must(t, os.WriteFile(path, []byte("not zstd"), 0o644))
	if _, _, err := openCodexReplay(context.Background(), path, nil, codexCacheBudget); err == nil {
		t.Fatal("expected a decoding error")
	}
	if size := codexCacheBytes(t); size != 0 {
		t.Fatalf("corrupt replay left %d bytes", size)
	}
}

func TestTranscriptCodexByTitle(t *testing.T) {
	for _, tc := range []struct {
		name, index, cwd string
		want             bool
	}{
		{"same directory", "id Topic", "/one", true},
		{"different directory", "id Topic", "/two", false},
		{"same title", "other Topic,id Topic", "/one", false},
		{"renamed away", "id Topic,id Renamed", "/one", false},
		{"old title reused", "other Topic,other Renamed,id Topic", "/one", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			rollout := filepath.Join(home, "sessions", "2026", "11", "26", "rollout-date-id.jsonl")
			log := strings.Replace(zstRollout, `"payload":{}`, `"payload":{"cwd":"/one"}`, 1)
			writeFiles(t, home, map[string]string{"sessions/2026/11/26/rollout-date-id.jsonl": log})
			var index strings.Builder
			for _, line := range strings.Split(tc.index, ",") {
				id, name, _ := strings.Cut(line, " ")
				fmt.Fprintf(&index, "{\"id\":%q,\"thread_name\":%q}\n", id, name)
			}
			must(t, os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte(index.String()), 0o644))
			codex := "codex"
			a := agentDetail{Agent: &codex, Cwd: &tc.cwd, TerminalTitle: "Topic | me"}
			tr := (&Server{home: home}).transcriptFor("k", "w1:p1", a, map[string]*transcript{})
			if (tr != nil) != tc.want || tr != nil && tr.path != rollout {
				t.Fatalf("transcript = %+v, want match %t", tr, tc.want)
			}
		})
	}
}
