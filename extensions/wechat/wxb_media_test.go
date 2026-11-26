package wechat

import (
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var (
	wxbPNG      = []byte{137, 80, 78, 71, 13, 10, 26, 10}
	wxbUploaded = map[string]any{"type": 2, "image_item": map[string]any{"mid_size": 16,
		"media": map[string]any{"encrypt_query_param": "synthetic", "aes_key": "synthetic", "encrypt_type": 1}}}
)

func wxbImage(id string) ImageAttachment {
	sum := sha256.Sum256(wxbPNG)
	return ImageAttachment{ID: id, Mime: "image/png", Size: len(wxbPNG), SHA256: hex.EncodeToString(sum[:])}
}

// wxbImageFixture is a state without an account and default image options.
type wxbImageFixture struct {
	path  string
	state *StateManager
	opts  imageReplyOptions
}

func newWxbImageFixture(t *testing.T) *wxbImageFixture {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := NewStateManager(nil, path, PolicyOptions{})
	mustNoErr(t, err)
	return &wxbImageFixture{path: path, state: s, opts: imageReplyOptions{deliveryKey: "image:delivery", images: []ImageAttachment{wxbImage("image-1")},
		getImageBytes: func(context.Context, ImageAttachment) ([]byte, error) { return wxbPNG, nil },
		uploadImage:   func(context.Context, []byte) (map[string]any, error) { return wxbUploaded, nil },
		sendImage:     func(context.Context, map[string]any, string) (string, error) { return "", nil }}}
}

func (f *wxbImageFixture) reload(t *testing.T) *StateManager {
	s, err := NewStateManager(nil, f.path, PolicyOptions{})
	mustNoErr(t, err)
	return s
}

// from: gateway/tests/wechat/media-delivery.test.ts: accepted images survive restart without downloading or uploading again
func TestWxbAcceptedImagesSurviveRestart(t *testing.T) {
	f := newWxbImageFixture(t)
	sent := 0
	o := f.opts
	o.sendImage = func(context.Context, map[string]any, string) (string, error) { sent++; return "", nil }
	_, err := deliverImages(context.Background(), f.state, testChat, o)
	mustNoErr(t, err)
	o.getImageBytes = func(context.Context, ImageAttachment) ([]byte, error) { return nil, errors.New("must not read") }
	_, err = deliverImages(context.Background(), f.reload(t), testChat, o)
	mustNoErr(t, err)
	if sent != 1 {
		t.Fatalf("sent %d", sent)
	}
}

// from: gateway/tests/wechat/media-delivery.test.ts: ambiguous image send persists in-flight before network and refuses replay after restart
func TestWxbAmbiguousImageSendInFlight(t *testing.T) {
	f := newWxbImageFixture(t)
	sent := 0
	o := f.opts
	o.sendImage = func(context.Context, map[string]any, string) (string, error) {
		sent++
		if !f.reload(t).ReplyDeliveries[o.deliveryKey].InFlight {
			t.Error("intent not on disk before the send")
		}
		return "", errors.New("network ambiguity")
	}
	if _, err := deliverImages(context.Background(), f.state, testChat, o); err == nil || !strings.Contains(err.Error(), "network ambiguity") {
		t.Fatalf("err = %v", err)
	}
	if !f.state.ReplyDeliveries[o.deliveryKey].InFlight {
		t.Fatal("not in flight")
	}
	if _, err := deliverImages(context.Background(), f.reload(t), testChat, f.opts); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("replay err = %v", err)
	}
	if sent != 1 {
		t.Fatalf("sent %d", sent)
	}
}

