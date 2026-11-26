package collie

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"
)

// codexPinnedSectionID is Codex's built-in "Pinned" thread section UUID in state_<n>.sqlite.
const codexPinnedSectionID = "01984de2-8f74-7c91-a3b2-5c5e937cf318"

var agyPinnedField = regexp.MustCompile(`(?m)^pinned[ \t]*:[ \t]*true[ \t]*\r?$`)

var pinnedCache struct {
	sync.Mutex
	at  time.Time
	ids map[string]bool
}

// codexPinnedIDs returns this computer's natively pinned Codex thread IDs,
// read at most every 5 seconds.
func (s *Server) codexPinnedIDs() map[string]bool {
	pinnedCache.Lock()
	defer pinnedCache.Unlock()
	if time.Since(pinnedCache.at) < 5*time.Second && pinnedCache.ids != nil {
		return pinnedCache.ids
	}
	ids := map[string]bool{}
	if path := s.codexStateDB(); path != "" {
		if db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(2000)"); err == nil {
			defer db.Close()
			var n int
			if db.QueryRow(`SELECT count(*) FROM pragma_table_info('threads') WHERE name = 'thread_section_id'`).Scan(&n) == nil && n > 0 {
				if rows, err := db.Query(`SELECT id FROM threads WHERE archived = 0 AND thread_section_id = ?`, codexPinnedSectionID); err == nil {
					for rows.Next() {
						var id string
						if rows.Scan(&id) == nil && id != "" {
							ids[id] = true
						}
					}
					rows.Close()
				}
			}
		}
	}
	pinnedCache.at, pinnedCache.ids = time.Now(), ids
	return ids
}

func forgetPinned() {
	pinnedCache.Lock()
	pinnedCache.at = time.Time{}
	pinnedCache.Unlock()
}

func (s *Server) codexPinnedList() []string {
	return slices.Collect(maps.Keys(s.codexPinnedIDs()))
}

func (s *Server) codexScheduledIDs() map[string]bool {
	ids := map[string]bool{}
	for _, id := range s.codexAutomationThreads() {
		ids[id] = true
	}
	return ids
}

// agyPinned reports whether an Antigravity conversation's annotation marks it pinned.
func agyPinned(root, id string) bool {
	if !conversationRe.MatchString(id) {
		return false
	}
	b, err := os.ReadFile(filepath.Join(root, "annotations", id+".pbtxt"))
	return err == nil && agyPinnedField.Match(b)
}

var agySchedCache struct {
	sync.Mutex
	at  time.Time
	ids map[string]bool
}

// agyScheduledIDs queries the running Antigravity desktop language server for
// conversations with an active "schedule" background task.
func (s *Server) agyScheduledIDs() map[string]bool {
	agySchedCache.Lock()
	defer agySchedCache.Unlock()
	if time.Since(agySchedCache.at) < 5*time.Second && agySchedCache.ids != nil {
		return agySchedCache.ids
	}
	ids := map[string]bool{}
	if ls := agyDesktopServer(); ls != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		out, err := ls.call(ctx, "ListBackgroundTasks", map[string]any{})
		cancel()
		if err != nil {
			var api *agyError
			if !errors.As(err, &api) || api.authFailed() {
				forgetAgyServer()
			}
		} else {
			var resp struct {
				Tasks []struct {
					ConversationID      string `json:"conversationId"`
					ConversationIDSnake string `json:"conversation_id"`
					Task                struct {
						ToolName      string `json:"toolName"`
						ToolNameSnake string `json:"tool_name"`
					} `json:"task"`
				} `json:"tasks"`
			}
			if json.Unmarshal(out, &resp) == nil {
				for _, t := range resp.Tasks {
					cid := cmp.Or(t.ConversationID, t.ConversationIDSnake)
					tool := cmp.Or(t.Task.ToolName, t.Task.ToolNameSnake)
					if cid != "" && tool == "schedule" {
						ids[cid] = true
					}
				}
			}
		}
	}
	agySchedCache.at, agySchedCache.ids = time.Now(), ids
	return ids
}

func forgetAgyScheduled() {
	agySchedCache.Lock()
	agySchedCache.at = time.Time{}
	agySchedCache.Unlock()
}

// claudeScheduledIn reads durable Claude Code scheduled tasks from
// <dir>/.claude/scheduled_tasks.json and returns their createdBySessionId set.
func claudeScheduledIn(dir string) map[string]bool {
	if dir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, ".claude", "scheduled_tasks.json"))
	if err != nil {
		return nil
	}
	var doc struct {
		Tasks []struct {
			CreatedBySessionID string `json:"createdBySessionId"`
		} `json:"tasks"`
	}
	if json.Unmarshal(b, &doc) != nil || len(doc.Tasks) == 0 {
		return nil
	}
	ids := map[string]bool{}
	for _, t := range doc.Tasks {
		if t.CreatedBySessionID != "" {
			ids[t.CreatedBySessionID] = true
		}
	}
	return ids
}

// scheduledList returns active Codex automation thread IDs plus durable Claude
// scheduled session IDs found in dirs.
func (s *Server) scheduledList(dirs []string) []string {
	ids := s.codexScheduledIDs()
	seen := map[string]bool{}
	for _, dir := range dirs {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		for id := range claudeScheduledIn(dir) {
			ids[id] = true
		}
	}
	return slices.Collect(maps.Keys(ids))
}

func (s *Server) pinnedOn(machine string) map[string]bool {
	if machine == "" {
		return s.codexPinnedIDs()
	}
	titles.Lock()
	defer titles.Unlock()
	if c := titles.by[machine]; c != nil {
		return c.pinned
	}
	return nil
}

func (s *Server) scheduledOn(machine string) map[string]bool {
	if machine == "" {
		return s.codexScheduledIDs()
	}
	titles.Lock()
	defer titles.Unlock()
	if c := titles.by[machine]; c != nil {
		return c.scheduled
	}
	return nil
}

func (s *Server) endedClaudeDirsOn(machine string) []string {
	history.Lock()
	defer history.Unlock()
	s.loadHistory()
	seen := map[string]bool{}
	var out []string
	for _, e := range history.list {
		if e.Machine == machine && e.Kind == "claude" && e.Cwd != "" && !seen[e.Cwd] {
			seen[e.Cwd] = true
			out = append(out, e.Cwd)
		}
	}
	return out
}
