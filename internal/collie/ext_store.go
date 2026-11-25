package collie

import (
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// What the hub keeps of extension conversations (extensions/README.md §4,
// §7): every conversation's name and one line of preview; the recent
// messages only of conversations you opened; and every send (the outbox).

const (
	inboxKeepMsgs    = 50
	inboxPreviewMost = 80
	outboxKeep       = 7 * 24 * time.Hour
)

// InboxConv is one chat of an extension.
type InboxConv struct {
	ID      string    `json:"id"` // <ext>:<chat>
	Ext     string    `json:"ext"`
	Chat    string    `json:"chat"`
	Name    string    `json:"name"`
	Kind    string    `json:"kind"` // dm | group | self
	LastAt  time.Time `json:"last_at"`
	Preview string    `json:"preview,omitempty"`
	Unread  int       `json:"unread,omitempty"`
	// Owner: the signed-in person's own chat with the bot (WeChat).
	Owner bool `json:"owner,omitempty"`
	// What you set: handled at all, by anyone in it or only you, by whom.
	On      bool   `json:"on"`
	Anyone  bool   `json:"anyone,omitempty"`
	Handler string `json:"handler"` // collie | agent
	Worker  string `json:"worker,omitempty"`
	// The request being answered: its message and when it came.
	AskMsg string    `json:"ask_msg,omitempty"`
	AskAt  time.Time `json:"ask_at,omitempty"`
	AskBy  string    `json:"ask_by,omitempty"`
	// The messages handed over so far (the next request carries what came after).
	Handed time.Time `json:"handed,omitempty"`
}

func (c *InboxConv) thread() string { return extThreadPrefix + c.ID }

// InboxMsg is one message of an open conversation.
type InboxMsg struct {
	ID         string    `json:"id"`
	Conv       string    `json:"conv"`
	At         time.Time `json:"at"`
	Sender     string    `json:"sender,omitempty"`
	SenderName string    `json:"sender_name,omitempty"`
	Mine       bool      `json:"mine,omitempty"`
	Text       string    `json:"text"`
	Ref        string    `json:"ref,omitempty"` // a send of Collie's: its outbox ref
}

// OutboxItem is one send to a conversation.
type OutboxItem struct {
	Ref       string    `json:"ref"`
	Conv      string    `json:"conv"`
	Kind      string    `json:"kind"`
	Text      string    `json:"text"`
	Footer    string    `json:"footer,omitempty"` // the end of Text
	ReplyTo   string    `json:"reply_to,omitempty"`
	Trigger   string    `json:"trigger,omitempty"`
	State     string    `json:"state"` // pending | sent | deferred | uncertain | failed | expired
	MessageID string    `json:"message_id,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Tries     int       `json:"tries,omitempty"`
	At        time.Time `json:"at"`
	DoneAt    time.Time `json:"done_at,omitempty"`
	Files     []OutFile `json:"files,omitempty"`
}

// OutFile is a picture or file a send carries, kept on the hub's disk.
type OutFile struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Mime string `json:"mime"`
}

const outPending = "pending"

type extStore struct {
	db    *sql.DB
	mu    sync.Mutex
	convs map[string]*InboxConv
}

func newExtStore(db *sql.DB) *extStore {
	st := &extStore{db: db, convs: map[string]*InboxConv{}}
	loadRows(db, `SELECT data FROM ext_convs`, func(c *InboxConv) { st.convs[c.ID] = c })
	return st
}

func (st *extStore) putConvLocked(c *InboxConv) {
	b, _ := json.Marshal(c)
	if _, err := st.db.Exec(`INSERT INTO ext_convs(id,ext,last_at,data) VALUES(?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET last_at=excluded.last_at, data=excluded.data`, c.ID, c.Ext, c.LastAt.Unix(), string(b)); err != nil {
		log.Printf("ext: save conversation: %v", err)
	}
}

// conv returns a copy of a conversation.
func (st *extStore) conv(id string) (InboxConv, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	c := st.convs[id]
	if c == nil {
		return InboxConv{}, false
	}
	return *c, true
}

// update changes one conversation under the lock and saves it.
func (st *extStore) update(id string, fn func(c *InboxConv)) (InboxConv, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	c := st.convs[id]
	if c == nil {
		return InboxConv{}, false
	}
	fn(c)
	st.putConvLocked(c)
	return *c, true
}

// upsert makes sure the conversation exists (made with mk), then applies fn.
func (st *extStore) upsert(id string, mk func() *InboxConv, fn func(c *InboxConv)) InboxConv {
	st.mu.Lock()
	defer st.mu.Unlock()
	c := st.convs[id]
	if c == nil {
		c = mk()
		st.convs[id] = c
	}
	fn(c)
	st.putConvLocked(c)
	return *c
}

func (st *extStore) list(ext string) []InboxConv {
	st.mu.Lock()
	out := make([]InboxConv, 0, len(st.convs))
	for _, c := range st.convs {
		if ext == "" || c.Ext == ext {
			out = append(out, *c)
		}
	}
	st.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastAt.Equal(out[j].LastAt) {
			return out[i].LastAt.After(out[j].LastAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// addMsg stores a message of an open conversation; false when it was there.
func (st *extStore) addMsg(m InboxMsg) bool {
	b, _ := json.Marshal(m)
	res, err := st.db.Exec(`INSERT OR IGNORE INTO ext_msgs(conv,id,at,data) VALUES(?,?,?,?)`, m.Conv, m.ID, m.At.UnixMilli(), string(b))
	if err != nil {
		log.Printf("ext: save message: %v", err)
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false
	}
	if _, err := st.db.Exec(`DELETE FROM ext_msgs WHERE conv = ? AND id NOT IN (SELECT id FROM ext_msgs WHERE conv = ? ORDER BY at DESC LIMIT ?)`,
		m.Conv, m.Conv, inboxKeepMsgs); err != nil {
		log.Printf("ext: trim messages: %v", err)
	}
	return true
}

func (st *extStore) hasMsg(conv, id string) bool {
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM ext_msgs WHERE conv = ? AND id = ?`, conv, id).Scan(&n); err != nil {
		log.Printf("ext: read messages: %v", err)
	}
	return n > 0
}

// msgs is a conversation's stored messages, oldest first.
func (st *extStore) msgs(conv string, since time.Time) []InboxMsg {
	var out []InboxMsg
	loadRows(st.db, `SELECT data FROM ext_msgs WHERE conv = ? AND at > ? ORDER BY at`,
		func(m *InboxMsg) { out = append(out, *m) }, conv, since.UnixMilli())
	return out
}

func (st *extStore) dropMsgs(conv string) {
	if _, err := st.db.Exec(`DELETE FROM ext_msgs WHERE conv = ?`, conv); err != nil {
		log.Printf("ext: drop messages: %v", err)
	}
}

// putOutIf saves o only while the stored send is still in one of the
// states given: an answer that came late never undoes a newer one.
func (st *extStore) putOutIf(o *OutboxItem, from ...string) bool {
	b, _ := json.Marshal(o)
	res, err := st.db.Exec(`UPDATE ext_outbox SET state = ?, data = ? WHERE ref = ? AND state IN (?`+strings.Repeat(",?", len(from)-1)+`)`,
		append([]any{o.State, string(b), o.Ref}, toAny(from)...)...)
	if err != nil {
		log.Printf("ext: save send: %v", err)
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func (st *extStore) putOut(o *OutboxItem) {
	b, _ := json.Marshal(o)
	if _, err := st.db.Exec(`INSERT INTO ext_outbox(ref,conv,at,state,data) VALUES(?,?,?,?,?)
		ON CONFLICT(ref) DO UPDATE SET state=excluded.state, data=excluded.data`, o.Ref, o.Conv, o.At.Unix(), o.State, string(b)); err != nil {
		log.Printf("ext: save send: %v", err)
	}
}

func (st *extStore) out(ref string) *OutboxItem {
	var o *OutboxItem
	loadRows(st.db, `SELECT data FROM ext_outbox WHERE ref = ?`, func(x *OutboxItem) { o = x }, ref)
	return o
}

// outs lists sends: of one conversation (conv != ""), or in some states.
func (st *extStore) outs(conv string, states ...string) []*OutboxItem {
	q, args := `SELECT data FROM ext_outbox WHERE 1=1`, []any{}
	if conv != "" {
		q += ` AND conv = ?`
		args = append(args, conv)
	}
	if len(states) > 0 {
		q += ` AND state IN (?` + strings.Repeat(",?", len(states)-1) + `)`
		for _, s := range states {
			args = append(args, s)
		}
	}
	var out []*OutboxItem
	loadRows(st.db, q+` ORDER BY at`, func(o *OutboxItem) { out = append(out, o) }, args...)
	return out
}

// recentOuts is conv's sends since a time.
func (st *extStore) recentOuts(conv string, since time.Time) []*OutboxItem {
	var out []*OutboxItem
	loadRows(st.db, `SELECT data FROM ext_outbox WHERE conv = ? AND at >= ? ORDER BY at`, func(o *OutboxItem) { out = append(out, o) }, conv, since.Unix())
	return out
}

// lastOut is conv's latest send, nil when none.
func (st *extStore) lastOut(conv string) *OutboxItem {
	var o *OutboxItem
	loadRows(st.db, `SELECT data FROM ext_outbox WHERE conv = ? ORDER BY at DESC LIMIT 1`, func(x *OutboxItem) { o = x }, conv)
	return o
}

// waitingConvs are the conversations with a send not yet out.
func (st *extStore) waitingConvs(states ...string) []string {
	rows, err := st.db.Query(`SELECT DISTINCT conv FROM ext_outbox WHERE state IN (?`+strings.Repeat(",?", len(states)-1)+`)`, toAny(states)...)
	if err != nil {
		log.Printf("ext: read sends: %v", err)
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if rows.Scan(&c) == nil {
			out = append(out, c)
		}
	}
	return out
}

// expireStale ends sends pending for too long (the extension stayed off).
func (st *extStore) expireStale(before time.Time) {
	for _, o := range st.outs("", outPending) {
		if o.At.Before(before) {
			o.State, o.Reason, o.DoneAt = "expired", "扩展一直没有运行，没有发出", time.Now()
			st.putOutIf(o, outPending)
		}
	}
}

func (st *extStore) trimOutbox(now time.Time) {
	for _, o := range st.outs("") {
		if o.At.Before(now.Add(-outboxKeep)) && o.State != outPending {
			for _, f := range o.Files {
				if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
					log.Printf("ext: drop a sent file: %v", err)
				}
			}
		}
	}
	if _, err := st.db.Exec(`DELETE FROM ext_outbox WHERE at < ? AND state != ?`, now.Add(-outboxKeep).Unix(), outPending); err != nil {
		log.Printf("ext: trim outbox: %v", err)
	}
}

func (st *extStore) kv(k string) string {
	var v string
	if err := st.db.QueryRow(`SELECT v FROM kv WHERE k = ?`, k).Scan(&v); err != nil && err != sql.ErrNoRows {
		log.Printf("ext: read %s: %v", k, err)
	}
	return v
}

func (st *extStore) setKV(k, v string) {
	if _, err := st.db.Exec(`INSERT INTO kv(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v); err != nil {
		log.Printf("ext: save %s: %v", k, err)
	}
}
