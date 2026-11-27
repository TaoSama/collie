//go:build darwin

package collie

import (
	"context"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

var darwinProxyCache struct {
	mu    sync.Mutex
	at    time.Time
	http  *url.URL
	https *url.URL
}

// subUsageProxyFunc respects environment proxy variables first, then falls back
// to macOS System Proxy (scutil --proxy) so launchd background nodes work
// seamlessly when the user runs a local proxy client.
func subUsageProxyFunc(req *http.Request) (*url.URL, error) {
	if u, err := http.ProxyFromEnvironment(req); err != nil || u != nil {
		return u, err
	}
	hp, sp := readDarwinSystemProxy()
	if req.URL != nil && strings.EqualFold(req.URL.Scheme, "https") && sp != nil {
		return sp, nil
	}
	return hp, nil
}

func readDarwinSystemProxy() (httpProxy, httpsProxy *url.URL) {
	darwinProxyCache.mu.Lock()
	defer darwinProxyCache.mu.Unlock()
	if time.Since(darwinProxyCache.at) < time.Minute {
		return darwinProxyCache.http, darwinProxyCache.https
	}
	darwinProxyCache.at = time.Now()
	darwinProxyCache.http = nil
	darwinProxyCache.https = nil

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--proxy").Output()
	if err != nil {
		return nil, nil
	}
	kv := keyValues(string(out), ":")
	if kv["HTTPEnable"] == "1" {
		if u := buildLocalProxyURL(kv["HTTPProxy"], kv["HTTPPort"]); u != nil {
			darwinProxyCache.http = u
		}
	}
	if kv["HTTPSEnable"] == "1" {
		if u := buildLocalProxyURL(kv["HTTPSProxy"], kv["HTTPSPort"]); u != nil {
			darwinProxyCache.https = u
		}
	}
	if darwinProxyCache.https == nil {
		darwinProxyCache.https = darwinProxyCache.http
	}
	return darwinProxyCache.http, darwinProxyCache.https
}

func buildLocalProxyURL(host, port string) *url.URL {
	port = strings.TrimSpace(port)
	if n, err := strconv.Atoi(port); err != nil || n <= 0 || n > 65535 {
		return nil
	}
	host = strings.TrimSpace(host)
	if host == "" {
		host = "127.0.0.1"
	}
	u, err := url.Parse("http://" + host + ":" + port)
	if err != nil {
		return nil
	}
	return u
}

// readLocalKeychainSecret reads a generic password from macOS Keychain.
// Never prints or logs the returned bytes.
func readLocalKeychainSecret(ctx context.Context, service string) ([]byte, bool) {
	return readLocalKeychainAccountSecret(ctx, service, "")
}

// readLocalKeychainAccountSecret reads a generic password from macOS Keychain,
// optionally narrowed to a specific account (-a). Never prints or logs the returned bytes.
func readLocalKeychainAccountSecret(ctx context.Context, service, account string) ([]byte, bool) {
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	args := []string{"find-generic-password", "-s", service}
	if account != "" {
		args = append(args, "-a", account)
	}
	args = append(args, "-w")
	out, err := exec.CommandContext(cctx, "/usr/bin/security", args...).Output()
	if err != nil {
		return nil, false
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, false
	}
	return []byte(trimmed), true
}

// readLocalKeychainMeta reads non-secret metadata (cdat/mdat) for a generic
// password without -w, so macOS does not display an authorization prompt.
func readLocalKeychainMeta(ctx context.Context, service, account string) (keychainMeta, bool) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	args := []string{"find-generic-password", "-s", service}
	if account != "" {
		args = append(args, "-a", account)
	}
	out, err := exec.CommandContext(cctx, "/usr/bin/security", args...).Output()
	if err != nil {
		return keychainMeta{}, false
	}
	return parseKeychainMetaOutput(string(out)), true
}
