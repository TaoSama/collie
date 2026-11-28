package collie

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHeadlessStopKillsGroupHoldingStdout(t *testing.T) {
	home := t.TempDir()
	ready := filepath.Join(home, "group")
	next := filepath.Join(home, "next")
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *-p=long-turn)
    trap 'exit 130' INT
    (
      trap '' INT
      printf '%%s\n' "$$" > %q
      exec sleep 30
    ) 2>/dev/null &
    wait
    ;;
  *-p=after-stop)
    touch %q
    ;;
esac
`, ready, next)
	if err := os.WriteFile(filepath.Join(home, "agy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	stubHeadlessBin(t, home)

	const sid = "a3da86ee-aedd-420c-9ad6-d35940037dee"
	s := &Server{home: home}
	e := &External{ID: "ag-" + sid, Kind: "agy", Cwd: home, Surface: surfaceCLI, Session: sid, CanSend: true, SendVia: "headless", desktop: true}
	if _, err := s.sendHeadless(context.Background(), e, "long-turn", ""); err != nil {
		t.Fatal(err)
	}
	var pgid int
	waitUntil(t, 5*time.Second, func() bool {
		raw, err := os.ReadFile(ready)
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		pgid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && pgid > 0
	})
	t.Cleanup(func() {
		if s.headless.running(sid) {
			if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				t.Errorf("kill fixture group: %v", err)
			}
			waitUntil(t, 5*time.Second, func() bool { return !s.headless.running(sid) })
		}
	})

	externalCache.Lock()
	externalCache.at = time.Now()
	externalCache.list = []*External{e}
	externalCache.Unlock()
	t.Cleanup(forgetExternal)
	r := httptest.NewRequest("POST", "/api/external/"+e.ID+"/stop", nil)
	r.SetPathValue("id", e.ID)
	w := httptest.NewRecorder()
	s.stopExternal(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"stopped":true`) {
		t.Fatalf("stopExternal = %d %s, want 200 stopped:true", w.Code, w.Body.String())
	}
	if _, err := s.sendHeadless(context.Background(), e, "after-stop", ""); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 8*time.Second, func() bool { return !s.headless.running(sid) })
	if _, err := os.Stat(next); err != nil {
		t.Fatalf("next turn did not run after stop: %v", err)
	}
}
