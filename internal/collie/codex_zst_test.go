package collie

import (
	"database/sql"
	"os"
	"path/filepath"
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

// codexCacheFixture points COLLIE_HOME at a new directory, and holds off the
// background prune, which would race the test's clean-up.
func codexCacheFixture(t *testing.T) {
	t.Helper()
	t.Setenv("COLLIE_HOME", t.TempDir())
	codexUnpack.Lock()
	codexUnpack.pruned = time.Now()
	codexUnpack.Unlock()
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestCodexRolloutUnpacksOnce(t *testing.T) {
	codexCacheFixture(t)
	path := filepath.Join(t.TempDir(), "rollout-2026-11-10T19-08-26-x.jsonl")
	if got := codexRollout(path); got != "" {
		t.Fatalf("no log: got %q, want \"\"", got)
	}
	mod := compressRollout(t, path, zstRollout)
	got := codexRollout(path)
	if got != filepath.Join(codexCacheDir(), path) {
		t.Fatalf("got %q, want the unpacked copy under the cache", got)
	}
	b, err := os.ReadFile(got)
	if err != nil || string(b) != zstRollout {
		t.Fatalf("unpacked = %q, %v", b, err)
	}
	if fi, _ := os.Stat(got); !fi.ModTime().Equal(mod) {
		t.Fatalf("unpacked dated %v, want the rollout's %v", fi.ModTime(), mod)
	}
	// Unpacked already: read as it is, not again.
	must(t, os.WriteFile(got, []byte("kept"), 0o600))
	must(t, os.Chtimes(got, mod, mod))
	if b, _ := os.ReadFile(codexRollout(path)); string(b) != "kept" {
		t.Fatalf("unpacked again: %q", b)
	}
	// A plain rollout is read where it is.
	must(t, os.WriteFile(path, []byte(zstRollout), 0o644))
	if got := codexRollout(path); got != path {
		t.Fatalf("got %q, want the plain rollout", got)
	}
}

func TestCodexAppRowsReadCompressedRollout(t *testing.T) {
	codexCacheFixture(t)
	home, _ := codexStateFixture(t)
	rollout := filepath.Join(home, ".codex", "sessions", "2026", "11", "10", "rollout-2026-11-10T19-08-26-cold.jsonl")
	compressRollout(t, rollout, zstRollout)
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
	tr := newTranscript(rows[0].path, rows[0].format)
	tr.openTail()
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
	if format != "codex" || path != filepath.Join(codexCacheDir(), rollout) {
		t.Fatalf("found %q (%s), want the unpacked rollout", path, format)
	}
	if got := firstUserLine(path, format); got != "list files" {
		t.Fatalf("first user line = %q", got)
	}
}

func TestPruneCodexCache(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(t.TempDir(), "rollout-live.jsonl")
	compressRollout(t, live, zstRollout)
	gone := filepath.Join(t.TempDir(), "rollout-gone.jsonl")
	fresh := filepath.Join(dir, ".unpack-fresh")
	for _, p := range []string{filepath.Join(dir, live), filepath.Join(dir, gone), fresh} {
		must(t, os.MkdirAll(filepath.Dir(p), 0o700))
		must(t, os.WriteFile(p, []byte(zstRollout), 0o600))
	}
	stale := filepath.Join(dir, ".unpack-stale")
	must(t, os.WriteFile(stale, nil, 0o600))
	old := time.Now().Add(-2 * codexUnpackStale)
	must(t, os.Chtimes(stale, old, old))
	pruneCodexCache(dir)
	for p, want := range map[string]bool{filepath.Join(dir, live): true, filepath.Join(dir, gone): false, fresh: true, stale: false} {
		if isFile(p) != want {
			t.Errorf("%s kept = %v, want %v", p, !want, want)
		}
	}
}

// A .zst that won't unpack is left out, and not tried again until it changes.
func TestCodexRolloutCorrupt(t *testing.T) {
	codexCacheFixture(t)
	path := filepath.Join(t.TempDir(), "rollout-bad.jsonl")
	must(t, os.WriteFile(path+codexZstSuffix, []byte("not zstd"), 0o644))
	if got := codexRollout(path); got != "" {
		t.Fatalf("got %q, want \"\"", got)
	}
	codexUnpack.Lock()
	_, remembered := codexUnpack.failed[path+codexZstSuffix]
	codexUnpack.Unlock()
	if !remembered || isFile(filepath.Join(codexCacheDir(), path)) {
		t.Fatalf("remembered = %v, want the failure kept and no copy", remembered)
	}
}

// herdr may not know a codex session: its thread is found by the terminal
// title ("<thread name> | <user>") in the session index, so an extension
// chat's agent answers with its own words, not its screen.
func TestTranscriptCodexByTitle(t *testing.T) {
	codexCacheFixture(t)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	id := "01a21669-6651-7a33-a746-8c631ff31b1a"
	rollout := filepath.Join(home, ".codex", "sessions", "2026", "11", "26", "rollout-2026-11-26T13-52-13-"+id+".jsonl")
	must(t, os.MkdirAll(filepath.Dir(rollout), 0o755))
	must(t, os.WriteFile(rollout, []byte(zstRollout), 0o644))
	must(t, os.WriteFile(filepath.Join(home, ".codex", "session_index.jsonl"), []byte(`{"id":"`+id+`","thread_name":"处理飞书私聊事件 #t1"}`+"\n"), 0o644))
	codex := "codex"
	tr := (&Server{home: home}).transcriptFor("k", "w1:p1", agentDetail{Agent: &codex, TerminalTitle: "处理飞书私聊事件 #t1 | me"}, map[string]*transcript{})
	if tr == nil {
		t.Fatal("not found by its title")
	}
	tr.update()
	msgs := tr.view()
	if len(msgs) == 0 || msgs[len(msgs)-1].Text != "Two files." {
		t.Fatalf("its last reply: %+v", msgs)
	}
}
