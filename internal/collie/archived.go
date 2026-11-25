package collie

import (
	"database/sql"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Codex and Antigravity have an archive of their own. A session archived
// there (in the app or in the terminal) leaves Collie's lists, and comes back
// once it is unarchived:
//
//   - Codex: threads.archived = 1 in <CODEX_HOME>/state_<n>.sqlite; its log
//     moves to archived_sessions/.
//   - Antigravity: the archived column of a conversation's header row
//     (chunk_index = -1) in conversations_fts, the search index inside
//     conversation_summaries.db. The app's index and the CLI's each keep one.
//
// The apps' own lists leave them out (desktop.go). An agent still open in a
// herdr pane is marked (paneSession.Archived) and the page hides it while it
// rests. The ended sessions the hub remembers (history.go) live on other
// computers: the hub asks each one which of them are archived along with its
// pane titles (titlesFor).

// agyArchivedSQL: the archived conversations in an Antigravity index.
const agyArchivedSQL = `SELECT conversation_id FROM conversations_fts WHERE chunk_index = -1 AND archived = 1`

var archivedCache struct {
	sync.Mutex
	at  time.Time
	ids map[string]bool
}

// archivedIDs: this computer's archived Codex threads and Antigravity
// conversations (their ids don't collide), read at most every 5 seconds.
func (s *Server) archivedIDs() map[string]bool {
	archivedCache.Lock()
	defer archivedCache.Unlock()
	if time.Since(archivedCache.at) < 5*time.Second && archivedCache.ids != nil {
		return archivedCache.ids
	}
	ids := map[string]bool{}
	var found []string
	if path := s.codexStateDB(); path != "" {
		found = append(found, sqliteColumn(path, "threads", `SELECT id FROM threads WHERE archived = 1`)...)
	}
	for _, root := range slices.Compact([]string{filepath.Join(s.home, ".gemini", "antigravity"), s.agyCLIHome()}) {
		// Older indexes have no search table: nothing archived there.
		if path := filepath.Join(root, "conversation_summaries.db"); isFile(path) {
			found = append(found, sqliteColumn(path, "conversations_fts", agyArchivedSQL)...)
		}
	}
	for _, id := range found {
		ids[id] = true
	}
	archivedCache.at, archivedCache.ids = time.Now(), ids
	return ids
}

func forgetArchived() {
	archivedCache.Lock()
	archivedCache.at = time.Time{}
	archivedCache.Unlock()
}

// archivedAmong: those of ids archived on this computer.
func (s *Server) archivedAmong(ids []string) []string {
	all := s.archivedIDs()
	out := []string{}
	for _, id := range ids {
		if all[id] {
			out = append(out, id)
		}
	}
	return out
}

// archivedOn: the archived sessions the hub knows of on a computer ("" =
// this one). A computer not heard from (or too old to say) hides nothing.
func (s *Server) archivedOn(machine string) map[string]bool {
	if machine == "" {
		return s.archivedIDs()
	}
	titles.Lock()
	defer titles.Unlock()
	if c := titles.by[machine]; c != nil {
		return c.archived
	}
	return nil
}

// endedOn: the remembered Codex and Antigravity sessions of one computer,
// the ones it is asked about.
func (s *Server) endedOn(machine string) []string {
	history.Lock()
	defer history.Unlock()
	s.loadHistory()
	var out []string
	for _, e := range history.list {
		if e.Machine == machine && hasOwnArchive(e.Kind) && e.Session != "" {
			out = append(out, e.Session)
		}
	}
	return out
}

func hasOwnArchive(kind string) bool { return kind == "codex" || kind == "agy" }

// withoutArchived drops the remembered sessions archived on their computer.
func (s *Server) withoutArchived(list []*Ended) []*Ended {
	sets := map[string]map[string]bool{}
	kept := list[:0]
	for _, e := range list {
		if hasOwnArchive(e.Kind) {
			a, ok := sets[e.Machine]
			if !ok {
				a = s.archivedOn(e.Machine)
				sets[e.Machine] = a
			}
			if a[e.Session] {
				continue
			}
		}
		kept = append(kept, e)
	}
	return kept
}

// splitIDs reads a comma-separated list of session ids.
func splitIDs(v string) []string {
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

// sqliteColumn runs a query that answers one text column, read-only, if
// the database has the table it reads.
func sqliteColumn(path, table, query string) []string {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		log.Printf("open %s: %v", path, err)
		return nil
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = ?`, table).Scan(&n); err != nil || n == 0 {
		return nil
	}
	rows, err := db.Query(query)
	if err != nil {
		log.Printf("read %s: %v", path, err)
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if rows.Scan(&v) == nil {
			out = append(out, v)
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("read %s: %v", path, err)
	}
	return out
}
