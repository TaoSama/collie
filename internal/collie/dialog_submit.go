package collie

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type dialogSelection struct {
	Sig    string `json:"sig,omitempty"`
	Choice string `json:"choice"`
}

type dialogPick struct {
	Label string `json:"label"`
	Sig   string `json:"sig"`
}

type dialogSubmissions struct {
	mu    sync.Mutex
	panes map[string]dialogSubmission
}

type dialogSubmission struct {
	busy bool
	sig  string
	at   time.Time
}

// The agent may still show a submitted dialog until the next terminal redraw.
const dialogRedraw = 2 * time.Second

func (s *Server) submitDialog(ctx context.Context, pane string, selection dialogSelection, olderNode bool) (dialogPick, error) {
	g := &s.dialogs
	g.mu.Lock()
	for id, d := range g.panes {
		if !d.busy && time.Since(d.at) >= dialogRedraw {
			delete(g.panes, id)
		}
	}
	last := g.panes[pane]
	if last.busy {
		g.mu.Unlock()
		return dialogPick{}, errNotAsking
	}
	if g.panes == nil {
		g.panes = map[string]dialogSubmission{}
	}
	g.panes[pane] = dialogSubmission{busy: true}
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		if last.sig == "" {
			delete(g.panes, pane)
		} else {
			g.panes[pane] = last
		}
		g.mu.Unlock()
	}()

	screen, err := s.herdr.ReadAsk(ctx, pane)
	if notFound(err) {
		return dialogPick{}, errNotAsking
	}
	if err != nil {
		return dialogPick{}, err
	}
	a, asking := visibleApproval(screen)
	if !asking {
		last = dialogSubmission{}
		return dialogPick{}, errNotAsking
	}
	sig := askSig(a)
	matches := selection.Sig == "" || selection.Sig == sig
	if !matches && olderNode {
		matches = selection.Sig == approvalSignature(a, false)
	}
	if last.sig == sig || !matches {
		return dialogPick{}, errNotAsking
	}
	c, ok := selection.pick(a)
	if !ok {
		return dialogPick{}, noChoiceError{choiceLabels(a)}
	}
	// If the response is lost after writing, the same screen may still be visible.
	err = s.herdr.chooseDialog(ctx, pane, c)
	last = dialogSubmission{sig: sig, at: time.Now()}
	if err != nil {
		return dialogPick{}, err
	}
	s.wakes.wake(pane)
	return dialogPick{Label: oneLine(c.Label), Sig: sig}, nil
}

func (d dialogSelection) pick(a Approval) (Choice, bool) {
	if d.Sig == "" { // the Router's CLI chooses from the screen it reads now
		if strings.EqualFold(strings.TrimSpace(d.Choice), "esc") {
			return Choice{Label: "Esc", Keys: []string{"esc"}}, true
		}
		return pickChoice(a, d.Choice)
	}
	var picked Choice
	found := false
	for _, c := range a.Choices {
		if strings.EqualFold(oneLine(c.Label), d.Choice) {
			if found {
				return Choice{}, false
			}
			picked, found = c, true
		}
	}
	return picked, found
}

// submitPaneDialog keeps the read and write on the pane's machine. An older
// node's missing route is the only case that falls back to routed herdr calls.
func (s *Server) submitPaneDialog(ctx context.Context, pane string, selection dialogSelection) (dialogPick, error) {
	local, machine := split(pane)
	if machine != "" {
		client, err := s.herdr.Client(machine)
		if err != nil {
			return dialogPick{}, err
		}
		var out dialogPick
		path := "/api/panes/" + url.PathEscape(local) + "/decide"
		err = nodeJSONWithStatusError(ctx, client, http.MethodPost, path, selection, &out, func(resp *http.Response) error {
			if resp.StatusCode == http.StatusConflict {
				var body struct{ Choices []string }
				if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
					return err
				}
				if len(body.Choices) != 0 {
					return noChoiceError{body.Choices}
				}
				return errNotAsking
			}
			return &nodeStatusError{Code: resp.StatusCode, msg: strings.TrimSpace(nodeErrorBody(resp, 512))}
		})
		var status *nodeStatusError
		if !errors.As(err, &status) || status.Code != http.StatusNotFound {
			return out, err
		}
		return s.submitDialog(ctx, pane, selection, true)
	}
	return s.submitDialog(ctx, pane, selection, false)
}

// paneDecide is only available inside the node's tunnel.
func (s *Server) paneDecide(w http.ResponseWriter, r *http.Request) {
	var selection dialogSelection
	if !decode(w, r, &selection) {
		return
	}
	picked, err := s.submitDialog(r.Context(), r.PathValue("id"), selection, false)
	writeDialogPick(w, picked, err)
}

func writeDialogPick(w http.ResponseWriter, picked dialogPick, err error) {
	var none noChoiceError
	switch {
	case errors.As(err, &none):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "choices": none.choices})
	case errors.Is(err, errNotAsking):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
	case err != nil:
		// This route must never return 404 after it starts handling a write.
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
	default:
		writeJSON(w, http.StatusOK, picked)
	}
}
