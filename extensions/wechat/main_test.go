package wechat

import (
	"io"
	"os"
	"testing"
)

// TestMain turns the trace on, as the tests check it, and keeps its lines
// out of test output unless WECHAT_TRACE is set.
func TestMain(m *testing.M) {
	traceOn = true
	if os.Getenv("WECHAT_TRACE") == "" {
		traceSink = io.Discard
	}
	os.Exit(m.Run())
}
