package wechat

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// FIPS-197 / SP 800-38A AES-128 ECB vector (first block).
const (
	wxaNISTKey    = "2b7e151628aed2a6abf7158809cf4f3c"
	wxaNISTPlain  = "6bc1bee22e409f96e93d7e117393172a"
	wxaNISTCipher = "3ad77bb40d7a3660a89ecaf32466ef97"
)

var wxaPNG = append([]byte{137, 80, 78, 71, 13, 10, 26, 10}, []byte("fixture-image")...)

func wxaEncrypt(plain []byte, keyHex string) []byte {
	key, _ := hex.DecodeString(keyHex)
	return encryptAES128ECB(plain, key)
}

func wxaDownload(t *testing.T, ref InboundMediaRef, f fetchFunc) (DownloadedMedia, error) {
	t.Helper()
	l := &loop{}
	var m DownloadedMedia
	var err error
	l.do(func() { m, err = downloadInboundMedia(context.Background(), l, fakeHTTP(f), ref) })
	return m, err
}

func wxaRespond(body []byte, header http.Header) fetchFunc {
	return func(*http.Request, []byte) (*http.Response, error) { return bytesResponse(200, body, header), nil }
}

// from: gateway/tests/wechat/cdn-download.test.ts: AES-128-ECB decryption matches the published vector with PKCS7 padding
func TestWxaAESVector(t *testing.T) {
	plain, _ := hex.DecodeString(wxaNISTPlain)
	ciphertext := wxaEncrypt(plain, wxaNISTKey)
	if hex.EncodeToString(ciphertext[:16]) != wxaNISTCipher {
		t.Fatalf("cipher %x", ciphertext[:16])
	}
	got, err := DecryptAES128ECB(ciphertext, wxaNISTKey)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("%x %v", got, err)
	}
	var mde *MediaDownloadError
	if _, err := DecryptAES128ECB(ciphertext[:20], wxaNISTKey); !errors.As(err, &mde) {
		t.Fatalf("misaligned: %v", err)
	}
	if _, err := DecryptAES128ECB(ciphertext, strings.Repeat("00", 16)); !errors.As(err, &mde) {
		t.Fatalf("wrong key fails padding: %v", err)
	}
	// The vector is also what the standard library computes.
	block, _ := aes.NewCipher(mustHex(wxaNISTKey))
	out := make([]byte, 16)
	block.Encrypt(out, plain)
	if hex.EncodeToString(out) != wxaNISTCipher {
		t.Fatal("standard AES disagrees")
	}
}

func mustHex(s string) []byte { b, _ := hex.DecodeString(s); return b }

// from: gateway/tests/wechat/cdn-download.test.ts: AES keys accept image hex, base64 raw bytes and base64 hex text
func TestWxaAESKeyForms(t *testing.T) {
	check := func(hexKey, b64 any, want string, ok bool) {
		t.Helper()
		if got, gotOK := NormalizeAESKey(hexKey, b64); got != want || gotOK != ok {
			t.Fatalf("NormalizeAESKey(%v, %v) = %q %v", hexKey, b64, got, gotOK)
		}
	}
	check(strings.ToUpper(wxaNISTKey), nil, wxaNISTKey, true)
	check(nil, base64.StdEncoding.EncodeToString(mustHex(wxaNISTKey)), wxaNISTKey, true)
	check(nil, base64.StdEncoding.EncodeToString([]byte(wxaNISTKey)), wxaNISTKey, true)
	// null is ok=false; undefined is "" with ok.
	check("zz", nil, "", false)
	check(nil, base64.StdEncoding.EncodeToString([]byte("short")), "", false)
	check(nil, nil, "", true)
}

