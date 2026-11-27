package collie

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Local subscription usage collection: each machine (hub or node) reads only
// its own locally signed-in agent credentials (Claude Code, Codex, Antigravity)
// in two phases so the hub deduplicates accounts BEFORE any upstream API call:
//
//  1. GET /api/sub-usage/inventory — pure local credential & metadata scan
//     (zero upstream network calls; returns non-secret account hashes, tier
//     hints, token expiry timestamps, and today's local token/cache stats).
//  2. GET /api/sub-usage/probe?provider=...&account_hash=... — targeted
//     read-only usage query on the single machine selected by the hub for a
//     given account. Never refreshes OAuth tokens and never writes credential
//     files or Keychain entries. Raw tokens never leave the machine.

const (
	subUsageLocalTTL     = 45 * time.Second
	subUsageProviderWait = 10 * time.Second
	subUsageStaleKeep    = 30 * time.Minute
	subUsageMaxRespBytes = 4 << 20
	subUsageExpiryLeeway = time.Minute
)

// subUsageInventory is what GET /api/sub-usage/inventory returns (Phase 1, zero network calls).
type subUsageInventory struct {
	CollectedAt string                  `json:"collected_at"`
	Today       tokenDayStats           `json:"today,omitempty"`
	Accounts    []subUsageInventoryItem `json:"accounts"`
}

// subUsageInventoryItem describes one locally signed-in agent subscription
// without making any upstream network call and without exposing any credentials.
type subUsageInventoryItem struct {
	Provider       string `json:"provider"` // claude | codex | antigravity
	AccountHash    string `json:"account_hash,omitempty"`
	Email          string `json:"email,omitempty"`
	Plan           string `json:"plan,omitempty"`
	PlanRaw        string `json:"plan_raw,omitempty"`
	PlanDisplay    string `json:"plan_display,omitempty"`
	TokenValid     bool   `json:"token_valid"`                // locally checked access token validity
	TokenExpiresAt string `json:"token_expires_at,omitempty"` // RFC3339 when known
	HasSnapshot    bool   `json:"has_snapshot,omitempty"`     // local offline snapshot available (e.g. Codex rollout)
}

// subUsageProbeResult is what GET /api/sub-usage/probe returns (Phase 2).
type subUsageProbeResult struct {
	Account    *subUsageAccount `json:"account,omitempty"`
	RateLimit  bool             `json:"rate_limit,omitempty"`  // HTTP 429
	RetryAfter int              `json:"retry_after,omitempty"` // seconds from Retry-After header
	AuthFailed bool             `json:"auth_failed,omitempty"` // HTTP 401/403 or locally expired
}

// subUsageMachineReport is what GET /api/sub-usage returns for one machine.
type subUsageMachineReport struct {
	CollectedAt string            `json:"collected_at"`
	TodayTokens int64             `json:"today_tokens,omitempty"`
	Today       tokenDayStats     `json:"today,omitempty"`
	Accounts    []subUsageAccount `json:"accounts"`
}

// subUsageAccount is one locally signed-in agent subscription on one machine.
// It contains only display metadata and quota numbers — never credentials.
type subUsageAccount struct {
	Provider    string           `json:"provider"` // claude | codex | antigravity
	AccountHash string           `json:"account_hash,omitempty"`
	Label       string           `json:"label,omitempty"`
	Email       string           `json:"email,omitempty"`
	Plan        string           `json:"plan,omitempty"`
	PlanRaw     string           `json:"plan_raw,omitempty"`
	PlanDisplay string           `json:"plan_display,omitempty"`
	Status      string           `json:"status"` // ok | limited | expired | error
	Error       string           `json:"error,omitempty"`
	AvailableAt string           `json:"available_at,omitempty"` // RFC3339 when limited/cooldown
	Windows     []subUsageWindow `json:"windows,omitempty"`
	Credit      *subUsageCredit  `json:"credit,omitempty"`
}

