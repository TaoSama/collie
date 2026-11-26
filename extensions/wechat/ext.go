package wechat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

const (
	contributionID = "wechat"
	extensionID    = "wechat"
	stateFileName  = "state.json"
	// Collie's ping comes every 30 s; secrets and status calls are quick.
	hostCallTimeout = 30 * time.Second
	statusScope     = "status"

	msgNotSignedIn    = "还没有绑定微信：在接入页扫码登录。"
	msgSessionExpired = "微信登录已过期或已在别处登录，请重新扫码。"
	msgStarting       = "正在连接微信……"
)

// options are what tests replace: the network and the clock.
type options struct {
	http      *http.Client
	authURL   string
	baseURL   string
	trustHost func(string) bool
	pollEvery int64
}

// secrets is what connector.secrets keeps: the login and the tokens used
// before, so WeChat can tell a re-login of the same bot.
type secrets struct {
	Credentials
	SavedAt   string   `json:"saved_at,omitempty"`
	OldTokens []string `json:"old_tokens,omitempty"`
}

// extension is the extension process: the protocol, the login and the
// gateway of the bound account.
type extension struct {
	opt     options
	peer    *extproto.Peer
	login   *loginClient
	loop    *loop
	dataDir string

	mu          sync.Mutex
	initialized bool
	restoring   bool
	creds       *Credentials
	oldTokens   []string
	qr          *pendingQR
	gateway     *Gateway
	health      *HealthTracker
	runDone     chan struct{}
	state       string // ok | not_signed_in | error
	stateError  string
	interest    []string
	stopped     bool
}

func newExtension(o options) *extension {
	if o.authURL == "" {
		o.authURL = DefaultILinkBaseURL
	}
	if o.pollEvery == 0 {
		o.pollEvery = DefaultPollIntervalMs
	}
	return &extension{opt: o, loop: &loop{}, state: extproto.StateNotSignedIn, stateError: msgNotSignedIn,
		login: &loginClient{http: o.http, origin: o.authURL, trust: o.trustHost}}
}

func (x *extension) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if method == extproto.MInitialize {
		return x.initialize(ctx, params)
	}
	x.mu.Lock()
	ready := x.initialized
	x.mu.Unlock()
	if !ready {
		return nil, extproto.Errorf(extproto.ErrNotInitialized, "initialize first")
	}
	switch method {
	case extproto.MPing:
		return struct{}{}, nil
	case extproto.MStatus:
		return x.status(), nil
	case extproto.MLoginStep:
		var p struct {
			Step   string `json:"step"`
			Secret string `json:"secret"`
		}
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		return x.loginStep(ctx, p.Step, p.Secret)
	case extproto.MLogout:
		return struct{}{}, x.logout(ctx)
	case extproto.MChannelSend:
		var p extproto.Send
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		x.mu.Lock()
		g, restoring := x.gateway, x.restoring
		x.mu.Unlock()
		if g == nil && restoring {
			return nil, extproto.Errorf(extproto.ErrNotReady, "WeChat is restoring its login")
		}
		if g == nil {
			return nil, extproto.Errorf(extproto.ErrNotSignedIn, "WeChat is not signed in")
		}
		res, err := g.HandleSend(ctx, p)
		if res.Deferred {
			res.Reason = humanReason(extproto.DeliveryDeferred, res.Reason)
		}
		return res, err
	case extproto.MChannelInterest:
		var p extproto.Interest
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		x.mu.Lock()
		x.interest = p.Chats
		g := x.gateway
		x.mu.Unlock()
		if g != nil {
			g.SetInterest(p.Chats)
		}
		return struct{}{}, nil
	}
	return nil, extproto.Errorf(extproto.ErrUnknownMethod, "unknown method %s", method)
}

func decodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return extproto.Errorf(extproto.ErrInvalidParams, "params are not valid")
	}
	return nil
}

