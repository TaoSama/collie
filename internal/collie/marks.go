package collie

import (
	"encoding/json"
	"log"
	"maps"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Marks are what the hub keeps about a session for you, whichever computer
// it is on: marked unread, archived in Collie, and the name you gave it.
// The page keys them by computer, agent and session.

type sessionMark struct {
	Unread         bool      `json:"unread,omitempty"`
	Archived       bool      `json:"archived,omitempty"` // legacy Claude archive
	CollieArchived bool      `json:"collie_archived,omitempty"`
	Title          string    `json:"title,omitempty"`
	At             time.Time `json:"at"`
}

func (m *sessionMark) empty() bool {
	return !m.Unread && !m.Archived && !m.CollieArchived && m.Title == ""
}

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

func (s *Server) saveMarks(next map[string]*sessionMark) error {
	if len(next) > maxMarks {
		keys := make([]string, 0, len(next))
		for k := range next {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return next[keys[i]].At.Before(next[keys[j]].At) })
		for _, k := range keys[:len(keys)-maxMarks] {
			delete(next, k)
		}
	}
	if s.sup == nil {
		return nil
	}
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	_, err = s.sup.db.Exec(`INSERT INTO kv(k,v) VALUES('session_marks',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, string(b))
	return err
}

// listMarks: GET /api/marks.
func (s *Server) listMarks(w http.ResponseWriter, r *http.Request) {
	marks.Lock()
	defer marks.Unlock()
	s.loadMarksLocked()
	writeJSON(w, 200, map[string]any{"marks": marks.m})
}

// setMark: POST /api/marks {key, unread?, collie_archived?, title?}; what is left
// out stays as it was.
func (s *Server) setMark(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key            string  `json:"key"`
		Unread         *bool   `json:"unread"`
		Archived       *bool   `json:"archived"`
		CollieArchived *bool   `json:"collie_archived"`
		Title          *string `json:"title"`
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
	var m sessionMark
	if old := marks.m[body.Key]; old != nil {
		m = *old
	}
	if body.Unread != nil {
		m.Unread = *body.Unread
	}
	if body.Archived != nil {
		m.Archived = *body.Archived
	}
	if body.CollieArchived != nil {
		m.CollieArchived = *body.CollieArchived
		m.Archived = false
	}
	if body.Title != nil {
		m.Title = *body.Title
	}
	m.At = time.Now()
	next := maps.Clone(marks.m)
	if m.empty() {
		delete(next, body.Key)
	} else {
		next[body.Key] = &m
	}
	if err := s.saveMarks(next); err != nil {
		log.Printf("save session marks: %v", err)
		writeJSON(w, 500, map[string]any{"error": "Collie 没能保存这次更改，请重试"})
		return
	}
	marks.m = next
	writeJSON(w, 200, map[string]any{"marks": marks.m})
}
