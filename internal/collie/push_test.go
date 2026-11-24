package collie

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// pushService stands in for a browser's push service: it decrypts what it
// is sent with the browser's keys, as the browser would.
type pushService struct {
	t    *testing.T
	srv  *httptest.Server
	ua   *ecdh.PrivateKey
	auth []byte

	mu    sync.Mutex
	got   []pushNote
	code  int // what it answers
	heads []http.Header
}

func newPushService(t *testing.T) *pushService {
	ps := &pushService{t: t, code: 201, auth: make([]byte, 16)}
	ps.ua, _ = ecdh.P256().GenerateKey(rand.Reader)
	rand.Read(ps.auth)
	ps.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ps.mu.Lock()
		defer ps.mu.Unlock()
		ps.heads = append(ps.heads, r.Header.Clone())
		if ps.code >= 300 {
			w.WriteHeader(ps.code)
			return
		}
		var n pushNote
		if err := json.Unmarshal(ps.decrypt(body), &n); err != nil {
			t.Errorf("payload: %v", err)
		}
		ps.got = append(ps.got, n)
		w.WriteHeader(ps.code)
	}))
	t.Cleanup(ps.srv.Close)
	return ps
}

func (ps *pushService) decrypt(body []byte) []byte {
	t := ps.t
	salt, idLen := body[:16], int(body[20])
	asPub, ct := body[21:21+idLen], body[21+idLen:]
	pub, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	shared, _ := ps.ua.ECDH(pub)
	info := append(append([]byte("WebPush: info\x00"), ps.ua.PublicKey().Bytes()...), asPub...)
	ikm, _ := hkdf.Key(sha256.New, shared, ps.auth, string(info), 32)
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if plain[len(plain)-1] != 2 {
		t.Fatalf("no last-record delimiter")
	}
	return plain[:len(plain)-1]
}

func (ps *pushService) sub(path, device string) pushSub {
	return pushSub{Endpoint: ps.srv.URL + path, P256dh: b64.EncodeToString(ps.ua.PublicKey().Bytes()), Auth: b64.EncodeToString(ps.auth), Device: device}
}

func (ps *pushService) take() []pushNote {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := ps.got
	ps.got = nil
	return out
}

// waitPush waits for n pushes (they go out off the notifier's path).
func (ps *pushService) waitPush(n int) []pushNote {
	ps.t.Helper()
	var out []pushNote
	for until := time.Now().Add(5 * time.Second); len(out) < n && time.Now().Before(until); time.Sleep(10 * time.Millisecond) {
		out = append(out, ps.take()...)
	}
	if len(out) != n {
		ps.t.Fatalf("want %d pushes, got %d: %+v", n, len(out), out)
	}
	return out
}

func withPush(t *testing.T, sp *Super) *pushService {
	t.Helper()
	wp, err := newWebPush(sp.db)
	if err != nil {
		t.Fatal(err)
	}
	ps := newPushService(t)
	wp.client = ps.srv.Client()
	sp.push = wp
	if err := wp.add(ps.sub("/s/phone", "phone")); err != nil {
		t.Fatal(err)
	}
	return ps
}

