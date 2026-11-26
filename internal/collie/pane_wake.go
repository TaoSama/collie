package collie

import (
	"sync"
	"time"
)

// paneWake lets the streams following a pane look again as soon as input
// reaches it, instead of on their next poll: what you typed echoes, and the
// message you sent shows, one network trip sooner. For a moment after, they
// also look more often, while the program answers and the agent writes it
// down. Each stream subscribes once; a wake while it is busy reading waits
// in its channel.
type paneWake struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]bool
}

const (
	wakeSnap  = 35 * time.Millisecond  // immediately after input: catch 2-4ms agent redraws
	wakeBurst = 200 * time.Millisecond // right after input: the burst rate, if any
	wakeFast  = 3 * time.Second        // then the quick rate until this
)

// pollRates: how often a stream looks normally, for a while after input,
// right after it, and immediately after it (0: disabled; a screen redraws in
// ~2-4ms after wheel/key input, a transcript takes longer to be written).
type pollRates struct{ normal, quick, burst, snap time.Duration }

// subscribe returns a channel that gets a value at the pane's input, and
// the function to call when the stream ends.
func (w *paneWake) subscribe(id string) (<-chan struct{}, func()) {
	c := make(chan struct{}, 1)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.subs == nil {
		w.subs = map[string]map[chan struct{}]bool{}
	}
	if w.subs[id] == nil {
		w.subs[id] = map[chan struct{}]bool{}
	}
	w.subs[id][c] = true
	return c, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.subs[id], c)
		if len(w.subs[id]) == 0 {
			delete(w.subs, id)
		}
	}
}

// wake tells every stream on the pane that input just reached it.
func (w *paneWake) wake(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for c := range w.subs[id] {
		select {
		case c <- struct{}{}:
		default: // one is already waiting to be noticed
		}
	}
}

// pollAfter waits one polling interval, quicker for a while after input
// (woke is when that last came). It returns early on input and reports
// false once the request is gone.
func pollAfter(done <-chan struct{}, wake <-chan struct{}, r pollRates, woke *time.Time) bool {
	d := r.normal
	switch since := time.Since(*woke); {
	case since < wakeSnap && r.snap > 0:
		d = r.snap
	case since < wakeBurst && r.burst > 0:
		d = r.burst
	case since < wakeFast:
		d = r.quick
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return false
	case <-wake:
		*woke = time.Now()
	case <-t.C:
	}
	return true
}
