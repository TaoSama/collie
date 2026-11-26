package collie

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	codexZstSuffix   = ".zst"
	codexCacheBudget = 256 << 20
	codexCachePrefix = "rollout-v2-"
	codexUnpackTemp  = ".unpack-"
)

func codexCacheDir() string { return filepath.Join(collieDir(), "cache", "codex") }

func codexRollout(path string) string {
	if path == "" || isFile(path) {
		return path
	}
	if filepath.IsAbs(path) && isFile(path+codexZstSuffix) {
		return path + codexZstSuffix
	}
	return ""
}

func compressedCodex(path, format string) bool {
	return format == "codex" && strings.HasSuffix(path, ".jsonl"+codexZstSuffix)
}

func openCodexDecoder(r io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
}

func codexFingerprint(path string, fi fs.FileInfo) string {
	return fmt.Sprintf("%s:%d:%d", path, fi.Size(), fi.ModTime().UnixNano())
}

type transcriptProgress struct {
	Read  int64 `json:"read"`
	Total int64 `json:"total"`
}

type codexUnpacking struct {
	done   chan struct{}
	cancel context.CancelFunc
	users  int
	read   atomic.Int64
	file   *os.File
	err    error
}

var codexUnpacks = struct {
	sync.Mutex
	jobs map[string]*codexUnpacking
}{jobs: map[string]*codexUnpacking{}}

// Limit simultaneous disk-heavy decodes. Same-source readers share a file
// descriptor and use independent offsets via ReadAt.
var codexDecodeSlot = make(chan struct{}, 1)

func openCodexReplay(ctx context.Context, path string, progress func(transcriptProgress), budget int64) (*os.File, func(), error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	dir := codexCacheDir()
	key := filepath.Join(dir, fmt.Sprintf("%s%x.jsonl", codexCachePrefix, sha256.Sum256([]byte(codexFingerprint(path, fi)))))
	codexUnpacks.Lock()
	if f, err := os.Open(key); err == nil {
		codexUnpacks.Unlock()
		now := time.Now()
		if err := os.Chtimes(key, now, now); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("codex: touch replay: %v", err)
		}
		return f, func() { closeCodexFile(f) }, nil
	}
	job := codexUnpacks.jobs[key]
	if job == nil {
		work, cancel := context.WithCancel(context.Background())
		job = &codexUnpacking{done: make(chan struct{}), cancel: cancel}
		codexUnpacks.jobs[key] = job
		go job.unpack(work, path, key, budget)
	}
	job.users++
	codexUnpacks.Unlock()

	release := func() {
		codexUnpacks.Lock()
		defer codexUnpacks.Unlock()
		job.users--
		if job.users == 0 {
			job.cancel()
			delete(codexUnpacks.jobs, key)
			select {
			case <-job.done:
				if job.file != nil {
					closeCodexFile(job.file)
				}
			default:
			}
		}
	}
	if progress != nil {
		progress(transcriptProgress{})
	}
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	var reported int64 = -1
	report := func() {
		n := job.read.Load()
		if progress != nil && n > 0 && n != reported {
			reported = n
			progress(transcriptProgress{Read: n, Total: fi.Size()})
		}
	}
	for {
		select {
		case <-ctx.Done():
			release()
			return nil, nil, ctx.Err()
		case <-job.done:
			report()
			if job.err != nil {
				release()
				return nil, nil, job.err
			}
			return job.file, release, nil
		case <-ticker.C:
			report()
		}
	}
}

func (job *codexUnpacking) unpack(ctx context.Context, path, key string, budget int64) {
	select {
	case codexDecodeSlot <- struct{}{}:
		job.file, job.err = job.decode(ctx, path, key, budget)
		<-codexDecodeSlot
	case <-ctx.Done():
		job.err = ctx.Err()
	}
	codexUnpacks.Lock()
	defer codexUnpacks.Unlock()
	if job.users == 0 && job.file != nil {
		closeCodexFile(job.file)
	}
	close(job.done)
}

type codexInput struct {
	ctx  context.Context
	in   io.Reader
	read *atomic.Int64
}

func (r codexInput) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.in.Read(p)
	r.read.Add(int64(n))
	return n, err
}

func (job *codexUnpacking) decode(ctx context.Context, path, key string, budget int64) (*os.File, error) {
	// Another process sharing COLLIE_HOME may have published it while we waited.
	if f, err := os.Open(key); err == nil {
		return f, nil
	}
	in, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer closeCodexFile(in)
	dec, err := openCodexDecoder(codexInput{ctx, in, &job.read})
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	dir := filepath.Dir(key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, codexUnpackTemp+"*")
	if err != nil {
		return nil, err
	}
	// Unlink before writing. Oversized replays stay readable by current
	// requests and disappear even if the process is killed.
	if err := os.Remove(f.Name()); err != nil {
		closeCodexFile(f)
		return nil, err
	}
	n, err := io.Copy(f, dec)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		closeCodexFile(f)
		return nil, err
	}
	if n <= budget {
		if err := publishCodexReplay(key, f, n, budget); err != nil {
			log.Printf("codex: cache replay: %v", err)
		}
	}
	return f, nil
}

func closeCodexFile(f *os.File) {
	if err := f.Close(); err != nil {
		log.Printf("codex: close replay: %v", err)
	}
}

// Serialize admission and cleanup across hub/node processes that share a
// data directory. Readers already holding a descriptor survive unlink.
func withCodexCache(dir string, fn func() error) error {
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer closeCodexFile(lock)
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	return fn()
}

func publishCodexReplay(key string, f *os.File, size, budget int64) error {
	dir := filepath.Dir(key)
	return withCodexCache(dir, func() error {
		if isFile(key) {
			return nil
		}
		if err := trimCodexCache(dir, budget-size); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(dir, ".publish-*")
		if err != nil {
			return err
		}
		if _, err = io.Copy(tmp, io.NewSectionReader(f, 0, size)); err != nil {
			closeCodexFile(tmp)
		} else if err = tmp.Close(); err == nil {
			err = os.Rename(tmp.Name(), key)
		}
		if err != nil {
			return errors.Join(err, os.Remove(tmp.Name()))
		}
		return nil
	})
}

func maintainCodexCache(ctx context.Context) {
	dir := codexCacheDir()
	for {
		if isDir(dir) {
			if err := withCodexCache(dir, func() error { return trimCodexCache(dir, codexCacheBudget) }); err != nil {
				log.Printf("codex: clean replay cache: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

// Drop pre-budget copies (mirrored absolute paths) and interrupted writes,
// then evict by last use until there's room for the incoming replay.
func trimCodexCache(dir string, budget int64) error {
	type cached struct {
		path string
		info fs.FileInfo
	}
	var files []cached
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		name := d.Name()
		switch {
		case filepath.Dir(path) == dir && strings.HasPrefix(name, codexCachePrefix) && strings.HasSuffix(name, ".jsonl"):
			fi, err := d.Info()
			if err != nil {
				return err
			}
			files = append(files, cached{path, fi})
			total += fi.Size()
		case strings.HasPrefix(name, ".publish-"), strings.HasSuffix(name, ".jsonl"):
			return os.Remove(path)
		case strings.HasPrefix(name, codexUnpackTemp):
			// Another process can be between CreateTemp and unlink.
			fi, err := d.Info()
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if time.Since(fi.ModTime()) > time.Hour {
				return os.Remove(path)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].info.ModTime().Before(files[j].info.ModTime()) })
	for _, f := range files {
		if total <= budget {
			break
		}
		if err := os.Remove(f.path); err != nil {
			return err
		}
		total -= f.info.Size()
	}
	return nil
}