// from: gateway/tests/wechat/media-delivery.test.ts: explicit image rejection resumes with deterministic client ID but never repeats acknowledged images
func TestWxbImageRejectionResumes(t *testing.T) {
	f := newWxbImageFixture(t)
	var ids []string
	o := f.opts
	o.images = []ImageAttachment{wxbImage("image-1"), wxbImage("image-2")}
	o.sendImage = func(_ context.Context, _ map[string]any, id string) (string, error) {
		ids = append(ids, id)
		if len(ids) == 2 {
			return "", &SendReceiptError{ReceiptRejectedRet}
		}
		return "", nil
	}
	if _, err := deliverImages(context.Background(), f.state, testChat, o); err == nil || !strings.Contains(err.Error(), "RejectedRet") {
		t.Fatalf("err = %v", err)
	}
	o.sendImage = func(_ context.Context, _ map[string]any, id string) (string, error) {
		ids = append(ids, id)
		return "", nil
	}
	_, err := deliverImages(context.Background(), f.reload(t), testChat, o)
	mustNoErr(t, err)
	if len(ids) != 3 || ids[1] != ids[2] || ids[0] == ids[1] {
		t.Fatalf("ids = %v", ids)
	}
}

// from: gateway/tests/wechat/media-delivery.test.ts: download and upload failures never create a pending send intent
func TestWxbImageFailuresNoIntent(t *testing.T) {
	f := newWxbImageFixture(t)
	sent := 0
	o := f.opts
	o.uploadImage = func(context.Context, []byte) (map[string]any, error) { return nil, errors.New("upload failed") }
	o.sendImage = func(context.Context, map[string]any, string) (string, error) { sent++; return "", nil }
	if _, err := deliverImages(context.Background(), f.state, testChat, o); err == nil || !strings.Contains(err.Error(), "upload failed") {
		t.Fatalf("err = %v", err)
	}
	if f.state.ReplyDeliveries[o.deliveryKey].InFlight {
		t.Fatal("upload failure left an intent")
	}
	bad := f.opts
	bad.getImageBytes = func(context.Context, ImageAttachment) ([]byte, error) { return []byte("bad-image"), nil }
	if _, err := deliverImages(context.Background(), f.state, testChat, bad); err == nil || !regexp.MustCompile(`metadata|signature`).MatchString(err.Error()) {
		t.Fatalf("bad bytes err = %v", err)
	}
	if f.state.ReplyDeliveries[o.deliveryKey].InFlight {
		t.Fatal("bad bytes left an intent")
	}
	o = f.opts
	o.sendImage = func(context.Context, map[string]any, string) (string, error) { sent++; return "", nil }
	_, err := deliverImages(context.Background(), f.state, testChat, o)
	mustNoErr(t, err)
	if sent != 1 {
		t.Fatalf("sent %d", sent)
	}
}

// from: gateway/tests/wechat/media-delivery.test.ts: persisted digest includes immutable image hash and metadata rejects unsafe descriptors
func TestWxbImageDigestAndMetadata(t *testing.T) {
	f := newWxbImageFixture(t)
	_, err := deliverImages(context.Background(), f.state, testChat, f.opts)
	mustNoErr(t, err)
	changed := f.opts
	other := wxbImage("image-1")
	other.SHA256 = strings.Repeat("f", 64)
	changed.images = []ImageAttachment{other}
	if _, err := deliverImages(context.Background(), f.state, testChat, changed); err == nil || !strings.Contains(err.Error(), "content or chunk plan changed") {
		t.Fatalf("err = %v", err)
	}
	img := wxbImage("image-1")
	withSize, withMime, withHash := img, img, img
	withSize.Size, withMime.Mime, withHash.SHA256 = 0, "image/svg+xml", "bad"
	id := func(s string) ImageAttachment { i := img; i.ID = s; return i }
	// null and an extra "url" field are not representable in the typed Go list.
	for i, value := range [][]ImageAttachment{{img, img}, {withSize}, {withMime}, {withHash}, {img, id("2"), id("3"), id("4")}} {
		if err := validateImageAttachments(value); err == nil || !strings.Contains(err.Error(), "Invalid") {
			t.Fatalf("case %d accepted: %v", i, err)
		}
	}
}

