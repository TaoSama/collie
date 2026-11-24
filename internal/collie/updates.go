package collie

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"

	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The hub looks up the latest release now and then, so the devices page can
// say which computers run an older Collie. The release page's redirect
// (…/releases/latest → …/tag/vX.Y.Z) needs no API token or rate limit.

const repo = "TaoSama/collie"

var (
	selfUpdateMu   sync.Mutex
	releaseRepoURL = "https://github.com/" + repo
	executablePath = func() (string, error) {
		exe, err := os.Executable()
		if err == nil {
			exe, err = filepath.EvalSymlinks(exe)
		}
		return exe, err
	}
	execUpdated = func(exe string) {
		time.Sleep(800 * time.Millisecond)
		err := syscall.Exec(exe, os.Args, os.Environ())
		log.Printf("restart after update: %v", err) // only reached if exec failed
		os.Exit(1)
	}
)

var latestRel struct {
	sync.Mutex
	once    sync.Once
	tag     string
	checked time.Time
	busy    bool
}

// latestVersion returns the newest release tag seen so far ("" until known).
// It checks every hour; soon asks for another check in the background when the
// last one is more than a few minutes old (the devices page, where updates are
// looked at).
func latestVersion(soon bool) string {
	latestRel.once.Do(func() {
		go func() {
			for {
				refreshLatest()
				time.Sleep(time.Hour)
			}
		}()
	})
	latestRel.Lock()
	defer latestRel.Unlock()
	if soon && !latestRel.busy && !latestRel.checked.IsZero() && time.Since(latestRel.checked) > 10*time.Minute {
		go refreshLatest()
	}
	return latestRel.tag
}

func refreshLatest() {
	latestRel.Lock()
	if latestRel.busy {
		latestRel.Unlock()
		return
	}
	latestRel.busy = true
	latestRel.Unlock()
	t := fetchLatestTag()
	latestRel.Lock()
	defer latestRel.Unlock()
	latestRel.busy, latestRel.checked = false, time.Now()
	if t != "" {
		latestRel.tag = t
	}
}

func fetchLatestTag() string {
	c := &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := c.Head(releaseRepoURL + "/releases/latest")
	if err != nil {
		return ""
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return ""
	}
	if t := loc[i+5:]; parseVersion(t) != nil {
		return t
	}
	return ""
}

// parseVersion reads "v1.2.3" (nil for "dev" and anything else).
func parseVersion(v string) []int {
	v, ok := strings.CutPrefix(v, "v")
	if !ok {
		return nil
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out[i] = n
	}
	return out
}

// outdated: have is a release older than latest. Builds from source ("dev")
// and unknown versions (nodes from before v0.2.5 don't say) aren't judged.
func outdated(have, latest string) bool {
	a, b := parseVersion(have), parseVersion(latest)
	if a == nil || b == nil {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func (s *Server) getVersion(w http.ResponseWriter, r *http.Request) {
	latest := latestVersion(false)
	var old []string
	for _, m := range s.herdr.Machines() {
		v := m.Version
		if m.Local {
			v = version
		}
		if (m.Local || m.Online) && outdated(v, latest) {
			old = append(old, m.Name)
		}
	}
	writeJSON(w, 200, map[string]any{"version": version, "latest": latest, "outdated": old})
}

// selfUpdate replaces this computer's collie with the latest release and
// restarts it in place (same process id, so launchd, systemd or nohup keep
// tracking it). herdr and the agents in it keep running meanwhile.
func (s *Server) selfUpdate(w http.ResponseWriter, r *http.Request) {
	if !selfUpdateMu.TryLock() {
		writeJSON(w, 409, map[string]any{"error": "升级已在进行中，请稍候"})
		return
	}
	restarting := false
	defer func() {
		if !restarting {
			selfUpdateMu.Unlock()
		}
	}()
	if parseVersion(version) == nil {
		writeJSON(w, 400, map[string]any{"error": "这台的 collie 是从源码编译的（" + version + "），在源码里 git pull 后重新编译来更新"})
		return
	}
	// The hub's tag may be up to an hour old: only take it when it is news here.
	latest := r.URL.Query().Get("tag")
	if parseVersion(latest) == nil || !outdated(version, latest) {
		latest = fetchLatestTag()
	}
	if latest == "" {
		writeJSON(w, 502, map[string]any{"error": "查不到最新版本：这台电脑连不上 github.com？"})
		return
	}
	if !outdated(version, latest) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": version, "latest": true})
		return
	}
	exe, err := executablePath()
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "找不到自己的程序文件：" + err.Error()})
		return
	}
	if err := downloadRelease(r.Context(), latest, exe); err != nil {
		writeJSON(w, 502, map[string]any{"error": "更新失败：" + err.Error()})
		return
	}
	log.Printf("updated %s → %s; restarting", version, latest)
	writeJSON(w, 200, map[string]any{"ok": true, "from": version, "version": latest})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	restarting = true
	go execUpdated(exe)
}

// downloadRelease puts release tag's binary for this platform at exe,
// checked against the release's checksums and by running it once.
func downloadRelease(ctx context.Context, tag, exe string) error {
	asset := "collie-" + runtime.GOOS + "-" + runtime.GOARCH
	base := releaseRepoURL + "/releases/download/" + tag + "/"
	c := &http.Client{Timeout: 5 * time.Minute}
	get := func(name string) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", base+name, nil)
		resp, err := c.Do(req)
		if err == nil && resp.StatusCode != 200 {
			resp.Body.Close()
			return nil, fmt.Errorf("下载 %s：%s", name, resp.Status)
		}
		return resp, err
	}
	resp, err := get("checksums.txt")
	if err != nil {
		return err
	}
	want := ""
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == asset {
			want = f[0]
		}
	}
	resp.Body.Close()
	if want == "" {
		return fmt.Errorf("%s 没有 %s 的版本", tag, asset)
	}
	resp, err = get(asset)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.CreateTemp(filepath.Dir(exe), ".collie.new.*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			os.Remove(tmp)
		}
	}()
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Chmod(0o755); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return fmt.Errorf("下载的文件校验不对，再试一次")
	}
	out, rerr := exec.CommandContext(ctx, tmp, "version").Output()
	if rerr != nil || !strings.Contains(string(out), tag) {
		return fmt.Errorf("新版本在这台电脑上跑不起来：%v %s", rerr, strings.TrimSpace(string(out)))
	}
	if err := os.Rename(tmp, exe); err != nil {
		return err
	}
	tmp = ""
	return nil
}
