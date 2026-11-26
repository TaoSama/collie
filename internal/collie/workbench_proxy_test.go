package collie

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// A module kept by the hub: one login through the proxy, used by every
// device; the site's scripts get neither Collie nor another module.
func TestWorkbenchProxyKeepsTheLogin(t *testing.T) {
	var got http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "abc", Path: "/", HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "csrf", Value: "x", Path: "/"})
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
			w.Header().Set("Location", "http://"+r.Host+"/home?a=1")
			w.WriteHeader(302)
		case "/logout":
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "", Path: "/", MaxAge: -1})
		default:
			got = r.Header.Clone()
			io.WriteString(w, "page")
		}
	}))
	defer up.Close()

	sp, h, dir := newExtTestHub(t)
	sp.wb = newWorkbench(sp.db, dir, sp.routerDir())
	turnOnWorkbench(t, h)
	srv := sp.s
	srv.token, srv.auths = "tok", newAuth(sp.db, "tok")
	srv.wbDomain, srv.wbGate = "wb.example.com", newWBGate()
	srv.web = fstest.MapFS{"index.html": {Data: []byte("ok")}}
	mods := []WBModule{
		{ID: "gw", Title: "gw", Group: "fav", Kind: wbKindWeb, URL: up.URL + "/app?x=1", Share: wbShareOff, HubLogin: true},
		{ID: "plain", Title: "plain", Group: "fav", Kind: wbKindWeb, URL: up.URL + "/", Share: wbShareOff},
	}
	if err := sp.wb.save(wbConfig{Groups: []WBGroup{{ID: "fav", Title: "收藏"}}, Modules: mods}, nil); err != nil {
		t.Fatal(err)
	}
	handler := srv.Routes()
	do := func(method, target, host string, hdr map[string]string) *httptest.ResponseRecorder {
		var body io.Reader
		if b, ok := hdr["body"]; ok {
			body = strings.NewReader(b)
		}
		r := httptest.NewRequest(method, target, body)
		r.Host = host
		for k, v := range hdr {
			if k != "body" {
				r.Header.Set(k, v)
			}
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	enter := func(id string) *url.URL {
		w := do("POST", "/api/wb/enter", "c.example.com", map[string]string{"Authorization": "Bearer tok", "Origin": "https://c.example.com", "body": `{"id":"` + id + `"}`})
		if w.Code != 200 {
			t.Fatalf("enter %s = %d %s", id, w.Code, w.Body)
		}
		var body struct{ URL string }
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(body.URL)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}

	// The ticket opens a session once, and lands on the module's page.
	ticket := enter("gw")
	proxy := ticket.Host
	w := do("GET", ticket.RequestURI(), proxy, nil)
	if w.Code != 303 || w.Header().Get("Location") != "/app?x=1" {
		t.Fatalf("enter = %d %q", w.Code, w.Header().Get("Location"))
	}
	sess := ""
	for _, c := range w.Result().Cookies() {
		if c.Name == wbSessCookie && c.HttpOnly && c.SameSite == http.SameSiteLaxMode && c.Domain == "" {
			sess = c.Value
		}
	}
	if sess == "" {
		t.Fatalf("no session cookie: %v", w.Header())
	}
	if w := do("GET", ticket.RequestURI(), proxy, nil); w.Code != 403 {
		t.Fatalf("ticket used twice = %d", w.Code)
	}
	cookie := wbSessCookie + "=" + sess

	// Logging in: the login stays on the hub, frames are allowed for Collie only,
	// redirects to the site come back through the proxy.
	w = do("GET", "/login", proxy, map[string]string{"Cookie": cookie})
	if w.Code != 302 || w.Header().Get("Location") != "http://"+proxy+"/home?a=1" {
		t.Fatalf("login = %d %q", w.Code, w.Header().Get("Location"))
	}
	if sc := w.Header().Values("Set-Cookie"); len(sc) != 1 || !strings.HasPrefix(sc[0], "csrf=x") {
		t.Fatalf("Set-Cookie to the browser = %q, want only the one scripts read", sc)
	}
	if w.Header().Get("X-Frame-Options") != "" {
		t.Fatal("X-Frame-Options kept")
	}
	if csp := strings.Join(w.Header().Values("Content-Security-Policy"), " | "); csp != "default-src 'self' | frame-ancestors https://c.example.com" {
		t.Fatalf("CSP = %q", csp)
	}
	if n := sp.wb.jarCount("gw"); n != 2 {
		t.Fatalf("jar holds %d cookies, want 2", n)
	}
	if fi, err := os.Stat(sp.wb.jarPath("gw")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("jar file: %v %v", fi, err)
	}

	// Another device, its own session: the site sees the login, and the
	// page's own cookies, but not ours; Origin and Referer are the site's.
	reopened := newWorkbench(sp.db, dir, sp.routerDir())
	_, restored, err := reopened.proxyModule("gw", "")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Host+"."+srv.wbDomain != proxy || reopened.jarCount("gw") != 2 {
		t.Fatal("proxy host or cookies were not restored from disk")
	}
	w = do("GET", enter("gw").RequestURI(), proxy, nil)
	cookie2 := w.Result().Cookies()[0].Name + "=" + w.Result().Cookies()[0].Value
	w = do("POST", "/api/x", proxy, map[string]string{"Cookie": cookie2 + "; js=1", "Origin": "http://" + proxy, "Referer": "http://" + proxy + "/app", "Sec-Fetch-Site": "same-origin"})
	if w.Code != 200 {
		t.Fatalf("POST from the page = %d %s", w.Code, w.Body)
	}
	if c := got.Get("Cookie"); !strings.Contains(c, "sid=abc") || !strings.Contains(c, "csrf=x") || !strings.Contains(c, "js=1") || strings.Contains(c, wbSessCookie) {
		t.Fatalf("upstream Cookie = %q", c)
	}
	if got.Get("Origin") != up.URL || got.Get("Referer") != up.URL+"/app" || got.Get("X-Forwarded-For") != "" {
		t.Fatalf("upstream Origin %q Referer %q XFF %q", got.Get("Origin"), got.Get("Referer"), got.Get("X-Forwarded-For"))
	}

	// Who may not use it.
	for _, c := range []struct {
		name, method, host string
		hdr                map[string]string
		want               int
	}{
		{"no session", "GET", proxy, nil, 401},
		{"another module's host", "GET", "plain.wb.example.com", map[string]string{"Cookie": cookie}, 404},
		{"a write from another module", "POST", proxy, map[string]string{"Cookie": cookie, "Origin": "http://plain.wb.example.com"}, 403},
		{"another module's script reading", "GET", proxy, map[string]string{"Cookie": cookie, "Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "cors"}, 403},
		{"a WebSocket from elsewhere", "GET", proxy, map[string]string{"Cookie": cookie, "Upgrade": "websocket", "Origin": "https://evil.example"}, 403},
		{"the Collie page opening the frame", "GET", proxy, map[string]string{"Cookie": cookie, "Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "navigate"}, 200},
		{"Collie's API on the module's host", "GET", proxy, map[string]string{"Authorization": "Bearer tok"}, 401},
	} {
		if w := do(c.method, "/api/state", c.host, c.hdr); w.Code != c.want {
			t.Errorf("%s = %d, want %d", c.name, w.Code, c.want)
		}
	}
	if w := do("POST", "/api/wb/enter", "c.example.com", map[string]string{"Authorization": "Bearer tok", "body": `{"id":"plain"}`}); w.Code != 404 {
		t.Errorf("ticket for a module the hub doesn't keep = %d", w.Code)
	}

	// A session ends with its device.
	srv.wbGate.sessions["gone"] = wbSession{host: strings.TrimSuffix(proxy, "."+srv.wbDomain), device: "no-such-device", ctx: context.Background()}
	if w := do("GET", "/", proxy, map[string]string{"Cookie": wbSessCookie + "=gone"}); w.Code != 401 {
		t.Errorf("session of a removed device = %d", w.Code)
	}

	// The site logging out drops the cookie; 清除登录态 drops them all;
	// turning hub_login off forgets the login.
	do("GET", "/logout", proxy, map[string]string{"Cookie": cookie})
	if n := sp.wb.jarCount("gw"); n != 1 {
		t.Fatalf("after the site's logout the jar holds %d, want 1", n)
	}
	if w := do("POST", "/api/wb/logout", "c.example.com", map[string]string{"Authorization": "Bearer tok", "body": `{"id":"gw"}`}); w.Code != 200 || sp.wb.jarCount("gw") != 0 {
		t.Fatalf("logout = %d, jar %d", w.Code, sp.wb.jarCount("gw"))
	}
	newTicket := enter("gw")
	if newTicket.Host == proxy {
		t.Fatal("clearing the login must give the module a fresh proxy host")
	}
	if w := do("GET", "/", proxy, nil); w.Code != 404 {
		t.Fatalf("retired proxy host = %d, want 404", w.Code)
	}
	proxy = newTicket.Host
	w = do("GET", newTicket.RequestURI(), proxy, nil)
	cookie = w.Header().Get("Set-Cookie")
	if w.Code != 303 || len(w.Result().Cookies()) != 1 {
		t.Fatalf("fresh session = %d, cookies %q", w.Code, cookie)
	}
	c := w.Result().Cookies()[0]
	cookie = c.Name + "=" + c.Value
	if w := do("GET", "/login", proxy, map[string]string{"Cookie": cookie}); w.Code != 302 {
		t.Fatalf("login on the fresh host = %d", w.Code)
	}
	cfg := sp.wb.config()
	cfg.Modules[0].HubLogin = false
	if err := sp.wb.save(cfg, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sp.wb.jarPath("gw")); !os.IsNotExist(err) || sp.wb.jarCount("gw") != 0 {
		t.Fatalf("jar kept after hub_login off: %v", err)
	}
	if w := do("GET", "/", proxy, map[string]string{"Cookie": cookie}); w.Code != 404 {
		t.Fatalf("proxy after hub_login off = %d", w.Code)
	}
}

func TestCookieJarRules(t *testing.T) {
	for _, c := range []struct {
		req, path string
		want      bool
	}{{"/", "/", true}, {"/a/b", "/a", true}, {"/ab", "/a", false}, {"/a/", "/a/", true}, {"/a", "/a/", false}} {
		if cookiePathMatch(c.req, c.path) != c.want {
			t.Errorf("cookiePathMatch(%q, %q) != %v", c.req, c.path, c.want)
		}
	}
	if defaultCookiePath("/a/b/c") != "/a/b" || defaultCookiePath("/x") != "/" {
		t.Error("defaultCookiePath")
	}
	now := time.Now()
	a := wbCookie{Name: "s", Value: "1", Path: "/", Expires: now}
	if b := a; !sameCookie(a, wbCookie{Name: "s", Value: "1", Path: "/", Expires: now.Add(time.Minute)}) || func() bool { b.Value = "2"; return sameCookie(a, b) }() {
		t.Error("sameCookie: a renewed expiry is no change, a new value is")
	}
	if dropCSPDirective("frame-ancestors 'self'", "frame-ancestors") != "" || dropCSPDirective("a b;  FRAME-ANCESTORS x ; c", "frame-ancestors") != "a b; c" {
		t.Error("dropCSPDirective")
	}
}

func TestWorkbenchLoginOrigin(t *testing.T) {
	sp, _, dir := newExtTestHub(t)
	wb := newWorkbench(sp.db, dir, sp.routerDir())
	cfg := wb.config()
	cfg.Modules = []WBModule{{ID: "site", Title: "Site", Group: "fav", Kind: wbKindWeb, URL: "https://example.com/start", HubLogin: true}}
	save := func(rawURL string) {
		t.Helper()
		cfg := cfg
		cfg.Modules = append([]WBModule(nil), cfg.Modules...)
		cfg.Modules[0].URL = rawURL
		if err := wb.save(cfg, nil); err != nil {
			t.Fatal(err)
		}
	}
	jar := func() *wbJar {
		t.Helper()
		_, j, err := wb.proxyModule("site", "")
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	save("https://example.com/start")
	first := jar()
	cookies := []*http.Cookie{{Name: "test", Value: "1", Path: "/"}}
	if err := wb.storeCookies("site", first, "/", cookies); err != nil {
		t.Fatal(err)
	}
	save("https://EXAMPLE.COM:443/next")
	if jar().Host != first.Host || wb.jarCount("site") != 1 {
		t.Fatal("same-origin edit changed the proxy host or login")
	}
	for _, rawURL := range []string{"https://example.net/next", "https://example.net:8443/next", "http://example.net:8443/next"} {
		old := jar()
		save(rawURL)
		if jar().Host == old.Host || wb.jarCount("site") != 0 {
			t.Fatalf("origin change to %s must start with a fresh host and empty jar", rawURL)
		}
	}
}

func TestWorkbenchClearedJarIgnoresRetiredHandle(t *testing.T) {
	sp, _, dir := newExtTestHub(t)
	wb := newWorkbench(sp.db, dir, sp.routerDir())
	cfg := wb.config()
	cfg.Modules = []WBModule{{ID: "site", Title: "Site", Group: "fav", Kind: wbKindWeb, URL: "https://example.com", HubLogin: true}}
	if err := wb.save(cfg, nil); err != nil {
		t.Fatal(err)
	}
	_, old, err := wb.proxyModule("site", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := wb.clearJar("site"); err != nil {
		t.Fatal(err)
	}
	_, fresh, err := wb.proxyModule("site", "")
	if err != nil {
		t.Fatal(err)
	}
	if old.ctx.Err() != context.Canceled || fresh.ctx.Err() != nil {
		t.Fatal("clearing the jar must cancel the retired handle only")
	}
	if err := wb.storeCookies("site", old, "/", []*http.Cookie{{Name: "test", Value: "1"}}); err != nil {
		t.Fatal(err)
	}
	if n := wb.jarCount("site"); n != 0 {
		t.Fatalf("fresh jar holds %d cookies, want 0", n)
	}
}

func TestWorkbenchDeviceRevocationStopsStream(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
			close(stopped)
		case <-ctx.Done():
		}
	}))
	defer up.Close()

	sp, h, dir := newExtTestHub(t)
	sp.wb = newWorkbench(sp.db, dir, sp.routerDir())
	turnOnWorkbench(t, h)
	srv := sp.s
	srv.token, srv.auths = "tok", newAuth(sp.db, "tok")
	srv.wbDomain, srv.wbGate = "wb.example.com", newWBGate()
	code, _ := srv.auths.newCode("browser")
	device, token, err := srv.auths.redeem(code, "browser", "Test", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := sp.wb.config()
	cfg.Modules = []WBModule{{ID: "site", Title: "Site", Group: "fav", Kind: wbKindWeb, URL: up.URL, HubLogin: true}}
	if err := sp.wb.save(cfg, nil); err != nil {
		t.Fatal(err)
	}
	handler := srv.Routes()
	r := httptest.NewRequest("POST", "http://c.example.com/api/wb/enter", strings.NewReader(`{"id":"site"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	var body struct{ URL string }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 || body.URL == "" {
		t.Fatalf("enter = %d %s (%v)", w.Code, w.Body, err)
	}

	r = httptest.NewRequest("GET", body.URL, nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	cookies := w.Result().Cookies()
	if w.Code != 303 || len(cookies) != 1 {
		t.Fatalf("redeem = %d, cookies %v", w.Code, cookies)
	}

	r = httptest.NewRequest("GET", "http://"+r.Host+"/events", nil).WithContext(ctx)
	r.AddCookie(cookies[0])
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), r)
	}()
	t.Cleanup(func() { <-done })
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("stream did not start")
	}

	revoke := httptest.NewRequest("POST", "http://c.example.com/api/devices/"+device.ID+"/delete", nil)
	revoke.Header.Set("Authorization", "Bearer tok")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, revoke)
	if w.Code != 200 {
		t.Fatalf("revocation = %d %s", w.Code, w.Body)
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("revocation did not cancel the upstream stream")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("revocation did not stop the proxy handler")
	}
}
