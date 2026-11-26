package collie

import (
	"testing"
	"time"
)

func TestPollAfterWakesOnInput(t *testing.T) {
	var w paneWake
	wake, stop := w.subscribe("w1:p1")
	defer stop()
	other, stopOther := w.subscribe("w1:p1") // a second stream on the same pane
	var woke time.Time
	done := make(chan struct{})
	got := make(chan time.Duration, 1)
	go func() {
		t0 := time.Now()
		pollAfter(done, wake, pollRates{normal: time.Hour, quick: time.Millisecond}, &woke)
		got <- time.Since(t0)
	}()
	time.Sleep(20 * time.Millisecond)
	w.wake("w2:p1") // another pane: no effect
	select {
	case <-got:
		t.Fatal("woke on another pane's input")
	case <-time.After(20 * time.Millisecond):
	}
	stopOther() // the other stream ending doesn't lose this one's wakes
	w.wake("w1:p1")
	select {
	case d := <-got:
		if d > time.Second {
			t.Fatalf("waited %v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("input did not wake the stream")
	}
	select {
	case <-other:
		t.Fatal("an ended stream still got a wake")
	default:
	}
	// Input while the stream is busy waits for its next poll.
	w.wake("w1:p1")
	t0 := time.Now()
	if !pollAfter(done, wake, pollRates{normal: time.Hour, quick: time.Hour}, &time.Time{}) || time.Since(t0) > time.Second {
		t.Fatal("a wake between polls was lost")
	}
	// Right after input it looks again quickly (snap and burst phases).
	t0 = time.Now()
	if !pollAfter(done, wake, pollRates{normal: time.Hour, quick: time.Hour, burst: time.Hour, snap: time.Millisecond}, &woke) || time.Since(t0) > time.Second {
		t.Fatal("no snap look right after input")
	}
	t0 = time.Now()
	if !pollAfter(done, wake, pollRates{normal: time.Hour, quick: time.Hour, burst: time.Millisecond}, &woke) || time.Since(t0) > time.Second {
		t.Fatal("no quick look after input")
	}
	close(done)
	if pollAfter(done, wake, pollRates{normal: time.Hour, quick: time.Hour}, &time.Time{}) {
		t.Fatal("a closed request should stop the stream")
	}
}
