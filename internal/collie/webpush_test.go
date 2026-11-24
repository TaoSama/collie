package collie

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := b64.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// RFC 8291, Appendix A: the worked example, byte for byte.
func TestEncryptPushRFC8291(t *testing.T) {
	sender, err := ecdh.P256().NewPrivateKey(mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := encryptPush([]byte("When I grow up, I want to be a watermelon"),
		mustB64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"),
		mustB64(t, "BTBZMqHH6r4Tts7J_aSIgg"), mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw"), sender)
	if err != nil {
		t.Fatal(err)
	}
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if b64.EncodeToString(got) != want {
		t.Fatalf("encrypted:\n got %s\nwant %s", b64.EncodeToString(got), want)
	}
}

func TestVAPIDKeyRoundTrip(t *testing.T) {
	k, err := newVAPIDKey()
	if err != nil {
		t.Fatal(err)
	}
	s, err := k.marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := parseVAPIDKey(s)
	if err != nil {
		t.Fatal(err)
	}
	if back.publicKey() != k.publicKey() || len(k.pub) != 65 || k.pub[0] != 4 {
		t.Fatalf("public key changed or malformed: %s vs %s", back.publicKey(), k.publicKey())
	}
}

// The VAPID header: a JWT for the endpoint's origin, signed by the key it names.
func TestVAPIDAuthorization(t *testing.T) {
	k, _ := newVAPIDKey()
	now := time.Unix(1_700_000_000, 0)
	h, err := k.authorization("https://fcm.googleapis.com/fcm/send/abc", "mailto:me@example.com", now)
	if err != nil {
		t.Fatal(err)
	}
	tok, key, ok := strings.Cut(strings.TrimPrefix(h, "vapid t="), ", k=")
	if !ok || key != k.publicKey() {
		t.Fatalf("header %q", h)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt %q", tok)
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(mustB64(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Aud != "https://fcm.googleapis.com" || claims.Sub != "mailto:me@example.com" || claims.Exp != now.Add(vapidJWTFor).Unix() {
		t.Fatalf("claims %+v", claims)
	}
	sig := mustB64(t, parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if len(sig) != 64 || !ecdsa.Verify(&k.priv.PublicKey, digest[:], r, s) {
		t.Fatal("signature does not verify")
	}
}

// What a push service is sent, and what its answer means.
func TestSendPush(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	var got *http.Request
	var body []byte
	code := 201
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(code)
	}))
	defer srv.Close()
	k, _ := newVAPIDKey()
	sub := pushSub{Endpoint: srv.URL + "/push/1", P256dh: b64.EncodeToString(ua.PublicKey().Bytes()), Auth: b64.EncodeToString(make([]byte, 16))}
	n, err := sendPush(t.Context(), srv.Client(), k, "mailto:x@y", sub, pushMessage{payload: []byte(`{"body":"hi"}`), topic: pushTopic("d:e1")})
	if err != nil || n != 201 {
		t.Fatalf("send: %d %v", n, err)
	}
	if got.Header.Get("Content-Encoding") != "aes128gcm" || got.Header.Get("TTL") != "3600" || got.Header.Get("Urgency") != "high" ||
		len(got.Header.Get("Topic")) != 32 || !strings.HasPrefix(got.Header.Get("Authorization"), "vapid t=") {
		t.Fatalf("headers %v", got.Header)
	}
	if len(body) < 86+16 || body[20] != 65 {
		t.Fatalf("body is not an aes128gcm record: %d bytes", len(body))
	}
	code = 410
	if n, err := sendPush(t.Context(), srv.Client(), k, "mailto:x@y", sub, pushMessage{payload: []byte("{}")}); n != 410 || err == nil {
		t.Fatalf("gone subscription: %d %v", n, err)
	}
}
