package collie

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

const (
	featureUsage          = "usage"
	usageFocusKVKey       = "ext_usage_focus"
	subUsageInventoryWait = 4 * time.Second
	subUsageProbeWait     = 11 * time.Second
	subUsageDefaultTTL    = 300 // 5 minutes default to avoid upstream 429s
	subUsageMinTTL        = 60
	subUsageMaxTTL        = 600
	subUsageManualCool    = 30 * time.Second
	subUsageHubStaleMax   = 30 * time.Minute
)

type subUsageConfig struct {
	PercentMode    string // remaining | used
	ShowTokens     bool
	DedupeAccounts bool
	RefreshEvery   time.Duration
	ProxyURL       string // optional proxy URL or machine=url list
	FocusOverride  string // "" | auto | all | claude | codex | antigravity
}

func parseSubUsageConfig(raw map[string]any) subUsageConfig {
	cfg := subUsageConfig{
		PercentMode:    "remaining",
		ShowTokens:     true,
		DedupeAccounts: true,
		RefreshEvery:   subUsageDefaultTTL * time.Second,
	}
	if s, _ := raw["percent_mode"].(string); strings.EqualFold(s, "used") {
		cfg.PercentMode = "used"
	}
	if b, ok := raw["show_tokens"].(bool); ok {
		cfg.ShowTokens = b
	}
	if b, ok := raw["dedupe_accounts"].(bool); ok {
		cfg.DedupeAccounts = b
	}
	if n, ok := subUsageInt(raw["refresh_s"]); ok {
		if n < subUsageMinTTL {
			n = subUsageMinTTL
		}
		if n > subUsageMaxTTL {
			n = subUsageMaxTTL
		}
		cfg.RefreshEvery = time.Duration(n) * time.Second
	}
	if s, _ := raw["proxy_url"].(string); strings.TrimSpace(s) != "" {
		cfg.ProxyURL = strings.TrimSpace(s)
	}
	return cfg
}

func normalizeSubUsageProxyURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.EqualFold(s, "direct") || strings.EqualFold(s, "none") {
		return "direct"
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return u.String()
	default:
		return ""
	}
}

// resolveSubUsageProxy resolves either a global proxy URL or a comma/newline
// list of machine=url entries (with optional *=url or bare url fallback).
func resolveSubUsageProxy(spec, machineID, machineName string, local bool) string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ""
	}
	var fallback string
	for _, part := range strings.FieldsFunc(spec, func(r rune) bool {
		return r == ',' || r == '，' || r == ';' || r == '\n'
	}) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if k, v, ok := strings.Cut(part, "="); ok && !strings.Contains(k, "://") {
			k = strings.TrimSpace(k)
			norm := normalizeSubUsageProxyURL(v)
			if k == "" || norm == "" {
				continue
			}
			if k == "*" || strings.EqualFold(k, "default") {
				fallback = norm
				continue
			}
			if (machineName != "" && strings.EqualFold(k, machineName)) ||
				(machineID != "" && strings.EqualFold(k, machineID)) ||
				(local && (strings.EqualFold(k, "hub") || strings.EqualFold(k, "local"))) {
				return norm
			}
			continue
		}
		if norm := normalizeSubUsageProxyURL(part); norm != "" {
			fallback = norm
		}
	}
	return fallback
}

func subUsageInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case int64:
		return int(x), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		return n, err == nil
	default:
		return 0, false
	}
}

type subUsageMachineNode struct {
	ID          string
	Name        string
	Local       bool
	Online      bool
	Unsupported bool // older node without /api/sub-usage/inventory
	Error       string
	Today       tokenDayStats
	Report      subUsageMachineReport
}

type subUsageSnapshot struct {
	CollectedAt time.Time
	Machines    []subUsageMachineNode
}

