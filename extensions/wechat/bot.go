package wechat

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

// start serves c: a gateway over the data directory's state. A new login
// fingerprint empties that state, and a baseline is drained before any
// message is handled.
func (x *extension) start(c Credentials) error {
	x.stopGateway()
	account := &Account{Token: c.Token, AccountID: c.BotID, UserID: c.UserID, BaseURL: c.BaseURL, SavedAt: isoTime(nowMs())}
	x.mu.Lock()
	dir := x.dataDir
	x.mu.Unlock()
	state, err := NewStateManager(account, filepath.Join(dir, stateFileName), PolicyOptions{})
	if err != nil {
		return err
	}
	ilink, err := NewILinkClient(state, x.loop, ILinkOptions{ConfiguredBaseURL: x.opt.baseURL, HTTP: x.opt.http})
	if err != nil {
		return err
	}
	health := &HealthTracker{}
	var g *Gateway
	x.loop.do(func() {
		g, err = NewGateway(GatewayOptions{State: state, ILink: ilink, Host: &peerHost{peer: x.peer, contribution: contributionID},
			DataDir: dir, Health: health, MediaHTTP: x.opt.http})
		if err == nil {
			health.StartRun()
		}
	})
	if err != nil {
		return err
	}
	done := make(chan struct{})
	x.mu.Lock()
	x.creds, x.gateway, x.health, x.runDone = &c, g, health, done
	interest := x.interest
	x.mu.Unlock()
	if interest != nil {
		g.SetInterest(interest)
	}
	x.setState(extproto.StateOK, "")
	go x.run(g, done)
	return nil
}

// run drains the baseline (retrying with backoff) and then polls until the
// gateway stops or the login expires.
func (x *extension) run(g *Gateway, done chan struct{}) {
	defer close(done)
	onError := func(err error) {
		writeLogLine(map[string]any{"event": "poll_failed", "errorType": SanitizeErrorType(err)}, "")
	}
	var err error
	for failures := 0; ; failures++ {
		ready := false
		x.loop.do(func() {
			ready = g.state.IsReadyForActivePolling()
			if !ready && !g.stopped {
				_, err = g.establishBaseline(g.ctx, 0)
				ready = err == nil
			}
		})
		if ready || g.Stopped() || errors.Is(err, ErrSessionExpired) {
			break
		}
		onError(err)
		x.loop.do(func() { g.loop.sleep(g.ctx, msDuration(pollBackoffMs(failures+1))) })
	}
	if err == nil && !g.Stopped() {
		x.wakeOutbox()
		err = g.RunLoop(x.opt.pollEvery, onError, onError)
	}
	if errors.Is(err, ErrSessionExpired) {
		// Collie shows the status and asks for a new scan.
		x.mu.Lock()
		if x.gateway == g {
			x.gateway = nil
		}
		x.mu.Unlock()
		x.setState(extproto.StateNotSignedIn, msgSessionExpired)
		return
	}
	if err != nil && !g.Stopped() {
		x.setState(extproto.StateError, SanitizeErrorType(err))
	}
}

// Stopped reports whether Stop was called.
func (g *Gateway) Stopped() (stopped bool) {
	g.loop.do(func() { stopped = g.stopped })
	return stopped
}

func (x *extension) stopGateway() {
	x.mu.Lock()
	g, done := x.gateway, x.runDone
	x.gateway, x.runDone = nil, nil
	x.mu.Unlock()
	if g == nil {
		return
	}
	g.Stop()
	if done != nil {
		<-done
	}
}

// logout forgets the login here and at Collie; the state stays and is
// emptied by the next login's new fingerprint.
func (x *extension) logout(ctx context.Context) error {
	x.stopGateway()
	x.mu.Lock()
	if x.creds != nil {
		x.oldTokens = rememberToken(x.oldTokens, x.creds.Token)
	}
	x.creds, x.qr = nil, nil
	x.mu.Unlock()
	sctx, cancel := context.WithTimeout(ctx, hostCallTimeout)
	defer cancel()
	if err := x.peer.Call(sctx, extproto.MSecretsDelete, extproto.Secrets{Contribution: contributionID}, nil); err != nil {
		return extproto.Errorf(extproto.ErrInternal, "deleting the login failed: %s", SanitizeErrorType(err))
	}
	x.setState(extproto.StateNotSignedIn, msgNotSignedIn)
	return nil
}

