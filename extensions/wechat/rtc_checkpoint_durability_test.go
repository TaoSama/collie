package wechat

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// The original replaces fs functions to watch and fail the steps of a
// checkpoint; checkpointStep is that seam here.

func rtcHook(t *testing.T, hook func(step, path string) error) {
	t.Helper()
	checkpointStep = hook
	t.Cleanup(func() { checkpointStep = nil })
}

func rtcRealDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	mustNoErr(t, err)
	return d
}

func rtcAncestors(dir string) []string {
	var out []string
	for d := dir; ; d = filepath.Dir(d) {
		out = append(out, d)
		if filepath.Dir(d) == d {
			return out
		}
	}
}

func rtcMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	mustNoErr(t, err)
	return st.Mode().Perm()
}

func rtcReadJSON(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	mustNoErr(t, err)
	return rtcCompactJSON(raw)
}

func rtcCompactJSON(raw []byte) string { return strings.Join(strings.Fields(string(raw)), "") }

// from: gateway/tests/runtime/checkpoint-durability.test.ts: checkpoint synchronizes file data before rename and the containing directory before returning
func TestRtcCheckpointOrder(t *testing.T) {
	dir := rtcRealDir(t)
	path := filepath.Join(dir, "state.json")
	var events []string
	rtcHook(t, func(step, _ string) error { events = append(events, step); return nil })
	mustNoErr(t, writePrivateJSON(path, map[string]string{"checkpoint": "new"}))
	want := []string{"open-file", "write-file", "fsync-file", "close-file", "rename"}
	for range rtcAncestors(dir) {
		want = append(want, "open-directory", "fsync-directory", "close-directory")
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v", events)
	}
	if got := rtcReadJSON(t, path); got != `{"checkpoint":"new"}` {
		t.Fatalf("content = %s", got)
	}
	if names := rtcReadDir(t, dir); !reflect.DeepEqual(names, []string{"state.json"}) {
		t.Fatalf("dir = %v", names)
	}
}

// from: gateway/tests/runtime/checkpoint-durability.test.ts: first checkpoint also synchronizes the entries of newly created parent directories
func TestRtcCheckpointNewParents(t *testing.T) {
	target := filepath.Join(rtcRealDir(t), "new", "nested")
	var synced []string
	rtcHook(t, func(step, path string) error {
		if step == "fsync-directory" {
			synced = append(synced, path)
		}
		return nil
	})
	mustNoErr(t, writePrivateJSON(filepath.Join(target, "state.json"), map[string]string{"checkpoint": "first"}))
	if !reflect.DeepEqual(synced, rtcAncestors(target)) {
		t.Fatalf("synced = %v", synced)
	}
}

// from: gateway/tests/runtime/checkpoint-durability.test.ts: checkpoint retries synchronize ancestors after ${failureLevel} directory EIO
func TestRtcCheckpointRetryAfterEIO(t *testing.T) {
	for _, level := range []string{"leaf", "parent", "existing-parent"} {
		t.Run(level, func(t *testing.T) {
			existing := rtcRealDir(t)
			target := filepath.Join(existing, "new", "nested")
			path := filepath.Join(target, "state.json")
			failDir := map[string]string{"leaf": target, "parent": filepath.Dir(target), "existing-parent": existing}[level]
			failure := &os.PathError{Op: "fsync", Path: failDir, Err: syscall.EIO}
			existingMode := rtcMode(t, existing)
			var synced []string
			fail := true
			rtcHook(t, func(step, p string) error {
				if step == "fsync-directory" {
					synced = append(synced, p)
					if fail && p == failDir {
						return failure
					}
				}
				return nil
			})
			if err := writePrivateJSON(path, map[string]string{"checkpoint": "first"}); err != failure {
				t.Fatalf("first = %v", err)
			}
			synced = nil
			if err := writePrivateJSON(path, map[string]string{"checkpoint": "retry"}); err != failure {
				t.Fatalf("retry = %v", err)
			}
			found := false
			for _, d := range synced {
				found = found || d == failDir
			}
			if !found {
				t.Fatal("retry must revisit the failed directory even when mkdir creates nothing")
			}
			fail, synced = false, nil
			mustNoErr(t, writePrivateJSON(path, map[string]string{"checkpoint": "recovered"}))
			if !reflect.DeepEqual(synced, rtcAncestors(target)) {
				t.Fatalf("synced = %v", synced)
			}
			if got := rtcReadJSON(t, path); got != `{"checkpoint":"recovered"}` {
				t.Fatalf("content = %s", got)
			}
			if rtcMode(t, path) != 0o600 || rtcMode(t, target) != 0o700 || rtcMode(t, filepath.Dir(target)) != 0o700 || rtcMode(t, existing) != existingMode {
				t.Fatal("modes changed")
			}
			if names := rtcReadDir(t, target); !reflect.DeepEqual(names, []string{"state.json"}) {
				t.Fatalf("dir = %v", names)
			}
		})
	}
}