type acctCacheEntry struct {
	acct           subUsageAccount
	fetchedAt      time.Time
	lastGood       *subUsageAccount
	lastGoodAt     time.Time
	lastMachine    string
	backoffUntil   time.Time
	rateLimitCount int
	failCount      int
}

// subUsageHub orchestrates two-phase dedupe-before-probe collection across
// hub + every online node:
//  1. Pure local inventory (GET /api/sub-usage/inventory, zero upstream calls)
//  2. Deduplicate by (provider, account_hash) and pick 1 valid machine per account
//  3. Targeted probe (GET /api/sub-usage/probe) on the 1 selected machine, with
//     failover to another machine of the same account on 401/network failure,
//     exponential backoff on 429/5xx, and zero OAuth refresh side effects.
type subUsageHub struct {
	s   *Server
	now func() time.Time

	mu          sync.Mutex
	acctState   map[string]*acctCacheEntry // account key → cached probe state
	authBlocked map[string]string          // key+"@"+machineID → blocked token_expires_at marker
	inFlight    chan struct{}
	lastSnap    *subUsageSnapshot
}

func (s *Server) usageHub() *subUsageHub {
	s.subUsageMu.Lock()
	defer s.subUsageMu.Unlock()
	if s.subUsageAgg == nil {
		s.subUsageAgg = &subUsageHub{
			s:           s,
			now:         time.Now,
			acctState:   map[string]*acctCacheEntry{},
			authBlocked: map[string]string{},
		}
	}
	return s.subUsageAgg
}

func (u *subUsageHub) resetCache() {
	u.mu.Lock()
	clear(u.acctState)
	clear(u.authBlocked)
	u.lastSnap = nil
	u.mu.Unlock()
}

func (u *subUsageHub) collect(ctx context.Context, cfg subUsageConfig, fresh bool) subUsageSnapshot {
	for {
		u.mu.Lock()
		if u.inFlight != nil {
			wait := u.inFlight
			u.mu.Unlock()
			select {
			case <-wait:
				fresh = false
				continue
			case <-ctx.Done():
				u.mu.Lock()
				if u.lastSnap != nil {
					out := cloneUsageSnapshot(*u.lastSnap)
					u.mu.Unlock()
					return out
				}
				u.mu.Unlock()
				return subUsageSnapshot{CollectedAt: u.now()}
			}
		}
		done := make(chan struct{})
		u.inFlight = done
		u.mu.Unlock()

		snap := u.runTwoPhase(ctx, cfg, fresh)

		u.mu.Lock()
		u.lastSnap = &snap
		u.inFlight = nil
		close(done)
		out := cloneUsageSnapshot(snap)
		u.mu.Unlock()
		return out
	}
}

type invTarget struct {
	machine Machine
	node    subUsageMachineNode
	inv     subUsageInventory
	legacy  bool // node only supports legacy /api/sub-usage
}

type probeCandidate struct {
	machineID   string
	machineName string
	local       bool
	item        subUsageInventoryItem
	expiresAt   time.Time
}

func accountGroupKey(prov, accountHash, machineID string, dedupe bool) string {
	prov = strings.ToLower(strings.TrimSpace(prov))
	h := strings.ToLower(strings.TrimSpace(accountHash))
	if dedupe && h != "" {
		return prov + ":" + h
	}
	return prov + "@m:" + machineID + ":" + h
}

