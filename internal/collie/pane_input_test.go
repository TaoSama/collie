package collie

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPageInputThatCanInterruptIsLogged(t *testing.T) {
	s, _, pane := chatServer(t, 0, "claude")
	var out bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(prev) })
	post := func(h http.HandlerFunc, body string) {
		t.Helper()
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.SetPathValue("id", pane)
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	post(s.sendText, `{"text":"hello"}`)
	post(s.sendKeys, `{"keys":["enter"]}`)
	if out.Len() != 0 {
		t.Fatalf("plain typing was logged: %q", out.String())
	}
	post(s.sendText, `{"text":"\u001b[<65;60;20M\u001b[<65;60;20M"}`)
	post(s.sendKeys, `{"keys":["esc"]}`)
	got := out.String()
	for _, want := range []string{"pane " + pane + ": 24 bytes holding 2 Esc from", "pane " + pane + ": keys esc from"} {
		if !strings.Contains(got, want) {
			t.Fatalf("log %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "<65") {
		t.Fatalf("the text itself was logged: %q", got)
	}
}

func TestEscChunksNeverEndInsideASequence(t *testing.T) {
	report := "\x1b[<65;119;25M" // 13 bytes: a cut every 1 KB falls anywhere in it
	long := strings.Repeat(report, 300) + "\x1b[1;5F"
	parts := escChunks(long, escChunkMax)
	if strings.Join(parts, "") != long {
		t.Fatal("chunks don't add up to the text")
	}
	for i, p := range parts {
		if len(p) > escChunkMax || !strings.HasPrefix(p, "\x1b") || strings.HasSuffix(p, "\x1b") {
			t.Fatalf("chunk %d (%d bytes) %q", i, len(p), p[max(0, len(p)-16):])
		}
		if i < len(parts)-1 && !strings.HasSuffix(p, "M") {
			t.Fatalf("chunk %d ends inside a report: %q", i, p[len(p)-8:])
		}
	}
	for _, whole := range []string{"", "hello", strings.Repeat("x", 5000), report + "abc"} {
		if got := escChunks(whole, escChunkMax); strings.Join(got, "") != whole || len(got) > 1 {
			t.Fatalf("%.20q went in %d pieces", whole, len(got))
		}
	}
	// A long paste: its start marker and text whole, the end marker after.
	paste := "\x1b[200~" + strings.Repeat("y", 900) + "\x1b[201~"
	if got := escChunks(paste, escChunkMax); len(got) != 2 || got[1] != "\x1b[201~" {
		t.Fatalf("paste cut as %d pieces", len(got))
	}
}

func TestLongEscapeTextGoesOutInShortWrites(t *testing.T) {
	s, f, pane := chatServer(t, 0, "claude")
	text := strings.Repeat("\x1b[<65;60;20M", 240) + "\x1b[1;5F"
	body, _ := json.Marshal(map[string]string{"text": text})
	r := httptest.NewRequest("POST", "/", strings.NewReader(string(body)))
	r.SetPathValue("id", pane)
	w := httptest.NewRecorder()
	s.sendText(w, r)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	writes := f.panes[pane].writes
	if len(writes) < 2 || strings.Join(writes, "") != text {
		t.Fatalf("%d writes, all there: %v", len(writes), strings.Join(writes, "") == text)
	}
	for _, x := range writes {
		if len(x) > escChunkMax {
			t.Fatalf("a %d-byte write", len(x))
		}
	}
}