// status is connector.status: the login, the gateway's health and the
// reply window.
func (x *extension) status() extproto.Status {
	x.mu.Lock()
	st := extproto.Status{State: x.state, Error: x.stateError}
	creds, g, health := x.creds, x.gateway, x.health
	x.mu.Unlock()
	if st.State == extproto.StateNotSignedIn {
		st.Guide = []string{"在接入页点「登录」，用微信扫码并在手机上确认。", "只处理扫码那个微信号发给 bot 的消息。"}
		return st
	}
	if creds == nil {
		return st
	}
	// iLink names people only by an id (o9cq…@im.wechat): no nickname to show.
	st.Account, st.AccountID = accountName, creds.UserID
	detail := map[string]any{"绑定人": shortUserID(creds.UserID)}
	if g != nil {
		x.loop.do(func() {
			g.statusDetail(detail, health)
			if h, ok := health.Snapshot(); ok {
				st.LastSync = h.LastPollSucceededAt
				if st.State == extproto.StateOK && h.Status == HealthStarting && h.LastPollSucceededAt == "" {
					st.Error = msgStarting
				}
			}
		})
	}
	st.Detail = detail
	return st
}

// statusDetail adds the gateway's phase, errors and window to detail.
func (g *Gateway) statusDetail(detail map[string]any, health *HealthTracker) {
	if h, ok := health.Snapshot(); ok {
		phase := "空闲"
		if h.Phase != nil {
			phase = map[string]string{"polling": "轮询中", "processing": "处理中"}[h.Phase.Name]
		}
		detail["阶段"] = phase
		detail["连续错误"] = h.ConsecutiveErrors
		if h.LastErrorType != "" {
			detail["上次错误"] = h.LastErrorType
		}
		if h.LastPollSucceededAt != "" {
			detail["上次成功轮询"] = h.LastPollSucceededAt
		}
	}
	if !g.state.IsReadyForActivePolling() {
		detail["基线"] = "建立中"
	}
	if b := g.state.AccountBudget; b != nil {
		left := b.RefreshedAt + g.state.Policy.ContextWindowMs - nowMs()
		if left > 0 {
			detail["回复窗口剩余"] = (time.Duration(left) * time.Millisecond).Round(time.Minute).String()
			detail["剩余额度"] = EffectiveRemaining(b.ContextBudgetState, g.state.Policy)
		} else {
			detail["回复窗口剩余"] = "已过期，等你发一条消息"
			detail["剩余额度"] = 0
		}
	}
	pending := 0
	for _, item := range g.state.Outbox {
		if item.Status == OutboxPending {
			pending++
		}
	}
	if pending > 0 {
		detail["待发回复"] = pending
	}
}

// Main runs the extension on stdin and stdout until stdin closes.
func Main(args []string) int {
	fs := flag.NewFlagSet("ext-wechat", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: collie ext-wechat\n\nRuns Collie's WeChat extension: extproto on stdin and stdout, logs on stderr.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	return serve(context.Background(), os.Stdin, os.Stdout, options{})
}

// serve speaks the protocol on r and w; stdin's end stops the gateway.
func serve(ctx context.Context, r io.Reader, w io.Writer, o options) int {
	x := newExtension(o)
	x.peer = extproto.NewPeer(r, w, "p", x.handle, nil)
	err := x.peer.Serve(ctx)
	x.stopGateway()
	if err != nil && !errors.Is(err, io.EOF) {
		writeLogLine(map[string]any{"event": "protocol_failed", "errorType": SanitizeErrorType(err)}, "")
		return 1
	}
	return 0
}

// accountName is the bound person as people see it: iLink gives no
// nickname, only an id.
const accountName = "你的微信"

// shortUserID keeps an iLink user id's ends: o9cq80…dp5A@im.wechat.
func shortUserID(id string) string {
	local, domain, _ := strings.Cut(id, "@")
	if r := []rune(local); len(r) > 12 {
		local = string(r[:6]) + "…" + string(r[len(r)-4:])
	}
	if domain != "" {
		return local + "@" + domain
	}
	return local
}