func (u *subUsageHub) runTwoPhase(ctx context.Context, cfg subUsageConfig, fresh bool) subUsageSnapshot {
	now := u.now()
	var targets []Machine
	if u.s != nil && u.s.herdr != nil {
		for _, m := range u.s.herdr.Machines() {
			if m.Local || m.Online {
				targets = append(targets, m)
			}
		}
	}

	// Phase 1: Pure local inventory across all online machines (zero upstream calls).
	invResults := make([]invTarget, len(targets))
	var wg sync.WaitGroup
	for i, m := range targets {
		wg.Add(1)
		go func(idx int, mc Machine) {
			defer wg.Done()
			name := mc.Name
			if mc.Local && name == "" {
				name = "hub"
			}
			t := invTarget{
				machine: mc,
				node: subUsageMachineNode{
					ID:     mc.ID,
					Name:   name,
					Local:  mc.Local,
					Online: true,
				},
			}
			if mc.Local {
				t.inv = u.s.localSubUsageCollector().inventory(ctx)
				t.node.Today = t.inv.Today
				invResults[idx] = t
				return
			}
			inv, statusCode, err := u.fetchNodeInventory(ctx, mc.ID)
			if err != nil {
				t.node.Error = "节点读取超时或隧道断开"
				invResults[idx] = t
				return
			}
			if statusCode == http.StatusNotFound {
				t.node.Unsupported = true
				t.node.Error = "节点版本较旧，升级后支持订阅用量"
				invResults[idx] = t
				return
			}
			if statusCode < 200 || statusCode >= 300 {
				t.node.Error = fmt.Sprintf("节点返回 HTTP %d", statusCode)
				invResults[idx] = t
				return
			}
			t.inv = inv
			t.node.Today = inv.Today
			invResults[idx] = t
		}(i, m)
	}
	wg.Wait()

	// Deduplicate accounts across machines BEFORE sending any upstream probe.
	groups := map[string][]probeCandidate{}
	var groupKeys []string
	for _, r := range invResults {
		if r.node.Error != "" || r.node.Unsupported {
			continue
		}
		for _, it := range r.inv.Accounts {
			prov := strings.ToLower(strings.TrimSpace(it.Provider))
			if prov == "" {
				continue
			}
			it.Provider = prov
			k := accountGroupKey(prov, it.AccountHash, r.node.ID, cfg.DedupeAccounts)
			if _, exists := groups[k]; !exists {
				groupKeys = append(groupKeys, k)
			}
			exp, _ := parseSubTime(it.TokenExpiresAt)
			groups[k] = append(groups[k], probeCandidate{
				machineID:   r.node.ID,
				machineName: r.node.Name,
				local:       r.node.Local,
				item:        it,
				expiresAt:   exp,
			})
		}
	}
	sort.Strings(groupKeys)

	// Phase 2: For each unique account, serve fresh cache OR probe 1 selected machine.
	resolved := make(map[string]subUsageAccount, len(groupKeys))
	var resMu sync.Mutex
	var pwg sync.WaitGroup
	for _, key := range groupKeys {
		cands := groups[key]
		pwg.Add(1)
		go func(k string, cs []probeCandidate) {
			defer pwg.Done()
			acct := u.resolveUniqueAccount(ctx, k, cs, cfg, fresh, now)
			resMu.Lock()
			resolved[k] = acct
			resMu.Unlock()
		}(key, cands)
	}
	pwg.Wait()

	// Map the single deduplicated probe result back to every machine that hosts it.
	nodes := make([]subUsageMachineNode, len(invResults))
	for i, r := range invResults {
		n := r.node
		n.Report.CollectedAt = now.UTC().Format(time.RFC3339)
		n.Report.Today = n.Today
		n.Report.TodayTokens = n.Today.Tokens
		if n.Error == "" && !n.Unsupported {
			for _, it := range r.inv.Accounts {
				prov := strings.ToLower(strings.TrimSpace(it.Provider))
				if prov == "" {
					continue
				}
				k := accountGroupKey(prov, it.AccountHash, n.ID, cfg.DedupeAccounts)
				if acct, ok := resolved[k]; ok {
					cp := cloneSubAccount(acct)
					if cp.Email == "" && it.Email != "" {
						cp.Email = it.Email
					}
					if cp.PlanDisplay == "" && it.PlanDisplay != "" {
						cp.PlanDisplay = it.PlanDisplay
					}
					n.Report.Accounts = append(n.Report.Accounts, cp)
				}
			}
		}
		nodes[i] = n
	}

	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Local != nodes[j].Local {
			return !nodes[i].Local // show agent machines before hub
		}
		return strings.ToLower(nodes[i].Name) < strings.ToLower(nodes[j].Name)
	})

	return subUsageSnapshot{
		CollectedAt: now,
		Machines:    nodes,
	}
}

