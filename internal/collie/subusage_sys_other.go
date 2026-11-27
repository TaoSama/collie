//go:build !darwin

package collie

import (
	"context"
	"net/http"
	"net/url"
)

func subUsageProxyFunc(req *http.Request) (*url.URL, error) {
	return http.ProxyFromEnvironment(req)
}

func readLocalKeychainSecret(_ context.Context, _ string) ([]byte, bool) {
	return nil, false
}