// from: gateway/tests/wechat/cdn-download.test.ts: image and file items become bounded CDN references
func TestWxaMediaItemsBecomeReferences(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(wxaNISTKey))
	content := ParseInboundContent(wxaItems(t,
		map[string]any{"type": 2, "image_item": map[string]any{"aeskey": wxaNISTKey, "media": map[string]any{"encrypt_query_param": "img-param"}}},
		map[string]any{"type": 4, "file_item": map[string]any{"file_name": "报告.pdf", "media": map[string]any{
			"full_url": "https://novac2c.cdn.weixin.qq.com/c2c/download?x=1", "aes_key": b64}}},
		map[string]any{"type": 1, "text_item": map[string]any{"text": "帮我看看"}}))
	want := []InboundMediaRef{{Kind: "image", EncryptQueryParam: "img-param", AESKeyHex: wxaNISTKey},
		{Kind: "file", FullURL: "https://novac2c.cdn.weixin.qq.com/c2c/download?x=1", AESKeyHex: wxaNISTKey, FileName: "报告.pdf"}}
	if !reflect.DeepEqual(content.Media, want) || content.Text != "帮我看看" {
		t.Fatalf("%+v", content)
	}
	alone := ParseInboundContent(wxaItems(t, map[string]any{"type": 2, "image_item": map[string]any{"media": map[string]any{"encrypt_query_param": "p"}}}))
	if !reflect.DeepEqual(alone, &InboundContent{Media: []InboundMediaRef{{Kind: "image", EncryptQueryParam: "p"}}}) {
		t.Fatalf("%+v", alone)
	}
	if got := ParseInboundContent(wxaItems(t, map[string]any{"type": 4, "file_item": map[string]any{"media": map[string]any{"encrypt_query_param": "p"}}})); !reflect.DeepEqual(got.UnsupportedKinds, []string{"file"}) {
		t.Fatalf("an unencrypted file reference is trusted: %+v", got)
	}
	if got := ParseInboundContent(wxaItems(t, map[string]any{"type": 5, "video_item": map[string]any{"media": map[string]any{"encrypt_query_param": "p"}}})); !reflect.DeepEqual(got.UnsupportedKinds, []string{"video"}) {
		t.Fatalf("%+v", got)
	}
}

// from: gateway/tests/wechat/cdn-download.test.ts: download URLs stay on the WeChat CDN
func TestWxaDownloadURLsStayOnCDN(t *testing.T) {
	if u, _ := MediaDownloadURL(InboundMediaRef{EncryptQueryParam: "a b"}); u != "https://novac2c.cdn.weixin.qq.com/c2c/download?encrypted_query_param=a+b" {
		t.Fatalf("%s", u)
	}
	if u, _ := MediaDownloadURL(InboundMediaRef{FullURL: "https://evil.example/x", EncryptQueryParam: "p"}); u != "https://novac2c.cdn.weixin.qq.com/c2c/download?encrypted_query_param=p" {
		t.Fatalf("%s", u)
	}
	var mde *MediaDownloadError
	if _, err := MediaDownloadURL(InboundMediaRef{FullURL: "http://novac2c.cdn.weixin.qq.com/x"}); !errors.As(err, &mde) {
		t.Fatalf("%v", err)
	}
}

// from: gateway/tests/wechat/cdn-download.test.ts: an encrypted image downloads, decrypts and is sniffed with its hash
func TestWxaEncryptedImageDownloads(t *testing.T) {
	var urls []string
	m, err := wxaDownload(t, InboundMediaRef{Kind: "image", EncryptQueryParam: "p", AESKeyHex: wxaNISTKey}, func(req *http.Request, _ []byte) (*http.Response, error) {
		urls = append(urls, req.URL.String())
		return bytesResponse(200, wxaEncrypt(wxaPNG, wxaNISTKey), nil), nil
	})
	mustNoErr(t, err)
	if len(urls) != 1 || !bytes.Equal(m.Bytes, wxaPNG) || m.Mime != "image/png" || m.Name != "image.png" || m.SHA256 != sha256Hex(string(wxaPNG)) {
		t.Fatalf("%v %+v", urls, m)
	}
}

// from: gateway/tests/wechat/cdn-download.test.ts: files keep a safe display name and extension MIME
func TestWxaFileSafeName(t *testing.T) {
	m, err := wxaDownload(t, InboundMediaRef{Kind: "file", EncryptQueryParam: "p", AESKeyHex: wxaNISTKey, FileName: "../../etc/notes.md"},
		wxaRespond(wxaEncrypt([]byte("# notes"), wxaNISTKey), nil))
	mustNoErr(t, err)
	if m.Name != "notes.md" || m.Mime != "text/markdown" || safeFileName("..", "attachment") != "attachment" {
		t.Fatalf("%+v", m)
	}
}