func (u *subUsageHub) resolveUniqueAccount(ctx context.Context, key string, cands []probeCandidate, cfg subUsageConfig, fresh bool, now time.Time) subUsageAccount {
	// Clear 401 blocks on machines whose local token_expires_at has advanced
	// (meaning the native agent refreshed its own access token).
	u.mu.Lock()
	st := u.acctState[key]
	if st == nil {
		st = &acctCacheEntry{}
		u.acctState[key] = st
	}
	for _, c := range cands {
		bkey := key + "@" + c.machineID
		if blockedExp, blocked := u.authBlocked[bkey]; blocked {
			if c.item.TokenValid && c.item.TokenExpiresAt != "" && c.item.TokenExpiresAt != blockedExp {
				delete(u.authBlocked, bkey)
			}
		}
	}

	// 1. Serve cached result if within TTL (or manual refresh cooldown).
	if !st.fetchedAt.IsZero() {
		age := now.Sub(st.fetchedAt)
		if (!fresh && age < cfg.RefreshEvery) || (fresh && age < subUsageManualCool) {
			out := cloneSubAccount(st.acct)
			u.mu.Unlock()
			return out
		}
	}

	// 2. Respect account-level backoff (e.g. 429 or repeated transient failures).
	if !st.backoffUntil.IsZero() && now.Before(st.backoffUntil) {
		out := u.cachedOrErrorLocked(st, cands, now, st.acct.Error)
		u.mu.Unlock()
		return out
	}

	// 3. Pick eligible machines (locally valid token & not 401-blocked), ordered by:
	//    non-hub > non-keychain (file-backed) > sticky lastMachine > latest token_expires_at > stable machine name.
	lastMachine := st.lastMachine
	var eligible []probeCandidate
	for _, c := range cands {
		if !c.item.TokenValid {
			continue
		}
		if _, blocked := u.authBlocked[key+"@"+c.machineID]; blocked {
			continue
		}
		eligible = append(eligible, c)
	}
	u.mu.Unlock()

	sort.SliceStable(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		if a.local != b.local {
			return !a.local
		}
		if a.item.FromKeychain != b.item.FromKeychain {
			return !a.item.FromKeychain
		}
		aSticky, bSticky := a.machineID == lastMachine && lastMachine != "", b.machineID == lastMachine && lastMachine != ""
		if aSticky != bSticky {
			return aSticky
		}
		if !a.expiresAt.Equal(b.expiresAt) {
			return a.expiresAt.After(b.expiresAt)
		}
		return strings.ToLower(a.machineName) < strings.ToLower(b.machineName)
	})

	// If no machine has a valid token, never attempt refresh — try offline snapshot if any,
	// otherwise report "等待本机 Agent 刷新" (keeping recent good cache if within stale limit).
	if len(eligible) == 0 {
		for _, c := range cands {
			if c.item.HasSnapshot {
				proxyURL := resolveSubUsageProxy(cfg.ProxyURL, c.machineID, c.machineName, c.local)
				res, err := u.probeMachine(ctx, c, proxyURL)
				if err == nil && res.Account != nil && len(res.Account.Windows) > 0 {
					u.mu.Lock()
					st.acct = cloneSubAccount(*res.Account)
					st.fetchedAt = now
					u.mu.Unlock()
					return *res.Account
				}
			}
		}
		u.mu.Lock()
		expiredAcct := buildInventoryFallbackAccount(cands, "expired", "等待本机 Agent 刷新")
		if st.lastGood != nil && now.Sub(st.lastGoodAt) <= subUsageHubStaleMax {
			cp := cloneSubAccount(*st.lastGood)
			cp.Error = "使用缓存 · 等待本机 Agent 刷新"
			st.acct = cp
			st.fetchedAt = now
			u.mu.Unlock()
			return cp
		}
		st.acct = expiredAcct
		st.fetchedAt = now
		u.mu.Unlock()
		return expiredAcct
	}

	// 4. Probe 1 selected machine; fail over to next candidate ONLY on 401/network failure.
	var lastErr string
	for _, cand := range eligible {
		proxyURL := resolveSubUsageProxy(cfg.ProxyURL, cand.machineID, cand.machineName, cand.local)
		res, err := u.probeMachine(ctx, cand, proxyURL)
		if err != nil {
			lastErr = "节点采集超时"
			continue
		}
		if res.AuthFailed {
			u.mu.Lock()
			u.authBlocked[key+"@"+cand.machineID] = cand.item.TokenExpiresAt
			u.mu.Unlock()
			lastErr = "等待本机 Agent 刷新"
			continue
		}
		if res.RateLimit {
			// 429 is account-wide: do NOT fail over to another machine!
			u.mu.Lock()
			st.rateLimitCount++
			backoff := rateLimitBackoff(st.rateLimitCount, res.RetryAfter)
			st.backoffUntil = now.Add(backoff)
			errMsg := "用量接口限流，已自动退避"
			out := u.cachedOrErrorLocked(st, cands, now, errMsg)
			st.acct = out
			st.fetchedAt = now
			u.mu.Unlock()
			return out
		}
		if res.Account != nil && (res.Account.Status == "ok" || res.Account.Status == "limited") {
			acct := cloneSubAccount(*res.Account)
			u.mu.Lock()
			st.acct = acct
			st.fetchedAt = now
			cp := cloneSubAccount(acct)
			st.lastGood = &cp
			st.lastGoodAt = now
			st.lastMachine = cand.machineID
			st.backoffUntil = time.Time{}
			st.rateLimitCount = 0
			st.failCount = 0
			u.mu.Unlock()
			return acct
		}
		if res.Account != nil && res.Account.Error != "" {
			lastErr = res.Account.Error
		}
	}

	// All eligible machines failed (either all 401 or all network/5xx).
	u.mu.Lock()
	allAuthBlocked := true
	for _, cand := range eligible {
		if _, blocked := u.authBlocked[key+"@"+cand.machineID]; !blocked {
			allAuthBlocked = false
			break
		}
	}
	if allAuthBlocked {
		out := buildInventoryFallbackAccount(cands, "expired", "等待本机 Agent 刷新")
		if st.lastGood != nil && now.Sub(st.lastGoodAt) <= subUsageHubStaleMax {
			out = cloneSubAccount(*st.lastGood)
			out.Error = "使用缓存 · 等待本机 Agent 刷新"
		}
		st.acct = out
		st.fetchedAt = now
		u.mu.Unlock()
		return out
	}
	st.failCount++
	st.backoffUntil = now.Add(transientBackoff(st.failCount))
	if lastErr == "" {
		lastErr = "暂不可达，稍后自动重试"
	}
	out := u.cachedOrErrorLocked(st, cands, now, lastErr)
	st.acct = out
	st.fetchedAt = now
	u.mu.Unlock()
	return out
}

