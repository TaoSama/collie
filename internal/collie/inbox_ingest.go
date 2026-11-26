package collie

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

// fileIncoming commits the message, conversation and any resulting trigger
// together. The scheduler and the inbox see the new state only after commit.
func (h *extHost) fileIncoming(m *ExtManifest, in incomingMsg) (InboxConv, *Trigger, error) {
	sp, store := h.sp, h.store
	sp.mu.Lock()
	defer sp.mu.Unlock()
	store.mu.Lock()
	defer store.mu.Unlock()

	id := convID(m.ID, in.Chat)
	c := inboxConversation(m, in, store.convs[id])
	// The switch is about other people's messages: your own trigger word
	// works in any conversation.
	request, triggered := afterTrigger(in.Text, triggerWord(sp.st.Settings.ExtTrigger))
	keep := c.On || (in.fromYou() && triggered)
	if keep && in.Mine {
		ours, err := h.ours(id, in.ID, in.Text, h.footerLocked())
		if err != nil {
			return InboxConv{}, nil, fmt.Errorf("check sent messages: %w", err)
		}
		keep = !ours // Collie's own answer is already shown as sent.
	}

	var text string
	if keep {
		if request, ok := in.triggerRequest(m, c, request, triggered); ok {
			var err error
			text, err = h.requestText(m, c, in, request)
			if err != nil {
				return InboxConv{}, nil, fmt.Errorf("read conversation context: %w", err)
			}
			c.AskMsg, c.AskAt, c.AskBy, c.Handed = in.ID, time.Now(), in.SenderName, in.At
			c.Unread = 0
		}
	}

	tx, err := store.db.Begin()
	if err != nil {
		return InboxConv{}, nil, err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			log.Printf("ext: roll back incoming message: %v", err)
		}
	}()
	if keep {
		added, err := putInboxMsg(tx, InboxMsg{ID: in.ID, Conv: id, At: in.At, Sender: in.Sender, SenderName: in.SenderName, Mine: in.fromYou(), Text: in.Text})
		if err != nil {
			return InboxConv{}, nil, fmt.Errorf("save message: %w", err)
		}
		if !added {
			return InboxConv{}, nil, nil // already committed
		}
		if err := trimInboxMsgs(tx, id); err != nil {
			return InboxConv{}, nil, fmt.Errorf("trim messages: %w", err)
		}
	}
	if err := putInboxConv(tx, &c); err != nil {
		return InboxConv{}, nil, fmt.Errorf("save conversation: %w", err)
	}

	var t *Trigger
	var th *Thread
	if text != "" {
		from := in.SenderName
		if in.fromYou() {
			from = "you"
		}
		t, th = sp.prepareTriggerLocked("ext:"+c.Ext, c.thread(), from, text, "ext:"+c.ID+":"+in.ID, "", in.ID)
		if t != nil {
			h.prepareConvDeliveryLocked(c, t)
			if err := sp.persistTriggerLocked(tx, t, th); err != nil {
				return InboxConv{}, nil, fmt.Errorf("save message trigger: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return InboxConv{}, nil, err
	}
	store.convs[id] = &c
	sp.publishTriggerLocked(t, th)
	return c, t, nil
}