// from: gateway/tests/wechat/cdn-download.test.ts: size caps reject declared and streamed oversize bodies
func TestWxaSizeCaps(t *testing.T) {
	cases := []struct {
		ref InboundMediaRef
		f   fetchFunc
	}{
		{InboundMediaRef{Kind: "image", EncryptQueryParam: "p"}, wxaRespond(wxaPNG, http.Header{"Content-Length": {strconv.Itoa(MaxInboundImageBytes + 1)}})},
		{InboundMediaRef{Kind: "image", EncryptQueryParam: "p"}, wxaRespond(bytes.Repeat([]byte{1}, MaxInboundImageBytes+1), nil)},
		{InboundMediaRef{Kind: "file", EncryptQueryParam: "p", AESKeyHex: wxaNISTKey}, wxaRespond(bytes.Repeat([]byte{1}, MaxInboundFileBytes+32), nil)},
	}
	for i, c := range cases {
		if _, err := wxaDownload(t, c.ref, c.f); err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("case %d: %v", i, err)
		}
	}
}

// from: gateway/tests/wechat/cdn-download.test.ts: non-image bytes, HTTP errors and network failures are download errors
func TestWxaDownloadErrors(t *testing.T) {
	ref := InboundMediaRef{Kind: "image", EncryptQueryParam: "p"}
	if _, err := wxaDownload(t, ref, wxaRespond([]byte("<html>"), nil)); err == nil || !strings.Contains(err.Error(), "unsupported format") {
		t.Fatalf("%v", err)
	}
	if _, err := wxaDownload(t, ref, func(*http.Request, []byte) (*http.Response, error) { return rawResponse(404, "no"), nil }); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("%v", err)
	}
	var mde *MediaDownloadError
	if _, err := wxaDownload(t, ref, func(*http.Request, []byte) (*http.Response, error) { return nil, errors.New("fetch failed") }); !errors.As(err, &mde) {
		t.Fatalf("%v", err)
	}
}

var wxaFileBytes = []byte("# 研究成果\n\n完整正文和附件数据\n")

func wxaMeta(data []byte) FileMetadata { return fileMetadataFor("研究成果.md", data) }

// wxaUploader is a FileUploader with a fake authorization call and CDN.
func wxaUploader(t *testing.T, max int64, request uploadRequest, cdn fetchFunc) (*FileUploader, *loop) {
	t.Helper()
	if cdn == nil {
		cdn = func(*http.Request, []byte) (*http.Response, error) {
			t.Error("unexpected CDN call")
			return nil, errors.New("unexpected")
		}
	}
	l := &loop{}
	c := newTestClient(t, newTestState(t, t.TempDir(), PolicyOptions{}), l, fakeHTTP(cdn))
	u, err := newFileUploader(c, request, max)
	mustNoErr(t, err)
	return u, l
}

func wxaUpload(u *FileUploader, l *loop, ctx context.Context, data []byte, meta FileMetadata) (map[string]any, error) {
	var item map[string]any
	var err error
	l.do(func() { item, err = u.upload(ctx, "chat", data, meta) })
	return item, err
}

func wxaDownloadOK() fetchFunc {
	return func(*http.Request, []byte) (*http.Response, error) {
		return bytesResponse(200, nil, http.Header{"X-Encrypted-Param": {"download"}}), nil
	}
}