func (u *subUsageHub) cachedOrErrorLocked(st *acctCacheEntry, cands []probeCandidate, now time.Time, errMsg string) subUsageAccount {
	if st.lastGood != nil && now.Sub(st.lastGoodAt) <= subUsageHubStaleMax {
		cp := cloneSubAccount(*st.lastGood)
		if errMsg != "" {
			cp.Error = "使用缓存 · " + errMsg
		}
		return cp
	}
	return buildInventoryFallbackAccount(cands, "error", errMsg)
}

func buildInventoryFallbackAccount(cands []probeCandidate, status, errMsg string) subUsageAccount {
	var first subUsageInventoryItem
	if len(cands) > 0 {
		first = cands[0].item
	}
	email := first.Email
	plan, planRaw, planDisp := first.Plan, first.PlanRaw, first.PlanDisplay
	for _, c := range cands {
		if email == "" && c.item.Email != "" {
			email = c.item.Email
		}
		if planDisp == "" && c.item.PlanDisplay != "" {
			plan, planRaw, planDisp = c.item.Plan, c.item.PlanRaw, c.item.PlanDisplay
		}
	}
	return subUsageAccount{
		Provider:    first.Provider,
		AccountHash: first.AccountHash,
		Email:       email,
		Plan:        plan,
		PlanRaw:     planRaw,
		PlanDisplay: planDisp,
		Status:      status,
		Error:       errMsg,
	}
}

