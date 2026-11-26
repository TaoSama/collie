package collie

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSSESendChangedJSON(t *testing.T) {
	w := httptest.NewRecorder()
	sse, ok := newSSE(w)
	if !ok {
		t.Fatal("response recorder does not support streaming")
	}
	var lastSuper, lastState []byte
	valid := map[string]string{"text": "line\nbreak"}
	invalid := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, frame := range []struct {
		event string
		value any
		last  *[]byte
		sent  bool
	}{
		{"super", invalid, &lastSuper, false},
		{"super", valid, &lastSuper, true},
		{"state", valid, &lastState, true},
		{"super", map[string]string{"text": "line\nbreak"}, &lastSuper, false},
		{"super", invalid, &lastSuper, true},
		{"super", invalid, &lastSuper, false},
		{"super", valid, &lastSuper, true},
	} {
		if sent, err := sse.sendChangedJSON(frame.event, frame.value, frame.last); err != nil || sent != frame.sent {
			t.Fatalf("send %s (%v): sent=%v, err=%v; want sent=%v", frame.event, frame.value, sent, err, frame.sent)
		}
	}
	want := "event: super\ndata: {\"text\":\"line\\nbreak\"}\n\n" +
		"event: state\ndata: {\"text\":\"line\\nbreak\"}\n\n" +
		"event: super\ndata: \n\n" +
		"event: super\ndata: {\"text\":\"line\\nbreak\"}\n\n"
	if got := w.Body.String(); got != want {
		t.Fatalf("SSE frames = %q, want %q", got, want)
	}
}

type failingSSERecorder struct {
	*httptest.ResponseRecorder
	err error
}

func (w failingSSERecorder) Write([]byte) (int, error) { return 0, w.err }

func TestSSESendChangedJSONWriteError(t *testing.T) {
	errWrite := errors.New("client disconnected")
	sse, ok := newSSE(failingSSERecorder{httptest.NewRecorder(), errWrite})
	if !ok {
		t.Fatal("response recorder does not support streaming")
	}
	var last []byte
	sent, err := sse.sendChangedJSON("state", "new value", &last)
	if sent || !errors.Is(err, errWrite) || last != nil {
		t.Fatalf("failed write: sent=%v, err=%v, cached=%q", sent, err, last)
	}
}