// from: gateway/tests/wechat/media-delivery.test.ts: ignored cancellation is bounded before upload and during uncertain send
func TestWxbImageCancellationBounded(t *testing.T) {
	for _, phase := range []string{"download", "send"} {
		t.Run(phase, func(t *testing.T) {
			f := newWxbImageFixture(t)
			ctx, cancel := context.WithTimeoutCause(context.Background(), 15*time.Millisecond, errors.New("synthetic deadline"))
			defer cancel()
			o := f.opts
			// Go dependencies end with their context; the deadline is what bounds them.
			wait := func(ctx context.Context) error { <-ctx.Done(); return context.Cause(ctx) }
			if phase == "download" {
				o.getImageBytes = func(ctx context.Context, _ ImageAttachment) ([]byte, error) { return nil, wait(ctx) }
			} else {
				o.sendImage = func(ctx context.Context, _ map[string]any, _ string) (string, error) { return "", wait(ctx) }
			}
			if _, err := deliverImages(ctx, f.state, testChat, o); err == nil || !strings.Contains(err.Error(), "synthetic deadline") {
				t.Fatalf("err = %v", err)
			}
			if got := f.state.ReplyDeliveries[o.deliveryKey].InFlight; got != (phase == "send") {
				t.Fatalf("in flight = %v", got)
			}
		})
	}
}

// wxbUploader is an image uploader over a fake request and a fake CDN.
func wxbUploader(t *testing.T, timeoutMs int64, request uploadRequest, cdn fetchFunc) (*ImageUploader, *loop) {
	l := &loop{}
	s := newTestState(t, t.TempDir(), PolicyOptions{})
	c := newTestClient(t, s, l, fakeHTTP(cdn))
	return &ImageUploader{&cdnUploader{client: c, request: request, label: KindImage, timeoutMs: timeoutMs}}, l
}

func wxbUpload(u *ImageUploader, l *loop, data []byte) (item map[string]any, err error) {
	l.do(func() { item, err = u.upload(context.Background(), testChat, data) })
	return item, err
}

var wxbUploadPNG = []byte{137, 80, 78, 71, 13, 10, 26, 10, 0, 1, 2, 3, 4, 5, 6, 7}

// from: gateway/tests/wechat/media-upload.test.ts: image upload encrypts captured Buffer and constructs the exact protocol fields
func TestWxbImageUploadProtocol(t *testing.T) {
	var auth map[string]any
	var ciphertext []byte
	data := append([]byte(nil), wxbUploadPNG...)
	u, l := wxbUploader(t, 0, func(_ context.Context, endpoint string, payload map[string]any, _ int64) (map[string]any, error) {
		if endpoint != "ilink/bot/getuploadurl" {
			t.Errorf("endpoint %s", endpoint)
		}
		auth = payload
		// The caller's buffer changes while the request is out: the upload must not see it.
		for i := range data {
			data[i] = 0
		}
		return map[string]any{"ret": 0, "upload_param": "upload+/="}, nil
	}, func(req *http.Request, body []byte) (*http.Response, error) {
		if req.URL.Scheme+"://"+req.URL.Host+req.URL.Path != cdnUploadURL || req.URL.Query().Get("encrypted_query_param") != "upload+/=" ||
			req.URL.Query().Get("filekey") != auth["filekey"] || req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/octet-stream" {
			t.Errorf("upload request %s %s", req.Method, req.URL)
		}
		ciphertext = body
		return bytesResponse(200, nil, http.Header{"X-Encrypted-Param": {"download-param"}}), nil
	})
	item, err := wxbUpload(u, l, data)
	mustNoErr(t, err)
	keys := []string{"aeskey", "filekey", "filesize", "media_type", "no_need_thumb", "rawfilemd5", "rawsize", "to_user_id"}
	if len(auth) != len(keys) {
		t.Fatalf("auth fields = %v", auth)
	}
	hex32 := regexp.MustCompile(`^[a-f0-9]{32}$`)
	sum := md5.Sum(wxbUploadPNG)
	if !hex32.MatchString(fmt.Sprint(auth["filekey"])) || !hex32.MatchString(fmt.Sprint(auth["aeskey"])) || auth["rawsize"] != len(wxbUploadPNG) ||
		auth["filesize"] != 32 || auth["rawfilemd5"] != hex.EncodeToString(sum[:]) || auth["media_type"] != 1 || auth["to_user_id"] != testChat ||
		auth["no_need_thumb"] != true {
		t.Fatalf("auth = %v", auth)
	}
	img := item["image_item"].(map[string]any)
	media := img["media"].(map[string]any)
	key, _ := base64.StdEncoding.DecodeString(media["aes_key"].(string))
	if item["type"] != 2 || img["mid_size"] != len(ciphertext) || media["encrypt_query_param"] != "download-param" || string(key) != auth["aeskey"] {
		t.Fatalf("item = %v", item)
	}
	raw, _ := hex.DecodeString(auth["aeskey"].(string))
	block, _ := aes.NewCipher(raw)
	plain := make([]byte, len(ciphertext))
	for i := 0; i < len(ciphertext); i += 16 {
		block.Decrypt(plain[i:i+16], ciphertext[i:i+16])
	}
	if string(plain[:len(wxbUploadPNG)]) != string(wxbUploadPNG) || plain[len(plain)-1] != 16 {
		t.Fatal("ciphertext does not decrypt to the captured bytes")
	}
}

