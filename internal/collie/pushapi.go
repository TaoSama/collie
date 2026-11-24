package collie

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// The page's side of Web Push: the hub's public key to subscribe with, and
// this browser's subscription (sw.js shows what is pushed to it).

func (s *Server) pushRoutes(mux *http.ServeMux) {
	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.auth(h)) }
	api("GET /api/push", s.pushStatus)
	api("POST /api/push/subscribe", s.pushSubscribe)
	api("POST /api/push/unsubscribe", s.pushUnsubscribe)
	api("POST /api/push/test", s.pushTest)
}

// pushDevice is the paired browser a request comes from ("" for the master token).
func (s *Server) pushDevice(r *http.Request) string {
	if _, d := s.identity(r); d != nil {
		return d.ID
	}
	return ""
}

// pushStatus: the key to subscribe with, and whether the hub has this
// browser's subscription (?endpoint=).
func (s *Server) pushStatus(w http.ResponseWriter, r *http.Request) {
	wp := s.sup.push
	sub := wp.get(r.URL.Query().Get("endpoint"))
	writeJSON(w, 200, map[string]any{"key": wp.key.publicKey(), "subscribed": sub != nil && sub.Device == s.pushDevice(r)})
}

type pushSubBody struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// pushSubscribe keeps this browser's PushSubscription (its toJSON()).
func (s *Server) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	var body pushSubBody
	if !decode(w, r, &body) {
		return
	}
	u, err := url.Parse(body.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || len(body.Endpoint) > 1024 {
		writeJSON(w, 400, map[string]any{"error": "endpoint?"})
		return
	}
	if k, err := b64.DecodeString(body.Keys.P256dh); err != nil || len(k) != 65 {
		writeJSON(w, 400, map[string]any{"error": "keys.p256dh?"})
		return
	}
	if a, err := b64.DecodeString(body.Keys.Auth); err != nil || len(a) != 16 {
		writeJSON(w, 400, map[string]any{"error": "keys.auth?"})
		return
	}
	sub := pushSub{Endpoint: body.Endpoint, P256dh: body.Keys.P256dh, Auth: body.Keys.Auth, Device: s.pushDevice(r), At: time.Now()}
	if err := s.sup.push.add(sub); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// pushUnsubscribe: notifications were turned off on this browser.
func (s *Server) pushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.sup.push.remove(body.Endpoint)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// pushTest pushes one notification to this browser, to see that they arrive.
func (s *Server) pushTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if !decode(w, r, &body) {
		return
	}
	wp := s.sup.push
	sub := wp.get(body.Endpoint)
	if sub == nil {
		writeJSON(w, 404, map[string]any{"error": "hub 上没有这个浏览器的推送订阅，重新打开一次通知开关"})
		return
	}
	s.sup.mu.Lock()
	subject := s.sup.pushSubjectLocked()
	s.sup.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), pushTimeout)
	defer cancel()
	n := pushNote{Title: pushTitle, Body: "推送通知可以用了：页面关掉也能收到需要你处理的事", Tag: "push-test", Hash: "#/prefs"}
	if err := wp.deliver(ctx, subject, *sub, n); err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error(), "gone": wp.get(body.Endpoint) == nil})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