// from: gateway/tests/wechat/file-upload.test.ts: file upload uses FILE authorization, exact AES bytes and plaintext string length
func TestWxaFileUploadAuthorization(t *testing.T) {
	var authorization map[string]any
	var ciphertext []byte
	mutable := bytes.Clone(wxaFileBytes)
	u, l := wxaUploader(t, 0, func(_ context.Context, endpoint string, payload map[string]any, _ int64) (map[string]any, error) {
		if endpoint != endpointGetUploadURL {
			t.Errorf("upload submitted %s", endpoint)
		}
		authorization = payload
		// Changing the caller's bytes now cannot change what is uploaded: they are encrypted already.
		for i := range mutable {
			mutable[i] = 0
		}
		return map[string]any{"ret": 0, "upload_param": "upload+/="}, nil
	}, func(req *http.Request, body []byte) (*http.Response, error) {
		q := req.URL.Query()
		if req.URL.Scheme+"://"+req.URL.Host != "https://novac2c.cdn.weixin.qq.com" || q.Get("encrypted_query_param") != "upload+/=" || q.Get("filekey") != authorization["filekey"] {
			t.Errorf("upload URL %s", req.URL)
		}
		ciphertext = body
		return bytesResponse(200, nil, http.Header{"X-Encrypted-Param": {"download"}}), nil
	})
	item, err := wxaUpload(u, l, context.Background(), mutable, wxaMeta(wxaFileBytes))
	mustNoErr(t, err)
	sum := md5.Sum(wxaFileBytes)
	if authorization["media_type"] != mediaTypeFile || authorization["rawsize"] != len(wxaFileBytes) || authorization["rawfilemd5"] != hex.EncodeToString(sum[:]) ||
		authorization["filesize"] != len(ciphertext) || authorization["no_need_thumb"] != true {
		t.Fatalf("authorization %v", authorization)
	}
	want := map[string]any{"type": ItemTypeFile, "file_item": map[string]any{"media": map[string]any{"encrypt_query_param": "download", "encrypt_type": 1,
		"aes_key": base64.StdEncoding.EncodeToString([]byte(authorization["aeskey"].(string)))}, "file_name": "研究成果.md", "len": strconv.Itoa(len(wxaFileBytes))}}
	if !reflect.DeepEqual(item, want) {
		t.Fatalf("item %v", item)
	}
	keyText, _ := base64.StdEncoding.DecodeString(item["file_item"].(map[string]any)["media"].(map[string]any)["aes_key"].(string))
	plain, err := DecryptAES128ECB(ciphertext, string(keyText))
	if err != nil || !bytes.Equal(plain, wxaFileBytes) {
		t.Fatalf("decrypted %q %v", plain, err)
	}
}

// from: gateway/tests/wechat/file-upload.test.ts: integrity and local size checks reject before authorizing any upload
func TestWxaFileUploadRejectsBeforeAuthorizing(t *testing.T) {
	calls := 0
	u, l := wxaUploader(t, int64(len(wxaFileBytes)), func(context.Context, string, map[string]any, int64) (map[string]any, error) {
		calls++
		return map[string]any{}, nil
	}, nil)
	longer := append(bytes.Clone(wxaFileBytes), 'x')
	bad := wxaMeta(wxaFileBytes)
	short, zeros, notDigest := bad, bad, bad
	short.ByteLength--
	zeros.SHA256 = strings.Repeat("0", 64)
	notDigest.SHA256 = "not-a-digest"
	// A non-number byte length cannot be expressed in Go's typed metadata.
	for _, c := range []struct {
		data []byte
		meta FileMetadata
	}{{nil, wxaMeta(nil)}, {longer, wxaMeta(wxaFileBytes)}, {wxaFileBytes, short}, {wxaFileBytes, zeros}, {wxaFileBytes, notDigest}} {
		if _, err := wxaUpload(u, l, context.Background(), c.data, c.meta); err == nil {
			t.Fatalf("accepted %+v", c.meta)
		}
	}
	for _, name := range []string{"", " ", ".", "..", "../report.md", "report\\data.md", "bad\nname.md", "bad\x00name.md", strings.Repeat("文", 86)} {
		m := wxaMeta(wxaFileBytes)
		m.FileName = name
		if _, err := wxaUpload(u, l, context.Background(), wxaFileBytes, m); err == nil || !strings.Contains(err.Error(), "valid file name") {
			t.Fatalf("name %q: %v", name, err)
		}
	}
	if calls != 0 {
		t.Fatalf("%d authorizations", calls)
	}
}

