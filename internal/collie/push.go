package collie

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The hub's side of Web Push: the browsers that subscribed (each belongs to
// the paired browser that subscribed it), and the decisions pushed to them.
// A page can't notify once the phone has suspended it (an installed app in
// the background, the screen locked), so the hub does, paced, deduplicated
// and held back as the other notification channels are (sendNotifications).

// pushSub is one browser's subscription (PushSubscription.toJSON()).
type pushSub struct {
	Endpoint string    `json:"endpoint"`
	P256dh   string    `json:"p256dh"`
	Auth     string    `json:"auth"`
	Device   string    `json:"device"` // the paired browser it belongs to, "" for the master token
	At       time.Time `json:"at"`
}

// pushNote is what sw.js shows for a push (its "push" handler).
type pushNote struct {
	Title   string            `json:"title"`
	Body    string            `json:"body"`
	Tag     string            `json:"tag"`              // the same decision replaces, never stacks
	Hash    string            `json:"hash"`             // #/d/…, opened on the page's own origin
	Answer  map[string]string `json:"answer,omitempty"` // the body POST /api/super/decide takes, with choice
	Choices []string          `json:"choices,omitempty"`
}

type webPush struct {
	db     *sql.DB
	key    *vapidKey
	client *http.Client

	mu   sync.Mutex
	subs map[string]*pushSub // by endpoint
	// Dialogs in sessions that aren't Collie's workers (theirs are entries):
	// what each pane was at the last look, the ones that have since stopped on
	// a dialog, and the dialog each last notified about.
	status  map[string]string
	dialogs map[string]time.Time
	pushed  map[string]pushedAsk
}

type pushedAsk struct {
	sig string
	at  time.Time
}

const (
	pushTitle = "Collie"
	// pushSettle: a dialog waits this long before it is pushed, so one that
	// Collie answers itself (a folder trust prompt) never reaches you.
	pushSettle = 3 * time.Second
	// pushRepeatWindow: the same dialog back this soon after its push
	// (a status flicker) was already notified.
	pushRepeatWindow = time.Minute
	pushTimeout      = 10 * time.Second
)

func newWebPush(db *sql.DB) (*webPush, error) {
	wp := &webPush{db: db, client: &http.Client{Timeout: pushTimeout}, subs: map[string]*pushSub{},
		status: map[string]string{}, dialogs: map[string]time.Time{}, pushed: map[string]pushedAsk{}}
	var raw string
	switch err := db.QueryRow(`SELECT v FROM kv WHERE k='vapid'`).Scan(&raw); {
	case err == nil:
		k, err := parseVAPIDKey(raw)
		if err != nil {
			return nil, err
		}
		wp.key = k
	case errors.Is(err, sql.ErrNoRows):
		k, err := newVAPIDKey()
		if err != nil {
			return nil, err
		}
		s, err := k.marshal()
		if err != nil {
			return nil, err
		}
		if _, err := db.Exec(`INSERT INTO kv(k,v) VALUES('vapid',?)`, s); err != nil {
			return nil, err
		}
		wp.key = k
	default:
		return nil, err
	}
	err := loadRows(db, `SELECT data FROM push_subs`, func(s *pushSub) { wp.subs[s.Endpoint] = s })
	return wp, err
}

func (wp *webPush) has() bool {
	if wp == nil {
		return false
	}
	wp.mu.Lock()
	defer wp.mu.Unlock()
	return len(wp.subs) > 0
}

func (wp *webPush) add(s pushSub) error {
	b, _ := json.Marshal(s)
	wp.mu.Lock()
	defer wp.mu.Unlock()
	if _, err := wp.db.Exec(`INSERT INTO push_subs(endpoint, device, data) VALUES(?,?,?)
		ON CONFLICT(endpoint) DO UPDATE SET device=excluded.device, data=excluded.data`, s.Endpoint, s.Device, string(b)); err != nil {
		return err
	}
	wp.subs[s.Endpoint] = &s
	return nil
}

func (wp *webPush) get(endpoint string) *pushSub {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	if s := wp.subs[endpoint]; s != nil {
		c := *s
		return &c
	}
	return nil
}