// subUsageWindow is one quota window (e.g. 5H, 7D, Gemini 5H, Partner 5H).
type subUsageWindow struct {
	Kind        string  `json:"kind"`
	Label       string  `json:"label"`
	Scope       string  `json:"scope,omitempty"` // "" (account-wide) | "model"
	ScopedModel string  `json:"scoped_model,omitempty"`
	UsedPercent float64 `json:"used_percent"` // 0..100
	ResetsAt    string  `json:"resets_at,omitempty"`
	CheckedAt   string  `json:"checked_at,omitempty"`
}

// subUsageCredit represents an optional credit balance or overage pool.
type subUsageCredit struct {
	Left  float64 `json:"left"`
	Limit float64 `json:"limit,omitempty"`
	Unit  string  `json:"unit,omitempty"` // credits | USD
}

// subUsageCollector gathers local agent subscription inventory & targeted probes.
type subUsageCollector struct {
	home   string
	sysmon *sysMonitor
	client *http.Client
	now    func() time.Time

	claudeURL string
	codexURL  string
	agyURLs   []string

	mu           sync.Mutex
	cached       *subUsageMachineReport
	cachedAt     time.Time
	lastGood     map[string]cachedSubAccount // provider → last good reading for transient fallback
	proxyClients map[string]*http.Client     // normalized proxy spec → cached http.Client
	inFlight     chan struct{}
}

type subUsageProxyCtxKey struct{}

type cachedSubAccount struct {
	acct subUsageAccount
	at   time.Time
}

func newSubUsageCollector(home string, sysmon *sysMonitor) *subUsageCollector {
	return &subUsageCollector{
		home:         home,
		sysmon:       sysmon,
		client:       subUsageHTTPClient(subUsageProviderWait),
		now:          time.Now,
		claudeURL:    defaultClaudeUsageURL,
		codexURL:     defaultCodexUsageURL,
		agyURLs:      append([]string(nil), defaultAgyBaseURLs...),
		lastGood:     map[string]cachedSubAccount{},
		proxyClients: map[string]*http.Client{},
	}
}

func (s *Server) localSubUsageCollector() *subUsageCollector {
	s.subUsageMu.Lock()
	defer s.subUsageMu.Unlock()
	if s.subUsage == nil {
		s.subUsage = newSubUsageCollector(s.home, s.sysmon)
	}
	return s.subUsage
}

func (s *Server) getSubUsage(w http.ResponseWriter, r *http.Request) {
	fresh := r.URL.Query().Get("fresh") == "1" || (r.URL.Query().Has("fresh") && r.URL.Query().Get("fresh") == "")
	rep := s.localSubUsageCollector().collect(r.Context(), fresh)
	writeJSON(w, 200, rep)
}

func (s *Server) getSubUsageInventory(w http.ResponseWriter, r *http.Request) {
	inv := s.localSubUsageCollector().inventory(r.Context())
	writeJSON(w, 200, inv)
}

func (s *Server) getSubUsageProbe(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prov := strings.ToLower(strings.TrimSpace(q.Get("provider")))
	hash := strings.ToLower(strings.TrimSpace(q.Get("account_hash")))
	proxy := strings.TrimSpace(q.Get("proxy"))
	res := s.localSubUsageCollector().probeOne(r.Context(), prov, hash, proxy)
	writeJSON(w, 200, res)
}

// inventory performs Phase 1: pure local file/Keychain inspection with zero
// upstream network calls.
func (c *subUsageCollector) inventory(ctx context.Context) subUsageInventory {
	now := c.now()
	var items []subUsageInventoryItem
	if it := c.inventoryClaude(ctx, now); it != nil {
		items = append(items, *it)
	}
	if it := c.inventoryCodex(ctx, now); it != nil {
		items = append(items, *it)
	}
	if it := c.inventoryAntigravity(now); it != nil {
		items = append(items, *it)
	}
	if items == nil {
		items = []subUsageInventoryItem{}
	}
	var today tokenDayStats
	if c.sysmon != nil && c.sysmon.meter != nil {
		today = c.sysmon.meter.todayDetail(now)
	}
	return subUsageInventory{
		CollectedAt: now.UTC().Format(time.RFC3339),
		Today:       today,
		Accounts:    items,
	}
}

