package collie

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Marks are what the hub keeps about a session for you, whichever computer
// it is on: marked unread, hidden (archived where its agent has no archive of
// its own: Claude Code), and the name you gave it, for the lists that don't
// read the agent's own again. The page keys them by computer and session.

type sessionMark struct {
	Unread   bool      `json:"unread,omitempty"`
	Archived bool      `json:"archived,omitempty"`
	Title    string    `json:"title,omitempty"`
	At       time.Time `json:"at"`
}

func (m *sessionMark) empty() bool { return !m.Unread && !m.Archived && m.Title == "" }

// maxMarks: the oldest go first beyond this many.
const maxMarks = 2000

var marks = struct {
	sync.Mutex
	m      map[string]*sessionMark
	loaded bool
}{}

func (s *Server) loadMarksLocked() {
	if marks.loaded {
		return
	}
	marks.loaded, marks.m = true, map[string]*sessionMark{}
	var raw string
	if s.sup != nil && s.sup.db.QueryRow(`SELECT v FROM kv WHERE k='session_marks'`).Scan(&raw) == nil {
		if err := json.Unmarshal([]byte(raw), &marks.m); err != nil {
			log.Printf("read session marks: %v", err)
			marks.m = map[string]*sessionMark{}
		}
	}
}

func (s *Server) saveMarksLocked() {
	if len(marks.m) > maxMarks {
		keys := make([]string, 0, len(marks.m))
		for k := range marks.m {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return marks.m[keys[i]].At.Before(marks.m[keys[j]].At) })
		for _, k := range keys[:len(keys)-maxMarks] {
			delete(marks.m, k)
		}
	}
	if s.sup == nil {
		return
	}
	b, _ := json.Marshal(marks.m)
	if _, err := s.sup.db.Exec(`INSERT INTO kv(k,v) VALUES('session_marks',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, string(b)); err != nil {
		log.Printf("save session marks: %v", err)
	}
}

// listMarks: GET /api/marks.
func (s *Server) listMarks(w http.ResponseWriter, r *http.Request) {
	marks.Lock()
	defer marks.Unlock()
	s.loadMarksLocked()
	writeJSON(w, 200, map[string]any{"marks": marks.m})
}

// setMark: POST /api/marks {key, unread?, archived?, title?}; what is left
// out stays as it was.
func (s *Server) setMark(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key      string  `json:"key"`
		Unread   *bool   `json:"unread"`
		Archived *bool   `json:"archived"`
		Title    *string `json:"title"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Key == "" {
		writeJSON(w, 400, map[string]any{"error": "缺少 key"})
		return
	}
	marks.Lock()
	defer marks.Unlock()
	s.loadMarksLocked()
	m := marks.m[body.Key]
	if m == nil {
		m = &sessionMark{}
	}
	if body.Unread != nil {
		m.Unread = *body.Unread
	}
	if body.Archived != nil {
		m.Archived = *body.Archived
	}
	if body.Title != nil {
		m.Title = *body.Title
	}
	m.At = time.Now()
	if m.empty() {
		delete(marks.m, body.Key)
	} else {
		marks.m[body.Key] = m
	}
	s.saveMarksLocked()
	writeJSON(w, 200, map[string]any{"marks": marks.m})
}
