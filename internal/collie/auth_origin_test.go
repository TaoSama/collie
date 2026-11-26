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
