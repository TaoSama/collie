package collie

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// The web UI calls Collie's sessions by their own title by default, or by
// worker name on request; the choice is a setting, sent with the super snapshot.
func TestWorkerTitleInSnapshot(t *testing.T) {
	if got := defaultState().Settings.WorkerTitle; got != workerTitleTopic {
		t.Fatalf("defaultState worker_title = %q, want %q", got, workerTitleTopic)
	}
	sp, _, call := newPendingTestEnv(t)
	cases := []struct {
		in   string
		want string
	}{
		{"", workerTitleTopic},
		{workerTitleTopic, workerTitleTopic},
		{workerTitleName, workerTitleName},
	}
	for _, tc := range cases {
		sp.st.Settings.WorkerTitle = tc.in
		var snap superView
		if err := json.Unmarshal(call("GET", "/api/super", nil).Body.Bytes(), &snap); err != nil {
			t.Fatal(err)
		}
		if snap.WorkerTitle != tc.want {
			t.Fatalf("snapshot worker_title for %q = %q, want %q", tc.in, snap.WorkerTitle, tc.want)
		}
	}
}

func TestWorkerTitleSettingsSaveAndLoad(t *testing.T) {
	sp, _, call := newPendingTestEnv(t)
	cases := []struct {
		in   string
		want string
	}{
		{workerTitleName, workerTitleName},
		{workerTitleTopic, workerTitleTopic},
		{"", workerTitleTopic},
		{"unexpected", workerTitleTopic},
	}
	for _, tc := range cases {
		res := call("POST", "/api/super/settings", map[string]any{
			"settings": Settings{
				RouterKind:  "claude",
				WorkerKind:  "claude",
				WorkerTitle: tc.in,
			},
		})
		if res.Code != 200 {
			t.Fatalf("POST /api/super/settings (%q): status %d, body %s", tc.in, res.Code, res.Body.String())
		}
		var got struct {
			Settings Settings `json:"settings"`
		}
		if err := json.Unmarshal(call("GET", "/api/super/settings", nil).Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Settings.WorkerTitle != tc.want {
			t.Fatalf("GET /api/super/settings worker_title after %q = %q, want %q", tc.in, got.Settings.WorkerTitle, tc.want)
		}
	}

	db, err := openDB(filepath.Join(t.TempDir(), "herd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO kv(k,v) VALUES('settings','{"router_kind":"claude","worker_kind":"claude"}')`); err != nil {
		t.Fatal(err)
	}
	st, err := loadState(db)
	if err != nil {
		t.Fatal(err)
	}
	if st.Settings.WorkerTitle != workerTitleTopic {
		t.Fatalf("loadState missing worker_title = %q, want %q", st.Settings.WorkerTitle, workerTitleTopic)
	}
	if _, err := db.Exec(`UPDATE kv SET v='{"router_kind":"claude","worker_kind":"claude","worker_title":"name"}' WHERE k='settings'`); err != nil {
		t.Fatal(err)
	}
	st, err = loadState(db)
	if err != nil {
		t.Fatal(err)
	}
	if st.Settings.WorkerTitle != workerTitleName {
		t.Fatalf("loadState explicit name worker_title = %q, want %q", st.Settings.WorkerTitle, workerTitleName)
	}
	_ = sp
}