// from: gateway/tests/wechat/file-upload.test.ts: local product limit is configurable and inclusive
func TestWxaFileUploadLimit(t *testing.T) {
	u, l := wxaUploader(t, int64(len(wxaFileBytes)), func(context.Context, string, map[string]any, int64) (map[string]any, error) {
		return map[string]any{"upload_param": "grant"}, nil
	}, wxaDownloadOK())
	item, err := wxaUpload(u, l, context.Background(), wxaFileBytes, wxaMeta(wxaFileBytes))
	mustNoErr(t, err)
	if item["file_item"].(map[string]any)["len"] != strconv.Itoa(len(wxaFileBytes)) {
		t.Fatalf("%v", item)
	}
	longer := append(bytes.Clone(wxaFileBytes), 'x')
	if _, err := wxaUpload(u, l, context.Background(), longer, wxaMeta(longer)); err == nil || !strings.Contains(err.Error(), "configured byte limit") {
		t.Fatalf("%v", err)
	}
	// Zero is "the default" in Go's options; negative and unsafe sizes are refused.
	for _, max := range []int64{-1, maxSafeInteger + 1} {
		if _, err := newFileUploader(nil, nil, max); err == nil || !strings.Contains(err.Error(), "byte limit") {
			t.Fatalf("max %d: %v", max, err)
		}
	}
}

// from: gateway/tests/wechat/file-upload.test.ts: upload-only retries preserve ciphertext, do not reauthorize and never send a message
func TestWxaFileUploadRetriesKeepCiphertext(t *testing.T) {
	grants := 0
	var bodies [][]byte
	u, l := wxaUploader(t, 0, func(_ context.Context, endpoint string, _ map[string]any, _ int64) (map[string]any, error) {
		if endpoint != endpointGetUploadURL {
			t.Errorf("endpoint %s", endpoint)
		}
		grants++
		return map[string]any{"upload_param": "grant"}, nil
	}, func(_ *http.Request, body []byte) (*http.Response, error) {
		bodies = append(bodies, body)
		if len(bodies) == 1 {
			return bytesResponse(503, nil, nil), nil
		}
		return bytesResponse(200, nil, http.Header{"X-Encrypted-Param": {"download"}}), nil
	})
	_, err := wxaUpload(u, l, context.Background(), wxaFileBytes, wxaMeta(wxaFileBytes))
	mustNoErr(t, err)
	// The shared backing store is a JavaScript detail; the bytes being identical is what matters.
	if grants != 1 || len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("grants %d bodies %d", grants, len(bodies))
	}
}

// from: gateway/tests/wechat/file-upload.test.ts: authorization rejection and untrusted CDN targets never upload
func TestWxaFileUploadRejectedGrants(t *testing.T) {
	for _, grant := range []map[string]any{{"ret": -2, "upload_param": "grant"}, {"errcode": 3, "upload_param": "grant"},
		{"upload_full_url": "https://attacker.test/c2c/upload", "upload_param": "unused"}, {}} {
		fetched := false
		u, l := wxaUploader(t, 0, func(context.Context, string, map[string]any, int64) (map[string]any, error) { return grant, nil },
			func(*http.Request, []byte) (*http.Response, error) {
				fetched = true
				return nil, errors.New("unexpected")
			})
		if _, err := wxaUpload(u, l, context.Background(), wxaFileBytes, wxaMeta(wxaFileBytes)); err == nil || fetched {
			t.Fatalf("grant %v: err %v fetched %v", grant, err, fetched)
		}
	}
}

// wxaHang waits for the request's end and answers its cause, like a
// transport that only stops when aborted.
func wxaHang(ctx context.Context) error {
	<-ctx.Done()
	return context.Cause(ctx)
}

