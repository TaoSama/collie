package collie

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultCodexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

type codexAuthFile struct {
	AuthMode string `json:"auth_mode"`
	Tokens   *struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

type codexWhamUsageResp struct {
	UserID    string `json:"user_id"`
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	PlanType  string `json:"plan_type"`
	RateLimit *struct {
		Allowed         *bool            `json:"allowed"`
		LimitReached    bool             `json:"limit_reached"`
		PrimaryWindow   *codexWhamWindow `json:"primary_window"`
		SecondaryWindow *codexWhamWindow `json:"secondary_window"`
	} `json:"rate_limit"`
	AdditionalRateLimits []struct {
		LimitName string `json:"limit_name"`
		RateLimit *struct {
			PrimaryWindow   *codexWhamWindow `json:"primary_window"`
			SecondaryWindow *codexWhamWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	} `json:"additional_rate_limits"`
	Credits *struct {
		HasCredits bool   `json:"has_credits"`
		Unlimited  bool   `json:"unlimited"`
		Balance    string `json:"balance"`
	} `json:"credits"`
}

type codexWhamWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds int64    `json:"limit_window_seconds"`
	ResetAfterSeconds  int64    `json:"reset_after_seconds"`
	ResetAt            int64    `json:"reset_at"`
}

type codexResolvedLocal struct {
	accessToken  string
	accountID    string
	expiresAt    time.Time
	tokenValid   bool
	email        string
	accountHash  string
	plan         string
	planDisplay  string
	fromKeychain bool
}

func (c *subUsageCollector) resolveCodexLocal(ctx context.Context, now time.Time) (codexResolvedLocal, bool) {
	auth, fromKC, ok := readCodexLocalAuth(ctx, c.home)
	if !ok || auth.Tokens == nil || strings.TrimSpace(auth.Tokens.AccessToken) == "" {
		return codexResolvedLocal{}, false
	}
	toks := auth.Tokens
	jwtEmail, jwtPlan, jwtAcctID := parseCodexIDTokenClaims(toks.IDToken)
	accessExp, accessPlan, accessAcctID := parseCodexAccessTokenClaims(toks.AccessToken)
	accountID := strings.TrimSpace(toks.AccountID)
	if accountID == "" {
		accountID = jwtAcctID
	}
	if accountID == "" {
		accountID = accessAcctID
	}
	plan := jwtPlan
	if plan == "" {
		plan = accessPlan
	}
	canonical := accountID
	if canonical == "" {
		canonical = jwtEmail
	}
	valid := true
	if !accessExp.IsZero() {
		valid = accessExp.After(now.Add(subUsageExpiryLeeway))
	}
	return codexResolvedLocal{
		accessToken:  strings.TrimSpace(toks.AccessToken),
		accountID:    accountID,
		expiresAt:    accessExp,
		tokenValid:   valid,
		email:        jwtEmail,
		accountHash:  hashAccountKey("codex", canonical),
		plan:         plan,
		planDisplay:  formatSubPlanDisplay("codex", plan, plan, ""),
		fromKeychain: fromKC,
	}, true
}

func (c *subUsageCollector) inventoryCodex(ctx context.Context, now time.Time) *subUsageInventoryItem {
	loc, ok := c.resolveCodexLocal(ctx, now)
	if !ok {
		if snap := readCodexSessionRateLimits(c.home, now); snap != nil {
			return &subUsageInventoryItem{
				Provider:    "codex",
				Plan:        snap.Plan,
				PlanRaw:     snap.PlanRaw,
				PlanDisplay: snap.PlanDisplay,
				TokenValid:  false,
				HasSnapshot: true,
			}
		}
		return nil
	}
	var expStr string
	if !loc.expiresAt.IsZero() {
		expStr = loc.expiresAt.UTC().Format(time.RFC3339)
	}
	return &subUsageInventoryItem{
		Provider:       "codex",
		AccountHash:    loc.accountHash,
		Email:          loc.email,
		Plan:           loc.plan,
		PlanRaw:        loc.plan,
		PlanDisplay:    loc.planDisplay,
		TokenValid:     loc.tokenValid,
		TokenExpiresAt: expStr,
		FromKeychain:   loc.fromKeychain,
	}
}

func (c *subUsageCollector) probeCodex(ctx context.Context, now time.Time, expectedHash string) subUsageProbeResult {
	loc, ok := c.resolveCodexLocal(ctx, now)
	if !ok {
		if snap := readCodexSessionRateLimits(c.home, now); snap != nil {
			return subUsageProbeResult{Account: snap}
		}
		return subUsageProbeResult{}
	}
	if expectedHash != "" && loc.accountHash != "" && !strings.EqualFold(expectedHash, loc.accountHash) {
		return subUsageProbeResult{}
	}
	acct := &subUsageAccount{
		Provider:    "codex",
		AccountHash: loc.accountHash,
		Email:       loc.email,
		Plan:        loc.plan,
		PlanRaw:     loc.plan,
		PlanDisplay: loc.planDisplay,
		Status:      "ok",
	}
	if !loc.tokenValid {
		acct.Status = "expired"
		acct.Error = "等待本机 Agent 刷新"
		return subUsageProbeResult{Account: acct, AuthFailed: true}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.codexURL, nil)
	if err != nil {
		acct.Status = "error"
		acct.Error = "构造查询请求失败"
		return subUsageProbeResult{Account: acct}
	}
	req.Header.Set("Authorization", "Bearer "+loc.accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex-cli")
	if loc.accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", loc.accountID)
	}

	code, hdr, body, err := c.doJSONWithHeaders(ctx, req)
	if err != nil {
		if snap := readCodexSessionRateLimits(c.home, now); snap != nil {
			if acct.Email != "" && snap.Email == "" {
				snap.Email = acct.Email
			}
			if acct.AccountHash != "" && snap.AccountHash == "" {
				snap.AccountHash = acct.AccountHash
			}
			snap.Error = "使用本地会话快照（在线接口不可达）"
			return subUsageProbeResult{Account: snap}
		}
		acct.Status = "error"
		acct.Error = "请求用量接口超时或网络不可达"
		return subUsageProbeResult{Account: acct}
	}
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		acct.Status = "expired"
		acct.Error = "等待本机 Agent 刷新"
		return subUsageProbeResult{Account: acct, AuthFailed: true}
	case code == http.StatusTooManyRequests:
		acct.Status = "error"
		acct.Error = "用量接口请求频率受限，稍后自动重试"
		return subUsageProbeResult{Account: acct, RateLimit: true, RetryAfter: parseRetryAfterSeconds(hdr, now)}
	case code < 200 || code >= 300:
		acct.Status = "error"
		acct.Error = fmt.Sprintf("用量接口返回 HTTP %d", code)
		return subUsageProbeResult{Account: acct}
	}

	var resp codexWhamUsageResp
	if err := parseSubUsageJSON(body, &resp); err != nil {
		acct.Status = "error"
		acct.Error = "用量接口返回格式无法解析"
		return subUsageProbeResult{Account: acct}
	}

	if e := strings.TrimSpace(resp.Email); e != "" {
		acct.Email = e
	}
	if id := strings.TrimSpace(resp.AccountID); id != "" {
		acct.AccountHash = hashAccountKey("codex", id)
	} else if acct.AccountHash == "" && acct.Email != "" {
		acct.AccountHash = hashAccountKey("codex", acct.Email)
	}
	if p := strings.TrimSpace(resp.PlanType); p != "" {
		acct.Plan = p
		acct.PlanRaw = p
		acct.PlanDisplay = formatSubPlanDisplay("codex", p, p, "")
	}

	checkedAt := now.UTC().Format(time.RFC3339)
	seenLabels := map[string]bool{}
	appendWhamWindow := func(w *codexWhamWindow, scope, scopedModel, prefix string) {
		if w == nil || w.UsedPercent == nil {
			return
		}
		kind, label := codexWindowKindLabel(w.LimitWindowSeconds)
		if prefix != "" {
			label = prefix + " " + label
			kind = strings.ToLower(prefix) + "_" + kind
		}
		if seenLabels[label] {
			return
		}
		seenLabels[label] = true
		var resetsAt string
		if w.ResetAt > 0 {
			resetsAt = time.Unix(w.ResetAt, 0).UTC().Format(time.RFC3339)
		} else if w.ResetAfterSeconds > 0 {
			resetsAt = now.Add(time.Duration(w.ResetAfterSeconds) * time.Second).UTC().Format(time.RFC3339)
		}
		acct.Windows = append(acct.Windows, subUsageWindow{
			Kind:        kind,
			Label:       label,
			Scope:       scope,
			ScopedModel: scopedModel,
			UsedPercent: clampPct(*w.UsedPercent),
			ResetsAt:    resetsAt,
			CheckedAt:   checkedAt,
		})
	}

	if resp.RateLimit != nil {
		appendWhamWindow(resp.RateLimit.PrimaryWindow, "", "", "")
		appendWhamWindow(resp.RateLimit.SecondaryWindow, "", "", "")
		if resp.RateLimit.LimitReached || (resp.RateLimit.Allowed != nil && !*resp.RateLimit.Allowed) {
			acct.Status = "limited"
		}
	}
	for _, add := range resp.AdditionalRateLimits {
		if add.RateLimit == nil {
			continue
		}
		name := strings.TrimSpace(add.LimitName)
		if name == "" {
			continue
		}
		appendWhamWindow(add.RateLimit.PrimaryWindow, "model", strings.ToLower(name), name)
		appendWhamWindow(add.RateLimit.SecondaryWindow, "model", strings.ToLower(name), name)
	}
	if resp.Credits != nil && resp.Credits.HasCredits && !resp.Credits.Unlimited {
		if bal, err := strconv.ParseFloat(strings.TrimSpace(resp.Credits.Balance), 64); err == nil && bal > 0 {
			acct.Credit = &subUsageCredit{Left: bal, Unit: "credits"}
		}
	}

	markExhaustedAccount(acct, now)
	return subUsageProbeResult{Account: acct}
}

