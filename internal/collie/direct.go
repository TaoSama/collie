package collie

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A browser on the same computer as a node can reach that node's terminals
// at http://127.0.0.1 (its local UI port) instead of going through the hub:
// a keystroke and its echo then never leave the computer. The hub hands out
// a token for it, only to a signed-in browser that connects from the node's
// own address; the node accepts the token, from the hub's page, on a few
// terminal routes and for the pictures shown in the chat. Anything else, or
// a token it doesn't know, goes the usual way.

// directTTL is how long a direct token works; the browser asks again after.
const directTTL = 12 * time.Hour

type directTokens struct {
	mu sync.Mutex
	m  map[string]time.Time // token → expiry
}

// mint hands out a token that still has most of its time, or a new one.
func (d *directTokens) mint() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.m == nil {
		d.m = map[string]time.Time{}
	}
	now := time.Now()
	for t, exp := range d.m {
		if now.After(exp) {
			delete(d.m, t)
		} else if exp.Sub(now) > directTTL/2 {
			return t
		}
	}
	tok := randomToken()
	d.m[tok] = now.Add(directTTL)
	return tok
}

func (d *directTokens) valid(tok string) bool {
	if tok == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	exp, ok := d.m[tok]
	return ok && time.Now().Before(exp)
}

// mintDirect (node, through the tunnel only): a token for the hub to pass on.
func (s *Server) mintDirect(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"token": s.direct.mint(), "ttl": int(directTTL.Seconds())})
}

// directRoute: the node routes a direct token opens. Terminal I/O, and
// pictures shown in the chat (no downloads or HTML pages).
func directRoute(method, path string, q url.Values) bool {
	if path == "/api/file" {
		as := q.Get("as")
		return method == "GET" && (as == "image" || as == "thumb") && q.Get("download") == "" && isImagePath(q.Get("path"))
	}
	rest, ok := strings.CutPrefix(path, "/api/panes/")
	if !ok {
		return method == "GET" && path == "/api/hello"
	}
	i := strings.LastIndexByte(rest, '/')
	if i <= 0 {
		return false
	}
	switch rest[i+1:] {
	case "stream":
		return method == "GET"
	case "text", "keys", "resize":
		return method == "POST"
	}
	return false
}

// directHandler serves /direct/… on the node's local UI port (local): the
// routes above, for the hub's page or the local UI's, holding a token this
// node minted.
func (s *Server) directHandler(hub, local string, api http.Handler) http.Handler {
	origins := map[string]bool{local: true}
	if u, err := url.Parse(hub); err == nil && u.Host != "" {
		origins[u.Scheme+"://"+u.Host] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/direct")
		if o := r.Header.Get("Origin"); o != "" {
			if !origins[o] {
				writeJSON(w, 403, map[string]any{"error": "origin not allowed"})
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", o)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", "X-File-Kind, X-File-Size, X-File-Path, X-File-Thumb")
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST")
				h.Set("Access-Control-Allow-Headers", "Content-Type")
				h.Set("Access-Control-Allow-Private-Network", "true")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		if !s.direct.valid(r.URL.Query().Get("access_token")) {
			writeJSON(w, 401, map[string]any{"error": "unauthorized"})
			return
		}
		if !directRoute(r.Method, path, r.URL.Query()) {
			writeJSON(w, 404, map[string]any{"error": "not found"})
			return
		}
		if path == "/api/hello" {
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path, r2.URL.RawPath = path, ""
		api.ServeHTTP(w, r2)
	})
}

// clientIP is where a request comes from, behind a reverse proxy on this
// machine (nginx in front of the hub) as well: the address that proxy saw,
// the last one in X-Forwarded-For (earlier ones are the client's own say).
func clientIP(r *http.Request) string {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if p := net.ParseIP(ip); p != nil && p.IsLoopback() {
		hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		if f := strings.TrimSpace(hops[len(hops)-1]); f != "" {
			return f
		}
	}
	return ip
}

// offerDirect (hub): a direct way to a computer's terminals, if the browser
// asking runs on that computer (same address as its node, which serves a
// local UI). {} otherwise: then everything goes through the hub as before.
func (s *Server) offerDirect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Machine string `json:"machine"`
	}
	if !decode(w, r, &body) {
		return
	}
	m, err := s.herdr.machine(body.Machine)
	if body.Machine == "" || err != nil || m.ui == "" || m.ip == "" || m.ip != clientIP(r) || m.client == nil {
		writeJSON(w, 200, map[string]any{})
		return
	}
	var got struct {
		Token string `json:"token"`
		TTL   int    `json:"ttl"`
	}
	if nodeJSON(r.Context(), m.client, "POST", "/api/direct/token", nil, &got) != nil || got.Token == "" {
		writeJSON(w, 200, map[string]any{}) // a node from before direct terminals
		return
	}
	writeJSON(w, 200, map[string]any{"url": m.ui + "/direct", "token": got.Token, "ttl": got.TTL})
}