func rateLimitBackoff(count, retryAfterSec int) time.Duration {
	steps := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute}
	idx := count - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(steps) {
		idx = len(steps) - 1
	}
	d := steps[idx]
	if retryAfterSec > 0 {
		ra := time.Duration(retryAfterSec) * time.Second
		if ra > d {
			d = ra
		}
		if d > 30*time.Minute {
			d = 30 * time.Minute
		}
	}
	return d
}

func transientBackoff(count int) time.Duration {
	steps := []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}
	idx := count - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(steps) {
		idx = len(steps) - 1
	}
	return steps[idx]
}

func (u *subUsageHub) fetchNodeInventory(ctx context.Context, machineID string) (subUsageInventory, int, error) {
	client, err := u.s.herdr.Client(machineID)
	if err != nil {
		return subUsageInventory{}, 0, err
	}
	nctx, cancel := context.WithTimeout(ctx, subUsageInventoryWait)
	defer cancel()
	req, err := http.NewRequestWithContext(nctx, http.MethodGet, "http://node/api/sub-usage/inventory", nil)
	if err != nil {
		return subUsageInventory{}, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return subUsageInventory{}, 0, err
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, subUsageMaxRespBytes))
	resp.Body.Close()
	if rerr != nil {
		return subUsageInventory{}, resp.StatusCode, rerr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return subUsageInventory{}, resp.StatusCode, nil
	}
	var inv subUsageInventory
	if err := json.Unmarshal(body, &inv); err != nil {
		return subUsageInventory{}, resp.StatusCode, err
	}
	return inv, resp.StatusCode, nil
}

func (u *subUsageHub) probeMachine(ctx context.Context, cand probeCandidate, proxyURL string) (subUsageProbeResult, error) {
	if cand.local {
		return u.s.localSubUsageCollector().probeOne(ctx, cand.item.Provider, cand.item.AccountHash, proxyURL), nil
	}
	client, err := u.s.herdr.Client(cand.machineID)
	if err != nil {
		return subUsageProbeResult{}, err
	}
	pctx, cancel := context.WithTimeout(ctx, subUsageProbeWait)
	defer cancel()
	qs := url.Values{}
	qs.Set("provider", cand.item.Provider)
	if cand.item.AccountHash != "" {
		qs.Set("account_hash", cand.item.AccountHash)
	}
	if proxyURL != "" {
		qs.Set("proxy", proxyURL)
	}
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, "http://node/api/sub-usage/probe?"+qs.Encode(), nil)
	if err != nil {
		return subUsageProbeResult{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return subUsageProbeResult{}, err
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, subUsageMaxRespBytes))
	resp.Body.Close()
	if rerr != nil {
		return subUsageProbeResult{}, rerr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return subUsageProbeResult{}, fmt.Errorf("probe HTTP %d", resp.StatusCode)
	}
	var out subUsageProbeResult
	if err := json.Unmarshal(body, &out); err != nil {
		return subUsageProbeResult{}, err
	}
	return out, nil
}

