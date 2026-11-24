package collie

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// defaultLocalUI is where a node serves the hub's web UI on its own computer.
const defaultLocalUI = "127.0.0.1:17788"

// nodeUI is this node's local UI address, told to the hub when connecting.
var nodeUI string

// startLocalUI makes the hub's web UI available at http://127.0.0.1:<port> on
// this computer, and returns that address ("" if it isn't serving). Browsers
// treat 127.0.0.1 as a secure origin, so Chrome offers "Install Collie" (a
// Dock app) and notifications work, which they don't on the hub's plain-HTTP
// LAN address. Each browser still signs in on its own; this only forwards.
//
// /direct/… is not forwarded: it is this node's own terminal routes, for a
// browser on this computer (see direct.go).
func startLocalUI(ctx context.Context, addr, hub string, srv *Server, api http.Handler) string {
	if addr == "" || addr == "off" {
		return ""
	}
	target, err := url.Parse(hub)
	if err != nil || target.Host == "" {
		log.Printf("本机界面：hub 地址不对（%q）", hub)
		return ""
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Header.Del("X-Forwarded-For")
		},
		FlushInterval: -1, // event streams (SSE) pass through as they come
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("连不上 hub（" + hub + "）：" + err.Error() + "\n"))
		},
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("本机界面没有启动（%s）：%v", addr, err)
		return ""
	}
	port := ln.Addr().(*net.TCPAddr).Port
	local := fmt.Sprintf("http://127.0.0.1:%d", port)
	log.Printf("本机界面：%s （转发到 %s；在 Chrome 里可以安装成 app）", local, hub)
	mux := http.NewServeMux()
	mux.Handle("/", proxy)
	mux.Handle("/direct/", srv.directHandler(hub, local, api))
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		hs.Shutdown(sc)
	}()
	go func() {
		if err := hs.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("本机界面停止：%v", err)
		}
	}()
	return local
}

// localApp: if the browser asking runs on a computer with a local UI (a node
// that connects from the same address, or the hub's own machine), where it is.
// A page on the LAN address can't look at 127.0.0.1 itself.
func (s *Server) localApp(w http.ResponseWriter, r *http.Request) {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip == "" || net.ParseIP(ip).IsLoopback() {
		writeJSON(w, 200, map[string]any{})
		return
	}
	ms := s.herdr.Machines()
	for _, m := range ms {
		if !m.Local && m.Online && m.ui != "" && m.ip == ip {
			writeJSON(w, 200, map[string]any{"url": m.ui, "machine": m.Name})
			return
		}
	}
	if addrs, err := net.InterfaceAddrs(); err == nil && s.port != 0 {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.String() == ip {
				writeJSON(w, 200, map[string]any{"url": fmt.Sprintf("http://127.0.0.1:%d", s.port), "machine": ms[0].Name})
				return
			}
		}
	}
	writeJSON(w, 200, map[string]any{})
}