func codexWindowKindLabel(secs int64) (kind, label string) {
	switch {
	case secs <= 0:
		return "five_hour", "5H"
	case secs <= 14400:
		h := max(1, (secs+1800)/3600)
		return "short_hour", fmt.Sprintf("%dH", h)
	case secs <= 21600:
		return "five_hour", "5H"
	case secs <= 93600:
		return "daily", "1D"
	case secs <= 691200:
		return "weekly", "7D"
	default:
		d := max(1, (secs+43200)/86400)
		if d >= 28 && d <= 31 {
			return "monthly", "月"
		}
		return "custom_days", fmt.Sprintf("%dD", d)
	}
}

func readCodexLocalAuth(ctx context.Context, home string) (codexAuthFile, bool, bool) {
	var out codexAuthFile
	if b, err := os.ReadFile(filepath.Join(codexRoot(home), "auth.json")); err == nil {
		if json.Unmarshal(b, &out) == nil && out.Tokens != nil && strings.TrimSpace(out.Tokens.AccessToken) != "" {
			return out, false, true
		}
	}
	if b, ok := readLocalKeychainSecret(ctx, "Codex Auth"); ok {
		if json.Unmarshal(b, &out) == nil && out.Tokens != nil && strings.TrimSpace(out.Tokens.AccessToken) != "" {
			return out, true, true
		}
	}
	return codexAuthFile{}, false, false
}

