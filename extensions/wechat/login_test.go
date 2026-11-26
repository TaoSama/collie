package wechat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/TaoSama/collie/internal/extproto"
)

// loginServer answers the login calls with a scripted list of statuses and
// records the verify codes it was given.
type loginServer struct {
	mu       sync.Mutex
	statuses []map[string]any
	codes    []string
	issued   int
}

func newLoginExtension(t *testing.T, statuses ...map[string]any) (*extension, *loginServer) {
	t.Helper()
	ls := &loginServer{statuses: statuses}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ls.mu.Lock()
		defer ls.mu.Unlock()
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			ls.issued++
			_ = json.NewEncoder(w).Encode(map[string]any{"qrcode": "c", "qrcode_img_content": "https://127.0.0.1/qr"})
		case "/ilink/bot/get_qrcode_status":
			ls.codes = append(ls.codes, r.URL.Query().Get("verify_code"))
			next := map[string]any{"status": "wait"}
			if len(ls.statuses) > 0 {
				next, ls.statuses = ls.statuses[0], ls.statuses[1:]
			}
			_ = json.NewEncoder(w).Encode(next)
		}
	}))
	t.Cleanup(srv.Close)
	x := newExtension(options{http: srv.Client(), authURL: srv.URL, trustHost: func(h string) bool { return h == "127.0.0.1" }})
	return x, ls
}

// from: extensions/wechat/README.md: the verify-code step asks for a secret and passes it to get_qrcode_status
func TestLoginVerifyCodeStep(t *testing.T) {
	x, ls := newLoginExtension(t, map[string]any{"status": "need_verifycode"}, map[string]any{"status": "need_verifycode"})
	ctx := context.Background()
	step, err := x.loginStep(ctx, "", "")
	mustNoErr(t, err)
	if step.Kind != extproto.LoginQR {
		t.Fatalf("first step %+v", step)
	}
	step, err = x.loginStep(ctx, step.Step, "")
	mustNoErr(t, err)
	if step.Kind != extproto.LoginSecret || step.Step != stepCode || step.Label == "" || step.Error != "" {
		t.Fatalf("code step %+v", step)
	}
	// Not digits: asked again without calling WeChat.
	step, err = x.loginStep(ctx, stepCode, "abc")
	mustNoErr(t, err)
	if step.Kind != extproto.LoginSecret || len(ls.codes) != 1 {
		t.Fatalf("invalid code step %+v, %d polls", step, len(ls.codes))
	}
	step, err = x.loginStep(ctx, stepCode, " 1234 ")
	mustNoErr(t, err)
	if step.Error != errWrongCode || ls.codes[len(ls.codes)-1] != "1234" {
		t.Fatalf("wrong code step %+v, codes %v", step, ls.codes)
	}
}

// from: extensions/wechat/README.md: an expired QR code is renewed at most three times
func TestLoginQRRenewedThreeTimes(t *testing.T) {
	expired := map[string]any{"status": "expired"}
	x, ls := newLoginExtension(t, expired, expired, expired, expired)
	ctx := context.Background()
	step, err := x.loginStep(ctx, "", "")
	mustNoErr(t, err)
	for i := 0; i < qrMaxRenewals; i++ {
		if step, err = x.loginStep(ctx, step.Step, ""); err != nil || step.Kind != extproto.LoginQR {
			t.Fatalf("renewal %d: %+v %v", i, step, err)
		}
	}
	if _, err := x.loginStep(ctx, stepQR, ""); err == nil {
		t.Fatal("a fourth expiry did not end the login")
	}
	if ls.issued != 1+qrMaxRenewals {
		t.Fatalf("%d codes issued", ls.issued)
	}
}

// from: extensions/wechat/README.md: delivery reasons reach Collie as sentences
func TestDeliveryReasonsAreSentences(t *testing.T) {
	for _, c := range []struct{ state, code, want string }{
		{DeliveryDeferred, DeferWindowExpired, "微信回复窗口已过期，等你在微信发一条消息后送达"},
		{DeliveryDeferred, "rejected", reasonTexts["rejected"]},
		{DeliveryUncertain, "", stateTexts[DeliveryUncertain]},
		{DeliveryExpired, reasonSuperseded, reasonTexts[reasonSuperseded]},
		{DeliverySent, "", ""},
	} {
		if got := humanReason(c.state, c.code); got != c.want {
			t.Errorf("humanReason(%s, %s) = %q, want %q", c.state, c.code, got, c.want)
		}
	}
}
