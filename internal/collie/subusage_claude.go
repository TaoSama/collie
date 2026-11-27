package collie

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultClaudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	claudeOAuthBetaHeader = "oauth-2025-04-20"
	defaultClaudeUA       = "claude-code/2.1.76"
)

type claudeLocalCreds struct {
	ClaudeAiOauth *struct {
		AccessToken      string `json:"accessToken"`
		ExpiresAt        int64  `json:"expiresAt"` // unix milliseconds
		SubscriptionType string `json:"subscriptionType"`
		RateLimitTier    string `json:"rateLimitTier"`
	} `json:"claudeAiOauth"`
}

type claudeMetaFile struct {
	OAuthAccount *struct {
		EmailAddress     string `json:"emailAddress"`
		AccountUUID      string `json:"accountUuid"`
		OrganizationUUID string `json:"organizationUuid"`
		OrganizationName string `json:"organizationName"`
		BillingType      string `json:"billingType"`
	} `json:"oauthAccount"`
}

type claudeUsageResp struct {
	FiveHour       *claudeUsageWindow `json:"five_hour"`
	SevenDay       *claudeUsageWindow `json:"seven_day"`
	SevenDayOpus   *claudeUsageWindow `json:"seven_day_opus"`
	SevenDaySonnet *claudeUsageWindow `json:"seven_day_sonnet"`
	ExtraUsage     *claudeExtraUsage  `json:"extra_usage"`
}

type claudeUsageWindow struct {
	Utilization *float64 `json:"utilization"` // 0..100
	ResetsAt    string   `json:"resets_at"`
}

type claudeExtraUsage struct {
	IsEnabled    bool     `json:"is_enabled"`
	MonthlyLimit *float64 `json:"monthly_limit"`
	UsedCredits  *float64 `json:"used_credits"`
	Utilization  *float64 `json:"utilization"`
}

type claudeResolvedLocal struct {
	accessToken  string
	expiresAt    time.Time
	tokenValid   bool
	email        string
	accountHash  string
	plan         string
	planRaw      string
	planDisplay  string
	fromKeychain bool
}

func (c *subUsageCollector) resolveClaudeLocal(ctx context.Context, now time.Time) (claudeResolvedLocal, bool) {
	creds, fromKC, ok := readClaudeLocalCreds(ctx, c.home)
	if !ok || creds.ClaudeAiOauth == nil || strings.TrimSpace(creds.ClaudeAiOauth.AccessToken) == "" {
		return claudeResolvedLocal{}, false
	}
	oauth := creds.ClaudeAiOauth
	email, acctUUID, orgUUID := readClaudeMeta(c.home)
	canonical := acctUUID
	if canonical == "" {
		canonical = email
	}
	if canonical != "" && orgUUID != "" {
		canonical += ":" + orgUUID
	}
	planRaw := strings.TrimSpace(oauth.RateLimitTier)
	if planRaw == "" {
		planRaw = strings.TrimSpace(oauth.SubscriptionType)
	}
	plan := strings.TrimSpace(oauth.SubscriptionType)

	var exp time.Time
	valid := true
	if oauth.ExpiresAt > 0 {
		exp = time.UnixMilli(oauth.ExpiresAt)
		valid = exp.After(now.Add(subUsageExpiryLeeway))
	}
	return claudeResolvedLocal{
		accessToken:  strings.TrimSpace(oauth.AccessToken),
		expiresAt:    exp,
		tokenValid:   valid,
		email:        email,
		accountHash:  hashAccountKey("claude", canonical),
		plan:         plan,
		planRaw:      planRaw,
		planDisplay:  formatSubPlanDisplay("claude", plan, planRaw, ""),
		fromKeychain: fromKC,
	}, true
}

func (c *subUsageCollector) inventoryClaude(ctx context.Context, now time.Time) *subUsageInventoryItem {
	loc, ok := c.resolveClaudeLocal(ctx, now)
	if !ok {
		return nil
	}
	var expStr string
	if !loc.expiresAt.IsZero() {
		expStr = loc.expiresAt.UTC().Format(time.RFC3339)
	}
	return &subUsageInventoryItem{
		Provider:       "claude",
		AccountHash:    loc.accountHash,
		Email:          loc.email,
		Plan:           loc.plan,
		PlanRaw:        loc.planRaw,
		PlanDisplay:    loc.planDisplay,
		TokenValid:     loc.tokenValid,
		TokenExpiresAt: expStr,
		FromKeychain:   loc.fromKeychain,
	}
}

