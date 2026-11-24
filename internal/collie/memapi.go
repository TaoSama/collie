package collie

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Collie's memory files, readable and editable from the web UI.

type memFile struct {
	Name string    `json:"name"`
	Text string    `json:"text"`
	Size int64     `json:"size"`
	Mod  time.Time `json:"mod"`
	// The old single-file memory, waiting to be moved into memory/.
	Legacy bool `json:"legacy,omitempty"`
}

func readMemFile(path string) *memFile {
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	b, _ := os.ReadFile(path)
	return &memFile{Name: filepath.Base(path), Text: string(b), Size: fi.Size(), Mod: fi.ModTime()}
}

func (s *Server) superMemory(w http.ResponseWriter, r *http.Request) {
	sp := s.sup
	dir := sp.routerDir()
	upFront := sp.upFrontSize()
	msgs := sp.messagesSize()
	out := map[string]any{
		"dir":        s.tilde(dir),
		"files":      sp.memFiles(),
		"up_front":   upFront,
		"messages":   msgs,
		"loaded":     upFront + msgs,
		"budget":     upFrontBudget,
		"dreaming":   sp.dreaming(),
		"next_dream": nil,
		"last_dream": nil,
	}
	if t := sp.lastDream(); !t.IsZero() {
		out["last_dream"] = t
	}
	if t := sp.nextDream(time.Now()); !t.IsZero() {
		out["next_dream"] = t
	}
	dreams := []*memFile{}
	if m, _ := filepath.Glob(filepath.Join(dir, "dreams", "*.md")); len(m) > 0 {
		// Newest first: "2026-11-21-1840" after "2026-11-21", so compare without ".md".
		sort.Slice(m, func(i, j int) bool { return strings.TrimSuffix(m[i], ".md") > strings.TrimSuffix(m[j], ".md") })
		if len(m) > 30 {
			m = m[:30]
		}
		for _, p := range m {
			if f := readMemFile(p); f != nil {
				dreams = append(dreams, f)
			}
		}
	}
	out["dreams"] = dreams
	writeJSON(w, 200, out)
}

func (s *Server) superSaveMemory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
		Text string `json:"text"`
	}
	if !decode(w, r, &body) {
		return
	}
	rel, ok := memRel(body.Path)
	if !ok {
		writeJSON(w, 400, map[string]any{"error": "只能改 memory/ 里的 .md 文件"})
		return
	}
	if len(body.Text) > 256<<10 {
		writeJSON(w, 400, map[string]any{"error": "太长了"})
		return
	}
	p := filepath.Join(s.sup.memDir(), rel)
	os.MkdirAll(filepath.Dir(p), 0o700)
	text := strings.TrimRight(body.Text, "\n") + "\n"
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// memRel is a memory file's path relative to memory/, if the page may change it.
func memRel(path string) (string, bool) {
	rel := filepath.Clean(filepath.FromSlash(path))
	ok := strings.HasSuffix(rel, ".md") && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, "..") && rel != "messages.md"
	return rel, ok
}

// superDeleteMemory removes a memory file, and a note's line in notes/index.md.
func (s *Server) superDeleteMemory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if !decode(w, r, &body) {
		return
	}
	rel, ok := memRel(body.Path)
	if !ok {
		writeJSON(w, 400, map[string]any{"error": "只能删 memory/ 里的 .md 文件"})
		return
	}
	dir := s.sup.memDir()
	if err := os.Remove(filepath.Join(dir, rel)); err != nil {
		fail(w, err)
		return
	}
	if index := filepath.Join("notes", "index.md"); filepath.Dir(rel) == "notes" && rel != index {
		if err := dropIndexLine(filepath.Join(dir, index), filepath.Base(rel)); err != nil {
			fail(w, err)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// dropIndexLine removes the lines of an index that name file ("agents.md",
// "notes/agents.md", "[x](agents.md)"), not ones naming "codex-agents.md".
func dropIndexLine(index, file string) error {
	b, err := os.ReadFile(index)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	re := regexp.MustCompile(`(^|[^\w.-])` + regexp.QuoteMeta(file) + `($|[^\w-])`)
	lines := strings.Split(string(b), "\n")
	kept := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "#") || !re.MatchString(l) {
			kept = append(kept, l)
		}
	}
	if len(kept) == len(lines) {
		return nil
	}
	return os.WriteFile(index, []byte(strings.Join(kept, "\n")), 0o600)
}

func (s *Server) superDream(w http.ResponseWriter, r *http.Request) {
	if err := s.sup.StartDream(r.Context(), "manual"); err != nil {
		writeJSON(w, 409, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
