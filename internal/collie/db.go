package collie

import (
	"database/sql"
	"encoding/json"
	"log"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// SQLite (WAL) is the source of truth for the super-agent: the trigger inbox,
// threads, workers, conversation, timers and settings (in ~/.collie/herd.db). The process keeps a
// recent working set in memory and upserts it back; history stays in the DB.

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
CREATE TABLE IF NOT EXISTS kv       (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS triggers (id TEXT PRIMARY KEY, n INTEGER NOT NULL, at INTEGER NOT NULL,
                                     status TEXT NOT NULL, source TEXT NOT NULL, thread TEXT NOT NULL, data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS triggers_status ON triggers(status);
CREATE INDEX IF NOT EXISTS triggers_thread ON triggers(thread, n);
CREATE TABLE IF NOT EXISTS threads  (id TEXT PRIMARY KEY, last_at INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS workers  (name TEXT PRIMARY KEY, created_at INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS entries  (id TEXT PRIMARY KEY, n INTEGER NOT NULL, at INTEGER NOT NULL, thread TEXT NOT NULL, data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS entries_thread ON entries(thread, n);
CREATE TABLE IF NOT EXISTS timers   (id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS push_subs (endpoint TEXT PRIMARY KEY, device TEXT NOT NULL, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS ext_convs  (id TEXT PRIMARY KEY, ext TEXT NOT NULL, last_at INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS ext_msgs   (conv TEXT NOT NULL, id TEXT NOT NULL, at INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY (conv, id));
CREATE TABLE IF NOT EXISTS ext_outbox (ref TEXT PRIMARY KEY, conv TEXT NOT NULL, at INTEGER NOT NULL, state TEXT NOT NULL, data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS ext_outbox_conv ON ext_outbox(conv, at);
CREATE INDEX IF NOT EXISTS ext_outbox_state ON ext_outbox(state, at);
CREATE INDEX IF NOT EXISTS ext_msgs_at ON ext_msgs(conv, at);
CREATE TABLE IF NOT EXISTS ext_audit  (at INTEGER NOT NULL, ext TEXT NOT NULL, action TEXT NOT NULL, ms INTEGER NOT NULL, ok INTEGER NOT NULL);
`

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return db, nil
}

func idNum(id string) int64 {
	n, _ := strconv.ParseInt(strings.TrimLeft(id, "abcdefghijklmnopqrstuvwxyz"), 10, 64)
	return n
}

func loadRows[T any](db *sql.DB, q string, into func(*T), args ...any) error {
	rows, err := db.Query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return err
		}
		v := new(T)
		if json.Unmarshal([]byte(data), v) == nil {
			into(v)
		}
	}
	return rows.Err()
}

func loadState(db *sql.DB) (*State, error) {
	st := defaultState()
	kv := map[string]string{}
	rows, err := db.Query(`SELECT k, v FROM kv`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k, v string
		rows.Scan(&k, &v)
		kv[k] = v
	}
	rows.Close()
	if v, ok := kv["seq"]; ok {
		st.Seq, _ = strconv.Atoi(v)
	}
	if v, ok := kv["settings"]; ok {
		json.Unmarshal([]byte(v), &st.Settings)
		st.Settings.migrateReply()
		st.Settings.WorkerTitle = normalizeWorkerTitle(st.Settings.WorkerTitle)
	}
	if v, ok := kv["policies"]; ok {
		json.Unmarshal([]byte(v), &st.Policies)
	}
	if v, ok := kv["dismissed"]; ok {
		var ids []string
		if json.Unmarshal([]byte(v), &ids) == nil {
			for _, id := range ids {
				if id != "" {
					st.Dismissed[id] = true
				}
			}
		}
	}
	if v, ok := kv["pinned"]; ok {
		var ids []string
		if json.Unmarshal([]byte(v), &ids) == nil {
			seenPin := map[string]bool{}
			for _, id := range ids {
				id = strings.TrimSpace(id)
				if id != "" && !seenPin[id] {
					seenPin[id] = true
					st.Pinned = append(st.Pinned, id)
				}
			}
			if n := len(st.Pinned); n > maxPinned {
				st.Pinned = append([]string(nil), st.Pinned[n-maxPinned:]...)
			}
		}
	}

	if v, ok := kv["held"]; ok {
		if err := json.Unmarshal([]byte(v), &st.Held); err != nil {
			log.Printf("load held pending: %v", err)
		}
	}
	if v, ok := kv["reminded_at"]; ok {
		if err := json.Unmarshal([]byte(v), &st.RemindedAt); err != nil {
			log.Printf("load reminder time: %v", err)
		}
	}

	// Keep the entire pending queue alongside the recent history window.
	if err := loadRows(db, `SELECT data FROM triggers
		WHERE status = 'queued' OR id IN (SELECT id FROM triggers ORDER BY n DESC LIMIT ?)
		ORDER BY n`, func(t *Trigger) { st.Triggers = append(st.Triggers, t) }, keepTriggers); err != nil {
		return nil, err
	}
	loadRows(db, `SELECT data FROM (SELECT data, n FROM entries ORDER BY n DESC LIMIT ?) ORDER BY n`, func(e *Entry) { st.Entries = append(st.Entries, e) }, keepEntries)
	// A subscribed thread still affects routing after its recent activity ages out.
	const keepThreads = 500
	if err := loadRows(db, `SELECT data FROM threads
		WHERE id IN (SELECT id FROM threads ORDER BY last_at DESC LIMIT ?)
		OR CASE WHEN json_valid(data) THEN json_array_length(data, '$.subscribers') > 0 END`,
		func(t *Thread) { st.Threads[t.ID] = t }, keepThreads); err != nil {
		return nil, err
	}
	loadRows(db, `SELECT data FROM workers`, func(w *Worker) { st.Workers[w.Name] = w })
	loadRows(db, `SELECT data FROM timers`, func(t *Timer) { st.Timers = append(st.Timers, t) })
	return st, nil
}

// saveState upserts the in-memory working set in one transaction.
func saveState(db *sql.DB, st *State, deletedTimers []string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	put := func(q string, args ...any) {
		if err == nil {
			_, err = tx.Exec(q, args...)
		}
	}
	js := func(v any) string { b, _ := json.Marshal(v); return string(b) }

	err = writeStateSeq(tx, st.Seq)
	put(`INSERT INTO kv(k,v) VALUES('settings',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, js(st.Settings))
	put(`INSERT INTO kv(k,v) VALUES('policies',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, js(st.Policies))
	dismissed := make([]string, 0, len(st.Dismissed))
	for id, on := range st.Dismissed {
		if on && id != "" {
			dismissed = append(dismissed, id)
		}
	}
	sort.Slice(dismissed, func(i, j int) bool {
		ni, nj := idNum(dismissed[i]), idNum(dismissed[j])
		if ni != nj {
			return ni < nj
		}
		return dismissed[i] < dismissed[j]
	})
	put(`INSERT INTO kv(k,v) VALUES('dismissed',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, js(dismissed))
	pinned := st.Pinned
	if pinned == nil {
		pinned = []string{}
	}
	if len(pinned) > maxPinned {
		pinned = pinned[len(pinned)-maxPinned:]
	}
	put(`INSERT INTO kv(k,v) VALUES('pinned',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, js(pinned))
	held := st.Held
	if held == nil {
		held = []string{}
	}
	put(`INSERT INTO kv(k,v) VALUES('held',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, js(held))
	put(`INSERT INTO kv(k,v) VALUES('reminded_at',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, js(st.RemindedAt))
	for _, t := range st.Triggers {
		if err == nil {
			err = writeTrigger(tx, t)
		}
	}
	for _, th := range st.Threads {
		if err == nil {
			err = writeThread(tx, th)
		}
	}
	for _, w := range st.Workers {
		if err == nil {
			err = writeWorker(tx, w)
		}
	}
	for _, e := range st.Entries {
		if err == nil {
			err = writeEntry(tx, e)
		}
	}
	for _, t := range st.Timers {
		put(`INSERT INTO timers(id,data) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, t.ID, js(t))
	}
	for _, id := range deletedTimers {
		put(`DELETE FROM timers WHERE id=?`, id)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// threadHistory reads older triggers of a thread straight from the DB, for
// recall beyond the in-memory working set.
func threadHistory(db *sql.DB, thread string, before int64, limit int) ([]*Trigger, error) {
	var out []*Trigger
	err := loadRows(db, `SELECT data FROM triggers WHERE thread = ? AND n < ? ORDER BY n DESC LIMIT ?`,
		func(t *Trigger) { out = append(out, t) }, thread, before, limit)
	return out, err
}

// entryHistory reads a thread's chat from before entry number `before`,
// newest first, straight from the DB (memory keeps only the recent ones).
func entryHistory(db *sql.DB, thread string, before int64, limit int) []*Entry {
	var out []*Entry
	loadRows(db, `SELECT data FROM entries WHERE (thread = ? OR (? = 'chat:main' AND thread = '')) AND n < ? ORDER BY n DESC LIMIT ?`,
		func(e *Entry) { out = append(out, e) }, thread, thread, before, limit)
	return out
}

// entryHistoryUntil reads a thread's chat from before `before` down to `until`
// (inclusive) plus up to `pad` older context entries, newest first, capped at maxRows.
func entryHistoryUntil(db *sql.DB, thread string, before, until int64, pad, maxRows int) ([]*Entry, bool) {
	var out []*Entry
	loadRows(db, `SELECT data FROM entries WHERE (thread = ? OR (? = 'chat:main' AND thread = '')) AND n < ? AND n >= ? ORDER BY n DESC LIMIT ?`,
		func(e *Entry) { out = append(out, e) }, thread, thread, before, until, maxRows+1)
	if len(out) > maxRows {
		return out[:maxRows], true
	}
	if pad < 0 {
		pad = 0
	}
	tail := entryHistory(db, thread, until, pad+1)
	more := len(tail) > pad
	if more {
		tail = tail[:pad]
	}
	out = append(out, tail...)
	return out, more
}

// threadEntries reads all stored entries of a thread from SQLite, newest first.
func threadEntries(db *sql.DB, thread string) []*Entry {
	var out []*Entry
	loadRows(db, `SELECT data FROM entries WHERE (thread = ? OR (? = 'chat:main' AND thread = '')) ORDER BY n DESC`,
		func(e *Entry) { out = append(out, e) }, thread, thread)
	return out
}

// loadAgentRuns restores the models agents were started with (Herdr.runs).
func loadAgentRuns(db *sql.DB, h *Herdr) {
	m := map[string][2]string{}
	var raw string
	if db.QueryRow(`SELECT v FROM kv WHERE k='agent_runs'`).Scan(&raw) == nil {
		json.Unmarshal([]byte(raw), &m)
	}
	h.LoadRuns(m, func(m map[string][2]string) {
		b, _ := json.Marshal(m)
		if _, err := db.Exec(`INSERT INTO kv(k,v) VALUES('agent_runs',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, string(b)); err != nil {
			log.Printf("save agent runs: %v", err)
		}
	})
}

// loadAgentNames restores the names Collie gave its agents (see Herdr.named)
// and keeps them saved in the kv table.
func loadAgentNames(db *sql.DB, h *Herdr) {
	m := map[string]string{}
	var raw string
	if db.QueryRow(`SELECT v FROM kv WHERE k='agent_names'`).Scan(&raw) == nil {
		json.Unmarshal([]byte(raw), &m)
	}
	h.LoadNames(m, func(m map[string]string) {
		b, _ := json.Marshal(m)
		if _, err := db.Exec(`INSERT INTO kv(k,v) VALUES('agent_names',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, string(b)); err != nil {
			log.Printf("save agent names: %v", err)
		}
	})
}