// probeOne performs Phase 2: targeted read-only usage fetch for a single
// provider/account_hash on this machine. Never refreshes tokens or writes files.
func (c *subUsageCollector) probeOne(ctx context.Context, provider, expectedHash, proxySpec string) subUsageProbeResult {
	now := c.now()
	pctx, cancel := context.WithTimeout(ctx, subUsageProviderWait)
	defer cancel()
	if proxySpec = normalizeSubUsageProxyURL(proxySpec); proxySpec != "" {
		pctx = context.WithValue(pctx, subUsageProxyCtxKey{}, proxySpec)
	}

	var res subUsageProbeResult
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		res = c.probeClaude(pctx, now, expectedHash)
	case "codex":
		res = c.probeCodex(pctx, now, expectedHash)
	case "antigravity":
		res = c.probeAntigravity(pctx, now, expectedHash)
	default:
		return subUsageProbeResult{}
	}
	if res.Account != nil {
		res.Account.Error = sanitizeSubUsageText(res.Account.Error)
	}
	return res
}

func (c *subUsageCollector) collect(ctx context.Context, fresh bool) subUsageMachineReport {
	for {
		c.mu.Lock()
		now := c.now()
		if !fresh && c.cached != nil && now.Sub(c.cachedAt) < subUsageLocalTTL {
			out := cloneMachineReport(*c.cached)
			c.mu.Unlock()
			return out
		}
		if c.inFlight != nil {
			wait := c.inFlight
			c.mu.Unlock()
			select {
			case <-wait:
				fresh = false
				continue
			case <-ctx.Done():
				c.mu.Lock()
				if c.cached != nil {
					out := cloneMachineReport(*c.cached)
					c.mu.Unlock()
					return out
				}
				c.mu.Unlock()
				return subUsageMachineReport{CollectedAt: now.UTC().Format(time.RFC3339), Accounts: []subUsageAccount{}}
			}
		}
		done := make(chan struct{})
		c.inFlight = done
		c.mu.Unlock()

		rep := c.probeAll(ctx)

		c.mu.Lock()
		c.cached = &rep
		c.cachedAt = c.now()
		c.inFlight = nil
		close(done)
		out := cloneMachineReport(rep)
		c.mu.Unlock()
		return out
	}
}

func (c *subUsageCollector) probeAll(ctx context.Context) subUsageMachineReport {
	now := c.now()
	providers := []string{"claude", "codex", "antigravity"}
	results := make([]*subUsageAccount, len(providers))
	var wg sync.WaitGroup
	for i, p := range providers {
		wg.Add(1)
		go func(idx int, prov string) {
			defer wg.Done()
			results[idx] = c.probeOne(ctx, prov, "", "").Account
		}(i, p)
	}
	wg.Wait()

	accounts := make([]subUsageAccount, 0, len(providers))
	c.mu.Lock()
	for _, a := range results {
		if a == nil {
			continue
		}
		a.Error = sanitizeSubUsageText(a.Error)
		if a.Status == "ok" || a.Status == "limited" {
			if len(a.Windows) > 0 || a.Credit != nil {
				c.lastGood[a.Provider] = cachedSubAccount{acct: cloneSubAccount(*a), at: now}
			}
		} else if a.Status == "error" && len(a.Windows) == 0 {
			if prev, ok := c.lastGood[a.Provider]; ok && now.Sub(prev.at) <= subUsageStaleKeep {
				fallback := cloneSubAccount(prev.acct)
				if a.Error != "" {
					fallback.Error = "使用缓存：" + a.Error
				}
				a = &fallback
			}
		}
		accounts = append(accounts, *a)
	}
	c.mu.Unlock()

	var today tokenDayStats
	if c.sysmon != nil && c.sysmon.meter != nil {
		today = c.sysmon.meter.todayDetail(now)
	}
	return subUsageMachineReport{
		CollectedAt: now.UTC().Format(time.RFC3339),
		TodayTokens: today.Tokens,
		Today:       today,
		Accounts:    accounts,
	}
}

