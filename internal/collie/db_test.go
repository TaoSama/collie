package collie

import (
	"path/filepath"
	"testing"
	"time"
)

// Everything the super-agent keeps must survive a restart.
func TestStateRoundTrip(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "herd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().Truncate(time.Second)

	st := defaultState()
	st.Seq = 7
	st.Triggers = append(st.Triggers, &Trigger{ID: "t1", Source: "chat", Thread: mainThread, Text: "hi", At: now, Status: "queued"})
	st.Threads[mainThread] = &Thread{ID: mainThread, LastAt: now}
	st.Workers["w1"] = &Worker{Name: "w1", Kind: "claude", CreatedAt: now, Status: "working"}
	st.Entries = append(st.Entries, &Entry{ID: "e2", Role: "collie", Thread: mainThread, Text: "ok", At: now})
	st.Timers = append(st.Timers, &Timer{ID: "m3", Spec: "daily 09:00", Text: "check", NextRun: now, Enabled: true})
	st.Settings.RouterKind = "codex"
	if err := saveState(db, st, nil); err != nil {
		t.Fatal(err)
	}

	got, err := loadState(db)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != 7 || len(got.Triggers) != 1 || got.Workers["w1"] == nil || len(got.Entries) != 1 || len(got.Timers) != 1 || got.Settings.RouterKind != "codex" {
		t.Fatalf("lost state: seq=%d triggers=%d workers=%v entries=%d timers=%d router=%q",
			got.Seq, len(got.Triggers), got.Workers, len(got.Entries), len(got.Timers), got.Settings.RouterKind)
	}

	if err := saveState(db, got, []string{"m3"}); err != nil {
		t.Fatal(err)
	}
	got, _ = loadState(db)
	if len(got.Timers) != 0 {
		t.Fatalf("deleted timer came back")
	}
}
