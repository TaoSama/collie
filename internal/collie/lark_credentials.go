package collie

import (
	"errors"
	"log"
	"regexp"
	"strings"
)

var (
	errLarkCredential = errors.New("这条飞书消息可能包含凭证，Collie 已拦截发送。")
	larkPrivateKey    = regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----`)
	larkKnownToken    = regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{32,}|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,}|glpat-[A-Za-z0-9_-]{20,}|AIza[A-Za-z0-9_-]{35}|ops_eyJ[A-Za-z0-9+/=_-]{20,}|xox[baprs]-[A-Za-z0-9-]{20,}|(?:AKIA|ASIA)[A-Z0-9]{16}|eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{16,})(?:[^A-Za-z0-9_-]|$)`)
	larkAuthField     = regexp.MustCompile("(?i)\\b(?:(authorization[\"'`]?\\s*:\\s*[\"'`]?(?:bearer|basic)\\s+)|(?:[a-z0-9]+[_-])*(?:token|(?:access|refresh|auth|id)[_-]?token|api[_-]?key|(?:app|api|client|webhook|signing|jwt)[_-]?secret|(?:secret|private)[_-]?key|password|passwd|(?:set-)?cookie)[\"'`]?\\s*[:=]\\s*[\"'`]?)([a-z0-9_./+~@%!=-]{8,})")
	larkValueLetters  = regexp.MustCompile(`(?i)[a-z].*[a-z].*[a-z]`)
	larkSignedLink    = regexp.MustCompile(`(?i)[?&](?:x-(?:amz|goog)-signature|sig)=[a-f0-9]{40,}\b`)
	larkPlaceholder   = regexp.MustCompile(`(?i)^(?:(?:your|example|sample|dummy|fake|test|mock|demo|placeholder|redacted|replace|insert|put|changeme|not[-_]a[-_]real)(?:[-_]|$)|(?:sk-|ghp_|glpat-|AIza)?x{8,}$|(?:null|undefined|configuration|configured|exampletoken)$)`)
)

func larkCredentialType(text string) string {
	if larkPrivateKey.MatchString(text) {
		return "private-key"
	}
	if larkSignedLink.MatchString(text) {
		return "signed-link"
	}
	for _, match := range larkKnownToken.FindAllStringSubmatch(text, -1) {
		if !larkPlaceholder.MatchString(match[1]) {
			return "known-token"
		}
	}
	for _, match := range larkAuthField.FindAllStringSubmatch(text, -1) {
		value := match[2]
		if larkPlaceholder.MatchString(value) {
			continue
		}
		if match[1] != "" || (len(value) >= 12 && !strings.ContainsAny(value, "./") &&
			larkValueLetters.MatchString(value) && strings.ContainsAny(value, "0123456789+~@%!")) {
			return "auth-field"
		}
	}
	return ""
}

func (h *extHost) checkLarkSend(c InboxConv, o *OutboxItem, notify bool, parts ...string) error {
	if c.Ext != larkxID {
		return nil
	}
	for _, text := range parts {
		match := larkCredentialType(text)
		if match == "" {
			continue
		}
		log.Printf("larkx guard policy=%s block conv=%q ref=%s trigger=%q match=%s",
			larkPolicyVersion, c.ID, o.Ref, o.Trigger, match)
		if notify {
			h.sp.mu.Lock()
			h.sp.addEntryLocked("event", mainThread, "已拦截发往飞书会话 "+c.ID+" 的消息，其中可能包含凭证。", o.Ref, "error")
			h.sp.mu.Unlock()
		}
		return errLarkCredential
	}
	return nil
}

// Pending entries created by an older build still pass the check before
// sending. Links Collie generated for those files have a signature by design.
func (h *extHost) checkPendingLarkSend(c InboxConv, o *OutboxItem) error {
	if c.Ext != larkxID {
		return nil
	}
	text := o.Text
	for _, f := range o.Files {
		if err := h.checkLarkSend(c, o, true, f.Name); err != nil {
			return err
		}
		if f.Link != "" {
			text = strings.ReplaceAll(text, f.Link, "")
		}
	}
	return h.checkLarkSend(c, o, true, text)
}