// from: gateway/tests/wechat/file-upload.test.ts: cancelled or timed out uploads finish without sending messages
func TestWxaFileUploadCancelledOrTimedOut(t *testing.T) {
	calls := 0
	u, l := wxaUploader(t, 0, func(context.Context, string, map[string]any, int64) (map[string]any, error) {
		calls++
		return map[string]any{}, nil
	}, nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("cancelled"))
	if _, err := wxaUpload(u, l, ctx, wxaFileBytes, wxaMeta(wxaFileBytes)); err == nil || !strings.Contains(err.Error(), "cancelled") || calls != 0 {
		t.Fatalf("%v calls %d", err, calls)
	}
	for _, phase := range []string{"authorization", "upload"} {
		u, l := wxaUploader(t, 0, func(ctx context.Context, _ string, _ map[string]any, _ int64) (map[string]any, error) {
			if phase == "authorization" {
				return nil, wxaHang(ctx)
			}
			return map[string]any{"upload_param": "grant"}, nil
		}, func(req *http.Request, _ []byte) (*http.Response, error) { return nil, wxaHang(req.Context()) })
		u.uploader.timeoutMs = 15
		// Both deadlines are 15 ms, so either the upload's or the authorization's may name the timeout.
		_, err := wxaUpload(u, l, context.Background(), wxaFileBytes, wxaMeta(wxaFileBytes))
		if err == nil || !regexp.MustCompile(`file upload (authorization )?timed out`).MatchString(err.Error()) {
			t.Fatalf("%s: %v", phase, err)
		}
	}
}

// from: gateway/tests/wechat/file-upload.test.ts: file transfer receives a size allowance while upload authorization stays bounded
func TestWxaFileUploadTransferAllowance(t *testing.T) {
	content := make([]byte, 1024*1024)
	var grantTimeout int64
	var transferLeft time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	u, l := wxaUploader(t, 0, func(_ context.Context, _ string, _ map[string]any, timeoutMs int64) (map[string]any, error) {
		grantTimeout = timeoutMs
		return map[string]any{"upload_param": "grant"}, nil
	}, func(req *http.Request, _ []byte) (*http.Response, error) {
		deadline, _ := req.Context().Deadline()
		transferLeft = time.Until(deadline)
		cancel()
		return nil, wxaHang(req.Context())
	})
	_, err := wxaUpload(u, l, ctx, content, fileMetadataFor("研究成果.md", content))
	if err == nil {
		t.Fatal("upload finished")
	}
	// Timers cannot be faked: the deadlines themselves are checked, 30 s to authorize and 31 s to transfer 1 MiB.
	if grantTimeout != MediaUploadTimeoutMs || transferLeft <= 30*time.Second || transferLeft > 31*time.Second {
		t.Fatalf("grant %d ms, transfer %v", grantTimeout, transferLeft)
	}
}

// from: gateway/tests/wechat/file-upload.test.ts: authorization cannot inherit the longer file transfer deadline
func TestWxaFileUploadAuthorizationDeadline(t *testing.T) {
	content := make([]byte, 1024*1024)
	var left time.Duration
	u, l := wxaUploader(t, 0, func(ctx context.Context, _ string, _ map[string]any, _ int64) (map[string]any, error) {
		deadline, _ := ctx.Deadline()
		left = time.Until(deadline)
		return nil, errors.New("stop")
	}, nil)
	_, _ = wxaUpload(u, l, context.Background(), content, fileMetadataFor("a.md", content))
	if left <= 29*time.Second || left > 30*time.Second {
		t.Fatalf("authorization deadline %v, want 30 s", left)
	}
}

// from: gateway/tests/wechat/file-upload.test.ts: parent cancellation interrupts an extended file transfer before any send
func TestWxaFileUploadParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	var once sync.Once
	u, l := wxaUploader(t, 0, func(context.Context, string, map[string]any, int64) (map[string]any, error) {
		return map[string]any{"upload_param": "grant"}, nil
	}, func(req *http.Request, _ []byte) (*http.Response, error) {
		once.Do(func() { cancel(errors.New("parent-stop")) })
		return nil, wxaHang(req.Context())
	})
	if _, err := wxaUpload(u, l, ctx, wxaFileBytes, wxaMeta(wxaFileBytes)); err == nil || !strings.Contains(err.Error(), "parent-stop") {
		t.Fatalf("%v", err)
	}
}
