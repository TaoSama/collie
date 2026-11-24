package collie

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// A decision is something only the human can settle: a dialog a worker
// stopped on (its "等你确认" event), or a Collie message with --opt or
// --check. Each is named by the entry that announced it, so a notification
// can link straight to it: <hub>/#/d/<entry id>. A session's dialog that
// Collie has no entry for is linked by its pane: <hub>/#/d/pane/<pane id>
// (remote panes carry "@machine"). The page asks GET /api/super/decision
// where it is and whether it is still open.

// DialogAsk is the dialog a worker stopped on: where it is, what it offers,
// and once settled, what was picked.
type DialogAsk struct {
	Pane    string   `json:"pane"`
	Choices []string `json:"choices,omitempty"`
	Sig     string   `json:"sig,omitempty"` // same question and choices: the same decision
	Done    *Answer  `json:"done,omitempty"`
}

// Answer is how a decision was settled. Choice is "" when it was answered in
// the terminal, where Collie can't tell which option it was, or dismissed
// (× or 已看 on a --opt/--check message).
type Answer struct {
	Choice    string    `json:"choice,omitempty"`
	By        string    `json:"by,omitempty"` // byYou | byCollie | "" (unknown)
	Dismissed bool      `json:"dismissed,omitempty"`
	At        time.Time `json:"at"`
}

