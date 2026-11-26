package collie

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A page on another subdomain of the hub's site gets the Lax cookie sent along
// with its requests; only reads and our own page may use it.
func TestCookieNeedsOwnOriginToChangeThings(t *testing.T) {
	s := &Server{token: "secret-tok", node: true}
	h := s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	do := func(method string, hdr map[string]string, cookie bool) int {
		r := httptest.NewRequest(method, "https://c.example.com/api/x", nil)
		if cookie {
			r.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-tok"})
		}
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, c := range []struct {
		name   string
		method string
		hdr    map[string]string
		cookie bool
		want   int
	}{
		{"read without Origin", "GET", nil, true, 204},
		{"read from a sibling subdomain", "GET", map[string]string{"Origin": "https://other.example.com"}, true, 204},
		{"write from our page", "POST", map[string]string{"Origin": "https://c.example.com"}, true, 204},
		{"write, browser says same-origin", "POST", map[string]string{"Sec-Fetch-Site": "same-origin"}, true, 204},
		{"write from a sibling subdomain", "POST", map[string]string{"Origin": "https://other.example.com", "Sec-Fetch-Site": "same-site"}, true, 401},
		{"write from an opaque origin", "POST", map[string]string{"Origin": "null"}, true, 401},
		{"write without Origin", "POST", nil, true, 401},
		{"delete from a sibling subdomain", "DELETE", map[string]string{"Origin": "https://other.example.com"}, true, 401},
		{"token in Authorization, no Origin (CLI, node)", "POST", map[string]string{"Authorization": "Bearer secret-tok"}, false, 204},
		{"token in Authorization from another hub's page", "POST", map[string]string{"Authorization": "Bearer secret-tok", "Origin": "https://hub2.example.org"}, false, 204},
		{"sibling sends cookie and a wrong token", "POST", map[string]string{"Authorization": "Bearer nope", "Origin": "https://other.example.com"}, true, 401},
	} {
		if got := do(c.method, c.hdr, c.cookie); got != c.want {
			t.Errorf("%s: %s = %d, want %d", c.name, c.method, got, c.want)
		}
	}
}

func TestBrowserLogoutOrigin(t *testing.T) {
	h := (&Server{}).Routes()
	for _, c := range []struct {
		name, origin, site string
		status             int
	}{
		{"same-origin origin", "https://c.example.com", "", http.StatusOK},
		{"same-origin metadata", "", "same-origin", http.StatusOK},
		{"sibling origin", "https://wb.example.com", "same-site", http.StatusForbidden},
		{"no origin", "", "", http.StatusForbidden},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "https://c.example.com/api/logout", nil)
			r.Header.Set("Origin", c.origin)
			r.Header.Set("Sec-Fetch-Site", c.site)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.status {
				t.Fatalf("logout = %d, want %d", w.Code, c.status)
			}
			cookies := w.Result().Cookies()
			if c.status == http.StatusForbidden {
				if len(cookies) != 0 {
					t.Fatal("refused logout changed a cookie")
				}
			} else if len(cookies) != 1 || cookies[0].Name != cookieName || cookies[0].MaxAge != -1 {
				t.Fatal("logout must expire the session cookie, even without an active session")
			}
		})
	}
}