func (wp *webPush) remove(endpoint string) {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	if _, err := wp.db.Exec(`DELETE FROM push_subs WHERE endpoint=?`, endpoint); err != nil {
		log.Printf("push: forget %s: %v", endpoint, err)
	}
	delete(wp.subs, endpoint)
}

// dropDevice forgets the subscriptions of a browser that was removed.
func (wp *webPush) dropDevice(id string) {
	if wp == nil || id == "" {
		return
	}
	wp.mu.Lock()
	defer wp.mu.Unlock()
	if _, err := wp.db.Exec(`DELETE FROM push_subs WHERE device=?`, id); err != nil {
		log.Printf("push: forget device %s: %v", id, err)
	}
	for e, s := range wp.subs {
		if s.Device == id {
			delete(wp.subs, e)
		}
	}
}

// deliver sends a note to one subscription, and forgets it when the push
// service says it is gone (404, 410: unsubscribed, the app uninstalled).
func (wp *webPush) deliver(ctx context.Context, subject string, s pushSub, n pushNote) error {
	b, _ := json.Marshal(n)
	code, err := sendPush(ctx, wp.client, wp.key, subject, s, pushMessage{payload: b, topic: pushTopic(n.Tag)})
	if code == http.StatusNotFound || code == http.StatusGone {
		log.Printf("push: %s is gone (%d), forgetting it", pushHost(s.Endpoint), code)
		wp.remove(s.Endpoint)
	}
	return err
}