// Decision is what the page needs to open one.
type Decision struct {
	ID      string   `json:"id,omitempty"`
	Kind    string   `json:"kind"` // kindDialog | kindOptions | kindCheck
	Task    string   `json:"task"` // the worker it is about, or Collie
	Pane    string   `json:"pane,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Options []string `json:"options,omitempty"`
	Sig     string   `json:"sig,omitempty"` // a dialog's fingerprint, to answer only that dialog
	Status  string   `json:"status"`        // decisionOpen …
	Answer  *Answer  `json:"answer,omitempty"`
}

const (
	kindDialog  = "dialog"
	kindOptions = "options"
	kindCheck   = "check"

	decisionOpen    = "open"
	decisionDone    = "done"
	decisionGone    = "gone"    // the worker ended first
	decisionExpired = "expired" // a --opt left unanswered past pendingMaxAge

	byYou    = "你"
	byCollie = "Collie"
)

// noticeChoiceMax keeps a choice short in a notification: the screen's
// wording (paths, commands) stays on the signed-in page.
const noticeChoiceMax = 48

// decisionLink is the stable address of a decision on the hub at base.
func decisionLink(base, id string) string {
	if base == "" || id == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/#/d/" + url.PathEscape(id)
}

// linkBaseLocked is the address notifications link to: the one the hub was
// given (-url), else the page that last said it was open.
func (sp *Super) linkBaseLocked() string {
	if sp.s != nil {
		for _, u := range sp.s.publicURLs {
			if u = strings.TrimSpace(u); strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
				return u
			}
		}
	}
	return sp.seenURL
}

// askSig fingerprints a dialog by what it asks, not where its cursor is.
func askSig(a Approval) string {
	h := sha1.New()
	fmt.Fprintln(h, oneLine(a.Title), oneLine(a.Question))
	for _, c := range a.Choices {
		fmt.Fprintln(h, oneLine(c.Label))
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:12]
}

func choiceLabels(a Approval) []string {
	var out []string
	for _, c := range a.Choices {
		out = append(out, oneLine(c.Label))
	}
	return out
}

// askRepeatWindow: the same dialog back this soon after it went away without
// an answer Collie saw (a redraw, a status flicker) is the same decision.
const askRepeatWindow = 10 * time.Minute

// lastAskLocked is the latest dialog decision on pane, if any.
func (sp *Super) lastAskLocked(pane string) *Entry {
	for i := len(sp.st.Entries) - 1; i >= 0; i-- {
		if e := sp.st.Entries[i]; e.Ask != nil && e.Ask.Pane == pane {
			return e
		}
	}
	return nil
}

// addAskLocked announces the dialog w stopped on (a nil a: it couldn't be
// read) and notifies you, once per decision: the same dialog showing again
// reopens the one already announced, without a second line or notification.
func (sp *Super) addAskLocked(w *Worker, a *Approval) {
	ask := &DialogAsk{Pane: w.PaneID}
	if a != nil {
		ask.Choices, ask.Sig = choiceLabels(*a), askSig(*a)
	}
	delete(sp.picked, w.PaneID) // a click on an earlier dialog isn't this one's answer
	if last := sp.lastAskLocked(w.PaneID); last != nil && ask.Sig != "" && last.Ask.Sig == ask.Sig &&
		(last.Ask.Done == nil || (last.Ask.Done.By == "" && time.Since(last.Ask.Done.At) < askRepeatWindow)) {
		last.Ask.Done = nil
		sp.changed()
		return
	}
	sp.appendEntryLocked(&Entry{ID: sp.st.nextID("e"), Role: "event", Thread: w.Thread, Text: w.Name + " 等你确认",
		At: time.Now(), Ref: w.Name, Tone: "blocked", Ask: ask})
}

// pickedFor: how long a page's word about the choice it clicked waits for
// the dialog to go away.
const pickedFor = time.Minute

// settleAskLocked records how pane's open dialog was answered. An unknown
// choice is the one a page said it clicked, if it did lately; a page saying
// so later fills it in.
func (sp *Super) settleAskLocked(pane, choice, by string) {
	e := sp.lastAskLocked(pane)
	if e == nil {
		return
	}
	if p, ok := sp.picked[pane]; choice == "" && ok && time.Since(p.At) < pickedFor {
		choice, by = p.Choice, p.By
	}
	delete(sp.picked, pane)
	switch d := e.Ask.Done; {
	case d == nil:
		e.Ask.Done = &Answer{Choice: choice, By: by, At: time.Now()}
	case d.Choice == "" && choice != "" && time.Since(d.At) < pickedFor:
		d.Choice, d.By = choice, by
	default:
		return
	}
	sp.changed()
}

// entryLocked finds an entry in the working set (idx is where), else in the
// database (idx -1).
func (sp *Super) entryLocked(id string) (e *Entry, idx int) {
	for i := len(sp.st.Entries) - 1; i >= 0; i-- {
		if sp.st.Entries[i].ID == id {
			return sp.st.Entries[i], i
		}
	}
	if sp.db != nil {
		var raw string
		var got Entry
		if sp.db.QueryRow(`SELECT data FROM entries WHERE id = ?`, id).Scan(&raw) == nil && json.Unmarshal([]byte(raw), &got) == nil {
			return &got, -1
		}
	}
	return nil, -1
}

func entryTask(e *Entry) string {
	if e.Ask != nil && e.Ref != "" {
		return e.Ref
	}
	if e.Re != nil && e.Re.Worker != "" {
		return e.Re.Worker
	}
	return byCollie
}

// isDecision: the entry asks you to decide, and the page can open it.
func isDecision(e *Entry) bool {
	return e.Ask != nil || (e.Role == "collie" && (e.Thread == "" || e.Thread == mainThread) && (len(e.Options) > 0 || e.Check))
}

// decisionLocked describes the decision entry id announced; nil if it isn't one.
func (sp *Super) decisionLocked(id string) *Decision {
	e, idx := sp.entryLocked(id)
	if e == nil || !isDecision(e) {
		return nil
	}
	d := &Decision{ID: e.ID, Task: entryTask(e)}
	if e.Ask != nil {
		d.Kind, d.Pane, d.Options, d.Sig, d.Status, d.Answer = kindDialog, e.Ask.Pane, e.Ask.Choices, e.Ask.Sig, decisionOpen, e.Ask.Done
		if d.Answer != nil {
			d.Status = decisionDone
		} else if w := sp.st.Workers[e.Ref]; w == nil || w.Ended {
			d.Status = decisionGone
		}
		return d
	}
	d.Kind, d.Options, d.Summary = kindOptions, cleanOptions(e.Options), pendingSummary(e.Text, nil)
	if len(d.Options) == 0 {
		d.Kind = kindCheck
	}
	d.Status, d.Answer = sp.entryOutcomeLocked(e, idx)
	return d
}

// entryOutcomeLocked says whether a --opt/--check message is still waiting,
// by the rules pendingLocked applies, and how it was settled.
func (sp *Super) entryOutcomeLocked(e *Entry, idx int) (string, *Answer) {
	if sp.st.Dismissed[e.ID] {
		return decisionDone, &Answer{By: byYou, Dismissed: true}
	}
	if idx >= 0 {
		if r := sp.replyLocked(idx); r != nil {
			return decisionDone, &Answer{Choice: clip(oneLine(humanText(r.Text)), 80), By: byYou, At: r.At}
		}
	}
	switch {
	case time.Since(e.At) > pendingMaxAge:
		return decisionExpired, nil
	case idx < 0: // gone from the working set: whatever settled it is too
		return decisionDone, nil
	}
	return decisionOpen, nil
}

// decisionForPaneLocked is the latest dialog decision on pane, as above;
// nil if Collie never recorded one there.
func (sp *Super) decisionForPaneLocked(pane string) *Decision {
	if e := sp.lastAskLocked(pane); e != nil {
		return sp.decisionLocked(e.ID)
	}
	return nil
}

// decisionOpenLocked: a notice about a decision that was settled before it
// went out is dropped.
func (sp *Super) decisionOpenLocked(id string) bool {
	d := sp.decisionLocked(id)
	return d == nil || d.Status == decisionOpen
}

// ---------------------------------------------------------------- notices

// line is what a notification says about a decision: whose it is and what it
// needs, never the screen behind it.
func (n notice) line() string {
	var opts []string
	for _, o := range n.options {
		opts = append(opts, clip(o, noticeChoiceMax))
	}
	switch {
	case len(opts) > 0:
		return n.task + " 需要你选：" + strings.Join(opts, " / ")
	case n.check:
		return n.task + " 需要你看一下"
	}
	return n.task + " 需要你确认"
}

func firstDecision(ns []notice) *notice {
	for i := range ns {
		if ns[i].decision != "" && ns[i].link != "" {
			return &ns[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------- answering

var errNotAsking = errors.New("not showing a dialog now")

// noChoiceError: none of the dialog's choices is the one asked for.
type noChoiceError struct{ choices []string }

func (noChoiceError) Error() string { return "no such choice" }

// answerPane picks a choice on pane's dialog (the one fingerprinted sig, when
// given) and records who did. It returns the label it picked.
func (sp *Super) answerPane(ctx context.Context, pane, sig, by string, pick func(Approval) (Choice, bool)) (string, error) {
	screen, err := sp.s.herdr.ReadAsk(ctx, pane)
	if err != nil {
		return "", err
	}
	if !screenAsks(screen) {
		return "", errNotAsking
	}
	a := parseApproval(screen)
	if sig != "" && askSig(a) != sig {
		return "", errNotAsking // another dialog took its place
	}
	c, ok := pick(a)
	if !ok {
		return "", noChoiceError{choiceLabels(a)}
	}
	if err := sp.choose(ctx, pane, c); err != nil {
		return "", err
	}
	label := oneLine(c.Label)
	sp.mu.Lock()
	if name := sp.workerByPaneLocked(pane); name != "" {
		sp.noteAnsweredLocked(sp.st.Workers[name])
	}
	sp.settleAskLocked(pane, label, by)
	sp.mu.Unlock()
	sp.push.answered(pane)
	return label, nil
}

// ---------------------------------------------------------------- API

// superDecision: where a decision is and whether it is still open, by its
// id or by a pane's dialog.
func (s *Server) superDecision(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sp := s.sup
	sp.mu.Lock()
	var d *Decision
	if id := q.Get("id"); id != "" {
		d = sp.decisionLocked(id)
	} else if pane := q.Get("pane"); pane != "" {
		d = sp.decisionForPaneLocked(pane)
	}
	sp.mu.Unlock()
	if d == nil {
		writeJSON(w, 404, map[string]any{"error": "没有这个决策"})
		return
	}
	writeJSON(w, 200, d)
}

// superDecide answers a decision with one of its choices, word for word: a
// button on a system notification, pressed without opening the page. A
// decision already settled, or a dialog that changed since, is not answered
// (409): the page opens instead.
func (s *Server) superDecide(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID     string `json:"id"`
		Pane   string `json:"pane"`
		Sig    string `json:"sig"` // the dialog the notification showed
		Choice string `json:"choice"`
	}
	if !decode(w, r, &body) {
		return
	}
	choice := oneLine(body.Choice)
	sp := s.sup
	pane, sig := body.Pane, body.Sig
	if body.ID != "" {
		// Checked and answered under one lock: the button and the page may
		// both answer at once.
		sp.mu.Lock()
		d := sp.decisionLocked(body.ID)
		code, out := 0, map[string]any{}
		switch {
		case d == nil:
			code, out = 404, map[string]any{"error": "没有这个决策"}
		case d.Status != decisionOpen:
			code, out = 409, map[string]any{"error": "已经处理过了", "decision": d}
		case d.Kind == kindCheck:
			sp.dismissLocked(d.ID)
			code, out = 200, map[string]any{"ok": true}
		case d.Kind == kindOptions && !slices.ContainsFunc(d.Options, func(o string) bool { return strings.EqualFold(o, choice) }):
			code, out = 400, map[string]any{"error": "没有这个选项"}
		case d.Kind == kindOptions:
			sp.sayLocked(choice, d.ID, "")
			code, out = 200, map[string]any{"ok": true, "picked": choice}
		default:
			pane, sig = d.Pane, d.Sig
		}
		sp.mu.Unlock()
		if code != 0 {
			if code == 200 && d.Kind == kindCheck {
				sp.flush()
			}
			writeJSON(w, code, out)
			return
		}
	}
	if pane == "" {
		writeJSON(w, 400, map[string]any{"error": "id or pane?"})
		return
	}
	picked, err := sp.answerPane(r.Context(), pane, sig, byYou, func(a Approval) (Choice, bool) {
		for _, c := range a.Choices {
			if strings.EqualFold(oneLine(c.Label), choice) {
				return c, true
			}
		}
		return Choice{}, false
	})
	if err != nil {
		writeJSON(w, 409, map[string]any{"error": "对话框变了，打开页面看看：" + err.Error()})
		return
	}
	sp.mu.Lock()
	if name := sp.workerByPaneLocked(pane); name != "" {
		sp.addEntryLocked("event", sp.st.Workers[name].Thread, "你在通知上替 "+name+" 选了："+clip(picked, 80), name, "info")
	}
	sp.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "picked": picked})
}

// superDecided: the page answered a pane's dialog from its card and says
// which choice it was, so the decision reads "选了 X" afterwards. It only
// names the choice: the dialog counts as answered once it is gone (a send
// that failed leaves it open).
func (s *Server) superDecided(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Pane   string `json:"pane"`
		Choice string `json:"choice"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Pane == "" {
		writeJSON(w, 400, map[string]any{"error": "pane?"})
		return
	}
	sp := s.sup
	choice := clip(oneLine(body.Choice), 80)
	sp.push.answered(body.Pane)
	sp.mu.Lock()
	if e := sp.lastAskLocked(body.Pane); e != nil && e.Ask.Done != nil {
		sp.settleAskLocked(body.Pane, choice, byYou)
	} else if e != nil {
		if sp.picked == nil {
			sp.picked = map[string]Answer{}
		}
		sp.picked[body.Pane] = Answer{Choice: choice, By: byYou, At: time.Now()}
	}
	sp.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (sp *Super) workerByPaneLocked(pane string) string {
	for _, w := range sp.st.Workers {
		if w.PaneID == pane && !w.Ended {
			return w.Name
		}
	}
	return ""
}
