// Package wechat is Collie's built-in WeChat extension: one iLink bot, bound
// by QR code, whose owner's messages go to Collie and whose answers go back
// within WeChat's reply window. It runs as `collie ext-wechat` and speaks
// extproto on stdin and stdout (extensions/README.md).
package wechat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
)

// clockOverride replaces the wall clock in tests; atomic because goroutines
// left by one test (typing signals) may still read it in the next.
var clockOverride atomic.Pointer[func() int64]

// nowMs is the wall clock in milliseconds.
func nowMs() int64 {
	if f := clockOverride.Load(); f != nil {
		return (*f)()
	}
	return time.Now().UnixMilli()
}

// loop makes the gateway's logic run one piece at a time, the way its state
// machine was designed: code between two waits is atomic. Whoever runs
// gateway logic holds mu; it lets go only while it waits on the network, the
// host or a timer (await). Exported entry points take the loop themselves,
// unexported ones expect it held.
type loop struct{ mu sync.Mutex }

func (l *loop) do(f func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f()
}

// await runs a blocking step with the loop released.
func (l *loop) await(f func()) {
	l.mu.Unlock()
	defer l.mu.Lock()
	f()
}

// wait blocks on ch or ctx with the loop released; false when ctx ended first.
func (l *loop) wait(ctx context.Context, ch <-chan struct{}) bool {
	ok := true
	l.await(func() {
		select {
		case <-ch:
		case <-ctx.Done():
			ok = false
		}
	})
	return ok
}

// sleep waits d with the loop released; false when ctx ended first.
func (l *loop) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	ok := true
	l.await(func() {
		select {
		case <-t.C:
		case <-ctx.Done():
			ok = false
		}
	})
	return ok
}

// jsonText is JSON.stringify for identities: arrays of strings and numbers,
// without HTML escaping.
func jsonText(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic("wechat: identity is not JSON: " + err.Error())
	}
	return string(bytes.TrimRight(b.Bytes(), "\n"))
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// hashJSON is sha256(JSON.stringify(v)) in hex.
func hashJSON(v any) string { return sha256Hex(jsonText(v)) }

// utf16Len counts what JavaScript's String.length counts: every limit and
// split point of the protocol is in UTF-16 code units.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func toUTF16(s string) []uint16 { return utf16.Encode([]rune(s)) }

func fromUTF16(u []uint16) string { return string(utf16.Decode(u)) }

func isHighSurrogate(c uint16) bool { return c >= 0xd800 && c <= 0xdbff }
func isLowSurrogate(c uint16) bool  { return c >= 0xdc00 && c <= 0xdfff }

// unicodeSafeBoundary moves end back one unit when it would split a
// surrogate pair.
func unicodeSafeBoundary(u []uint16, end int) int {
	if end > 0 && end < len(u) && isHighSurrogate(u[end-1]) && isLowSurrogate(u[end]) {
		return end - 1
	}
	return end
}

// utf16Prefix is the longest prefix of s within max units, never splitting
// a character.
func utf16Prefix(s string, max int) string {
	u := toUTF16(s)
	if len(u) <= max {
		return s
	}
	return fromUTF16(u[:unicodeSafeBoundary(u, max)])
}

func isoTime(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z") }

func sortStable[T any](s []T, less func(a, b T) bool) {
	sort.SliceStable(s, func(i, j int) bool { return less(s[i], s[j]) })
}

// firstErr keeps the first of several errors.
func firstErr(first, err error) error {
	if first != nil {
		return first
	}
	return err
}
