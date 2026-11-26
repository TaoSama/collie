package collie

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

func (sp *Super) queueNotifyLocked(e *Entry) {
	if sp.st.Settings.NotifyWebhook == "" && !sp.st.Settings.NotifyLark && !sp.push.has() {
		return
	}
	n := notice{tone: e.Tone, text: e.Text, re: e.Re, options: e.Options, check: e.Check}
	switch {
	case e.Ask != nil: // a dialog's screen stays on the page
		n.decision, n.task, n.options, n.pane = e.ID, entryTask(e), e.Ask.Choices, e.Ask.Pane
	case isDecision(e):
		n.decision, n.task, n.summary = e.ID, entryTask(e), pendingSummary(e.Text, nil)
	case e.Tone == toneRemind:
		n.remind = e.Ref
	case !n.needsYou() && sp.st.Settings.NotifyLevel == "quiet" && e.Role == "collie":
		return
	}
	sp.notifyBuf = append(sp.notifyBuf, n)
}

// A notice is one thing to tell you: what Collie said (and what about), or
// what needs you. One that needs you is a decision: it says whose it is and
// what it needs (line), and links to it; the text stays on the page.
type notice struct {
	tone, text string
	re         *Quote
	options    []string
	check      bool
	decision   string // the entry that announced it
	task       string // the worker it is about, or Collie
	link       string // set when it goes out
	summary    string // a --opt/--check message's first line
	pane       string // a dialog's pane
	remind     string // the daily reminder: the oldest thing it lists
}

func (n notice) needsYou() bool {
	return n.tone == "blocked" || len(n.options) > 0 || n.check || n.remind != ""
}

func (n notice) label() string {
	switch {
	case n.needsYou():
		return "Collie · 需要你"
	case n.tone == "error":
		return "Collie · 出错了"
	}
	return "Collie"
}

// noticesText is the plain form (webhooks, and the card's fallback).
func noticesText(ns []notice) string {
	var parts []string
	for _, n := range ns {
		if n.decision != "" {
			p := n.label() + "：" + n.line()
			if n.summary != "" {
				p += "\n" + n.summary
			}
			if n.link != "" {
				p += "\n去处理：" + n.link
			}
			parts = append(parts, p)
			continue
		}
		p := n.label() + "：" + clip(n.text, 400)
		if n.re != nil {
			p = "↩ " + n.re.line() + "\n" + p
		}
		if len(n.options) > 0 {
			p += "\n可选：" + strings.Join(n.options, " / ")
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "\n\n")
}

// seenPage: page is in front of you, or (away) has just gone to the
// background. You stop looking when the last page in front of you leaves,
// not a minute later: a phone put away gets its notifications at once.
func (sp *Super) seenPage(page, url string, away bool) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.seenPages == nil {
		sp.seenPages = map[string]time.Time{}
	}
	for p, at := range sp.seenPages {
		if time.Since(at) > lookingFor {
			delete(sp.seenPages, p)
		}
	}
	if url != "" {
		sp.seenURL = url
	}
	if !away {
		sp.seenAt = time.Now()
		sp.seenPages[page] = sp.seenAt
		return
	}
	if _, ok := sp.seenPages[page]; !ok {
		return
	}
	delete(sp.seenPages, page)
	sp.seenAt = time.Time{}
	for _, at := range sp.seenPages {
		if at.After(sp.seenAt) {
			sp.seenAt = at
		}
	}
}

// lookingFor: a page in front of you says so every 30 seconds.
const lookingFor = 75 * time.Second

// notifyEvery paces notifications: what comes up in between goes out together.
const notifyEvery = 20 * time.Second

// sendNotifications paces outgoing notifications: messages are coalesced and
// sent at most every 20 seconds, and not at all while you are looking at
// Collie (you see them there).
func (sp *Super) sendNotifications() {
	sp.mu.Lock()
	if len(sp.notifyBuf) == 0 && !sp.push.dialogsDue() || time.Since(sp.notifyAt) < notifyEvery {
		sp.mu.Unlock()
		return
	}
	url, toLark := sp.st.Settings.NotifyWebhook, sp.st.Settings.NotifyLark
	looking := time.Since(sp.seenAt) < lookingFor
	base := sp.linkBaseLocked()
	subject := sp.pushSubjectLocked()
	// A decision settled while it waited here (Collie answered, you did on
	// the page) is no longer news.
	var notices []notice
	var pushes []pushNote
	for _, n := range sp.notifyBuf {
		if n.decision != "" {
			if !sp.decisionOpenLocked(n.decision) {
				continue
			}
			n.link = decisionLink(base, n.decision)
		}
		if p := noticeNote(n); p != nil {
			pushes = append(pushes, *p)
		}
		notices = append(notices, n)
	}
	sp.notifyBuf, sp.notifyAt = nil, time.Now()
	sp.mu.Unlock()
	dialogs := sp.push.takeDialogs()
	if looking {
		if n := len(pushes) + len(dialogs); n > 0 {
			log.Printf("push: %d not sent, a page of Collie is in front of you", n)
		}
		return
	}
	if sp.push.has() && (len(pushes) > 0 || len(dialogs) > 0) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			sp.push.broadcast(ctx, subject, append(pushes, sp.dialogNotes(ctx, dialogs)...))
		}()
	}
	if len(notices) == 0 || (url == "" && !toLark) {
		return
	}
	link := ""
	if base != "" {
		link = strings.TrimRight(base, "/") + "/#/collie"
	}
	if toLark {
		if err := larkSend(notices, link); err != nil {
			log.Printf("notify lark: %v", err)
		}
	}
	if url == "" {
		return
	}
	text := noticesText(notices)
	payload := map[string]any{
		"msg_type": "text", "content": map[string]string{"text": text}, // Feishu/Lark bot
		"text": text, // Slack-style
	}
	if d := firstDecision(notices); d != nil {
		payload["link"] = d.link
	}
	body, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err != nil {
		log.Printf("notify: %v", err)
	} else {
		resp.Body.Close()
	}
}
