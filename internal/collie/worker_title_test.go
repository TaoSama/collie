package collie

import (
	"encoding/json"
	"testing"
)

// The web UI calls Collie's sessions by worker name or by their own title;
// the choice is a setting, sent with the super snapshot.
func TestWorkerTitleInSnapshot(t *testing.T) {
	sp, _, call := newPendingTestEnv(t)
	for _, want := range []string{workerTitleTopic, ""} {
		sp.st.Settings.WorkerTitle = want
		var snap superView
		if err := json.Unmarshal(call("GET", "/api/super", nil).Body.Bytes(), &snap); err != nil {
			t.Fatal(err)
		}
		if snap.WorkerTitle != want {
			t.Fatalf("snapshot worker_title = %q, want %q", snap.WorkerTitle, want)
		}
	}
}
