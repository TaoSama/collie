package collie

import (
	"testing"
	"time"
)

func TestFleetNodeChangeWaitsForSuperSubscribers(t *testing.T) {
	for _, name := range []string{"attach", "detach"} {
		t.Run(name, func(t *testing.T) {
			h := newHerdr("", "hub", t.TempDir())
			srv := &Server{herdr: h}
			sup := &Super{subs: map[chan struct{}]bool{}}
			srv.setSuper(sup)
			node := &Machine{ID: "node", Online: true}
			change := func() { h.attach(node) }
			if name == "detach" {
				h.attach(node)
				change = func() { h.detach(node.ID, node) }
			}
			wake, unsubscribe := sup.subscribe()
			defer unsubscribe()

			// subscribe and unsubscribe change the subscriber map under this lock.
			sup.mu.Lock()
			started, done := make(chan struct{}), make(chan struct{})
			go func() {
				close(started)
				change()
				close(done)
			}()
			<-started
			select {
			case <-wake:
				sup.mu.Unlock()
				<-done
				t.Fatal("node change notified subscribers while their lock was held")
			case <-time.After(50 * time.Millisecond):
				sup.mu.Unlock()
			}

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("node change did not complete after the subscriber lock was released")
			}
			select {
			case <-wake:
			default:
				t.Fatal("node change did not notify subscribers")
			}
		})
	}
}
