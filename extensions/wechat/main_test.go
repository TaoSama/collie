package wechat

import (
	"io"
	"os"
	"testing"
)

// TestMain keeps trace lines out of test output unless WECHAT_TRACE is set.
func TestMain(m *testing.M) {
	if os.Getenv("WECHAT_TRACE") == "" {
		traceSink = io.Discard
	}
	os.Exit(m.Run())
}
