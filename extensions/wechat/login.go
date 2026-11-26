package wechat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

// QR login: get_bot_qrcode gives a code to scan, get_qrcode_status
// long-polls how it stands. A confirmed scan names the bot's token, its id,
// the person who scanned (the only one served) and the base URL.
const (
	loginClientVersion = "2.4.6"
	qrIntervalSeconds  = 2
	qrWait             = 8 * time.Second
	qrMaxAge           = 5 * time.Minute
	qrMaxRenewals      = 3
	stepQR             = "qr"
	stepCode           = "code"
	maxLocalTokens     = 10
	loginTimeout       = 15 * time.Second

	qrWaiting      = "wait"
	qrScanned      = "scaned" // sic, the wire value
	qrConfirmed    = "confirmed"
	qrExpired      = "expired"
	qrRedirect     = "scaned_but_redirect"
	qrNeedCode     = "need_verifycode"
	qrCodeBlocked  = "verify_code_blocked"
	qrBoundAlready = "binded_redirect"

	hintScan      = "用微信扫描二维码，在手机上确认绑定。"
	hintScanned   = "已扫码，请在手机上确认。"
	hintEnterCode = "微信要求输入手机上显示的数字：在输入框填写后继续。"
	labelCode     = "手机上显示的数字"
	errWrongCode  = "数字不对，请重新输入手机上显示的数字。"
	errLoginRetry = "暂时连不上微信，稍后自动重试。"
)

var (
	verifyCodePattern = regexp.MustCompile(`^\d{1,12}$`)
	redirectHost      = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)
	errQRTooOften     = errors.New("二维码多次过期，请重新开始登录")
	errCodeBlocked    = errors.New("验证码错误次数过多，请稍后再试")
	errBoundAlready   = errors.New("这个微信已经绑定过另一个 bot")
)

// Credentials are a confirmed login's secrets.
type Credentials struct {
	Token   string `json:"token"`
	BotID   string `json:"bot_id"`
	UserID  string `json:"user_id"`
	BaseURL string `json:"base_url"`
}

type pendingQR struct {
	code, content, base string
	started             time.Time
	renewed             int
	scanned             bool
}

func versionNumber(v string) int {
	n := 0
	parts := strings.SplitN(v, ".", 3)
	for i := 0; i < 3; i++ {
		x := 0
		if i < len(parts) {
			x, _ = strconv.Atoi(parts[i])
		}
		n = n<<8 | x&0xff
	}
	return n
}

func tencentHost(h string) bool {
	h = strings.ToLower(h)
	return h == "weixin.qq.com" || strings.HasSuffix(h, ".weixin.qq.com")
}

// loginClient makes the unauthenticated login calls.
type loginClient struct {
	http *http.Client
	// origin is where login starts; tests point it at a fake.
	origin string
	// trust accepts hosts other than Tencent's (tests only).
	trust func(host string) bool
}

func (c *loginClient) allowed(host string) bool {
	return tencentHost(host) || c.trust != nil && c.trust(host)
}

func (c *loginClient) validBase(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		u.Path != "" && u.Path != "/" || !c.allowed(u.Hostname()) {
		return "", errors.New(invalidBaseURLDescription)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func (c *loginClient) call(ctx context.Context, method, base, path string, body any, wait time.Duration) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+"/"+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("iLink-App-Id", "bot")
	req.Header.Set("iLink-App-ClientVersion", strconv.Itoa(versionNumber(loginClientVersion)))
	req.Header.Set("X-WECHAT-UIN", randomUIN())
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := noRedirect(c.http).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, context.DeadlineExceeded
		}
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxILinkResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("WeChat iLink HTTP %d on login", resp.StatusCode)
	}
	parsed, err := ParseILinkJSON(string(raw))
	if err != nil {
		return nil, err
	}
	out, ok := parsed.(map[string]any)
	if !ok {
		return nil, errNonObject
	}
	return out, nil
}

func (c *loginClient) beginQR(ctx context.Context, tokens []string) (pendingQR, error) {
	if len(tokens) > maxLocalTokens {
		tokens = tokens[len(tokens)-maxLocalTokens:]
	}
	if tokens == nil {
		tokens = []string{}
	}
	out, err := c.call(ctx, http.MethodPost, c.origin, "ilink/bot/get_bot_qrcode?bot_type=3", map[string]any{"local_token_list": tokens}, loginTimeout)
	if err != nil {
		return pendingQR{}, err
	}
	code, _ := out["qrcode"].(string)
	content, _ := out["qrcode_img_content"].(string)
	u, perr := url.Parse(content)
	if code == "" || perr != nil || u.Scheme != "https" || u.User != nil || !c.allowed(u.Hostname()) {
		return pendingQR{}, errors.New("WeChat answered an invalid QR code")
	}
	return pendingQR{code: code, content: content, base: c.origin, started: time.Now()}, nil
}

type qrStatus struct {
	state, redirectBase string
	creds               Credentials
}