// broadcast sends notes to every subscription.
func (wp *webPush) broadcast(ctx context.Context, subject string, notes []pushNote) {
	wp.mu.Lock()
	subs := make([]pushSub, 0, len(wp.subs))
	for _, s := range wp.subs {
		subs = append(subs, *s)
	}
	wp.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range subs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, n := range notes {
				ctx, cancel := context.WithTimeout(ctx, pushTimeout)
				err := wp.deliver(ctx, subject, s, n)
				cancel()
				if err != nil {
					log.Printf("push to %s: %v", pushHost(s.Endpoint), err)
					if wp.get(s.Endpoint) == nil {
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}

func pushHost(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil {
		return u.Host
	}
	return "?"
}

// ---------------------------------------------------------------- dialogs

// watch takes the latest look at every agent (the tick lists them) and
// notes the panes that have just stopped on a dialog, when there is anyone
// to push to. A pane seen for the first time is not news: it was already
// asking before the hub started.
func (wp *webPush) watch(list []Agent) {
	if wp == nil {
		return
	}
	wp.mu.Lock()
	defer wp.mu.Unlock()
	next := make(map[string]string, len(list))
	for _, a := range list {
		if a.Agent == nil {
			continue
		}
		next[a.PaneID] = a.AgentStatus
		if prev, ok := wp.status[a.PaneID]; ok && len(wp.subs) > 0 && prev != "blocked" && a.AgentStatus == "blocked" {
			if _, waiting := wp.dialogs[a.PaneID]; !waiting {
				wp.dialogs[a.PaneID] = time.Now()
			}
		}
	}
	wp.status = next
	for p := range wp.dialogs {
		if next[p] != "blocked" {
			delete(wp.dialogs, p) // answered, or gone, before it went out
		}
	}
}

// dialogsDue: some dialog has waited long enough to go out.
func (wp *webPush) dialogsDue() bool {
	if wp == nil {
		return false
	}
	wp.mu.Lock()
	defer wp.mu.Unlock()
	for _, at := range wp.dialogs {
		if time.Since(at) >= pushSettle {
			return true
		}
	}
	return false
}

// takeDialogs hands over the panes due to be notified.
func (wp *webPush) takeDialogs() []string {
	if wp == nil {
		return nil
	}
	wp.mu.Lock()
	defer wp.mu.Unlock()
	var out []string
	for p, at := range wp.dialogs {
		if time.Since(at) >= pushSettle {
			out = append(out, p)
			delete(wp.dialogs, p)
		}
	}
	return out
}

// repeat reports whether the dialog sig on pane was just pushed, and if not,
// records that it is being pushed now.
func (wp *webPush) repeat(pane, sig string) bool {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	if p, ok := wp.pushed[pane]; ok && sig != "" && p.sig == sig && time.Since(p.at) < pushRepeatWindow {
		return true
	}
	for k, p := range wp.pushed {
		if time.Since(p.at) > pushRepeatWindow {
			delete(wp.pushed, k)
		}
	}
	wp.pushed[pane] = pushedAsk{sig, time.Now()}
	return false
}

// answered: you answered pane's dialog (a notification's button, the page's
// card); the same dialog showing again is a new decision.
func (wp *webPush) answered(pane string) {
	if wp == nil {
		return
	}
	wp.mu.Lock()
	delete(wp.pushed, pane)
	wp.mu.Unlock()
}

// ---------------------------------------------------------------- notes

// pushSubjectLocked is how a push service reaches whoever runs this hub.
func (sp *Super) pushSubjectLocked() string {
	if b := sp.linkBaseLocked(); strings.HasPrefix(b, "https://") {
		return strings.TrimRight(b, "/")
	}
	return "mailto:collie@localhost"
}

// noticeNote is the push for a decision Collie recorded: a worker's dialog,
// a --opt or --check message. nil for anything else.
func noticeNote(n notice) *pushNote {
	if n.decision == "" {
		return nil
	}
	p := &pushNote{Title: pushTitle, Body: n.line(), Hash: "#/d/" + url.PathEscape(n.decision),
		Answer: map[string]string{"id": n.decision}, Choices: n.options}
	if n.summary != "" {
		p.Body += "\n" + n.summary
	}
	if n.pane != "" { // a dialog: one per pane, as on the page
		p.Tag = n.pane
		if len(p.Choices) == 0 {
			p.Answer = nil
		}
	} else {
		p.Tag = "d:" + n.decision
		if len(p.Choices) == 0 {
			p.Choices = []string{"已看"}
		}
	}
	return p
}

// dialogNotes are the pushes for panes that stopped on a dialog: what each
// asks, read now, for the ones still asking that aren't a worker's (their
// entries notify) and weren't just notified.
func (sp *Super) dialogNotes(ctx context.Context, panes []string) []pushNote {
	if len(panes) == 0 {
		return nil
	}
	views := map[string]AgentView{}
	for _, v := range sp.s.state(withCachedLists(ctx)).Agents {
		views[v.PaneID] = v
	}
	workers := map[string]bool{}
	sp.mu.Lock()
	for _, pane := range panes {
		workers[pane] = sp.workerByPaneLocked(pane) != ""
	}
	sp.mu.Unlock()
	var out []pushNote
	for _, pane := range panes {
		v, ok := views[pane]
		if !ok || v.Status != "blocked" || workers[pane] {
			continue
		}
		var choices []string
		sig := ""
		if screen, err := sp.s.herdr.ReadAsk(ctx, pane); err != nil {
			log.Printf("push: reading %s's dialog: %v", pane, err)
		} else if screenAsks(screen) {
			a := parseApproval(screen)
			choices, sig = choiceLabels(a), askSig(a)
		}
		if sp.push.repeat(pane, sig) {
			continue
		}
		n := notice{task: sp.s.sessionTitle(v), options: choices}
		p := pushNote{Title: pushTitle, Body: n.line(), Tag: pane, Hash: "#/d/pane/" + url.PathEscape(pane)}
		if len(choices) > 0 {
			p.Choices, p.Answer = choices, map[string]string{"pane": pane, "sig": sig}
		}
		out = append(out, p)
	}
	return out
}

// autoNameRe: a handle herdr made up ("claude-2"), which says nothing
// (autoName in app.js: change both).
var autoNameRe = regexp.MustCompile(`^(claude|codex|agy|antigravity)-\d+$`)

// sessionTitle is what the page calls a session (title in app.js).
func (s *Server) sessionTitle(v AgentView) string {
	switch {
	case v.IsRouter:
		return byCollie
	case v.Topic != "":
		return v.Topic
	case v.Name != "" && !autoNameRe.MatchString(v.Name):
		return v.Name
	case v.Cwd == "":
		return v.PaneID
	case filepath.Clean(v.Cwd) == filepath.Clean(s.home):
		return "~"
	}
	return filepath.Base(v.Cwd)
}
