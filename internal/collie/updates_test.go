package collie

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestOutdated(t *testing.T) {
	for _, c := range []struct {
		have, latest string
		want         bool
	}{
		{"v0.2.4", "v0.2.5", true},
		{"v0.2.10", "v0.2.9", false},
		{"v0.3.0", "v0.2.9", false},
		{"v1.0.0", "v1.0.0", false},
		{"dev", "v0.2.5", false},
		{"", "v0.2.5", false},
		{"v0.2.4", "", false},
	} {
		if got := outdated(c.have, c.latest); got != c.want {
			t.Errorf("outdated(%q, %q) = %v", c.have, c.latest, got)
		}
	}
}

func TestSelfUpdateConcurrencyMutexAndTempCleanup(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "collie")
	oldScript := "#!/bin/sh\necho collie v0.2.41\n"
	if err := os.WriteFile(exe, []byte(oldScript), 0o755); err != nil {
		t.Fatal(err)
	}

	newTag := "v0.2.42"
	newScript := "#!/bin/sh\necho collie " + newTag + "\n"
	sum := sha256.Sum256([]byte(newScript))
	goodSum := hex.EncodeToString(sum[:])
	asset := "collie-" + runtime.GOOS + "-" + runtime.GOARCH

	var serveBadChecksum atomic.Bool
	inDownload := make(chan struct{})
	var signalOnce sync.Once
	releaseDownload := make(chan struct{})

	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			w.Header().Set("Location", ts.URL+"/releases/tag/"+newTag)
			w.WriteHeader(http.StatusFound)
		case "/releases/download/" + newTag + "/checksums.txt":
			cs := goodSum
			if serveBadChecksum.Load() {
				cs = strings.Repeat("0", 64)
			}
			fmt.Fprintf(w, "%s  %s\n", cs, asset)
		case "/releases/download/" + newTag + "/" + asset:
			signalOnce.Do(func() { close(inDownload) })
			<-releaseDownload
			w.Write([]byte(newScript))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	origVer, origURL, origExe, origExec := version, releaseRepoURL, executablePath, execUpdated
	restarted := make(chan string, 1)
	version = "v0.2.41"
	releaseRepoURL = ts.URL
	executablePath = func() (string, error) { return exe, nil }
	execUpdated = func(p string) {
		restarted <- p
	}
	t.Cleanup(func() {
		version, releaseRepoURL, executablePath, execUpdated = origVer, origURL, origExe, origExec
		// Ensure the package-level lock is clean after test completion.
		if !selfUpdateMu.TryLock() {
			selfUpdateMu.Unlock()
		} else {
			selfUpdateMu.Unlock()
		}
	})

	srv := &Server{}

	// 1) Failed update (bad checksum) cleans up .collie.new.* and unlocks selfUpdateMu.
	serveBadChecksum.Store(true)
	close(releaseDownload)
	recFail := httptest.NewRecorder()
	srv.selfUpdate(recFail, httptest.NewRequest("POST", "/api/update", nil))
	if recFail.Code != 502 {
		t.Fatalf("bad checksum status = %d, want 502: %s", recFail.Code, recFail.Body.String())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".collie.new.*")); len(leftovers) != 0 {
		t.Fatalf("temp files left after failed update: %v", leftovers)
	}
	gotOld, _ := os.ReadFile(exe)
	if string(gotOld) != oldScript {
		t.Fatalf("exe modified on failed update: %q", string(gotOld))
	}

	// 2) Concurrent calls: first blocks mid-download; parallel callers get 409 "升级已在进行中"
	// without racing or deleting the first caller's temp file.
	serveBadChecksum.Store(false)
	inDownload = make(chan struct{})
	signalOnce = sync.Once{}
	releaseDownload = make(chan struct{})

	var wg sync.WaitGroup
	recPrimary := httptest.NewRecorder()
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv.selfUpdate(recPrimary, httptest.NewRequest("POST", "/api/update", nil))
	}()

	<-inDownload

	const contenders = 4
	contenderCodes := make([]int, contenders)
	contenderBodies := make([]string, contenders)
	var cwg sync.WaitGroup
	for i := range contenders {
		cwg.Add(1)
		go func(idx int) {
			defer cwg.Done()
			rec := httptest.NewRecorder()
			srv.selfUpdate(rec, httptest.NewRequest("POST", "/api/update", nil))
			contenderCodes[idx] = rec.Code
			contenderBodies[idx] = rec.Body.String()
		}(i)
	}
	cwg.Wait()

	for i := range contenders {
		if contenderCodes[i] != 409 || !strings.Contains(contenderBodies[i], "升级已在进行中") {
			t.Fatalf("contender %d: status=%d body=%s, want 409 with 升级已在进行中", i, contenderCodes[i], contenderBodies[i])
		}
	}

	// Release primary download; it must finish cleanly, atomically replace exe, and leave 0 temp files.
	close(releaseDownload)
	wg.Wait()

	if recPrimary.Code != 200 {
		t.Fatalf("primary update status = %d: %s", recPrimary.Code, recPrimary.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(recPrimary.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res["ok"] != true || res["from"] != "v0.2.41" || res["version"] != newTag {
		t.Fatalf("unexpected update response: %+v", res)
	}
	if gotExe := <-restarted; gotExe != exe {
		t.Fatalf("execUpdated path = %q, want %q", gotExe, exe)
	}
	// While restarting is held (before syscall.Exec replaces process), duplicate calls still get 409.
	recDuringRestart := httptest.NewRecorder()
	srv.selfUpdate(recDuringRestart, httptest.NewRequest("POST", "/api/update", nil))
	if recDuringRestart.Code != 409 {
		t.Fatalf("call during restart window = %d, want 409", recDuringRestart.Code)
	}
	selfUpdateMu.Unlock()

	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".collie.new.*")); len(leftovers) != 0 {
		t.Fatalf("temp files left after successful update: %v", leftovers)
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Fatalf("legacy fixed exe+.new should never be created: %v", err)
	}
	gotNew, err := os.ReadFile(exe)
	if err != nil || string(gotNew) != newScript {
		t.Fatalf("updated binary content = %q, err = %v", string(gotNew), err)
	}
	fi, err := os.Stat(exe)
	if err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("updated binary mode = %v, want executable", fi.Mode())
	}
}