func (x *extension) initialize(ctx context.Context, raw json.RawMessage) (any, error) {
	var p extproto.InitializeParams
	if err := decodeParams(raw, &p); err != nil {
		return nil, err
	}
	if p.Protocol != extproto.Protocol {
		return nil, extproto.Errorf(extproto.ErrWrongProtocol, "protocol %d is not %d", p.Protocol, extproto.Protocol)
	}
	if p.Extension.DataDir == "" {
		return nil, extproto.Errorf(extproto.ErrInvalidParams, "initialize needs data_dir")
	}
	x.mu.Lock()
	x.dataDir, x.initialized, x.restoring = filepath.Clean(p.Extension.DataDir), true, true
	x.mu.Unlock()
	// Restoring the login is slow work: it runs after the answer.
	go x.restore()
	return extproto.InitializeResult{Protocol: extproto.Protocol, ID: extensionID, Version: p.Extension.Version}, nil
}

// restore loads the saved login and starts serving it.
func (x *extension) restore() {
	defer func() {
		x.mu.Lock()
		x.restoring = false
		x.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), hostCallTimeout)
	defer cancel()
	var out extproto.Secrets
	if err := x.peer.Call(ctx, extproto.MSecretsLoad, extproto.Secrets{Contribution: contributionID}, &out); err != nil {
		x.setState(extproto.StateError, "读取登录态失败："+SanitizeErrorType(err))
		return
	}
	if out.Data == "" {
		return
	}
	s, err := decodeSecrets(out.Data)
	if err != nil {
		x.setState(extproto.StateError, "登录态已损坏，请重新扫码")
		return
	}
	x.mu.Lock()
	x.oldTokens = s.OldTokens
	x.mu.Unlock()
	if err := x.start(s.Credentials); err != nil {
		x.setState(extproto.StateError, SanitizeErrorType(err))
	}
}

func decodeSecrets(data string) (secrets, error) {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return secrets{}, err
	}
	var s secrets
	if err := json.Unmarshal(raw, &s); err != nil {
		return secrets{}, err
	}
	if s.Token == "" || s.BotID == "" || s.UserID == "" {
		return secrets{}, errors.New("WeChat credentials missing required fields")
	}
	return s, nil
}

// bind keeps a confirmed login and starts serving it.
func (x *extension) bind(ctx context.Context, c Credentials) error {
	x.mu.Lock()
	old := x.oldTokens
	if x.creds != nil && x.creds.Token != c.Token {
		old = rememberToken(old, x.creds.Token)
	}
	x.oldTokens = old
	x.mu.Unlock()
	raw, err := json.Marshal(secrets{Credentials: c, SavedAt: isoTime(nowMs()), OldTokens: old})
	if err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, hostCallTimeout)
	defer cancel()
	if err := x.peer.Call(sctx, extproto.MSecretsSave, extproto.Secrets{Contribution: contributionID, Data: base64.StdEncoding.EncodeToString(raw)}, nil); err != nil {
		return extproto.Errorf(extproto.ErrInternal, "saving the login failed: %s", SanitizeErrorType(err))
	}
	return x.start(c)
}

func rememberToken(tokens []string, token string) []string {
	if token == "" || containsString(tokens, token) {
		return tokens
	}
	tokens = append(append([]string(nil), tokens...), token)
	if len(tokens) > maxLocalTokens {
		tokens = tokens[len(tokens)-maxLocalTokens:]
	}
	return tokens
}

func (x *extension) wakeOutbox() {
	if err := x.peer.Notify(extproto.EChanged, extproto.Changed{Contribution: contributionID, Scope: "outbox"}); err != nil {
		writeLogLine(map[string]any{"event": "outbox_notify_failed", "errorType": SanitizeErrorType(err)}, "")
	}
}

func (x *extension) setState(state, message string) {
	x.mu.Lock()
	changed := x.state != state || x.stateError != message
	x.state, x.stateError = state, message
	x.mu.Unlock()
	if changed && x.peer != nil {
		if err := x.peer.Notify(extproto.EChanged, extproto.Changed{Contribution: contributionID, Scope: statusScope}); err != nil {
			writeLogLine(map[string]any{"event": "status_notify_failed", "errorType": SanitizeErrorType(err)}, "")
		}
	}
}
