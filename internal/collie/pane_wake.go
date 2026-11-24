package collie

import (
	"sync"
	"time"
)

// paneWake lets the streams following a pane look again as soon as input
// reaches it, instead of on their next poll: what you typed echoes, and the
// message you sent shows, one network trip sooner. For a moment after, they
// also look more often, while the agent writes it down.
type paneWake struct {
	mu sync.Mutex
	ch map[string]chan struct{}
}

// wakeFast is how long a stream keeps polling at its quick rate after input.
const wakeFast = 3 * time.Second

// wait returns a channel closed at the pane's next input.
func (w *paneWake) wait(id string) <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ch == nil {
		w.ch = map[string]chan struct{}{}
	}
	c, ok := w.ch[id]
	if !ok {
		c = make(chan struct{})
		w.ch[id] = c
	}
	return c
}

// wake tells every stream waiting on the pane that input just reached it.
func (w *paneWake) wake(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if c, ok := w.ch[id]; ok {
		close(c)
		delete(w.ch, id)
	}
}

// forget drops a pane's entry when a stream on it ends. Another stream still
// waiting on it misses at most one early wake and polls as usual.
func (w *paneWake) forget(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.ch, id)
}

// pollAfter waits one polling interval: normal, or quick until fastUntil.
// It returns early when the pane gets input (moving fastUntil) and reports
// false once the request is gone.
func (s *Server) pollAfter(done <-chan struct{}, id string, normal, quick time.Duration, fastUntil *time.Time) bool {
	d := normal
	if time.Now().Before(*fastUntil) {
		d = quick
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		s.wakes.forget(id)
		return false
	case <-s.wakes.wait(id):
		*fastUntil = time.Now().Add(wakeFast)
	case <-t.C:
	}
	return true
}
