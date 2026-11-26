package collie

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"
)

// A node joins this computer to a Collie hub. It dials out to the hub (so
// laptops behind NAT work, and no port is opened here), keeps one multiplexed
// connection, and serves two kinds of streams the hub opens on it:
//
//	"herdr" — piped straight to this computer's herdr socket
//	"http"  — this computer's own pane API: chat transcripts, screens,
//	          directories, launching agents
//	"ext"   — an extension that runs here (ext_proc.go)
type nodeConfig struct {
	Hub   string `json:"hub"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

func nodeConfigPath() string { return filepath.Join(collieDir(), "node.json") }

func loadNodeConfig() (nodeConfig, bool) {
	var c nodeConfig
	b, err := os.ReadFile(nodeConfigPath())
	if err != nil || json.Unmarshal(b, &c) != nil || c.Hub == "" || c.Token == "" {
		return c, false
	}
	return c, true
}

func runNode(args []string) {
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	host = strings.TrimSuffix(host, ".local")
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	hub := fs.String("hub", "", "hub address, e.g. http://10.0.0.5:7788")
	code := fs.String("code", "", "pairing code shown by the hub (first time only)")
	name := fs.String("name", host, "how this computer is shown on the hub")
	pairOnly := fs.Bool("pair-only", false, "join the hub and exit (a service runs `collie node` afterwards)")
	ui := fs.String("ui", envOr("COLLIE_UI", defaultLocalUI), "serve the hub's web UI on this computer at this address (off: don't)")
	sock := fs.String("socket", envOr("HERDR_SOCKET_PATH", filepath.Join(home, ".config/herdr/herdr.sock")), "herdr API socket")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: collie node --hub <address> --code <pairing code>   (first time)\n       collie node                                          (afterwards)\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	os.MkdirAll(collieDir(), 0o700)

	cfg, ok := loadNodeConfig()
	if *code != "" {
		if *hub == "" {
			*hub = cfg.Hub
		}
		if *hub == "" {
			fs.Usage()
			os.Exit(2)
		}
		var err error
		if cfg, err = pairNode(*hub, *code, *name); err != nil {
			fmt.Fprintln(os.Stderr, "配对失败：", err)
			os.Exit(1)
		}
		fmt.Printf("  已加入 %s，这台电脑在 hub 上叫「%s」(%s)\n", cfg.Hub, cfg.Name, cfg.ID)
		if *pairOnly {
			return
		}
	} else if !ok {
		fmt.Fprintln(os.Stderr, "这台电脑还没加入 hub。先在 hub 的「设备 → 连接电脑」里拿到命令，形如：\n  collie node --hub http://<hub 地址>:7788 --code <配对码>")
		os.Exit(2)
	} else if *hub != "" {
		cfg.Hub = strings.TrimRight(*hub, "/")
	}

	// Without --name, keep the name chosen when this computer joined.
	named := false
	fs.Visit(func(f *flag.Flag) { named = named || f.Name == "name" })
	if !named && cfg.Name != "" {
		*name = cfg.Name
	}

	bin, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(bin); err == nil {
		bin = r
	}
	bin = stableBin(bin, collieDir())
	srv := &Server{herdr: newHerdr(*sock, host, home), home: home, node: true}
	srv.herdr.logged = srv.promptLogged
	api := srv.NodeRoutes()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go maintainCodexCache(ctx)
	nodeUI = startLocalUI(ctx, *ui, cfg.Hub, srv, api)
	srv.sysmon = startSysmon(ctx, home)
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := nodeSession(ctx, cfg, *name, home, bin, *sock, api)
		if errors.Is(err, errRevoked) {
			fmt.Fprintln(os.Stderr, "hub 拒绝了这台电脑（可能已在设备列表里移除）。重新配对：collie node --hub", cfg.Hub, "--code <新配对码>")
			os.Exit(1)
		}
		if ctx.Err() != nil {
			break
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		log.Printf("hub 连接断开：%v；%s 后重连", err, backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func pairNode(hub, code, name string) (nodeConfig, error) {
	hub = strings.TrimRight(strings.TrimSpace(hub), "/")
	if !strings.Contains(hub, "://") {
		hub = "http://" + hub
	}
	body, _ := json.Marshal(map[string]string{"code": code, "kind": "machine", "name": name, "os": runtime.GOOS + "/" + runtime.GOARCH})
	// Dial the hub directly, as nodeSession does: a sandbox's http_proxy
	// usually can't reach a hub on the private network and answers 502.
	direct := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := direct.Post(hub+"/api/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		return nodeConfig{}, fmt.Errorf("连不上 hub %s：%v", hub, err)
	}
	defer resp.Body.Close()
	var out struct {
		ID    string `json:"id"`
		Token string `json:"token"`
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 || out.Token == "" {
		if out.Error == "" {
			// The hub always explains itself in JSON; a bare status came from
			// something in between (a sandbox egress gateway, a proxy).
			out.Error = resp.Status + "：这不是 hub 的回复，是中间的网关或代理回的"
			if sv := resp.Header.Get("Server"); sv != "" {
				out.Error += "（Server: " + sv + "）"
			}
			out.Error += "。这台机器可能连不到 " + hub + "，试试 curl --noproxy '*' " + hub + "（能打开 hub 页面才对）"
		}
		return nodeConfig{}, errors.New(out.Error)
	}
	cfg := nodeConfig{Hub: hub, ID: out.ID, Name: name, Token: out.Token}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(nodeConfigPath(), b, 0o600); err != nil {
		return cfg, err
	}
	return cfg, nil
}

var errRevoked = errors.New("revoked")

// hubProxy picks the proxy for the hub's address; a variable for tests.
var hubProxy = http.ProxyFromEnvironment

// dialHub opens the TCP connection to the hub at addr, through the HTTP proxy
// the environment names for it (HTTPS_PROXY / HTTP_PROXY, minus NO_PROXY),
// for networks that only reach the internet that way.
func dialHub(ctx context.Context, d *net.Dialer, hub *url.URL, addr string) (net.Conn, error) {
	proxy, err := hubProxy(&http.Request{URL: hub})
	if err != nil {
		return nil, fmt.Errorf("proxy setting: %w", err)
	}
	if proxy == nil {
		return d.DialContext(ctx, "tcp", addr)
	}
	paddr := proxy.Host
	if proxy.Port() == "" {
		port := "80"
		if proxy.Scheme == "https" {
			port = "443"
		}
		paddr = net.JoinHostPort(proxy.Hostname(), port)
	}
	conn, err := d.DialContext(ctx, "tcp", paddr)
	if err != nil {
		return nil, fmt.Errorf("proxy %s: %w", paddr, err)
	}
	if proxy.Scheme == "https" {
		conn = tls.Client(conn, &tls.Config{ServerName: proxy.Hostname()})
	}
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	req := &http.Request{Method: "CONNECT", URL: &url.URL{Opaque: addr}, Host: addr, Header: http.Header{}}
	if pu := proxy.User; pu != nil {
		pw, _ := pu.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(pu.Username()+":"+pw)))
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("proxy %s: %w", paddr, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("proxy %s: %w", paddr, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("proxy %s: CONNECT %s: %s", paddr, addr, resp.Status)
	}
	conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		return &bufConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

func nodeSession(ctx context.Context, cfg nodeConfig, name, home, bin, sock string, api http.Handler) error {
	u, err := url.Parse(cfg.Hub)
	if err != nil {
		return err
	}
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), map[string]string{"https": "443"}[u.Scheme])
		if u.Scheme != "https" {
			addr = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := dialHub(ctx, &d, u, addr)
	if err != nil {
		return err
	}
	if u.Scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: u.Hostname()})
		hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = tc.HandshakeContext(hctx)
		cancel()
		if err != nil {
			conn.Close()
			return err
		}
		conn = tc
	}
	req, _ := http.NewRequest("GET", strings.TrimRight(cfg.Hub, "/")+nodeConnectPath, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", nodeUpgradeProtocol)
	req.Header.Set("X-Collie-Name", name)
	req.Header.Set("X-Collie-Home", home)
	req.Header.Set("X-Collie-OS", runtime.GOOS+"/"+runtime.GOARCH)
	req.Header.Set("X-Collie-Bin", bin)
	req.Header.Set("X-Collie-Version", version)
	req.Header.Set("X-Collie-Agents", agentsHeader(localAgents(home, true)))
	if nodeUI != "" {
		req.Header.Set("X-Collie-UI", nodeUI)
	}
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := req.Write(conn); err != nil {
		conn.Close()
		return err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return err
	}
	if resp.StatusCode == 401 {
		conn.Close()
		return errRevoked
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return fmt.Errorf("hub answered %s", resp.Status)
	}
	conn.SetDeadline(time.Time{})

	sess, err := yamux.Server(&bufConn{Conn: conn, r: br}, nodeTunnelConfig())
	if err != nil {
		conn.Close()
		return err
	}
	defer sess.Close()
	log.Printf("已连接 hub %s（本机：%s）", cfg.Hub, name)

	ln := &chanListener{ch: make(chan net.Conn), done: make(chan struct{}), addr: conn.LocalAddr()}
	hs := &http.Server{Handler: api, ReadHeaderTimeout: 10 * time.Second}
	go hs.Serve(ln)
	defer hs.Close()
	go func() { <-ctx.Done(); sess.Close() }()

	for {
		st, err := sess.Accept()
		if err != nil {
			ln.Close()
			return err
		}
		go func(st net.Conn) {
			kind, err := readLine(st)
			if err != nil {
				st.Close()
				return
			}
			switch kind {
			case nodeStreamHerdr:
				h, err := dialHerdr(context.Background(), sock)
				if err != nil {
					log.Printf("herdr: %v", err)
					st.Close()
					return
				}
				go func() { io.Copy(h, st); h.(*net.UnixConn).CloseWrite() }()
				io.Copy(st, h)
				h.Close()
				st.Close()
			case nodeStreamHTTP:
				select {
				case ln.ch <- st:
				case <-ln.done:
					st.Close()
				}
			case nodeStreamExt:
				localExtProcs.serve(st, collieDir())
			default:
				st.Close()
			}
		}(st)
	}
}

// readLine reads one "\n"-terminated header without buffering past it.
func readLine(c net.Conn) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for len(b) < 32 {
		if _, err := c.Read(one); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return string(b), nil
		}
		b = append(b, one[0])
	}
	return "", errors.New("header too long")
}

// chanListener hands tunnel streams to an http.Server.
type chanListener struct {
	ch   chan net.Conn
	done chan struct{}
	addr net.Addr
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }
