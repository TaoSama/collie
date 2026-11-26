package collie

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

func TestOutboxMaintenance(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "herd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	st := newExtStore(db)
	cutoff := time.Unix(1000, 500)
	older := cutoff.Add(-time.Second)
	sameSecondBefore := cutoff.Add(-time.Nanosecond)
	sameSecondAfter := cutoff.Add(time.Nanosecond)

	t.Run("expiration", func(t *testing.T) {
		for _, o := range []OutboxItem{
			{Ref: "expire-older", At: older, State: outPending},
			{Ref: "expire-before", At: sameSecondBefore, State: outPending},
			{Ref: "expire-equal", At: cutoff, State: outPending},
			{Ref: "expire-after", At: sameSecondAfter, State: outPending},
			{Ref: "expire-sent", At: older, State: extproto.DeliverySent},
		} {
			st.putOut(&o)
		}
		st.expireStale(cutoff)
		for ref, want := range map[string]string{
			"expire-older": "expired", "expire-before": "expired",
			"expire-equal": outPending, "expire-after": outPending, "expire-sent": extproto.DeliverySent,
		} {
			if o := st.out(ref); o == nil || o.State != want {
				t.Errorf("%s = %+v, want state %s", ref, o, want)
			}
		}
	})

	t.Run("retention", func(t *testing.T) {
		dir := t.TempDir()
		for _, tt := range []struct {
			ref, state     string
			at             time.Time
			wantRow        bool
			wantAttachment bool
		}{
			{"trim-older", extproto.DeliverySent, older, false, false},
			{"trim-before", extproto.DeliverySent, sameSecondBefore, true, false},
			{"trim-equal", extproto.DeliverySent, cutoff, true, true},
			{"trim-after", extproto.DeliverySent, sameSecondAfter, true, true},
			{"trim-pending", outPending, older, true, true},
		} {
			t.Run(tt.ref, func(t *testing.T) {
				path := filepath.Join(dir, tt.ref)
				if err := os.WriteFile(path, []byte("attachment"), 0o600); err != nil {
					t.Fatal(err)
				}
				st.putOut(&OutboxItem{Ref: tt.ref, At: tt.at, State: tt.state, Files: []OutFile{{Path: path}}})
				st.trimOutbox(cutoff.Add(outboxKeep))
				if got := st.out(tt.ref) != nil; got != tt.wantRow {
					t.Errorf("row kept = %t, want %t", got, tt.wantRow)
				}
				_, err := os.Stat(path)
				if tt.wantAttachment && err != nil || !tt.wantAttachment && !os.IsNotExist(err) {
					t.Errorf("attachment stat error = %v, want file present = %t", err, tt.wantAttachment)
				}
			})
		}
	})

	t.Run("retry", func(t *testing.T) {
		for _, o := range []OutboxItem{
			{Ref: "retry-new", Conv: "fake:new", At: cutoff, State: outPending},
			{Ref: "retry-other", Conv: "fakew:old", At: older, State: outPending},
			{Ref: "retry-case", Conv: "Fake:old", At: older, State: outPending},
			{Ref: "retry-sent", Conv: "fake:sent", At: older, State: extproto.DeliverySent},
			{Ref: "retry-old", Conv: "fake:old", At: older, State: outPending},
		} {
			st.putOut(&o)
		}
		var refs []string
		for _, o := range st.pendingOutsFor("fake") {
			refs = append(refs, o.Ref)
		}
		if want := []string{"retry-old", "retry-new"}; !slices.Equal(refs, want) {
			t.Errorf("retry candidates = %v, want %v", refs, want)
		}
	})
}