// pollQR asks how code stands; running out of time is "wait".
func (c *loginClient) pollQR(ctx context.Context, p pendingQR, verifyCode string) (qrStatus, error) {
	q := url.Values{"qrcode": {p.code}}
	if verifyCode != "" {
		q.Set("verify_code", verifyCode)
	}
	out, err := c.call(ctx, http.MethodGet, p.base, "ilink/bot/get_qrcode_status?"+q.Encode(), nil, qrWait)
	if errors.Is(err, context.DeadlineExceeded) {
		return qrStatus{state: qrWaiting}, nil
	}
	if err != nil {
		return qrStatus{}, err
	}
	st := qrStatus{state: fmt.Sprint(out["status"])}
	switch st.state {
	case qrWaiting, qrScanned, qrExpired, qrNeedCode, qrCodeBlocked, qrBoundAlready:
	case qrRedirect:
		host, _ := out["redirect_host"].(string)
		if !redirectHost.MatchString(host) {
			return qrStatus{}, errors.New("WeChat answered an invalid redirect")
		}
		if st.redirectBase, err = c.validBase("https://" + host); err != nil {
			return qrStatus{}, err
		}
	case qrConfirmed:
		str := func(k string) string { s, _ := out[k].(string); return s }
		st.creds = Credentials{Token: str("bot_token"), BotID: str("ilink_bot_id"), UserID: str("ilink_user_id"), BaseURL: str("baseurl")}
		if st.creds.Token == "" || st.creds.BotID == "" || st.creds.UserID == "" || st.creds.BaseURL == "" {
			return qrStatus{}, errors.New("WeChat confirmed without credentials")
		}
		if st.creds.BaseURL, err = c.validBase(st.creds.BaseURL); err != nil {
			return qrStatus{}, err
		}
	default:
		return qrStatus{}, errors.New("WeChat answered an unknown login status")
	}
	return st, nil
}

// loginStep is connector.login.step. "" starts over; "qr" asks again;
// "code" carries, as secret, the number WeChat showed on the phone.
func (x *extension) loginStep(ctx context.Context, step, secret string) (extproto.LoginStep, error) {
	x.mu.Lock()
	p := x.qr
	tokens := append([]string(nil), x.oldTokens...)
	x.mu.Unlock()
	if step == "" || p == nil || time.Since(p.started) > qrMaxAge {
		return x.beginQR(ctx, tokens, nil)
	}
	code := ""
	if step == stepCode {
		code = strings.TrimSpace(secret)
		if !verifyCodePattern.MatchString(code) {
			return codeStep(""), nil
		}
	}
	st, err := x.login.pollQR(ctx, *p, code)
	if err != nil {
		if err.Error() != invalidBaseURLDescription {
			// Asked again in a moment: the code may still succeed.
			prompt := qrPrompt(p)
			prompt.Error = errLoginRetry
			return prompt, nil
		}
		x.forgetQR(p)
		return extproto.LoginStep{}, extproto.Errorf(extproto.ErrInternal, "%s", SanitizeErrorType(err))
	}
	switch st.state {
	case qrScanned:
		x.mu.Lock()
		p.scanned = true
		x.mu.Unlock()
	case qrRedirect:
		x.mu.Lock()
		p.base = st.redirectBase
		x.mu.Unlock()
	case qrNeedCode:
		if code != "" {
			return codeStep(errWrongCode), nil
		}
		return codeStep(""), nil
	case qrExpired:
		if p.renewed >= qrMaxRenewals {
			x.forgetQR(p)
			return extproto.LoginStep{}, extproto.Errorf(extproto.ErrInternal, "%s", errQRTooOften.Error())
		}
		return x.beginQR(ctx, tokens, p)
	case qrCodeBlocked:
		x.forgetQR(p)
		return extproto.LoginStep{}, extproto.Errorf(extproto.ErrInternal, "%s", errCodeBlocked.Error())
	case qrBoundAlready:
		x.forgetQR(p)
		return extproto.LoginStep{}, extproto.Errorf(extproto.ErrInternal, "%s", errBoundAlready.Error())
	case qrConfirmed:
		x.forgetQR(p)
		if err := x.bind(ctx, st.creds); err != nil {
			return extproto.LoginStep{}, err
		}
		return extproto.LoginStep{Done: true, Account: accountName}, nil
	}
	return qrPrompt(p), nil
}

func qrPrompt(p *pendingQR) extproto.LoginStep {
	hint := hintScan
	if p.scanned {
		hint = hintScanned
	}
	return extproto.LoginStep{Step: stepQR, Kind: extproto.LoginQR, URL: p.content, Interval: qrIntervalSeconds, Hint: hint}
}

// codeStep asks for the number WeChat shows on the phone.
func codeStep(errText string) extproto.LoginStep {
	return extproto.LoginStep{Step: stepCode, Kind: extproto.LoginSecret, Label: labelCode, Hint: hintEnterCode, Error: errText}
}

// beginQR asks for a code; renewing keeps the first one's start and count.
func (x *extension) beginQR(ctx context.Context, tokens []string, old *pendingQR) (extproto.LoginStep, error) {
	p, err := x.login.beginQR(ctx, tokens)
	if err != nil {
		return extproto.LoginStep{}, extproto.Errorf(extproto.ErrInternal, "%s", SanitizeErrorType(err))
	}
	if old != nil {
		p.renewed, p.started = old.renewed+1, old.started
	}
	x.mu.Lock()
	x.qr = &p
	x.mu.Unlock()
	return qrPrompt(&p), nil
}

func (x *extension) forgetQR(p *pendingQR) {
	x.mu.Lock()
	if x.qr == p {
		x.qr = nil
	}
	x.mu.Unlock()
}
