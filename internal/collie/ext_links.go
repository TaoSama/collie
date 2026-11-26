package collie

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A file too big to go through an extension (extensions/README.md §3.3) is
// sent as a link instead: the hub serves that one file for a day, without
// signing in, to whoever has the link; an HMAC signature keyed by the hub's
// token makes the link good for nothing else.

const (
	downloadLinkTTL = 24 * time.Hour
	// extFileInlineMost is the largest file sent inside channel.send: its
	// base64 has to fit one protocol frame (extproto.MaxFrame).
	extFileInlineMost = 40 << 20
	// replyFileMost bounds a file sent with `collie reply --file`.
	replyFileMost = 512 << 20
	// stagedKeep is how long an uploaded file waits for the reply it goes with.
	stagedKeep = 24 * time.Hour
)

var (
	outRefRe   = regexp.MustCompile(`^o[0-9a-f]{16}$`)
	stagedIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
	sigRe      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// downloadSig signs file n of send ref until exp (unix seconds).
func (s *Server) downloadSig(ref string, n int, exp int64) []byte {
	mac := hmac.New(sha256.New, []byte("collie-download\n"+s.token))
	fmt.Fprintf(mac, "%s\n%d\n%d", ref, n, exp)
	return mac.Sum(nil)
}

// fileLink is the address of file n of send ref, good until a day after at.
// The same send always gets the same link, so a retry sends the same text.
func (h *extHost) fileLink(ref string, n int, at time.Time) (string, error) {
	base := ""
	for _, u := range h.s.publicURLs {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
			base = u
			break
		}
	}
	if base == "" || h.s.token == "" {
		return "", errors.New("hub 没有对外地址（启动参数 --url），发不了下载链接")
	}
	exp := at.Add(downloadLinkTTL).Unix()
	return fmt.Sprintf("%s/dl/%s/%d?exp=%d&sig=%s", base, ref, n, exp, hex.EncodeToString(h.s.downloadSig(ref, n, exp))), nil
}

// serveDownload is GET /dl/{ref}/{n}: the file, while its link is good.
// Anything wrong is a plain 404, saying nothing about what exists.
func (s *Server) serveDownload(w http.ResponseWriter, r *http.Request) {
	ref, q := r.PathValue("ref"), r.URL.Query()
	n, nerr := strconv.Atoi(r.PathValue("n"))
	exp, eerr := strconv.ParseInt(q.Get("exp"), 10, 64)
	sig := q.Get("sig")
	if s.ext == nil || s.token == "" || !outRefRe.MatchString(ref) || nerr != nil || n < 0 || eerr != nil || !sigRe.MatchString(sig) ||
		time.Now().Unix() >= exp {
		http.NotFound(w, r)
		return
	}
	got, _ := hex.DecodeString(sig)
	if !hmac.Equal(s.downloadSig(ref, n, exp), got) {
		http.NotFound(w, r)
		return
	}
	o := s.ext.store.out(ref)
	if o == nil || n >= len(o.Files) {
		http.NotFound(w, r)
		return
	}
	f := o.Files[n]
	fh, err := os.Open(f.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	mime := f.Mime
	if mime == "" {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Disposition", attachmentDisposition(f.Name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox") // served without sign-in, beside the app
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "", fi.ModTime(), fh)
}

func (h *extHost) stagedDir() string { return filepath.Join(h.home, "ext-outbox", "staged") }

// stageReplyFile is POST /api/super/reply-files: `collie reply --file`
// uploads each file here first, streamed, and names it in the reply by the
// id it gets back.
func (s *Server) stageReplyFile(w http.ResponseWriter, r *http.Request) {
	if s.ext == nil {
		writeJSON(w, 404, map[string]any{"error": "extensions run on the hub"})
		return
	}
	if r.ContentLength > replyFileMost {
		writeJSON(w, 413, map[string]any{"error": fmt.Sprintf("文件太大了（最多 %d MB）", replyFileMost>>20)})
		return
	}
	dir := s.ext.stagedDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fail(w, err)
		return
	}
	id := randomToken()[:32]
	p := filepath.Join(dir, id)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fail(w, err)
		return
	}
	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, replyFileMost))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if rerr := os.Remove(p); rerr != nil {
			log.Printf("ext: drop a half upload: %v", rerr)
		}
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSON(w, 413, map[string]any{"error": fmt.Sprintf("文件太大了（最多 %d MB）", replyFileMost>>20)})
			return
		}
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "size": n})
}

// stagedPath is where an uploaded file waits; false for an id that isn't one.
func (h *extHost) stagedPath(id string) (string, bool) {
	if !stagedIDRe.MatchString(id) {
		return "", false
	}
	return filepath.Join(h.stagedDir(), id), true
}

// trimStaged drops uploads no reply took.
func (h *extHost) trimStaged(now time.Time) {
	ents, err := os.ReadDir(h.stagedDir())
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("ext: read staged uploads: %v", err)
		}
		return
	}
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil || now.Sub(fi.ModTime()) < stagedKeep {
			continue
		}
		if err := os.Remove(filepath.Join(h.stagedDir(), e.Name())); err != nil && !os.IsNotExist(err) {
			log.Printf("ext: drop a staged upload: %v", err)
		}
	}
}
