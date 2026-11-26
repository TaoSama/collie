package wechat

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const (
	MaxInboundImageBytes = 10 * 1024 * 1024
	MaxInboundFileBytes  = 20 * 1024 * 1024
	MediaDownloadSetupMs = 15_000
	MediaUploadTimeoutMs = 30_000
	maxUploadAttempts    = 3
	cdnDownloadURL       = "https://novac2c.cdn.weixin.qq.com/c2c/download"
	cdnUploadURL         = "https://novac2c.cdn.weixin.qq.com/c2c/upload"
	cdnUploadHost        = "novac2c.cdn.weixin.qq.com"
	cdnUploadPath        = "/c2c/upload"
	cdnHostSuffix        = ".weixin.qq.com"
	aesBlockBytes        = 16
	maxDownloadParam     = 8192
	defaultImageName     = "image"
	defaultFileName      = "attachment"
	fallbackFileMime     = "application/octet-stream"
	mediaTypeImage       = 1
	mediaTypeFile        = 3
)

var (
	imageExtensions = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}
	fileMimes       = map[string]string{
		".pdf": "application/pdf", ".txt": "text/plain", ".md": "text/markdown", ".csv": "text/csv",
		".json": "application/json", ".zip": "application/zip", ".log": "text/plain",
		".doc": "application/msword", ".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		".xls": "application/vnd.ms-excel", ".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		".ppt": "application/vnd.ms-powerpoint", ".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	}
	unsafeNameChars = regexp.MustCompile(`[\x00-\x1f\x7f"<>|:*?]`)
	lowerHexKey     = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// DownloadedMedia is one decrypted attachment.
type DownloadedMedia struct {
	Kind   string
	Name   string
	Mime   string
	Bytes  []byte
	SHA256 string
}

// MediaDownloadError is an attachment that could not be received.
type MediaDownloadError struct{ msg string }

func (e *MediaDownloadError) Error() string { return e.msg }

func mediaError(format string, args ...any) error {
	return &MediaDownloadError{fmt.Sprintf(format, args...)}
}

func maxMediaBytes(kind string) int64 {
	if kind == KindImage {
		return MaxInboundImageBytes
	}
	return MaxInboundFileBytes
}

// safeFileName keeps a display-safe base name.
func safeFileName(raw, fallback string) string {
	base := path.Base(strings.ReplaceAll(raw, `\`, "/"))
	if raw == "" {
		base = ""
	}
	base = strings.TrimSpace(unsafeNameChars.ReplaceAllString(base, "_"))
	name := fallback
	if base != "" && base != "." && base != ".." && base != "/" {
		name = base
	}
	if u := toUTF16(name); len(u) > MaxMediaFileNameChars {
		name = fromUTF16(u[len(u)-MaxMediaFileNameChars:])
	}
	return name
}

func sniffImageMime(b []byte) string {
	switch {
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}):
		return "image/png"
	case len(b) >= 3 && b[0] == 255 && b[1] == 216 && b[2] == 255:
		return "image/jpeg"
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// DecryptAES128ECB opens a CDN object: AES-128-ECB with PKCS7 padding.
func DecryptAES128ECB(ciphertext []byte, keyHex string) ([]byte, error) {
	if !lowerHexKey.MatchString(keyHex) {
		return nil, mediaError("WeChat media key is invalid")
	}
	if len(ciphertext) == 0 || len(ciphertext)%aesBlockBytes != 0 {
		return nil, mediaError("WeChat media ciphertext is not block aligned")
	}
	key, _ := hex.DecodeString(keyHex)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, mediaError("WeChat media decryption failed")
	}
	out := make([]byte, len(ciphertext))
	for i := 0; i < len(ciphertext); i += aesBlockBytes {
		block.Decrypt(out[i:i+aesBlockBytes], ciphertext[i:i+aesBlockBytes])
	}
	pad := int(out[len(out)-1])
	if pad == 0 || pad > aesBlockBytes {
		return nil, mediaError("WeChat media decryption failed")
	}
	for _, b := range out[len(out)-pad:] {
		if int(b) != pad {
			return nil, mediaError("WeChat media decryption failed")
		}
	}
	return out[:len(out)-pad], nil
}

func encryptAES128ECB(plain, key []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic("wechat: bad AES key size")
	}
	pad := aesBlockBytes - len(plain)%aesBlockBytes
	padded := append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(padded))
	for i := 0; i < len(padded); i += aesBlockBytes {
		block.Encrypt(out[i:i+aesBlockBytes], padded[i:i+aesBlockBytes])
	}
	return out
}

// MediaDownloadURL accepts a server URL only on the WeChat CDN; otherwise
// the query parameter route is used.
func MediaDownloadURL(ref InboundMediaRef) (string, error) {
	if ref.FullURL != "" {
		if u, err := url.Parse(ref.FullURL); err == nil && u.Scheme == "https" && strings.HasSuffix(u.Hostname(), cdnHostSuffix) &&
			u.Port() == "" && u.User == nil && u.Fragment == "" && !strings.Contains(ref.FullURL, "#") {
			return u.String(), nil
		}
	}
	if ref.EncryptQueryParam == "" {
		return "", mediaError("WeChat media download URL is not allowed")
	}
	return cdnDownloadURL + "?encrypted_query_param=" + url.QueryEscape(ref.EncryptQueryParam), nil
}

func fileMime(name string, b []byte) string {
	if m := sniffImageMime(b); m != "" {
		return m
	}
	if m, ok := fileMimes[strings.ToLower(path.Ext(name))]; ok {
		return m
	}
	return fallbackFileMime
}

// downloadInboundMedia fetches, decrypts and checks one attachment within
// its kind's size cap. The loop is released while it downloads.
func downloadInboundMedia(ctx context.Context, l *loop, hc *http.Client, ref InboundMediaRef) (DownloadedMedia, error) {
	maxBytes := maxMediaBytes(ref.Kind)
	// PKCS7 adds at most one block to the plaintext.
	maxCiphertext := maxBytes
	if ref.AESKeyHex != "" {
		maxCiphertext += aesBlockBytes
	}
	budget, err := transferTimeoutMs(maxCiphertext, MediaDownloadSetupMs)
	if err != nil {
		return DownloadedMedia{}, err
	}
	dctx, cancel := withTimeout(ctx, budget, mediaError("WeChat %s download timed out", ref.Kind))
	defer cancel()
	u, err := MediaDownloadURL(ref)
	if err != nil {
		return DownloadedMedia{}, err
	}
	var raw []byte
	var fetchErr error
	l.await(func() {
		req, err := http.NewRequestWithContext(dctx, http.MethodGet, u, nil)
		if err != nil {
			fetchErr = mediaError("WeChat media download request failed")
			return
		}
		resp, err := noRedirect(hc).Do(req)
		if err != nil {
			if cause := abortCause(dctx); cause != nil {
				fetchErr = cause
			} else {
				fetchErr = mediaError("WeChat media download request failed")
			}
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fetchErr = mediaError("WeChat media download failed HTTP %d", resp.StatusCode)
			return
		}
		raw, fetchErr = readBounded(dctx, resp, maxCiphertext)
	})
	if fetchErr != nil {
		return DownloadedMedia{}, fetchErr
	}
	data := raw
	if ref.AESKeyHex != "" {
		if data, err = DecryptAES128ECB(raw, ref.AESKeyHex); err != nil {
			return DownloadedMedia{}, err
		}
	}
	if len(data) == 0 || int64(len(data)) > maxBytes {
		return DownloadedMedia{}, mediaError("WeChat media exceeds the size limit")
	}
	m := DownloadedMedia{Kind: ref.Kind, Bytes: data}
	if ref.Kind == KindImage {
		sniffed := sniffImageMime(data)
		if sniffed == "" {
			return DownloadedMedia{}, mediaError("WeChat image has an unsupported format")
		}
		m.Mime, m.Name = sniffed, defaultImageName+imageExtensions[sniffed]
	} else {
		m.Name = safeFileName(ref.FileName, defaultFileName)
		m.Mime = fileMime(m.Name, data)
	}
	sum := sha256.Sum256(data)
	m.SHA256 = hex.EncodeToString(sum[:])
	return m, nil
}

func readBounded(ctx context.Context, resp *http.Response, maxBytes int64) ([]byte, error) {
	if length := resp.Header.Get("Content-Length"); length != "" {
		n, err := strconv.ParseInt(length, 10, 64)
		if err != nil || !digitsOnly.MatchString(length) || n > maxBytes {
			return nil, mediaError("WeChat media exceeds the size limit")
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		if cause := abortCause(ctx); cause != nil {
			return nil, cause
		}
		return nil, mediaError("WeChat media download request failed")
	}
	if int64(len(data)) > maxBytes {
		return nil, mediaError("WeChat media exceeds the size limit")
	}
	return data, nil
}

func noRedirect(hc *http.Client) *http.Client {
	c := http.Client{}
	if hc != nil {
		c = *hc
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirect }
	return &c
}

// uploadRequest is an authorized iLink call made for an upload.
type uploadRequest func(ctx context.Context, endpoint string, payload map[string]any, timeoutMs int64) (map[string]any, error)

// UploadedCDNMedia is where WeChat will find an uploaded object.
type UploadedCDNMedia struct {
	Media struct {
		EncryptQueryParam string `json:"encrypt_query_param"`
		AESKey            string `json:"aes_key"`
		EncryptType       int    `json:"encrypt_type"`
	}
	PlaintextBytes  int
	CiphertextBytes int
}

type uploadFailure struct {
	msg       string
	retryable bool
}

func (e *uploadFailure) Error() string { return e.msg }

func validateUploadURL(value string) (string, error) {
	invalid := errors.New("WeChat CDN upload URL is not allowed")
	if unsafeURLChars.MatchString(value) {
		return "", invalid
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() != cdnUploadHost || u.Path != cdnUploadPath || u.Port() != "" ||
		u.User != nil || u.Fragment != "" || strings.Contains(value, "#") {
		return "", invalid
	}
	return u.String(), nil
}

// cdnUploader uploads media alone: that sends no chat message and spends no
// budget, so a failed upload can be repeated.
type cdnUploader struct {
	client    *ILinkClient
	request   uploadRequest
	timeoutMs int64
	label     string // image | file
}

func (u *cdnUploader) upload(ctx context.Context, chatID string, data []byte, mediaType int) (UploadedCDNMedia, error) {
	if len(data) == 0 {
		return UploadedCDNMedia{}, errors.New("WeChat media requires non-empty bytes")
	}
	if strings.TrimSpace(chatID) == "" {
		return UploadedCDNMedia{}, fmt.Errorf("WeChat %s requires a chat ID", u.label)
	}
	if err := abortCause(ctx); err != nil {
		return UploadedCDNMedia{}, err
	}
	plainMD5 := md5.Sum(data)
	key := make([]byte, aesBlockBytes)
	filekeyBytes := make([]byte, aesBlockBytes)
	if _, err := rand.Read(key); err != nil {
		return UploadedCDNMedia{}, err
	}
	if _, err := rand.Read(filekeyBytes); err != nil {
		return UploadedCDNMedia{}, err
	}
	keyHex, filekey := hex.EncodeToString(key), hex.EncodeToString(filekeyBytes)
	ciphertext := encryptAES128ECB(data, key)
	timeoutMs := u.timeoutMs
	if timeoutMs == 0 {
		timeoutMs = MediaUploadTimeoutMs
		if u.label == KindFile {
			t, err := transferTimeoutMs(int64(len(data)), MediaUploadTimeoutMs)
			if err != nil {
				return UploadedCDNMedia{}, err
			}
			timeoutMs = t
		}
	}
	uctx, cancel := withTimeout(ctx, timeoutMs, fmt.Errorf("WeChat %s upload timed out", u.label))
	defer cancel()
	authMs := min(timeoutMs, MediaUploadTimeoutMs)
	actx, acancel := withTimeout(uctx, authMs, fmt.Errorf("WeChat %s upload authorization timed out", u.label))
	response, err := u.request(actx, endpointGetUploadURL, map[string]any{
		"filekey": filekey, "media_type": mediaType, "to_user_id": chatID, "rawsize": len(data),
		"rawfilemd5": hex.EncodeToString(plainMD5[:]), "filesize": len(ciphertext), "no_need_thumb": true, "aeskey": keyHex,
	}, authMs)
	acancel()
	if err != nil {
		return UploadedCDNMedia{}, err
	}
	if cause := abortCause(uctx); cause != nil {
		return UploadedCDNMedia{}, cause
	}
	for _, f := range []string{"ret", "errcode"} {
		if v, ok := response[f]; ok && !isZero(v) {
			return UploadedCDNMedia{}, fmt.Errorf("WeChat %s upload authorization rejected", u.label)
		}
	}
	var target string
	if full, _ := response["upload_full_url"].(string); strings.TrimSpace(full) != "" {
		if target, err = validateUploadURL(strings.TrimSpace(full)); err != nil {
			return UploadedCDNMedia{}, err
		}
	} else if param, _ := response["upload_param"].(string); param != "" {
		target = cdnUploadURL + "?encrypted_query_param=" + url.QueryEscape(param) + "&filekey=" + url.QueryEscape(filekey)
	} else {
		return UploadedCDNMedia{}, fmt.Errorf("WeChat %s upload authorization is missing a URL", u.label)
	}
	for attempt := 0; attempt < maxUploadAttempts; attempt++ {
		if cause := abortCause(uctx); cause != nil {
			return UploadedCDNMedia{}, cause
		}
		param, err := u.post(uctx, target, ciphertext)
		if err == nil {
			out := UploadedCDNMedia{PlaintextBytes: len(data), CiphertextBytes: len(ciphertext)}
			out.Media.EncryptQueryParam = param
			out.Media.AESKey = base64.StdEncoding.EncodeToString([]byte(keyHex))
			out.Media.EncryptType = 1
			return out, nil
		}
		if cause := abortCause(uctx); cause != nil {
			return UploadedCDNMedia{}, cause
		}
		var f *uploadFailure
		if !errors.As(err, &f) || !f.retryable || attempt == maxUploadAttempts-1 {
			return UploadedCDNMedia{}, err
		}
	}
	return UploadedCDNMedia{}, errors.New("WeChat CDN upload failed")
}

// post sends the ciphertext once; the download parameter WeChat answers.
func (u *cdnUploader) post(ctx context.Context, target string, ciphertext []byte) (string, error) {
	var param string
	var failure error
	u.client.loop.await(func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(ciphertext))
		if err != nil {
			failure = &uploadFailure{"WeChat CDN upload request failed", true}
			return
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := u.client.http.Do(req)
		if err != nil {
			failure = &uploadFailure{"WeChat CDN upload request failed", true}
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxILinkResponseBytes))
		if resp.StatusCode != http.StatusOK {
			failure = &uploadFailure{fmt.Sprintf("WeChat CDN upload failed HTTP %d", resp.StatusCode), resp.StatusCode >= 500}
			return
		}
		param = resp.Header.Get("x-encrypted-param")
		if strings.TrimSpace(param) == "" || len(param) > maxDownloadParam || controlChars.MatchString(param) {
			failure = &uploadFailure{"WeChat CDN upload is missing a valid download parameter", true}
		}
	})
	return param, failure
}
