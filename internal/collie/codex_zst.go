package collie

import (
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Codex compresses a rollout nobody has written to for a week into
// <name>.jsonl.zst, and its thread table keeps the plain name. Collie reads
// logs by byte offset, a line at a time, as they grow, so a compressed one is
// unpacked once into COLLIE_HOME/cache/codex/ under its own absolute path,
// with its time; every reader then takes it for the plain log it was. Codex
// never writes to a compressed rollout again, so a copy is never refreshed.
// It is done while listing, not on opening: the sidebar orders the threads
// by their logs. Paths are Unix ones: Collie isn't built for Windows.

const (
	codexZstSuffix   = ".zst"
	codexUnpackTemp  = ".unpack-" // prefix of a copy still being written
	codexCachePrune  = time.Hour  // how often unpacked logs whose .zst is gone are removed
	codexUnpackStale = time.Hour  // a half-written unpacking older than this was abandoned
)

var codexUnpack struct {
	sync.Mutex // one unpacking at a time: a 40 MB log takes a moment
	pruned     time.Time
	failed     map[string]time.Time // .zst → its time, when it couldn't be unpacked
}

func codexCacheDir() string { return filepath.Join(collieDir(), "cache", "codex") }

// codexRollout is where the rollout recorded at path can be read: path
// itself, the unpacked copy of path.zst when Codex has compressed it, or ""
// when neither is there.
func codexRollout(path string) string {
	if path == "" || isFile(path) {
		return path
	}
	zst := path + codexZstSuffix
	zi, err := os.Stat(zst)
	if err != nil || !zi.Mode().IsRegular() || !filepath.IsAbs(path) {
		return ""
	}
	plain := filepath.Join(codexCacheDir(), path)
	if unpacked(plain, zi) {
		return plain
	}
	codexUnpack.Lock()
	defer codexUnpack.Unlock()
	if time.Since(codexUnpack.pruned) >= codexCachePrune {
		codexUnpack.pruned = time.Now()
		go pruneCodexCache(codexCacheDir())
	}
	if unpacked(plain, zi) {
		return plain
	}
	if at, ok := codexUnpack.failed[zst]; ok && at.Equal(zi.ModTime()) {
		return ""
	}
	if err := unpackZst(zst, plain, zi.ModTime()); err != nil {
		log.Printf("codex: unpack %s: %v", zst, err)
		if codexUnpack.failed == nil {
			codexUnpack.failed = map[string]time.Time{}
		}
		codexUnpack.failed[zst] = zi.ModTime()
		return ""
	}
	return plain
}

// unpacked: plain is the copy of the .zst described by zi. Times are compared
// to the second, which every filesystem keeps.
func unpacked(plain string, zi fs.FileInfo) bool {
	fi, err := os.Stat(plain)
	return err == nil && fi.ModTime().Truncate(time.Second).Equal(zi.ModTime().Truncate(time.Second))
}

// unpackZst writes src's contents to dst, whole or not at all, dated mod.
func unpackZst(src, dst string, mod time.Time) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	dec, err := zstd.NewReader(in, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
	if err != nil {
		return err
	}
	defer dec.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(dst), codexUnpackTemp+"*")
	if err != nil {
		return err
	}
	tmp := out.Name()
	_, err = io.Copy(out, dec)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chtimes(tmp, time.Now(), mod)
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// pruneCodexCache removes the unpacked logs whose compressed rollout is gone
// (Codex archived or deleted the thread), and abandoned unpackings.
func pruneCodexCache(dir string) {
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), codexUnpackTemp) {
			if fi, err := d.Info(); err == nil && time.Since(fi.ModTime()) < codexUnpackStale {
				return nil
			}
		} else if isFile(string(filepath.Separator) + rel + codexZstSuffix) {
			return nil
		}
		if err := os.Remove(p); err != nil {
			log.Printf("codex: prune %s: %v", p, err)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		log.Printf("codex: prune %s: %v", dir, err)
	}
}
