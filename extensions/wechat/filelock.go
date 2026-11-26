package wechat

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func processIsAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}

func reclaimableLock(lockPath string) (bool, error) {
	st, err := os.Stat(lockPath)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	expired := time.Since(st.ModTime()) >= lockStaleAfter
	raw, err := os.ReadFile(lockPath)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var rec struct {
		PID any `json:"pid"`
	}
	if json.Unmarshal(raw, &rec) != nil {
		// A live owner may not have finished writing its record yet.
		return expired, nil
	}
	pid, isInt := safeInt(rec.PID)
	// No other live process can share our pid, so a record carrying it was
	// left by a previous process whose pid was reused (containers).
	if isInt && int(pid) == os.Getpid() {
		heldLocksMu.Lock()
		held := heldLocks[lockPath]
		heldLocksMu.Unlock()
		return !held, nil
	}
	if !expired {
		return false, nil
	}
	return !isInt || pid <= 0 || !processIsAlive(int(pid)), nil
}

// withFileLock serializes inbox writers across processes with an exclusive
// lock file, reclaiming locks whose owner died.
func withFileLock(target string, action func() error) error {
	lockPath := target + ".lock"
	started := time.Now()
	for {
		if err := os.MkdirAll(filepath.Dir(target), privateDirMode); err != nil {
			return err
		}
		f, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateFileMode)
		if err == nil {
			record, _ := json.Marshal(map[string]int64{"pid": int64(os.Getpid()), "createdAt": nowMs()})
			_, werr := f.Write(record)
			if werr == nil {
				werr = f.Sync()
			}
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				return werr
			}
			heldLocksMu.Lock()
			heldLocks[lockPath] = true
			heldLocksMu.Unlock()
			actionErr := action()
			heldLocksMu.Lock()
			delete(heldLocks, lockPath)
			heldLocksMu.Unlock()
			if rmErr := os.Remove(lockPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) && actionErr == nil {
				return rmErr
			}
			return actionErr
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		reclaim, err := reclaimableLock(lockPath)
		if err != nil {
			return err
		}
		if reclaim {
			if err := os.Remove(lockPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			continue
		}
		if time.Since(started) >= lockWaitTimeout {
			return errInboxLockTimeout
		}
		time.Sleep(lockRetryDelay)
	}
}