// from: gateway/tests/runtime/checkpoint-durability.test.ts: checkpoint process restart cannot forget ancestors created before a failed synchronization
func TestRtcCheckpointRestartRevisitsAncestors(t *testing.T) {
	// A checkpoint keeps nothing in memory, so a new process is another call;
	// the path goes through a symlink as in the original.
	dir := rtcRealDir(t)
	alias := filepath.Join(dir, "alias")
	mustNoErr(t, os.Symlink(dir, alias))
	target := filepath.Join(dir, "new", "nested")
	path := filepath.Join(alias, "new", "nested", "state.json")
	attempt := func(failDir string) ([]string, error) {
		var synced []string
		rtcHook(t, func(step, p string) error {
			if step == "fsync-directory" {
				synced = append(synced, p)
				if p == failDir {
					return &os.PathError{Op: "fsync", Path: p, Err: syscall.EIO}
				}
			}
			return nil
		})
		err := writePrivateJSON(path, map[string]string{"checkpoint": "new-process"})
		return synced, err
	}
	if _, err := attempt(target); !errors.Is(err, syscall.EIO) {
		t.Fatalf("first = %v", err)
	}
	synced, err := attempt(filepath.Dir(target))
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("restarted = %v", err)
	}
	if !reflect.DeepEqual(synced, []string{target, filepath.Dir(target)}) {
		t.Fatalf("restarted synced = %v", synced)
	}
	synced, err = attempt("")
	mustNoErr(t, err)
	if !reflect.DeepEqual(synced, rtcAncestors(target)) {
		t.Fatalf("recovered synced = %v", synced)
	}
	if rtcMode(t, path) != 0o600 || rtcMode(t, target) != 0o700 {
		t.Fatal("modes changed")
	}
}

// from: gateway/tests/runtime/checkpoint-durability.test.ts: checkpoint fails closed on directory ${stage} ${code} and releases opened descriptors
func TestRtcCheckpointDirectoryFailures(t *testing.T) {
	for _, c := range []struct {
		stage string
		code  syscall.Errno
	}{{"open", syscall.EISDIR}, {"fsync", syscall.EINVAL}, {"fsync", syscall.ENOTSUP}, {"fsync", syscall.EIO}, {"close", syscall.EIO}} {
		t.Run(c.stage+"-"+c.code.Error(), func(t *testing.T) {
			dir := rtcRealDir(t)
			failure := &os.PathError{Op: c.stage, Path: dir, Err: c.code}
			opened, closed := 0, 0
			rtcHook(t, func(step, p string) error {
				if p != dir {
					return nil
				}
				switch step {
				case "open-directory":
					if c.stage == "open" {
						return failure
					}
					opened++
				case "fsync-directory":
					if c.stage == "fsync" {
						return failure
					}
				case "close-directory":
					closed++
					if c.stage == "close" {
						return failure
					}
				}
				return nil
			})
			if err := writePrivateJSON(filepath.Join(dir, "state.json"), map[string]string{"checkpoint": "new"}); err != failure {
				t.Fatalf("error = %v", err)
			}
			if opened != closed || closed != map[bool]int{true: 0, false: 1}[c.stage == "open"] {
				t.Fatalf("opened %d closed %d", opened, closed)
			}
			if names := rtcReadDir(t, dir); !reflect.DeepEqual(names, []string{"state.json"}) {
				t.Fatalf("dir = %v", names)
			}
		})
	}
}

// from: gateway/tests/runtime/checkpoint-durability.test.ts: directory synchronization failure in reply intent prevents the transport send
func TestRtcCheckpointIntentFailurePreventsSend(t *testing.T) {
	dir := rtcRealDir(t)
	s := newTestState(t, dir, PolicyOptions{})
	openWindow(t, s, testChat)
	key := "directory-failure"
	failure := &os.PathError{Op: "fsync", Path: dir, Err: syscall.EIO}
	rtcHook(t, func(step, _ string) error {
		if step == "fsync-directory" && s.HasUncertainReply(key) {
			return failure
		}
		return nil
	})
	sends := 0
	c := newTestClient(t, s, &loop{}, fakeHTTP(func(req *http.Request, _ []byte) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "sendmessage") {
			sends++
		}
		if strings.HasSuffix(req.URL.Path, "getconfig") {
			return jsonResponse(200, map[string]any{"ret": 0, "typing_ticket": "synthetic-ticket"}), nil
		}
		return jsonResponse(200, map[string]any{"ret": 0}), nil
	}))
	if _, err := c.SendMarkdownReply(context.Background(), testChat, "reply", ReplyOptions{DeliveryKey: key}); err != failure {
		t.Fatalf("error = %v", err)
	}
	checkpointStep = nil
	c.loop.do(func() {
		if sends != 0 || s.HasUncertainReply(key) {
			t.Fatalf("sends %d, live intent %v", sends, s.HasUncertainReply(key))
		}
	})
	// The rename may already be visible after a failed directory sync, so a reload keeps the intent.
	reloaded := newTestState(t, dir, PolicyOptions{})
	if !reloaded.HasUncertainReply(key) {
		t.Fatal("reload lost the persisted intent")
	}
}
