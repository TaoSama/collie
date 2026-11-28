package collie

import (
	"errors"
	"net/http"
	"time"
)

func (s *Server) superTaskList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tasks": s.sup.taskFollowups()})
}

func (s *Server) superTask(w http.ResponseWriter, r *http.Request) {
	var update TaskUpdate
	if decode(w, r, &update) {
		s.writeTaskUpdate(w, update)
	}
}

func (s *Server) superTaskRemind(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path    string    `json:"path"`
		Version uint64    `json:"version"`
		At      time.Time `json:"at"`
		LocalAt string    `json:"local_at"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.LocalAt != "" {
		if !body.At.IsZero() {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "provide at or local_at, not both"})
			return
		}
		at, err := time.ParseInLocation("2006-01-02T15:04", body.LocalAt, time.Local)
		if err != nil || at.Format("2006-01-02T15:04") != body.LocalAt {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "local_at needs YYYY-MM-DDTHH:MM in the hub time zone"})
			return
		}
		body.At = at
	}
	s.writeTaskUpdate(w, TaskUpdate{Action: "remind", Path: body.Path, Version: body.Version, At: body.At})
}

func (s *Server) writeTaskUpdate(w http.ResponseWriter, update TaskUpdate) {
	task, err := s.sup.updateTask(update)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, task)
	case errors.Is(err, errTaskConflict):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
	case errors.Is(err, errTaskMissing):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
	case errors.Is(err, errTaskInvalid):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
	default:
		queuedWriteError(w, err)
	}
}
