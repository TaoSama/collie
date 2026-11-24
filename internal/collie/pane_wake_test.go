package collie

import (
	"testing"
	"time"
)

func TestPollAfterWakesOnInput(t *testing.T) {
	s := &Server{}
	var fast time.Time
	done := make(chan struct{})
	got := make(chan time.Duration, 1)
	go func() {
		t0 := time.Now()
		s.pollAfter(done, "w1:p1", time.Hour, time.Millisecond, &fast)
		got <- time.Since(t0)
	}()
	time.Sleep(20 * time.Millisecond)
	s.wakes.wake("w2:p1") // another pane: no effect
	select {
	case <-got:
		t.Fatal("woke on another pane's input")
	case <-time.After(20 * time.Millisecond):
	}
	s.wakes.wake("w1:p1")
	select {
	case d := <-got:
		if d > time.Second {
			t.Fatalf("waited %v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("input did not wake the stream")
	}
	if !time.Now().Before(fast) {
		t.Fatal("no quick polling after input")
	}
	// While quick, the wait is the quick interval.
	t0 := time.Now()
	if !s.pollAfter(done, "w1:p1", time.Hour, time.Millisecond, &fast) || time.Since(t0) > time.Second {
		t.Fatal("quick interval not used")
	}
	close(done)
	if s.pollAfter(done, "w1:p1", time.Hour, time.Hour, &time.Time{}) {
		t.Fatal("a closed request should stop the stream")
	}
}
