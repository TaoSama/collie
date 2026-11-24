package collie

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Notifications can also go out as a Feishu/Lark message from the hub's own
// lark-cli: its bot writes to whoever is logged in to it. No lark-cli on the
// hub, or no one logged in, and nothing is sent.

type larkTarget struct {
	mu      sync.Mutex
	bin     string
	openID  string
	who     string
	err     string
	checked time.Time
}

var lark larkTarget

// get says who lark-cli would write to, looking again every 10 minutes (or
// at once when asked), so installing or logging in later just works.
func (l *larkTarget) get(fresh bool) (bin, openID, who, problem string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if fresh || time.Since(l.checked) > 10*time.Minute {
		l.checked = time.Now()
		l.bin, l.openID, l.who, l.err = larkLookup()
	}
	return l.bin, l.openID, l.who, l.err
}

func larkLookup() (bin, openID, who, problem string) {
	home, _ := os.UserHomeDir()
	if bin = findBin("lark-cli", home); bin == "" {
		return "", "", "", "hub 上没有 lark-cli"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "auth", "status").Output()
	var st struct {
		Identities struct {
			Bot struct {
				Available bool `json:"available"`
			} `json:"bot"`
			User struct {
				OpenID   string `json:"openId"`
				UserName string `json:"userName"`
			} `json:"user"`
		} `json:"identities"`
	}
	if err != nil || json.Unmarshal(out, &st) != nil {
		return bin, "", "", "lark-cli 读不到登录状态"
	}
	if !st.Identities.Bot.Available {
		return bin, "", "", "lark-cli 的机器人身份不可用"
	}
	if st.Identities.User.OpenID == "" {
		return bin, "", "", "lark-cli 没有登录用户（lark-cli auth login）"
	}
	return bin, st.Identities.User.OpenID, st.Identities.User.UserName, ""
}

// larkJumpURL adds Feishu's official external-browser query parameters before
// any #fragment so both mobile (iOS/Android) and desktop Feishu open Collie in
// the system default browser (reusing the 1-year login cookie) instead of
// Feishu's ephemeral built-in WebView.
func larkJumpURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return raw
	}
	if u.Path == "" {
		u.Path = "/"
	}
	q := u.Query()
	var extra []string
	if !q.Has("lk_mobile_jump_to_browser") {
		extra = append(extra, "lk_mobile_jump_to_browser=true")
	}
	if !q.Has("lk_jump_to_browser") {
		extra = append(extra, "lk_jump_to_browser=true")
	}
	if len(extra) > 0 {
		if u.RawQuery != "" {
			u.RawQuery += "&" + strings.Join(extra, "&")
		} else {
			u.RawQuery = strings.Join(extra, "&")
		}
	}
	return u.String()
}

// larkSend writes to the logged-in user, as the bot (a message from yourself
// would not ring): a card, or plain text if the card is turned down.
func larkSend(ns []notice, link string) error {
	bin, openID, _, problem := lark.get(false)
	if problem != "" {
		return errors.New(problem)
	}
	jumpLink := larkJumpURL(link)
	send := func(args ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		args = append([]string{"im", "+messages-send", "--as", "bot", "--user-id", openID}, args...)
		if out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput(); err != nil {
			return errors.New(clip(string(out), 300))
		}
		return nil
	}
	err := send("--msg-type", "interactive", "--content", larkCard(ns, jumpLink))
	if err == nil {
		return nil
	}
	text := noticesText(ns)
	if jumpLink != "" {
		text += "\n\n打开 Collie：" + jumpLink
	}
	if err2 := send("--text", text); err2 != nil {
		lark.get(true) // logged out, or moved: look again next time
		return err2
	}
	log.Printf("notify lark: card refused, sent as text: %v", err)
	return nil
}

// larkCard is a Feishu card (JSON 2.0): the header says how much it matters
// (orange when it needs you, as on the page), the body is Collie's own
// Markdown, and a button opens Collie.
func larkCard(ns []notice, link string) string {
	link = larkJumpURL(link)
	title, color := "Collie", "default" // plain white: colour only when it needs you
	for _, n := range ns {
		switch {
		case n.needsYou():
			title, color = n.label(), "orange"
		case n.tone == "error" && color != "orange":
			title, color = n.label(), "red"
		}
	}
	var els []any
	general := false // something besides decisions: the card ends with a way into Collie
	for i, n := range ns {
		if i > 0 {
			els = append(els, map[string]any{"tag": "hr"})
		}
		if n.decision != "" {
			els = append(els, larkDecision(n)...)
			general = general || n.link == ""
			continue
		}
		general = true
		t := clip(n.text, 1500)
		if n.re != nil {
			t = "<font color='grey'>↩ " + larkEscape(n.re.line()) + "</font>\n" + t
		}
		if len(ns) > 1 && ((n.tone != "" && n.tone != "collie") || n.needsYou()) {
			t = "**" + n.label() + "**\n" + t
		}
		if len(n.options) > 0 {
			var qs []string
			for _, o := range n.options {
				qs = append(qs, "「"+larkEscape(o)+"」")
			}
			t += "\n<font color='grey'>可选：" + strings.Join(qs, " · ") + "</font>"
		}
		els = append(els, map[string]any{"tag": "markdown", "content": t})
	}
	if link != "" && general {
		els = append(els, larkButton("打开 Collie", "default", link))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(map[string]any{
		"schema": "2.0",
		"config": map[string]any{"summary": map[string]string{"content": clip(cardSummary(ns), 60)}},
		"header": map[string]any{"title": map[string]string{"tag": "plain_text", "content": title}, "template": color},
		"body":   map[string]any{"elements": els},
	})
	return strings.TrimSuffix(buf.String(), "\n")
}

// larkDecision is a decision in the card: whose it is and what it needs, and
// a button to the page where it is settled. What the agent's screen says
// stays there, behind the sign-in.
func larkDecision(n notice) []any {
	t := "**" + larkEscape(n.line()) + "**"
	if n.summary != "" {
		t += "\n<font color='grey'>" + larkEscape(n.summary) + "</font>"
	}
	els := []any{map[string]any{"tag": "markdown", "content": t}}
	if n.link != "" {
		els = append(els, larkButton("去处理", "primary", larkJumpURL(n.link)))
	}
	return els
}

func larkButton(label, kind, link string) map[string]any {
	return map[string]any{
		"tag": "button", "type": kind, "width": "default",
		"text":      map[string]string{"tag": "plain_text", "content": label},
		"behaviors": []any{map[string]string{"type": "open_url", "default_url": link}},
	}
}

// larkEscape keeps quoted text from being read as card Markdown.
var larkEscape = strings.NewReplacer("<", "&lt;", ">", "&gt;", "*", "\\*", "`", "'", "[", "\\[", "_", "\\_").Replace

// cardSummary is what the chat list shows for the card: the first line of
// what matters most (needs you, then an error, else the latest), without
// the Markdown marks the list would show as they are.
var mdMarks = strings.NewReplacer("**", "", "__", "", "`", "")

func cardSummary(ns []notice) string {
	pick := ns[len(ns)-1]
	for _, n := range ns {
		if n.tone == "error" {
			pick = n
		}
	}
	for _, n := range ns {
		if n.needsYou() {
			pick = n
		}
	}
	if pick.decision != "" {
		return pick.line()
	}
	for _, l := range strings.Split(pick.text, "\n") {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "#>-*• "))
		if l = strings.TrimSpace(mdMarks.Replace(l)); l != "" {
			return l
		}
	}
	return pick.label()
}
