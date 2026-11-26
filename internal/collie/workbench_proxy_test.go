package collie

import (
	"io"
	"net/http"
	"net/http/httptest"
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
	enter := func(id string) string {
		w := do("POST", "/api/wb/enter", "c.example.com", map[string]string{"Authorization": "Bearer tok", "Origin": "https://c.example.com", "body": `{"id":"` + id + `"}`})
		if w.Code != 200 {
			t.Fatalf("enter %s = %d %s", id, w.Code, w.Body)
		}
		u := strings.TrimPrefix(strings.SplitN(w.Body.String(), `"url":"`, 2)[1], "http://gw.wb.example.com")
		return strings.SplitN(u, `"`, 2)[0]
	}
	const proxy = "gw.wb.example.com"

	// The ticket opens a session once, and lands on the module's page.
	ticket := enter("gw")
	w := do("GET", ticket, proxy, nil)
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
	if w := do("GET", ticket, proxy, nil); w.Code != 403 {
		t.Fatalf("ticket used twice = %d", w.Code)
	}
	cookie := wbSessCookie + "=" + sess

	// Logging in: the login stays on the hub, frames are allowed for Collie only,
	// redirects to the site come back through the proxy.
	w = do("GET", "/login", proxy, map[string]string{"Cookie": cookie})
	if w.Code != 302 || w.Header().Get("Location") != "http://gw.wb.example.com/home?a=1" {
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
	sp.wb.jars = map[string]map[string]wbCookie{} // read back from disk
	w = do("GET", enter("gw"), proxy, nil)
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
	srv.wbGate.sessions["gone"] = wbSession{module: "gw", device: "no-such-device", expires: time.Now().Add(time.Hour)}
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
	do("GET", "/login", proxy, map[string]string{"Cookie": cookie})
	mods[0].HubLogin = false
	if err := sp.wb.save(wbConfig{Groups: []WBGroup{{ID: "fav", Title: "收藏"}}, Modules: mods}, nil); err != nil {
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
