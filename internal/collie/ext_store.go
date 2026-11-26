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

	"github.com/TaoSama/collie/internal/extproto"
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
	// AtMe: an unread message @-mentions you, or everyone.
	AtMe bool `json:"at_me,omitempty"`
	// AppUnread: the chat app's own unread count (capability read), counted
	// up between its reports; ReadBase: how much of it you read in Collie
	// without the app knowing. Unread is AppUnread - ReadBase.
	AppUnread int `json:"app_unread,omitempty"`
	ReadBase  int `json:"read_base,omitempty"`
	// Muted in the chat app: never an alert in Collie, its unread count grey.
	Muted bool `json:"muted,omitempty"`
	// Folded away in the chat app's list: left out of the inbox list.
	Folded bool `json:"folded,omitempty"`
	// Avatar: the chat's picture, an address the hub fetches (inbox_avatar.go).
	Avatar string `json:"avatar,omitempty"`
	// Tags: what the chat is besides its kind, as the extension tells it.
	Tags []string `json:"tags,omitempty"`
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
	// Topics: a group's topics that are conversations of their own (the
	// Feishu switch 群话题各开对话), each with its request and agent.
	Topics map[string]*ConvAsk `json:"topics,omitempty"`
	// The messages handed over so far (the next request carries what came after).
	Handed time.Time `json:"handed,omitempty"`
}

func (c *InboxConv) thread() string { return extThreadPrefix + c.ID }

// markRead: you have read it in Collie. The app's count stays where it is
// until the app is told or you read it there.
func (c *InboxConv) markRead() {
	c.Unread, c.AtMe, c.ReadBase = 0, false, c.AppUnread
}

// appRead takes the chat app's unread count and, when it knows, whether an
// unread message @-mentions you; false when nothing changed.
// A count under what you read in Collie means you read on there: the base
// follows it down.
func (c *InboxConv) appRead(n int, atMe *bool) bool {
	was := *c
	n = max(n, 0)
	c.AppUnread, c.ReadBase = n, min(c.ReadBase, n)
	c.Unread = n - c.ReadBase
	if atMe != nil {
		c.AtMe = *atMe
	}
	if c.Unread == 0 {
		c.AtMe = false
	}
	return c.AppUnread != was.AppUnread || c.ReadBase != was.ReadBase || c.Unread != was.Unread || c.AtMe != was.AtMe
}

// threadOf is a topic's thread ("" the chat's own).
func (c *InboxConv) threadOf(topic string) string {
	if topic == "" {
		return c.thread()
	}
	return c.thread() + extTopicSep + topic
}

// convKey names a conversation with its own agent: the chat, or a topic of it.
func (c *InboxConv) convKey(topic string) string {
	if topic == "" {
		return c.ID
	}
	return c.ID + extTopicSep + topic
}

// ConvAsk is the request being answered in a conversation and its agent.
type ConvAsk struct {
	Worker string    `json:"worker,omitempty"`
	AskMsg string    `json:"ask_msg,omitempty"`
	AskAt  time.Time `json:"ask_at,omitempty"`
	AskBy  string    `json:"ask_by,omitempty"`
}

// topicsMost bounds the group topics a chat remembers, newest kept.
const topicsMost = 50

// askOf: the chat's own request and agent, or a topic's.
func (c *InboxConv) askOf(topic string) ConvAsk {
	if topic == "" {
		return ConvAsk{Worker: c.Worker, AskMsg: c.AskMsg, AskAt: c.AskAt, AskBy: c.AskBy}
	}
	if a := c.Topics[topic]; a != nil {
		return *a
	}
	return ConvAsk{}
}

// setAsk changes the chat's own request and agent, or a topic's.
func (c *InboxConv) setAsk(topic string, change func(*ConvAsk)) {
	a := c.askOf(topic)
	change(&a)
	if topic == "" {
		c.Worker, c.AskMsg, c.AskAt, c.AskBy = a.Worker, a.AskMsg, a.AskAt, a.AskBy
		return
	}
	if c.Topics == nil {
		c.Topics = map[string]*ConvAsk{}
	}
	c.Topics[topic] = &a
	for len(c.Topics) > topicsMost {
		oldest := ""
		for k, v := range c.Topics {
			if oldest == "" || v.AskAt.Before(c.Topics[oldest].AskAt) {
				oldest = k
			}
		}
		delete(c.Topics, oldest)
	}
}

// InboxMsg is one message of an open conversation.
type InboxMsg struct {
	ID         string                `json:"id"`
	Conv       string                `json:"conv"`
	At         time.Time             `json:"at"`
	Sender     string                `json:"sender,omitempty"`
	SenderName string                `json:"sender_name,omitempty"`
	Mine       bool                  `json:"mine,omitempty"`
	Text       string                `json:"text"`
	Ref        string                `json:"ref,omitempty"` // a send of Collie's: its outbox ref
	// Bot: Collie's answer, sent from your account (it ends in the footer).
	Bot bool `json:"bot,omitempty"`
	View       *extproto.MessageView `json:"view,omitempty"`
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
	// Link: too big to go through the extension, it goes as this download
	// link (ext_links.go) instead.
	Link string `json:"link,omitempty"`
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

type inboxWriter interface {
	Exec(string, ...any) (sql.Result, error)
}

func putInboxConv(w inboxWriter, c *InboxConv) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = w.Exec(`INSERT INTO ext_convs(id,ext,last_at,data) VALUES(?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET last_at=excluded.last_at, data=excluded.data`, c.ID, c.Ext, c.LastAt.Unix(), string(b))
	return err
}