// parseCodexIDTokenClaims decodes only the non-secret JWT payload claims
// (email, plan type, account id) without retaining the token.
func parseCodexIDTokenClaims(idToken string) (email, plan, accountID string) {
	parts := strings.Split(strings.TrimSpace(idToken), ".")
	if len(parts) < 2 {
		return "", "", ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", ""
	}
	var claims struct {
		Email string `json:"email"`
		Auth  struct {
			PlanType  string `json:"chatgpt_plan_type"`
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return "", "", ""
	}
	return strings.TrimSpace(claims.Email), strings.TrimSpace(claims.Auth.PlanType), strings.TrimSpace(claims.Auth.AccountID)
}

// parseCodexAccessTokenClaims reads only the non-secret exp/plan/account_id
// fields from the JWT payload so expiry can be checked offline before any call.
func parseCodexAccessTokenClaims(accessToken string) (exp time.Time, plan, accountID string) {
	parts := strings.Split(strings.TrimSpace(accessToken), ".")
	if len(parts) < 2 {
		return time.Time{}, "", ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, "", ""
	}
	var claims struct {
		Exp  int64 `json:"exp"`
		Auth struct {
			PlanType  string `json:"chatgpt_plan_type"`
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return time.Time{}, "", ""
	}
	if claims.Exp > 0 {
		exp = time.Unix(claims.Exp, 0).UTC()
	}
	return exp, strings.TrimSpace(claims.Auth.PlanType), strings.TrimSpace(claims.Auth.AccountID)
}

// readCodexSessionRateLimits scans the most recent Codex session rollout JSONL
// within the last 12 hours as an offline fallback when wham/usage is unreachable.
func readCodexSessionRateLimits(home string, now time.Time) *subUsageAccount {
	root := filepath.Join(codexRoot(home), "sessions")
	type cand struct {
		path string
		mod  time.Time
	}
	cutoff := now.Add(-12 * time.Hour)
	var cands []cand
	for day := now.AddDate(0, 0, -1); !day.After(now.AddDate(0, 0, 1)); day = day.AddDate(0, 0, 1) {
		dir := filepath.Join(root, day.Format("2006/01/02"))
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			fi, err := e.Info()
			if err != nil || fi.ModTime().Before(cutoff) {
				continue
			}
			cands = append(cands, cand{path: filepath.Join(dir, e.Name()), mod: fi.ModTime()})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	for i := 0; i < len(cands) && i < 3; i++ {
		if acct := parseCodexRolloutRateLimits(cands[i].path, now); acct != nil {
			return acct
		}
	}
	return nil
}

func parseCodexRolloutRateLimits(path string, now time.Time) *subUsageAccount {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	const tailBytes = 256 << 10
	if fi.Size() > tailBytes {
		_, _ = f.Seek(fi.Size()-tailBytes, 0)
	}
	var last struct {
		Timestamp time.Time `json:"timestamp"`
		Payload   struct {
			RateLimits *struct {
				PlanType string `json:"plan_type"`
				Primary  *struct {
					UsedPercent   *float64 `json:"used_percent"`
					WindowMinutes int64    `json:"window_minutes"`
					ResetsAt      int64    `json:"resets_at"`
				} `json:"primary"`
				Secondary *struct {
					UsedPercent   *float64 `json:"used_percent"`
					WindowMinutes int64    `json:"window_minutes"`
					ResetsAt      int64    `json:"resets_at"`
				} `json:"secondary"`
			} `json:"rate_limits"`
		} `json:"payload"`
	}
	found := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 2<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"rate_limits"`)) {
			continue
		}
		var cur = last
		cur.Payload.RateLimits = nil
		if json.Unmarshal(line, &cur) == nil && cur.Payload.RateLimits != nil &&
			(cur.Payload.RateLimits.Primary != nil || cur.Payload.RateLimits.Secondary != nil) {
			last = cur
			found = true
		}
	}
	if !found || last.Payload.RateLimits == nil {
		return nil
	}
	rl := last.Payload.RateLimits
	acct := &subUsageAccount{
		Provider:    "codex",
		Plan:        strings.TrimSpace(rl.PlanType),
		PlanRaw:     strings.TrimSpace(rl.PlanType),
		PlanDisplay: formatSubPlanDisplay("codex", rl.PlanType, rl.PlanType, ""),
		Status:      "ok",
	}
	checked := last.Timestamp
	if checked.IsZero() {
		checked = now
	}
	checkedStr := checked.UTC().Format(time.RFC3339)
	if rl.Primary != nil && rl.Primary.UsedPercent != nil {
		kind, label := codexWindowKindLabel(rl.Primary.WindowMinutes * 60)
		var rst string
		if rl.Primary.ResetsAt > 0 {
			rst = time.Unix(rl.Primary.ResetsAt, 0).UTC().Format(time.RFC3339)
		}
		acct.Windows = append(acct.Windows, subUsageWindow{
			Kind: kind, Label: label, UsedPercent: clampPct(*rl.Primary.UsedPercent), ResetsAt: rst, CheckedAt: checkedStr,
		})
	}
	if rl.Secondary != nil && rl.Secondary.UsedPercent != nil {
		kind, label := codexWindowKindLabel(rl.Secondary.WindowMinutes * 60)
		var rst string
		if rl.Secondary.ResetsAt > 0 {
			rst = time.Unix(rl.Secondary.ResetsAt, 0).UTC().Format(time.RFC3339)
		}
		acct.Windows = append(acct.Windows, subUsageWindow{
			Kind: kind, Label: label, UsedPercent: clampPct(*rl.Secondary.UsedPercent), ResetsAt: rst, CheckedAt: checkedStr,
		})
	}
	if len(acct.Windows) == 0 {
		return nil
	}
	markExhaustedAccount(acct, now)
	return acct
}