func cloneMachineReport(r subUsageMachineReport) subUsageMachineReport {
	out := r
	out.Today = cloneTokenDayStats(r.Today)
	out.Accounts = make([]subUsageAccount, len(r.Accounts))
	for i, a := range r.Accounts {
		out.Accounts[i] = cloneSubAccount(a)
	}
	return out
}

func cloneSubAccount(a subUsageAccount) subUsageAccount {
	out := a
	if len(a.Windows) > 0 {
		out.Windows = append([]subUsageWindow(nil), a.Windows...)
	}
	if a.Credit != nil {
		cp := *a.Credit
		out.Credit = &cp
	}
	return out
}

func subUsageHTTPClient(timeout time.Duration) *http.Client {
	return subUsageHTTPClientWithProxyFn(timeout, subUsageProxyFunc)
}

func subUsageHTTPClientWithProxyFn(timeout time.Duration, proxyFn func(*http.Request) (*url.URL, error)) *http.Client {
	tr := &http.Transport{
		Proxy:                 proxyFn,
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (c *subUsageCollector) httpClientFor(ctx context.Context) *http.Client {
	proxySpec, _ := ctx.Value(subUsageProxyCtxKey{}).(string)
	if proxySpec == "" {
		return c.client
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.proxyClients == nil {
		c.proxyClients = map[string]*http.Client{}
	}
	if cli, ok := c.proxyClients[proxySpec]; ok {
		return cli
	}
	var proxyFn func(*http.Request) (*url.URL, error)
	if !strings.EqualFold(proxySpec, "direct") {
		u, err := url.Parse(proxySpec)
		if err != nil {
			return c.client
		}
		proxyFn = http.ProxyURL(u)
	}
	cli := subUsageHTTPClientWithProxyFn(subUsageProviderWait, proxyFn)
	if len(c.proxyClients) >= 8 {
		clear(c.proxyClients)
	}
	c.proxyClients[proxySpec] = cli
	return cli
}

func (c *subUsageCollector) doJSONWithHeaders(ctx context.Context, req *http.Request) (int, http.Header, []byte, error) {
	resp, err := c.httpClientFor(ctx).Do(req.WithContext(ctx))
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, subUsageMaxRespBytes))
	if err != nil {
		return resp.StatusCode, resp.Header, nil, err
	}
	return resp.StatusCode, resp.Header, body, nil
}

func parseRetryAfterSeconds(hdr http.Header, now time.Time) int {
	if hdr == nil {
		return 0
	}
	v := strings.TrimSpace(hdr.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
		return sec
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return int(math.Ceil(t.Sub(now).Seconds()))
	}
	return 0
}

func hashAccountKey(provider, canonical string) string {
	canonical = strings.ToLower(strings.TrimSpace(canonical))
	if canonical == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(provider)) + ":" + canonical))
	return hex.EncodeToString(sum[:8])
}

func clampPct(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

var (
	secretBearerRe = regexp.MustCompile(`(?i)\b(bearer|token)\s+[A-Za-z0-9._\-+/=]{8,}`)
	secretPrefixRe = regexp.MustCompile(`\b(sk-ant-[A-Za-z0-9_\-]{6,}|sk-[A-Za-z0-9_\-]{12,}|ya29\.[A-Za-z0-9._\-]{8,}|eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}(?:\.[A-Za-z0-9_\-]*)?|1//[A-Za-z0-9_\-]{10,})`)
)

// sanitizeSubUsageText strips any accidental credential substrings from error text.
func sanitizeSubUsageText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = secretBearerRe.ReplaceAllString(s, "$1 [redacted]")
	s = secretPrefixRe.ReplaceAllString(s, "[redacted]")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

func parseSubUsageJSON(data []byte, dst any) error {
	if len(data) == 0 {
		return fmt.Errorf("empty response")
	}
	return json.Unmarshal(data, dst)
}