func (st *extStore) putConvLocked(c *InboxConv) {
	if err := putInboxConv(st.db, c); err != nil {
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

// putInboxMsg returns false with no error only for an existing message.
func putInboxMsg(w inboxWriter, m InboxMsg) (bool, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return false, err
	}
	res, err := w.Exec(`INSERT INTO ext_msgs(conv,id,at,data) VALUES(?,?,?,?) ON CONFLICT(conv,id) DO NOTHING`, m.Conv, m.ID, m.At.UnixMilli(), string(b))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func trimInboxMsgs(w inboxWriter, conv string) error {
	_, err := w.Exec(`DELETE FROM ext_msgs WHERE conv = ? AND id NOT IN (SELECT id FROM ext_msgs WHERE conv = ? ORDER BY at DESC LIMIT ?)`,
		conv, conv, inboxKeepMsgs)
	return err
}

func (st *extStore) addMsg(m InboxMsg) {
	added, err := putInboxMsg(st.db, m)
	if err != nil {
		log.Printf("ext: save message: %v", err)
	} else if added {
		if err := trimInboxMsgs(st.db, m.Conv); err != nil {
			log.Printf("ext: trim messages: %v", err)
		}
	}
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
	out, err := st.readMsgs(conv, since)
	if err != nil {
		log.Printf("ext: read messages: %v", err)
	}
	return out
}

func (st *extStore) readMsgs(conv string, since time.Time) ([]InboxMsg, error) {
	var out []InboxMsg
	err := loadRows(st.db, `SELECT data FROM ext_msgs WHERE conv = ? AND at > ? ORDER BY at`,
		func(m *InboxMsg) { out = append(out, *m) }, conv, since.UnixMilli())
	return out, err
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

// outs lists matching sends, oldest first. Callers supply a fixed SQL condition.
func (st *extStore) outs(condition string, args ...any) []*OutboxItem {
	var out []*OutboxItem
	if err := loadRows(st.db, `SELECT data FROM ext_outbox WHERE `+condition+` ORDER BY at`,
		func(o *OutboxItem) { out = append(out, o) }, args...); err != nil {
		log.Printf("ext: read sends: %v", err)
	}
	return out
}

// pendingOutsFor selects one extension's pending sends by its literal prefix.
func (st *extStore) pendingOutsFor(ext string) []*OutboxItem {
	return st.outs(`state = ? AND instr(conv, ?) = 1`, outPending, ext+":")
}

// recentOuts is conv's sends since a time.
func (st *extStore) recentOuts(conv string, since time.Time) ([]*OutboxItem, error) {
	var out []*OutboxItem
	err := loadRows(st.db, `SELECT data FROM ext_outbox WHERE conv = ? AND at >= ? ORDER BY at`, func(o *OutboxItem) { out = append(out, o) }, conv, since.Unix())
	return out, err
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
	// The indexed time is in seconds. Check the full timestamp at the cutoff.
	for _, o := range st.outs(`state = ? AND at <= ?`, outPending, before.Unix()) {
		if o.At.Before(before) {
			o.State, o.Reason, o.DoneAt = "expired", "扩展一直没有运行，没有发出", time.Now()
			st.putOutIf(o, outPending)
		}
	}
}

func (st *extStore) trimOutbox(now time.Time) {
	before := now.Add(-outboxKeep)
	// Attachment cleanup uses the full time, while row deletion uses seconds.
	for _, o := range st.outs(`at <= ? AND state != ?`, before.Unix(), outPending) {
		if o.At.Before(before) && o.State != outPending {
			for _, f := range o.Files {
				if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
					log.Printf("ext: drop a sent file: %v", err)
				}
			}
		}
	}
	if _, err := st.db.Exec(`DELETE FROM ext_outbox WHERE at < ? AND state != ?`, before.Unix(), outPending); err != nil {
		log.Printf("ext: trim outbox: %v", err)
	}
}

func (st *extStore) kv(k string) string {
	v, err := st.readKV(k)
	if err != nil {
		log.Printf("ext: read %s: %v", k, err)
	}
	return v
}

func (st *extStore) readKV(k string) (string, error) {
	var v string
	err := st.db.QueryRow(`SELECT v FROM kv WHERE k = ?`, k).Scan(&v)
	if err == sql.ErrNoRows {
		err = nil
	}
	return v, err
}

func (st *extStore) setKV(k, v string) {
	if err := st.writeKV(k, v); err != nil {
		log.Printf("ext: save %s: %v", k, err)
	}
}

func (st *extStore) writeKV(k, v string) error {
	_, err := st.db.Exec(`INSERT INTO kv(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}
