package collie

import (
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A module whose 「由 hub 保管登录」 is on opens through the hub at
// https://<id>.<wb domain>/ (-wb-domain), not at its own address. The hub
// keeps the cookies its site sets (workbench_jar.go) and sends them on, so a
// login made on one device holds on all of them.
//
// Each module gets a host of its own: it is a different origin from Collie
// and from the other modules, so the site's scripts reach neither Collie's
// API (whose cookie is for Collie's host only) nor another module's login.
// The way in is a ticket: the Collie page asks for one (POST /api/wb/enter),
// the frame opens /__collie/enter?t=… on the module's host, and the hub
// swaps the ticket for a session cookie of that host. The wb domain must be
// on the same site as Collie (c.example.com and *.wb.example.com): the
// session cookie is SameSite=Lax, which an embedded frame of another site
// does not get.

const (
	wbEnterPath    = "/__collie/enter"
	wbSessCookie   = "collie_wb"
	wbTicketTTL    = time.Minute
	wbSessionTTL   = 12 * time.Hour
	wbUpstreamWait = 30 * time.Second
)

type wbTicket struct {
	module, device, to, parent string
	expires                    time.Time
}

// wbSession is a device let in to one module. device is the hash of its
// token ("" for the master token): removing the device ends it.
type wbSession struct {
	module, device, parent string
	expires                time.Time
}

// wbGate holds the tickets and sessions, in memory: after a restart the page
// asks for a new ticket when it opens the module again.
type wbGate struct {
	mu       sync.Mutex
	tickets  map[string]wbTicket
	sessions map[string]wbSession
	upstream http.RoundTripper
}

func newWBGate() *wbGate {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = wbUpstreamWait
	return &wbGate{tickets: map[string]wbTicket{}, sessions: map[string]wbSession{}, upstream: t}
}

func (g *wbGate) newTicket(t wbTicket) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	for k, v := range g.tickets {
		if now.After(v.expires) {
			delete(g.tickets, k)
		}
	}
	k := randomToken()
	t.expires = now.Add(wbTicketTTL)
	g.tickets[k] = t
	return k
}

// redeem swaps a ticket for module for a session, once. The session the
// browser already had there (old) ends: each open would otherwise leave one.
func (g *wbGate) redeem(ticket, module, old string) (string, wbTicket, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.tickets[ticket]
	delete(g.tickets, ticket)
	now := time.Now()
	if !ok || now.After(t.expires) || t.module != module {
		return "", wbTicket{}, false
	}
	if o, ok := g.sessions[old]; ok && o.module == module {
		delete(g.sessions, old)
	}
	for k, v := range g.sessions {
		if now.After(v.expires) {
			delete(g.sessions, k)
		}
	}
	k := randomToken()
	g.sessions[k] = wbSession{module: t.module, device: t.device, parent: t.parent, expires: now.Add(wbSessionTTL)}
	return k, t, true
}

func (g *wbGate) session(k, module string) (wbSession, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.sessions[k]
	if !ok || s.module != module || time.Now().After(s.expires) {
		return wbSession{}, false
	}
	return s, true
}

// wbHostModule is the module a request's host names, if it is one of ours.
func (s *Server) wbHostModule(r *http.Request) (string, bool) {
	if s.wbDomain == "" {
		return "", false
	}
	h := r.Host
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	id, ok := strings.CutSuffix(strings.ToLower(h), "."+s.wbDomain)
	return id, ok && wbIDRe.MatchString(id)
}

// wbProxyURL is where module id opens, on the scheme and port of request r
// to Collie.
func (s *Server) wbProxyURL(r *http.Request, id string) string {
	port := ""
	if _, p, err := net.SplitHostPort(r.Host); err == nil {
		port = ":" + p
	}
	return requestScheme(r) + "://" + id + "." + s.wbDomain + port
}

func requestScheme(r *http.Request) string {
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		return "https"
	}
	return "http"
}