func cloneUsageSnapshot(s subUsageSnapshot) subUsageSnapshot {
	out := subUsageSnapshot{
		CollectedAt: s.CollectedAt,
		Machines:    make([]subUsageMachineNode, len(s.Machines)),
	}
	for i, n := range s.Machines {
		out.Machines[i] = cloneMachineNode(n)
	}
	return out
}

func cloneMachineNode(n subUsageMachineNode) subUsageMachineNode {
	out := n
	out.Today = cloneTokenDayStats(n.Today)
	out.Report = cloneMachineReport(n.Report)
	return out
}

func (h *extHost) usageConfigLocked() subUsageConfig {
	set := h.settingLocked(featureUsage)
	cfg := parseSubUsageConfig(set.Config)
	if h.store != nil {
		cfg.FocusOverride = strings.ToLower(strings.TrimSpace(h.store.kv(usageFocusKVKey)))
	}
	return cfg
}

func (h *extHost) usageHeaderStatus(ctx context.Context, model string, fresh bool) headerStatusSlot {
	m := h.manifest(featureUsage)
	slot := headerStatusSlot{
		ID:    featureUsage,
		Name:  "订阅用量",
		Label: "用量",
		State: "ok",
	}
	if m != nil {
		slot.Name = m.Name
		slot.Label = extLabel(m)
		slot.IconURL = m.IconURL
	}
	h.mu.Lock()
	cfg := h.usageConfigLocked()
	h.mu.Unlock()

	snap := h.s.usageHub().collect(ctx, cfg, fresh)
	hs := buildSubUsageHeaderStatus(snap, cfg, model, h.s.usageHub().now())
	slot.Status = &hs
	return slot
}

func (h *extHost) usageStatus(ctx context.Context) *extproto.Status {
	h.mu.Lock()
	cfg := h.usageConfigLocked()
	h.mu.Unlock()
	snap := h.s.usageHub().collect(ctx, cfg, false)
	accts := aggregateSubAccounts(snap.Machines, cfg.DedupeAccounts, h.s.usageHub().now())
	return &extproto.Status{
		State:    extproto.StateOK,
		Account:  fmt.Sprintf("%d 台在线电脑 · %d 个订阅", len(snap.Machines), len(accts)),
		LastSync: snap.CollectedAt.UTC().Format(time.RFC3339),
	}
}

func (h *extHost) doUsageAction(ctx context.Context, name string, args map[string]any) (*actionOut, error) {
	switch name {
	case "subscription_usage":
		model, _ := args["model"].(string)
		fresh, _ := args["fresh"].(bool)
		h.mu.Lock()
		cfg := h.usageConfigLocked()
		h.mu.Unlock()
		snap := h.s.usageHub().collect(ctx, cfg, fresh)
		text := formatSubUsageText(snap, cfg, model, h.s.usageHub().now())
		return &actionOut{ActionResult: extproto.ActionResult{Text: text}}, nil
	case "set_header_focus":
		focus, _ := args["focus"].(string)
		focus = strings.ToLower(strings.TrimSpace(focus))
		switch focus {
		case "", "auto", "all", "claude", "codex", "antigravity":
		default:
			return nil, extproto.Errorf(extproto.ErrInvalidParams, "unsupported focus %q", focus)
		}
		if err := h.store.writeKV(usageFocusKVKey, focus); err != nil {
			return nil, extproto.Errorf(extproto.ErrInternal, "保存展示偏好失败：%v", err)
		}
		label := "跟随当前会话模型"
		switch focus {
		case "", "auto":
			label = "跟随当前会话模型"
		case "all":
			label = "展示全部订阅"
		default:
			label = "顶栏固定显示 " + subProviderGroupTitle(focus)
		}
		return &actionOut{ActionResult: extproto.ActionResult{Text: label}}, nil
	default:
		return nil, extproto.Errorf(errRefused, "%s has no action %q", featureUsage, name)
	}
}
