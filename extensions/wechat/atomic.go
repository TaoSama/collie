package wechat

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	privateDirMode  = 0o700
	privateFileMode = 0o600
)

// writePrivateJSON replaces path with data: temp file, fsync, rename, then
// fsync of every directory up to the root. A prior failed checkpoint may
// have created ancestors, even in another process, so all of them are
// synchronized on every write before it counts as done. A directory that
// cannot be synchronized fails the checkpoint, without a fallback.
func writePrivateJSON(path string, data any) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, privateDirMode); err != nil {
		return err
	}
	content, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	temp := filepath.Join(dir, fmt.Sprintf(".gateway-state-%d-%s.tmp", time.Now().UnixMilli(), hex.EncodeToString(suffix[:])))
	defer func() {
		if rmErr := os.Remove(temp); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) && err == nil {
			err = rmErr
		}
	}()
	f, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		return err
	}
	if err := checkpointHook("open-file", temp); err != nil {
		f.Close()
		return err
	}
	if err := checkpointHook("write-file", temp); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return err
	}
	if err := checkpointHook("fsync-file", temp); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := checkpointHook("close-file", temp); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	if err := checkpointHook("rename", path); err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	for d := real; ; d = filepath.Dir(d) {
		if err := syncDir(d); err != nil {
			return err
		}
		if filepath.Dir(d) == d {
			return nil
		}
	}
}

func syncDir(dir string) error {
	if err := checkpointHook("open-directory", dir); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := checkpointHook("fsync-directory", dir); err != nil {
		d.Close()
		return checkpointCloseHook(dir, err)
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return checkpointCloseHook(dir, err)
	}
	if err := d.Close(); err != nil {
		return err
	}
	return checkpointHook("close-directory", dir)
}

// checkpointStep, when set, sees every step of a checkpoint and may fail it
// (tests: the disk failures they stand for cannot be produced on demand).
var checkpointStep func(step, path string) error

func checkpointHook(step, path string) error {
	if checkpointStep == nil {
		return nil
	}
	return checkpointStep(step, path)
}

// checkpointCloseHook reports the close of a directory that failed earlier;
// the first failure wins.
func checkpointCloseHook(dir string, failure error) error {
	if err := checkpointHook("close-directory", dir); err != nil && failure == nil {
		return err
	}
	return failure
}

// readPrivateJSON decodes path into v; false when there is no file.
func readPrivateJSON(path string, maxBytes int64, v any) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	if maxBytes > 0 && st.Size() > maxBytes {
		return false, errStateTooLarge
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

var errStateTooLarge = errors.New("state exceeds byte budget")