// wbEnter gives the page a ticket into a module kept by the hub.
func (s *Server) wbEnter(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &body) {
		return
	}
	if s.wbDomain == "" {
		writeJSON(w, 409, map[string]any{"error": "hub 没有设置工作台代理域名（-wb-domain）"})
		return
	}
	m, ok := s.sup.wb.module(body.ID)
	if !ok || m.Kind != wbKindWeb || !m.HubLogin {
		writeJSON(w, 404, map[string]any{"error": "这个模块没有打开「由 hub 保管登录」"})
		return
	}
	master, d := s.identity(r)
	device := ""
	if !master {
		if d == nil || d.Kind != "browser" {
			writeJSON(w, 403, map[string]any{"error": "只有已登录的浏览器能打开"})
			return
		}
		device = d.Hash
	}
	u, err := url.Parse(m.URL)
	if err != nil {
		fail(w, err)
		return
	}
	parent := r.Header.Get("Origin")
	if parent == "" || parent == "null" {
		parent = requestScheme(r) + "://" + r.Host
	}
	t := s.wbGate.newTicket(wbTicket{module: m.ID, device: device, to: u.RequestURI(), parent: parent})
	writeJSON(w, 200, map[string]any{"url": s.wbProxyURL(r, m.ID) + wbEnterPath + "?t=" + t})
}

// wbStillIn: the device behind a session is still paired.
func (s *Server) wbStillIn(device string) bool {
	if device == "" || s.auths == nil {
		return device == ""
	}
	return s.auths.has(device)
}

