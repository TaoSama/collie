package collie

import (
	"context"
	"fmt"
	"log"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// choicesText lists a dialog's choices for the router, numbered as `collie
// answer` takes them.
func choicesText(a *Approval) string {
	if a == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nChoices:")
	for i, c := range a.Choices {
		fmt.Fprintf(&b, "\n  %d. %s", i+1, c.Label)
	}
	return b.String()
}

// trustOwnDir answers the "do you trust this folder?" dialog for the router's
// own working directory. Collie created that directory and wrote every file in
// it, so asking the human adds friction without adding safety. Any other
// dialog (and any other directory) is left for the human.
func (sp *Super) trustOwnDir(ctx context.Context, pane string) {
	if dir := sp.routerCwd(sp.routerOn()); sp.answerTrust(ctx, pane, dir) {
		log.Printf("router: trusted its own directory %s", dir)
	}
}

func isTrustChoice(label string) bool {
	l := strings.ToLower(strings.TrimSpace(label))
	return (strings.HasPrefix(l, "yes") && strings.Contains(l, "trust")) || strings.HasPrefix(l, "trust and continue")
}

func (sp *Super) noteAnsweredLocked(w *Worker) {
	if sp.lastAnswer == nil {
		sp.lastAnswer = map[string]time.Time{}
	}
	if w != nil {
		if w.PaneID != "" {
			sp.lastAnswer[w.PaneID] = time.Now()
		}
		if w.Status == "blocked" {
			w.Status = "idle"
		}
	}
}

// answerTrust picks "Yes, I trust this folder" / "Trust and continue" when
// pane shows a folder-trust dialog for dir, the folder Collie started that
// agent in. It reports whether it answered. Nothing else is ever answered this way.
func (sp *Super) answerTrust(ctx context.Context, pane, dir string) bool {
	sp.mu.Lock()
	if sp.lastTrust == nil {
		sp.lastTrust = map[string]time.Time{}
	}
	if time.Since(sp.lastTrust[pane]) < 5*time.Second {
		sp.mu.Unlock()
		return false
	}
	sp.lastTrust[pane] = time.Now()
	sp.mu.Unlock()

	rr, err := sp.s.herdr.Read(ctx, pane, "detection", "text", 0)
	if err != nil || !trustDialogFor(rr.Text, dir, sp.s.tilde(dir)) {
		return false
	}
	for _, c := range parseApproval(rr.Text).Choices {
		if isTrustChoice(c.Label) {
			return sp.s.herdr.chooseDialog(ctx, pane, c) == nil
		}
	}
	return false
}

// chooseDialog answers a dialog in pane with one of its choices: cursor movement and
// Enter when the menu has a cursor, else its digit for a cursorless numbered menu.
func (h *Herdr) chooseDialog(ctx context.Context, pane string, c Choice) error {
	if len(c.Keys) > 0 {
		return h.SendKeys(ctx, pane, c.Keys...)
	}
	if c.Text != "" {
		return h.SendText(ctx, pane, c.Text)
	}
	return h.SendKeys(ctx, pane, "enter")
}

// pickChoice finds the choice the router named: its number as shown ("3"),
// its position, or words of its label.
func pickChoice(a Approval, want string) (Choice, bool) {
	want = strings.TrimSpace(want)
	for _, c := range a.Choices {
		if c.Text != "" && c.Text == want {
			return c, true
		}
	}
	if n, err := strconv.Atoi(want); err == nil && n >= 1 && n <= len(a.Choices) {
		return a.Choices[n-1], true
	}
	var hit []Choice
	for _, c := range a.Choices {
		if want != "" && strings.Contains(strings.ToLower(c.Label), strings.ToLower(want)) {
			hit = append(hit, c)
		}
	}
	if len(hit) == 1 {
		return hit[0], true
	}
	return Choice{}, false
}

// trustDialogFor: the screen asks whether to trust a folder, and names dir
// (as given, with ~, or by its last part: the screen may show the path a
// symlink resolves to).
func trustDialogFor(screen, dir, tilde string) bool {
	if !strings.Contains(strings.ToLower(screen), "trust") || dir == "" {
		return false
	}
	if strings.Contains(screen, dir) || strings.Contains(screen, tilde) {
		return true
	}
	return regexp.MustCompile(`(?m)/` + regexp.QuoteMeta(path.Base(dir)) + `/?\s*$`).MatchString(screen)
}
