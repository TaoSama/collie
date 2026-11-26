package collie

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolResultSettlesWithoutOutput(t *testing.T) {
	cases := []struct {
		name, format, call, result string
		failed                     bool
	}{
		{
			name:   "claude",
			format: "claude",
			call:   `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call","name":"Edit","input":{"file_path":"a.txt"}}]}}`,
			result: `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call","content":[]}]}}`,
		},
		{
			name:   "claude explicit error",
			format: "claude",
			call:   `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call","name":"Edit","input":{"file_path":"a.txt"}}]}}`,
			result: `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call","content":"","is_error":true}]}}`,
			failed: true,
		},
		{
			name:   "codex",
			format: "codex",
			call:   `{"type":"response_item","payload":{"type":"function_call","call_id":"call","name":"exec_command","arguments":"{\"command\":\"true\"}"}}`,
			result: `{"type":"response_item","payload":{"type":"function_call_output","call_id":"call","output":""}}`,
		},
		{
			name:   "codex custom",
			format: "codex",
			call:   `{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"call","name":"apply_patch","input":"*** Begin Patch"}}`,
			result: `{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call","output":[]}}`,
		},
		{
			name:   "agy timing only",
			format: "agy",
			call:   `{"type":"PLANNER_RESPONSE","source":"MODEL","tool_calls":[{"name":"run_command","args":{"CommandLine":"true"}}]}`,
			result: `{"type":"GENERIC","source":"MODEL","content":"Created At: x\nCompleted At: y"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTranscript("unused", tc.format)
			var sync msgSync
			if !tr.line([]byte(tc.call)) {
				t.Fatal("tool call did not change the transcript")
			}
			assertToolPatch(t, sync.patch(tr), false, false)
			if !tr.line([]byte(tc.result)) {
				t.Fatal("tool result did not change the transcript")
			}
			assertToolPatch(t, sync.patch(tr), true, tc.failed)
		})
	}
}

func assertToolPatch(t *testing.T, patch *msgPatch, done, failed bool) {
	t.Helper()
	if patch == nil || patch.From != 0 || len(patch.Msgs) != 1 {
		t.Fatalf("expected a patch for the original tool, got %+v", patch)
	}
	msg := patch.Msgs[0]
	jsonMsg, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Kind != "tool" || msg.Output != "" || msg.Error != failed || strings.Contains(string(jsonMsg), `"tool_done":true`) != done {
		t.Fatalf("expected empty output with done=%v and failed=%v, got %s", done, failed, jsonMsg)
	}
}
