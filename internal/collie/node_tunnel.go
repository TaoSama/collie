package collie

import (
	"io"
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