// The VAPID key and subscriptions outlive a restart; a removed device's go.
func TestWebPushStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herd.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	wp, err := newWebPush(db)
	if err != nil {
		t.Fatal(err)
	}
	ps := newPushService(t)
	wp.add(ps.sub("/a", "phone"))
	wp.add(ps.sub("/b", "phone"))
	wp.add(ps.sub("/c", "laptop"))
	db.Close()

	db, _ = openDB(path)
	defer db.Close()
	again, err := newWebPush(db)
	if err != nil {
		t.Fatal(err)
	}
	if again.key.publicKey() != wp.key.publicKey() {
		t.Fatal("the VAPID key changed on restart: every subscription would break")
	}
	if len(again.subs) != 3 {
		t.Fatalf("subscriptions after restart: %d", len(again.subs))
	}
	again.dropDevice("phone")
	again.remove(ps.srv.URL + "/c")
	if again.has() {
		t.Fatalf("left: %+v", again.subs)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM push_subs`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows left in push_subs", n)
	}
}

// A push service saying the subscription is gone forgets it.
func TestWebPushForgetsGoneSubscription(t *testing.T) {
	sp := newRouterTestSuper(t, newFakeHerdr(t))
	ps := withPush(t, sp)
	ps.code = http.StatusGone
	sp.push.broadcast(t.Context(), "mailto:x@y", []pushNote{{Title: "Collie", Body: "x", Tag: "t"}})
	if sp.push.has() {
		t.Fatal("kept a subscription its push service said is gone")
	}
}

// A worker's dialog is pushed like the other channels' notices: once, with
// whose it is and its choices, linked to the decision, and not while you
// are looking or once it is settled.
func TestPushWorkerDialog(t *testing.T) {
	sp, _, w, _ := decisionTestSuper(t)
	ps := withPush(t, sp)
	sp.workerSettled(t.Context(), w, true)
	e := sp.st.Entries[len(sp.st.Entries)-1]
	sp.sendNotifications()
	n := ps.waitPush(1)[0]
	if n.Body != "collie-readme 需要你选：Yes / No, and tell Claude what to do differently (esc)" ||
		n.Tag != w.PaneID || n.Hash != "#/d/"+e.ID || n.Answer["id"] != e.ID || len(n.Choices) != 2 {
		t.Fatalf("push = %+v", n)
	}
	if h := ps.heads[0]; h.Get("Topic") != pushTopic(w.PaneID) || !strings.Contains(h.Get("Authorization"), "k="+sp.push.key.publicKey()) {
		t.Fatalf("headers %v", h)
	}

	// Settled before it went out: nothing.
	sp.st.Entries = sp.st.Entries[:len(sp.st.Entries)-1]
	sp.mu.Lock()
	delete(sp.st.Workers, w.Name)
	sp.mu.Unlock()
	w2 := &Worker{Name: "collie-other", Kind: "claude", Thread: mainThread, PaneID: w.PaneID, Status: "blocked"}
	sp.st.Workers[w2.Name] = w2
	sp.workerSettled(t.Context(), w2, true)
	sp.mu.Lock()
	sp.settleAskLocked(w2.PaneID, "Yes", byCollie)
	sp.mu.Unlock()
	sp.notifyAt = time.Time{}
	sp.sendNotifications()

	// Looking at a page: held back, then sent once that page goes to the background.
	sp.seenPage("p1", "https://c.example", false)
	sp.mu.Lock()
	sp.addEntryReplyLocked("collie", mainThread, "要不要合并？", "", "", nil, []string{"合并", "再等等"}, false)
	sp.mu.Unlock()
	sp.notifyAt = time.Time{}
	sp.sendNotifications()
	time.Sleep(50 * time.Millisecond)
	if got := ps.take(); len(got) != 0 {
		t.Fatalf("pushed while you were looking: %+v", got)
	}
	sp.seenPage("p1", "", true)
	sp.mu.Lock()
	sp.addEntryReplyLocked("collie", mainThread, "现在看一下部署？\n细节在下面", "", "", nil, nil, true)
	sp.mu.Unlock()
	sp.notifyAt = time.Time{}
	sp.sendNotifications()
	n = ps.waitPush(1)[0]
	if n.Body != "Collie 需要你看一下\n现在看一下部署？ 细节在下面" || !strings.HasPrefix(n.Tag, "d:") || len(n.Choices) != 1 || n.Choices[0] != "已看" {
		t.Fatalf("--check push = %+v", n)
	}
}

// Looking ends when the last page in front of you leaves.
func TestSeenPagesAway(t *testing.T) {
	sp := newRouterTestSuper(t, newFakeHerdr(t))
	sp.seenPage("phone", "", false)
	sp.seenPage("laptop", "", false)
	sp.seenPage("phone", "", true)
	if time.Since(sp.seenAt) > time.Second {
		t.Fatal("the laptop is still in front of you")
	}
	sp.seenPage("laptop", "", true)
	if !sp.seenAt.IsZero() {
		t.Fatal("still looking with no page in front of you")
	}
	sp.seenPage("stranger", "", true) // never said it was here
	if !sp.seenAt.IsZero() {
		t.Fatal("an unknown page changed it")
	}
}

// Any other session that stops on a dialog is pushed (the page did that for
// every session): after it has settled, once, with its choices to answer.
func TestPushSessionDialog(t *testing.T) {
	f := newFakeHerdr(t)
	sp := newRouterTestSuper(t, f)
	ps := withPush(t, sp)
	sp.seenAt = time.Time{}
	pane := f.add("w9", "site", "", "claude", "/work/site")
	look := func() {
		list, err := sp.s.herdr.Agents(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		sp.push.watch(list)
	}
	f.panes[pane].screen, f.panes[pane].status = testDialog, "blocked"
	look() // already asking when first seen: not news
	if sp.push.dialogsDue() || len(sp.push.dialogs) != 0 {
		t.Fatal("a dialog from before the hub looked counted as new")
	}
	f.panes[pane].status = "working"
	look()
	f.panes[pane].status = "blocked"
	look()
	if sp.push.dialogsDue() {
		t.Fatal("due before it settled")
	}
	sp.push.dialogs[pane] = time.Now().Add(-pushSettle)
	look()
	sp.sendNotifications()
	n := ps.waitPush(1)[0]
	if n.Body != "site 需要你选：Yes / No, and tell Claude what to do differently (esc)" || n.Tag != pane ||
		n.Hash != "#/d/pane/"+pane || n.Answer["pane"] != pane || n.Answer["sig"] == "" || len(n.Choices) != 2 {
		t.Fatalf("push = %+v", n)
	}
	if strings.Contains(n.Body, "git push") {
		t.Fatalf("the screen leaked: %q", n.Body)
	}

	// A flicker: the same dialog right back is not pushed again.
	f.panes[pane].status = "working"
	look()
	f.panes[pane].status = "blocked"
	look()
	sp.push.dialogs[pane] = time.Now().Add(-pushSettle)
	sp.notifyAt = time.Time{}
	sp.sendNotifications()
	time.Sleep(50 * time.Millisecond)
	if got := ps.take(); len(got) != 0 {
		t.Fatalf("pushed the same dialog again: %+v", got)
	}
	// Answered, then asked again: a new decision.
	sp.push.answered(pane)
	f.panes[pane].status = "working"
	look()
	f.panes[pane].status = "blocked"
	look()
	sp.push.dialogs[pane] = time.Now().Add(-pushSettle)
	sp.notifyAt = time.Time{}
	sp.sendNotifications()
	ps.waitPush(1)

	// Answered before it went out: dropped.
	f.panes[pane].status = "working"
	look()
	f.panes[pane].status = "blocked"
	look()
	f.panes[pane].status = "working"
	look()
	if len(sp.push.dialogs) != 0 {
		t.Fatal("kept a dialog that was answered before it went out")
	}
}

// The page's endpoints: the key, subscribing, the test push, forgetting it.
func TestPushAPI(t *testing.T) {
	sp := newRouterTestSuper(t, newFakeHerdr(t))
	ps := withPush(t, sp)
	sp.push.remove(ps.srv.URL + "/s/phone")
	call := func(method, path string, body any) (int, map[string]any) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		rec := httptest.NewRecorder()
		h := map[string]http.HandlerFunc{"/api/push": sp.s.pushStatus, "/api/push/subscribe": sp.s.pushSubscribe,
			"/api/push/unsubscribe": sp.s.pushUnsubscribe, "/api/push/test": sp.s.pushTest}[strings.SplitN(path, "?", 2)[0]]
		h(rec, httptest.NewRequest(method, path, rd))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	sub := ps.sub("/s/new", "")
	if code, out := call("GET", "/api/push?endpoint="+sub.Endpoint, nil); code != 200 || out["key"] != sp.push.key.publicKey() || out["subscribed"] != false {
		t.Fatalf("status = %d %v", code, out)
	}
	body := map[string]any{"endpoint": sub.Endpoint, "keys": map[string]string{"p256dh": sub.P256dh, "auth": sub.Auth}}
	if code, _ := call("POST", "/api/push/subscribe", map[string]any{"endpoint": "http://evil/", "keys": body["keys"]}); code != 400 {
		t.Fatalf("took a plain-http endpoint: %d", code)
	}
	if code, out := call("POST", "/api/push/subscribe", body); code != 200 {
		t.Fatalf("subscribe = %d %v", code, out)
	}
	if _, out := call("GET", "/api/push?endpoint="+sub.Endpoint, nil); out["subscribed"] != true {
		t.Fatalf("not subscribed after subscribing: %v", out)
	}
	if code, out := call("POST", "/api/push/test", map[string]string{"endpoint": sub.Endpoint}); code != 200 {
		t.Fatalf("test push = %d %v", code, out)
	}
	if n := ps.waitPush(1)[0]; n.Tag != "push-test" || n.Hash != "#/prefs" {
		t.Fatalf("test push: %+v", n)
	}
	call("POST", "/api/push/unsubscribe", map[string]string{"endpoint": sub.Endpoint})
	if code, _ := call("POST", "/api/push/test", map[string]string{"endpoint": sub.Endpoint}); code != 404 || sp.push.has() {
		t.Fatalf("still subscribed after unsubscribing: %d", code)
	}
}
