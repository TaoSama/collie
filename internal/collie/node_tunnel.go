package collie

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/hashicorp/yamux"
)

// The hub and node use these values on the wire.
const (
	nodeConnectPath     = "/api/node/connect"
	nodeUpgradeProtocol = "collie-node"
	nodeStreamHerdr     = "herdr"
	nodeStreamHTTP      = "http"
	nodeStreamExt       = "ext"
)

func nodeTunnelConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.KeepAliveInterval = 15 * time.Second
	return cfg
}

// streamDialer opens a fresh multiplexed stream for one purpose on a node.
func streamDialer(open func() (net.Conn, error), kind string) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		c, err := open()
		if err != nil {
			return nil, err
		}
		if _, err := c.Write([]byte(kind + "\n")); err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	}
}
