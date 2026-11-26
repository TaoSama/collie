package collie

import (
	"context"
	"log"
	"slices"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

// Read state (extensions/README.md §2, capability read): the chat app's
// own unread counts come in every extReadEvery and stand over Collie's;
// what you read in Collie stays read (InboxConv.ReadBase), and is marked
// read in the app as well when the extension's read-back switch is on.

const (
	extReadEvery = 30 * time.Second
	extReadFirst = 5 * time.Second // after a start, once it has signed in
	extReadBatch = 200             // chats a call
)

// syncRead asks the extension for its chats' unread counts.
func (r *extRun) syncRead(ctx context.Context) {
	if !r.man.has(capRead) || !r.running() || !r.signedIn() {
		return
	}
	var chats []string
	for _, c := range r.h.store.list(r.man.ID) {
		chats = append(chats, c.Chat)
	}
	for part := range slices.Chunk(chats, extReadBatch) {
		var res extproto.ReadStates
		if err := r.call(ctx, extproto.MReadState, extproto.ReadChats{Contribution: r.man.Contributes.Connector.ID, Chats: part}, &res); err != nil {
			log.Printf("ext %s: read state: %v", r.man.ID, err)
			return
		}
		r.h.store.appRead(r.man.ID, res.Chats)
	}
}

// signedIn: its last status was ok; signed out, there is nothing to ask.
func (r *extRun) signedIn() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status.State == extproto.StateOK
}

// appRead files the app's counts: only the chats that changed are written.
func (st *extStore) appRead(ext string, counts map[string]extproto.ChatRead) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for chat, cr := range counts {
		c := st.convs[convID(ext, chat)]
		if c == nil {
			continue
		}
		changed := c.appRead(cr.Unread, cr.AtMe)
		if cr.Avatar != "" && cr.Avatar != c.Avatar {
			c.Avatar, changed = cr.Avatar, true
		}
		if cr.Name != "" && cr.Name != c.Name {
			c.Name, changed = cr.Name, true
		}
		if changed {
			st.putConvLocked(c)
		}
	}
}

// readConvs marks conversations read in Collie, and in their apps where
// read-back is on. It answers how many it marked.
func (h *extHost) readConvs(ids []string) int {
	byExt := map[string][]string{}
	n := 0
	for _, id := range ids {
		var had bool
		c, ok := h.store.update(id, func(c *InboxConv) { had = c.Unread > 0 || c.AppUnread > 0; c.markRead() })
		if !ok {
			continue
		}
		n++
		if had {
			byExt[c.Ext] = append(byExt[c.Ext], c.Chat)
		}
	}
	for ext, chats := range byExt {
		m, r := h.manifest(ext), h.run(ext)
		if m == nil || r == nil || !m.readBack(h.setting(ext).Config) {
			continue
		}
		go r.markRead(chats)
	}
	return n
}

// markRead tells the app; its next report shows what took.
func (r *extRun) markRead(chats []string) {
	if !r.running() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), extCallWait)
	defer cancel()
	for part := range slices.Chunk(chats, extReadBatch) {
		var res extproto.ReadMarked
		if err := r.call(ctx, extproto.MReadMark, extproto.ReadChats{Contribution: r.man.Contributes.Connector.ID, Chats: part}, &res); err != nil {
			log.Printf("ext %s: mark read: %v", r.man.ID, err)
			return
		}
		if len(res.Failed) > 0 {
			log.Printf("ext %s: marked %d read, %d not: %v", r.man.ID, len(res.Marked), len(res.Failed), res.Failed)
		}
	}
	r.syncRead(ctx)
}
