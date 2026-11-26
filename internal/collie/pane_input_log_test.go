package collie

import (
	"bytes"
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
