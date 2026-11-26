package wechat

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// rtcRunnerIdle waits until the runner has no work running.
func rtcRunnerIdle(t *testing.T, l *loop, r *InboundRunner) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		idle := false
		l.do(func() { idle = r.running == 0 })
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("runner did not settle")
		}
		time.Sleep(time.Millisecond)
	}
}

// from: gateway/tests/runtime/inbound-runner.test.ts: a failed head message blocks later messages in the same chat until retry succeeds
func TestRtcInboundRunnerBlocksFailedHead(t *testing.T) {
	l := &loop{}
	inbox, err := NewFileInboundInbox(filepath.Join(t.TempDir(), "inbound.json"), func() string { return "account" }, 10)
	mustNoErr(t, err)
	for _, key := range []string{"first", "second"} {
		mustNoErr(t, inbox.Put(InboundMessage{ChatID: "chat", Text: key, MessageID: key, DeliveryKey: key}))
	}
	var started []string
	var errs []error
	failFirst := true
	r := NewInboundRunner(l, InboundRunnerOptions{Inbox: inbox,
		Process: func(m InboundMessage) error {
			started = append(started, m.DeliveryKey)
			if m.DeliveryKey == "first" && failFirst {
				failFirst = false
				return errors.New("first attempt failed")
			}
			return nil
		},
		Steer:   func(InboundMessage) (bool, error) { return false, nil },
		Covered: func(InboundMessage) error { return nil },
		OnError: func(err error) { errs = append(errs, err) }})
	pending := func() []string {
		var keys []string
		l.do(func() {
			list, err := inbox.List()
			mustNoErr(t, err)
			for _, m := range list {
				keys = append(keys, m.DeliveryKey)
			}
		})
		return keys
	}
	check := func(wantStarted, wantPending []string, wantErrs int) {
		t.Helper()
		l.do(r.Schedule)
		rtcRunnerIdle(t, l, r)
		var gotStarted []string
		var gotErrs int
		l.do(func() { gotStarted, gotErrs = append([]string(nil), started...), len(errs) })
		if !reflect.DeepEqual(gotStarted, wantStarted) || !reflect.DeepEqual(pending(), wantPending) || gotErrs != wantErrs {
			t.Fatalf("started %v pending %v errors %d, want %v %v %d", gotStarted, pending(), gotErrs, wantStarted, wantPending, wantErrs)
		}
	}
	check([]string{"first"}, []string{"first", "second"}, 1)
	check([]string{"first", "first"}, []string{"second"}, 1)
	check([]string{"first", "first", "second"}, nil, 1)
}