// The collie an update execs into reaps the children the old one left: here
// a sleep that ends after the exec.
func TestReapInherited(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestReapHelper$")
	cmd.Env = append(os.Environ(), "COLLIE_REAP_HELPER=exec")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(string(out), "reaped") {
		t.Fatalf("helper: %s", out)
	}
}

func TestReapHelper(t *testing.T) {
	switch os.Getenv("COLLIE_REAP_HELPER") {
	case "exec": // the old collie: a child, then the update's exec
		child := exec.Command("sleep", "0.3")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		os.Setenv("COLLIE_REAP_HELPER", "updated")
		env := append(os.Environ(), "COLLIE_REAP_CHILD="+strconv.Itoa(child.Process.Pid), updatedEnv+"=1")
		t.Fatal(syscall.Exec(os.Args[0], os.Args, env))
	case "updated":
		if os.Getenv(updatedEnv) == "" {
			t.Fatal("no update mark")
		}
		reapInherited()
		if os.Getenv(updatedEnv) != "" {
			t.Fatal("the mark passes on to children")
		}
		time.Sleep(time.Second)
		stat, _ := exec.Command("ps", "-o", "stat=", "-p", os.Getenv("COLLIE_REAP_CHILD")).Output()
		if s := strings.TrimSpace(string(stat)); s != "" {
			t.Fatalf("the old child is %q", s)
		}
		fmt.Println("reaped")
	default:
		t.Skip("run by TestReapInherited")
	}
}