// from: gateway/tests/wechat/media-upload.test.ts: full upload URL takes precedence and arbitrary destinations are rejected before fetch
func TestWxbUploadURLValidation(t *testing.T) {
	unsafe := []string{"http://novac2c.cdn.weixin.qq.com/c2c/upload", "https://evil.test/c2c/upload",
		"https://novac2c.cdn.weixin.qq.com.evil.test/c2c/upload", "https://user:secret@novac2c.cdn.weixin.qq.com/c2c/upload",
		cdnUploadURL + "/other", cdnUploadURL + "#secret", "https://novac2c.cdn.weixin.qq.com:444/c2c/upload"}
	for _, target := range unsafe {
		fetched := false
		u, l := wxbUploader(t, 0, func(context.Context, string, map[string]any, int64) (map[string]any, error) {
			return map[string]any{"upload_full_url": target, "upload_param": "fallback"}, nil
		}, func(*http.Request, []byte) (*http.Response, error) {
			fetched = true
			return nil, errors.New("unexpected")
		})
		if _, err := wxbUpload(u, l, wxbUploadPNG); err == nil || !strings.Contains(err.Error(), "URL is not allowed") || fetched {
			t.Fatalf("%s: err %v, fetched %v", target, err, fetched)
		}
	}
	full := cdnUploadURL + "?exact=from-server"
	u, l := wxbUploader(t, 0, func(context.Context, string, map[string]any, int64) (map[string]any, error) {
		return map[string]any{"upload_full_url": full, "upload_param": "unused"}, nil
	}, func(req *http.Request, _ []byte) (*http.Response, error) {
		if req.URL.String() != full {
			t.Errorf("fetched %s", req.URL)
		}
		return bytesResponse(200, nil, http.Header{"X-Encrypted-Param": {"download"}}), nil
	})
	_, err := wxbUpload(u, l, wxbUploadPNG)
	mustNoErr(t, err)
}

// from: gateway/tests/wechat/media-upload.test.ts: upload validates all supported signatures and caps bytes before any network call
func TestWxbUploadValidatesBytes(t *testing.T) {
	for data, want := range map[string]string{string(wxbUploadPNG): "image/png", string([]byte{255, 216, 255}): "image/jpeg",
		"GIF89a": "image/gif", "RIFF0000WEBP": "image/webp"} {
		if got, err := validateImageBytes([]byte(data)); err != nil || got != want {
			t.Fatalf("%q: %s %v", data, got, err)
		}
	}
	requested := false
	u, l := wxbUploader(t, 0, func(context.Context, string, map[string]any, int64) (map[string]any, error) {
		requested = true
		return map[string]any{}, nil
	}, func(*http.Request, []byte) (*http.Response, error) { return nil, errors.New("unexpected") })
	for _, data := range [][]byte{{}, make([]byte, MaxImageBytes+1), []byte("<svg>bad</svg>")} {
		if _, err := wxbUpload(u, l, data); err == nil {
			t.Fatalf("%d bytes accepted", len(data))
		}
	}
	if requested {
		t.Fatal("network called for invalid bytes")
	}
}

