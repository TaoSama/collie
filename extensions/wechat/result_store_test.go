package wechat

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestResultStorePruneWritesOnlyChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, resultsFileName)
	fingerprint := func() string { return "account" }
	var reports []string
	writes := 0
	s, err := newResultStore(dir, fingerprint, func(ref, state, reason string) {
		if state != DeliveryExpired || reason != reasonExpired || writes != 3 {
			t.Fatalf("report %q: state=%q reason=%q checkpoints=%d", ref, state, reason, writes)
		}
		reports = append(reports, ref)
	})
	mustNoErr(t, err)
	rtcHook(t, func(step, target string) error {
		if step == "rename" && target == path {
			writes++
		}
		return nil
	})
	mustNoErr(t, s.prune(rtdNow))
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("empty prune created a store: %v", err)
	}
	for _, ref := range []string{"second", "first"} {
		plan := rtdPlan(func(p *ResultDelivery) { p.Key = ref })
		_, err := s.create(plan, rtdBytes)
		mustNoErr(t, err)
	}
	mustNoErr(t, s.prune(rtdNow))
	if writes != 2 {
		t.Fatalf("idle prune made an extra checkpoint: %d total", writes)
	}
	mustNoErr(t, s.prune(rtdNow+100_000))
	mustNoErr(t, s.prune(rtdNow+100_000))
	if writes != 3 || !slices.Equal(reports, []string{"first", "second"}) {
		t.Fatalf("after expiry: checkpoints=%d reports=%v", writes, reports)
	}
	mustNoErr(t, s.prune(rtdNow+200_000))
	mustNoErr(t, s.prune(rtdNow+200_000))
	if writes != 4 || len(s.plans) != 0 || len(reports) != 2 {
		t.Fatalf("after retention: checkpoints=%d plans=%d reports=%v", writes, len(s.plans), reports)
	}
}

func TestResultStorePruneRetriesFailedCheckpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, resultsFileName)
	fingerprint := func() string { return "account" }
	s, err := newResultStore(dir, fingerprint, nil)
	mustNoErr(t, err)
	plan := rtdPlan(nil)
	_, err = s.create(plan, rtdBytes)
	mustNoErr(t, err)

	failure := errors.New("checkpoint did not finish")
	fail := true
	writes := 0
	rtcHook(t, func(step, target string) error {
		if step == "rename" && target == path {
			writes++
			if fail {
				return failure
			}
		}
		return nil
	})
	if _, err := s.receipt(plan.ID, ResultAccepted, plan.AttemptID); !errors.Is(err, failure) {
		t.Fatalf("receipt save error = %v", err)
	}
	fail = false
	mustNoErr(t, s.prune(rtdNow))
	mustNoErr(t, s.prune(rtdNow))
	reloaded, err := newResultStore(dir, fingerprint, nil)
	mustNoErr(t, err)
	if got := reloaded.plans[plan.ID]; writes != 2 || got == nil || got.Status != ResultClaimed {
		t.Fatalf("after retry: checkpoints=%d plan=%+v", writes, got)
	}
}