func wbRefuse(w http.ResponseWriter, code int, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	w.Write([]byte(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Collie</title><p style="font:15px system-ui;margin:2em;color:#666">` + text + `</p>`))
}

// serveWBProxy answers every request to a module's host.
func (s *Server) serveWBProxy(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !s.workbenchOn() {
		wbRefuse(w, 409, wbOffText)
		return
	}
	m, ok := s.sup.wb.module(id)
	if !ok || m.Kind != wbKindWeb || !m.HubLogin {
		wbRefuse(w, 404, "没有这个模块，或它没有打开「由 hub 保管登录」。")
		return
	}
	secure := requestScheme(r) == "https"
	if r.URL.Path == wbEnterPath {
		old := ""
		if c, err := r.Cookie(wbSessCookie); err == nil {
			old = c.Value
		}
		k, t, ok := s.wbGate.redeem(r.URL.Query().Get("t"), id, old)
		if !ok {
			wbRefuse(w, 403, "链接已失效：回到 Collie 的工作台重新打开这个模块。")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: wbSessCookie, Value: k, Path: "/", HttpOnly: true, Secure: secure,
			SameSite: http.SameSiteLaxMode, MaxAge: int(wbSessionTTL / time.Second)})
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, t.to, http.StatusSeeOther)
		return
	}
	c, err := r.Cookie(wbSessCookie)
	if err != nil {
		wbRefuse(w, 401, "请从 Collie 的工作台打开这个模块。")
		return
	}
	sess, ok := s.wbGate.session(c.Value, id)
	if !ok || !s.wbStillIn(sess.device) {
		wbRefuse(w, 401, "登录已过期：回到 Collie 的工作台重新打开这个模块。")
		return
	}
	self := s.wbProxyURL(r, id)
	// The jar signs whatever comes in with the session cookie, so only the
	// module's own pages may use it: not another module's scripts (same
	// site, so they get the cookie too), nor another site's form.
	if !cookieAllowed(r) || !wbOwnRequest(r, self) {
		wbRefuse(w, 403, "这个请求不是从模块自己的页面发出的。")
		return
	}
	up, err := url.Parse(m.URL)
	if err != nil {
		wbRefuse(w, 502, "模块的网址无效。")
		return
	}
	upOrigin := up.Scheme + "://" + up.Host
	rp := &httputil.ReverseProxy{
		Transport:     s.wbGate.upstream,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: up.Scheme, Host: up.Host})
			out := pr.Out.Header
			for _, h := range []string{"Forwarded", "X-Real-Ip"} {
				out.Del(h)
			}
			if o := out.Get("Origin"); o == self {
				out.Set("Origin", upOrigin)
			} else if o != "" {
				out.Del("Origin")
			}
			if ref := out.Get("Referer"); ref != "" {
				if rest, ok := strings.CutPrefix(ref, self); ok {
					out.Set("Referer", upOrigin+rest)
				} else {
					out.Del("Referer")
				}
			}
			s.wbRequestCookies(pr, m.ID, up.Scheme == "https")
		},
		ModifyResponse: func(resp *http.Response) error {
			s.wbResponseCookies(resp, m.ID, r.URL.Path)
			h := resp.Header
			if loc := h.Get("Location"); loc != "" {
				if lu, err := url.Parse(loc); err == nil && lu.Host != "" && strings.EqualFold(lu.Host, up.Host) {
					lu.Scheme, lu.Host = "", ""
					h.Set("Location", self+lu.String())
				}
			}
			h.Del("X-Frame-Options")
			var keep []string
			for _, v := range h.Values("Content-Security-Policy") {
				if v = dropCSPDirective(v, "frame-ancestors"); v != "" {
					keep = append(keep, v)
				}
			}
			h["Content-Security-Policy"] = append(keep, "frame-ancestors "+sess.parent)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("workbench proxy %s: %v", m.ID, err)
			wbRefuse(w, 502, "打不开 "+up.Host+"。")
		},
	}
	rp.ServeHTTP(w, r)
}

// wbOwnRequest: a request a page of the module made, or a navigation to it
// (the Collie page opening the frame, a link). Browsers say where a request
// comes from in Sec-Fetch-Site; a WebSocket also carries its Origin.
func wbOwnRequest(r *http.Request, self string) bool {
	if r.Header.Get("Upgrade") != "" {
		if o := r.Header.Get("Origin"); o != "" && o != self {
			return false
		}
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return r.Header.Get("Sec-Fetch-Mode") == "navigate"
}

// wbRequestCookies sends the jar's cookies, and the cookies the page's own
// scripts set, but never ours. A cookie scripts can read is the browser's
// when it has one: the page may have changed it since.
func (s *Server) wbRequestCookies(pr *httputil.ProxyRequest, id string, secure bool) {
	jar, err := s.sup.wb.cookiesFor(id, pr.In.URL.Path, secure)
	if err != nil {
		log.Printf("workbench jar %s: %v", id, err)
	}
	have := map[string]bool{}
	var parts []string
	for _, c := range pr.In.Cookies() {
		if c.Name != wbSessCookie && c.Name != cookieName {
			have[c.Name] = true
			parts = append(parts, c.Name+"="+c.Value)
		}
	}
	for _, c := range jar {
		if c.HTTPOnly || !have[c.Name] {
			parts = append(parts, c.Name+"="+c.Value)
		}
	}
	pr.Out.Header.Del("Cookie")
	if len(parts) > 0 {
		pr.Out.Header.Set("Cookie", strings.Join(parts, "; "))
	}
}

// wbResponseCookies keeps the site's cookies in the jar. Those its scripts
// may read (not HttpOnly) go on to the browser as well, for this host only.
func (s *Server) wbResponseCookies(resp *http.Response, id, reqPath string) {
	set := resp.Cookies()
	resp.Header.Del("Set-Cookie")
	if err := s.sup.wb.storeCookies(id, reqPath, set); err != nil {
		log.Printf("workbench jar %s: %v", id, err)
	}
	for _, c := range set {
		if c.HttpOnly || c.Name == wbSessCookie {
			continue
		}
		c.Domain = ""
		if v := c.String(); v != "" {
			resp.Header.Add("Set-Cookie", v)
		}
	}
}

// dropCSPDirective removes one directive from a CSP header value.
func dropCSPDirective(csp, name string) string {
	var keep []string
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if f := strings.Fields(d); strings.EqualFold(f[0], name) {
			continue
		}
		keep = append(keep, d)
	}
	return strings.Join(keep, "; ")
}
