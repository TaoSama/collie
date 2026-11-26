package collie

import (
	"encoding/json"
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

func TestDeliveryAcknowledgesPersistedState(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "herd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	h := &extHost{store: newExtStore(db)}
	r := &extRun{h: h, man: &ExtManifest{ID: "wechat"}}
	ref := "reply"
	h.store.putOut(&OutboxItem{Ref: ref, Conv: "wechat:chat", At: time.Now(), State: extproto.DeliveryDeferred})
	raw, err := json.Marshal(extproto.Delivery{Ref: ref, State: extproto.DeliverySent, MessageID: "wx-message"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_save BEFORE UPDATE ON ext_outbox BEGIN SELECT RAISE(ABORT, 'write failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.handle(t.Context(), extproto.MChannelDelivery, raw); err == nil {
		t.Fatal("acknowledged a failed write")
	}
	if got := h.store.out(ref); got.State != extproto.DeliveryDeferred {
		t.Fatalf("after failed write: %+v", got)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_save`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.handle(t.Context(), extproto.MChannelDelivery, raw); err != nil {
		t.Fatal(err)
	}
	got := h.store.out(ref)
	if got.State != extproto.DeliverySent || got.MessageID != "wx-message" || got.DoneAt.IsZero() {
		t.Fatalf("after replay: %+v", got)
	}
	if _, err := r.handle(t.Context(), extproto.MChannelDelivery, raw); err != nil {
		t.Fatal(err)
	}
	if again := h.store.out(ref); !again.DoneAt.Equal(got.DoneAt) {
		t.Fatalf("duplicate changed the receipt: %+v", again)
	}
}