// from: gateway/tests/wechat/media-upload.test.ts: server failures retry only upload and require HTTP 200 with a download header
func TestWxbUploadRetries(t *testing.T) {
	attempts, grants := 0, 0
	u, l := wxbUploader(t, 0, func(context.Context, string, map[string]any, int64) (map[string]any, error) {
		grants++
		return map[string]any{"upload_param": "grant"}, nil
	}, func(*http.Request, []byte) (*http.Response, error) {
		attempts++
		switch attempts {
		case 1:
			return bytesResponse(503, nil, nil), nil
		case 2:
			return bytesResponse(200, nil, nil), nil
		}
		return bytesResponse(200, nil, http.Header{"X-Encrypted-Param": {"accepted"}}), nil
	})
	_, err := wxbUpload(u, l, wxbUploadPNG)
	mustNoErr(t, err)
	if grants != 1 || attempts != 3 {
		t.Fatalf("grants %d attempts %d", grants, attempts)
	}
	for _, status := range []int{201, 302, 400, 403} {
		count := 0
		u, l := wxbUploader(t, 0, func(context.Context, string, map[string]any, int64) (map[string]any, error) {
			return map[string]any{"upload_param": "grant"}, nil
		}, func(*http.Request, []byte) (*http.Response, error) {
			count++
			return bytesResponse(status, nil, nil), nil
		})
		if _, err := wxbUpload(u, l, wxbUploadPNG); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) || count != 1 {
			t.Fatalf("status %d: err %v, %d attempts", status, err, count)
		}
	}
}

// from: gateway/tests/wechat/media-upload.test.ts: upload deadline bounds dependencies that ignore AbortSignal without leaking request errors
func TestWxbUploadDeadline(t *testing.T) {
	// Go calls end with their context, so the fakes wait on it instead of ignoring it.
	for _, phase := range []string{"authorization", "upload"} {
		t.Run(phase, func(t *testing.T) {
			var signal atomic.Value
			u, l := wxbUploader(t, 15, func(ctx context.Context, _ string, _ map[string]any, _ int64) (map[string]any, error) {
				signal.Store(ctx)
				if phase == "authorization" {
					<-ctx.Done()
					return nil, context.Cause(ctx)
				}
				return map[string]any{"upload_param": "grant"}, nil
			}, func(req *http.Request, _ []byte) (*http.Response, error) {
				<-req.Context().Done()
				return nil, req.Context().Err()
			})
			started := time.Now()
			if _, err := wxbUpload(u, l, wxbUploadPNG); err == nil || !strings.Contains(err.Error(), "timed out") {
				t.Fatalf("err = %v", err)
			}
			if time.Since(started) > time.Second {
				t.Fatal("deadline not bounded")
			}
			if phase == "authorization" && signal.Load().(context.Context).Err() == nil {
				t.Fatal("authorization context not aborted")
			}
		})
	}
	u, l := wxbUploader(t, 0, func(context.Context, string, map[string]any, int64) (map[string]any, error) {
		return map[string]any{"upload_param": "secret-grant"}, nil
	}, func(*http.Request, []byte) (*http.Response, error) {
		return nil, errors.New("https://example.test/private?secret-grant")
	})
	if _, err := wxbUpload(u, l, wxbUploadPNG); err == nil || err.Error() != "WeChat CDN upload request failed" {
		t.Fatalf("err = %v", err)
	}
}