func (c *subUsageCollector) probeClaude(ctx context.Context, now time.Time, expectedHash string) subUsageProbeResult {
	loc, ok := c.resolveClaudeLocal(ctx, now)
	if !ok {
		return subUsageProbeResult{}
	}
	if expectedHash != "" && loc.accountHash != "" && !strings.EqualFold(expectedHash, loc.accountHash) {
		return subUsageProbeResult{}
	}
	acct := &subUsageAccount{
		Provider:    "claude",
		AccountHash: loc.accountHash,
		Email:       loc.email,
		Plan:        loc.plan,
		PlanRaw:     loc.planRaw,
		PlanDisplay: loc.planDisplay,
		Status:      "ok",
	}
	if !loc.tokenValid {
		acct.Status = "expired"
		acct.Error = "等待本机 Agent 刷新"
		return subUsageProbeResult{Account: acct, AuthFailed: true}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.claudeURL, nil)
	if err != nil {
		acct.Status = "error"
		acct.Error = "构造查询请求失败"
		return subUsageProbeResult{Account: acct}
	}
	req.Header.Set("Authorization", "Bearer "+loc.accessToken)
	req.Header.Set("anthropic-beta", claudeOAuthBetaHeader)
	req.Header.Set("User-Agent", c.claudeUserAgent())
	req.Header.Set("Accept", "application/json")

	code, hdr, body, err := c.doJSONWithHeaders(ctx, req)
	if err != nil {
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

	var resp claudeUsageResp
	if err := parseSubUsageJSON(body, &resp); err != nil {
		acct.Status = "error"
		acct.Error = "用量接口返回格式无法解析"
		return subUsageProbeResult{Account: acct}
	}

	checkedAt := now.UTC().Format(time.RFC3339)
	addWin := func(kind, label, scope, scopedModel string, w *claudeUsageWindow) {
		if w == nil || w.Utilization == nil {
			return
		}
		acct.Windows = append(acct.Windows, subUsageWindow{
			Kind:        kind,
			Label:       label,
			Scope:       scope,
			ScopedModel: scopedModel,
			UsedPercent: clampPct(*w.Utilization),
			ResetsAt:    strings.TrimSpace(w.ResetsAt),
			CheckedAt:   checkedAt,
		})
	}
	addWin("five_hour", "5H", "", "", resp.FiveHour)
	addWin("seven_day", "7D", "", "", resp.SevenDay)
	addWin("seven_day_sonnet", "Sonnet 7D", "model", "sonnet", resp.SevenDaySonnet)
	addWin("seven_day_opus", "Opus 7D", "model", "opus", resp.SevenDayOpus)

	if resp.ExtraUsage != nil && resp.ExtraUsage.IsEnabled {
		if resp.ExtraUsage.Utilization != nil {
			acct.Windows = append(acct.Windows, subUsageWindow{
				Kind:        "extra_usage",
				Label:       "加量",
				UsedPercent: clampPct(*resp.ExtraUsage.Utilization),
				CheckedAt:   checkedAt,
			})
		}
		if resp.ExtraUsage.MonthlyLimit != nil && *resp.ExtraUsage.MonthlyLimit > 0 && resp.ExtraUsage.UsedCredits != nil {
			limit := *resp.ExtraUsage.MonthlyLimit
			left := limit - *resp.ExtraUsage.UsedCredits
			if left < 0 {
				left = 0
			}
			acct.Credit = &subUsageCredit{Left: left, Limit: limit, Unit: "credits"}
		}
	}

	markExhaustedAccount(acct, now)
	return subUsageProbeResult{Account: acct}
}

func (c *subUsageCollector) claudeUserAgent() string {
	if c.sysmon != nil {
		c.sysmon.mu.Lock()
		ver := c.sysmon.versions["claude"]
		c.sysmon.mu.Unlock()
		if ver != "" {
			return "claude-code/" + ver
		}
	}
	return defaultClaudeUA
}

func readClaudeLocalCreds(ctx context.Context, home string) (claudeLocalCreds, bool, bool) {
	var out claudeLocalCreds
	root := claudeRoot(home)
	if b, err := os.ReadFile(filepath.Join(root, ".credentials.json")); err == nil {
		if json.Unmarshal(b, &out) == nil && out.ClaudeAiOauth != nil && strings.TrimSpace(out.ClaudeAiOauth.AccessToken) != "" {
			return out, false, true
		}
	}
	services := []string{"Claude Code-credentials"}
	if cfgDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); cfgDir != "" {
		sum := sha256.Sum256([]byte(cfgDir))
		services = append([]string{"Claude Code-credentials-" + hex.EncodeToString(sum[:4])}, services...)
	}
	for _, svc := range services {
		if b, ok := readLocalKeychainSecret(ctx, svc); ok {
			if json.Unmarshal(b, &out) == nil && out.ClaudeAiOauth != nil && strings.TrimSpace(out.ClaudeAiOauth.AccessToken) != "" {
				return out, true, true
			}
		}
	}
	return claudeLocalCreds{}, false, false
}

func readClaudeMeta(home string) (email, accountUUID, orgUUID string) {
	paths := []string{filepath.Join(home, ".claude.json")}
	if cfgDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); cfgDir != "" {
		paths = append([]string{filepath.Join(cfgDir, ".claude.json")}, paths...)
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m claudeMetaFile
		if json.Unmarshal(b, &m) == nil && m.OAuthAccount != nil {
			email = strings.TrimSpace(m.OAuthAccount.EmailAddress)
			accountUUID = strings.TrimSpace(m.OAuthAccount.AccountUUID)
			orgUUID = strings.TrimSpace(m.OAuthAccount.OrganizationUUID)
			if email != "" || accountUUID != "" {
				return email, accountUUID, orgUUID
			}
		}
	}
	return "", "", ""
}

func markExhaustedAccount(acct *subUsageAccount, now time.Time) {
	if acct.Status != "ok" {
		return
	}
	var earliest time.Time
	exhausted := false
	for _, w := range acct.Windows {
		if w.Scope == "model" {
			continue
		}
		if w.UsedPercent >= 99.9 {
			if t, ok := parseSubTime(w.ResetsAt); ok {
				if !t.After(now) {
					continue
				}
				if earliest.IsZero() || t.Before(earliest) {
					earliest = t
				}
			}
			exhausted = true
		}
	}
	if exhausted {
		acct.Status = "limited"
		if !earliest.IsZero() {
			acct.AvailableAt = earliest.UTC().Format(time.RFC3339)
		}
	}
}
