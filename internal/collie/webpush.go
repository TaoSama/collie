package collie

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Web Push (RFC 8030) with VAPID (RFC 8292) and the aes128gcm payload
// encryption of RFC 8291: what lets the hub reach a browser whose page is
// closed, or an installed app in the background. The browser's push service
// (FCM for Chrome, Mozilla's, Apple's) holds the message until the device
// takes it; the service worker shows it (sw.js).

var b64 = base64.RawURLEncoding

// vapidKey is the hub's own key pair: browsers subscribe with its public half
// and only accept pushes signed by it.
type vapidKey struct {
	priv *ecdsa.PrivateKey
	pub  []byte // uncompressed P-256 point, as browsers take it
}

func newVAPIDKey() (*vapidKey, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return vapidFrom(k)
}

func vapidFrom(k *ecdsa.PrivateKey) (*vapidKey, error) {
	pub, err := k.PublicKey.ECDH()
	if err != nil {
		return nil, err
	}
	return &vapidKey{priv: k, pub: pub.Bytes()}, nil
}

// marshal / parseVAPIDKey keep the key as PKCS #8, base64url.
func (v *vapidKey) marshal() (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(v.priv)
	if err != nil {
		return "", err
	}
	return b64.EncodeToString(der), nil
}

func parseVAPIDKey(s string) (*vapidKey, error) {
	der, err := b64.DecodeString(s)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok || ek.Curve != elliptic.P256() {
		return nil, errors.New("vapid key is not P-256")
	}
	return vapidFrom(ek)
}

// publicKey is what pushManager.subscribe takes as applicationServerKey.
func (v *vapidKey) publicKey() string { return b64.EncodeToString(v.pub) }

// vapidJWTFor is how long a push's signature is good for (at most 24 hours).
const vapidJWTFor = 12 * time.Hour

// authorization signs for the push service at endpoint (its origin is the
// audience). subject is how the service can reach whoever runs the hub.
func (v *vapidKey) authorization(endpoint, subject string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("bad push endpoint %q", endpoint)
	}
	head := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, _ := json.Marshal(map[string]any{"aud": u.Scheme + "://" + u.Host, "exp": now.Add(vapidJWTFor).Unix(), "sub": subject})
	signing := head + "." + b64.EncodeToString(claims)
	h := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, v.priv, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64) // JWS ES256: r and s, 32 bytes each
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signing + "." + b64.EncodeToString(sig) + ", k=" + v.publicKey(), nil
}

// pushRecordSize is the record size the payload goes out with; one record
// holds any payload the push services accept (about 4 KB).
const pushRecordSize = 4096

// encryptPush seals plaintext for the browser that subscribed with p256dh
// (its public key) and auth (its secret), as one aes128gcm record. salt and
// the sender's key are fresh for every message; tests pass fixed ones.
func encryptPush(plaintext, p256dh, auth, salt []byte, sender *ecdh.PrivateKey) ([]byte, error) {
	curve := ecdh.P256()
	ua, err := curve.NewPublicKey(p256dh)
	if err != nil {
		return nil, fmt.Errorf("subscription key: %w", err)
	}
	if len(auth) != 16 {
		return nil, errors.New("subscription auth secret must be 16 bytes")
	}
	if sender == nil {
		if sender, err = curve.GenerateKey(rand.Reader); err != nil {
			return nil, err
		}
	}
	if salt == nil {
		salt = make([]byte, 16)
		if _, err := io.ReadFull(rand.Reader, salt); err != nil {
			return nil, err
		}
	}
	shared, err := sender.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPub := sender.PublicKey().Bytes()
	keyInfo := append(append([]byte("WebPush: info\x00"), p256dh...), asPub...)
	ikm, err := hkdf.Key(sha256.New, shared, auth, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(plaintext)+1+gcm.Overhead() > pushRecordSize {
		return nil, errors.New("push payload too large")
	}
	record := append(append([]byte{}, plaintext...), 2) // 2: the last (only) record, no padding
	var out bytes.Buffer
	out.Write(salt)
	binary.Write(&out, binary.BigEndian, uint32(pushRecordSize))
	out.WriteByte(byte(len(asPub)))
	out.Write(asPub)
	out.Write(gcm.Seal(nil, nonce, record, nil))
	return out.Bytes(), nil
}

// pushMessage is one push to one subscription.
type pushMessage struct {
	payload []byte
	topic   string // a later push with the same topic replaces this one if it hasn't been delivered
	ttl     time.Duration
}

// pushTTL: how long a push service keeps a message for a device that is off.
// A decision older than this has usually been settled some other way.
const pushTTL = time.Hour

// sendPush delivers msg to sub and returns the push service's status code.
func sendPush(ctx context.Context, client *http.Client, key *vapidKey, subject string, sub pushSub, msg pushMessage) (int, error) {
	p256dh, err := b64.DecodeString(sub.P256dh)
	if err != nil {
		return 0, fmt.Errorf("subscription key: %w", err)
	}
	auth, err := b64.DecodeString(sub.Auth)
	if err != nil {
		return 0, fmt.Errorf("subscription auth: %w", err)
	}
	body, err := encryptPush(msg.payload, p256dh, auth, nil, nil)
	if err != nil {
		return 0, err
	}
	authz, err := key.authorization(sub.Endpoint, subject, time.Now())
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	ttl := msg.ttl
	if ttl <= 0 {
		ttl = pushTTL
	}
	req.Header.Set("Authorization", authz)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(int(ttl.Seconds())))
	req.Header.Set("Urgency", "high") // wakes a phone that is dozing
	if msg.topic != "" {
		req.Header.Set("Topic", msg.topic)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return resp.StatusCode, fmt.Errorf("push service: %s %s", resp.Status, bytes.TrimSpace(b))
	}
	return resp.StatusCode, nil
}

// pushTopic turns a notification's tag into a Topic header: at most 32
// characters of the URL-safe base64 alphabet.
func pushTopic(tag string) string {
	h := sha256.Sum256([]byte(tag))
	return b64.EncodeToString(h[:])[:32]
}
