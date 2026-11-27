package collie

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var defaultAgyBaseURLs = []string{
	"https://daily-cloudcode-pa.googleapis.com",
	"https://cloudcode-pa.googleapis.com",
}

type agyCLIOAuthFile struct {
	Token *struct {
		AccessToken string `json:"access_token"`
		Expiry      string `json:"expiry"`
	} `json:"token"`
	AuthMethod   string `json:"auth_method"`
	IDTokenEmail string `json:"id_token_email"`
}

type geminiOAuthCredsFile struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	ExpiryDate  int64  `json:"expiry_date"` // unix milliseconds
}

type agyLoadCodeAssistResp struct {
	CurrentTier *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"currentTier"`
	PaidTier *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"paidTier"`
	CloudAICompanionProject json.RawMessage `json:"cloudaicompanionProject"`
}

type agyFetchModelsResp struct {
	Models map[string]struct {
		DisplayName string `json:"displayName"`
		QuotaInfo   *struct {
			RemainingFraction *float64 `json:"remainingFraction"`
			ResetTime         string   `json:"resetTime"`
		} `json:"quotaInfo"`
	} `json:"models"`
}

type agyResolvedLocal struct {
	accessToken string
	email       string
	accountHash string
	expiresAt   time.Time
	tokenValid  bool
}

func (c *subUsageCollector) resolveAgyLocal(now time.Time) (agyResolvedLocal, bool) {
	token, email, expiry, ok := readAgyLocalToken(c.home)
	if !ok || token == "" {
		return agyResolvedLocal{}, false
	}
	valid := true
	if !expiry.IsZero() {
		valid = expiry.After(now.Add(subUsageExpiryLeeway))
	}
	return agyResolvedLocal{
		accessToken: token,
		email:       email,
		accountHash: hashAccountKey("antigravity", email),
		expiresAt:   expiry,
		tokenValid:  valid,
	}, true
}

func (c *subUsageCollector) inventoryAntigravity(now time.Time) *subUsageInventoryItem {
	loc, ok := c.resolveAgyLocal(now)
	if !ok {
		return nil
	}
	var expStr string
	if !loc.expiresAt.IsZero() {
		expStr = loc.expiresAt.UTC().Format(time.RFC3339)
	}
	return &subUsageInventoryItem{
		Provider:       "antigravity",
		AccountHash:    loc.accountHash,
		Email:          loc.email,
		TokenValid:     loc.tokenValid,
		TokenExpiresAt: expStr,
	}
}

func (c *subUsageCollector) probeAntigravity(ctx context.Context, now time.Time, expectedHash string) subUsageProbeResult {
	loc, ok := c.resolveAgyLocal(now)
	if !ok {
		return subUsageProbeResult{}
	}
	if expectedHash != "" && loc.accountHash != "" && !strings.EqualFold(expectedHash, loc.accountHash) {
		return subUsageProbeResult{}
	}
	acct := &subUsageAccount{
		Provider:    "antigravity",
		AccountHash: loc.accountHash,
		Email:       loc.email,
		Status:      "ok",
	}
	if !loc.tokenValid {
		acct.Status = "expired"
		acct.Error = "等待本机 Agent 刷新"
		return subUsageProbeResult{Account: acct, AuthFailed: true}
	}

	ua := fmt.Sprintf("antigravity/1.23.2 %s/%s", runtime.GOOS, runtime.GOARCH)
	var (
		baseUsed  string
		projectID string
		lastCode  int
		lastHdr   http.Header
		lastErr   error
	)
	loadBody, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"ideType":    "ANTIGRAVITY",
			"platform":   "PLATFORM_UNSPECIFIED",
			"pluginType": "GEMINI",
		},
	})

	for _, base := range c.agyURLs {
		base = strings.TrimRight(strings.TrimSpace(base), "/")
		if base == "" {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1internal:loadCodeAssist", bytes.NewReader(loadBody))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Authorization", "Bearer "+loc.accessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", ua)
		code, hdr, body, err := c.doJSONWithHeaders(ctx, req)
		if err != nil {
			lastErr = err
			continue
		}
		lastCode = code
		lastHdr = hdr
		if code == http.StatusUnauthorized || code == http.StatusForbidden {
			acct.Status = "expired"
			acct.Error = "等待本机 Agent 刷新"
			return subUsageProbeResult{Account: acct, AuthFailed: true}
		}
		if code == http.StatusTooManyRequests {
			acct.Status = "error"
			acct.Error = "用量接口请求频率受限，稍后自动重试"
			return subUsageProbeResult{Account: acct, RateLimit: true, RetryAfter: parseRetryAfterSeconds(hdr, now)}
		}
		if code < 200 || code >= 300 {
			continue
		}
		var lr agyLoadCodeAssistResp
		if parseSubUsageJSON(body, &lr) != nil {
			continue
		}
		tierID, tierName := "", ""
		if lr.PaidTier != nil && strings.TrimSpace(lr.PaidTier.ID) != "" {
			tierID = strings.TrimSpace(lr.PaidTier.ID)
			tierName = strings.TrimSpace(lr.PaidTier.Name)
		} else if lr.CurrentTier != nil {
			tierID = strings.TrimSpace(lr.CurrentTier.ID)
			tierName = strings.TrimSpace(lr.CurrentTier.Name)
		}
		acct.Plan = tierID
		acct.PlanRaw = tierID
		acct.PlanDisplay = formatSubPlanDisplay("antigravity", tierID, tierID, tierName)
		projectID = parseAgyProjectID(lr.CloudAICompanionProject)
		baseUsed = base
		break
	}

	if baseUsed == "" {
		acct.Status = "error"
		if lastCode == http.StatusTooManyRequests {
			acct.Error = "用量接口请求频率受限，稍后自动重试"
			return subUsageProbeResult{Account: acct, RateLimit: true, RetryAfter: parseRetryAfterSeconds(lastHdr, now)}
		}
		if lastCode > 0 {
			acct.Error = fmt.Sprintf("用量接口返回 HTTP %d", lastCode)
		} else if lastErr != nil {
			acct.Error = "请求用量接口超时或网络不可达"
		} else {
			acct.Error = "未能连接 Antigravity 用量接口"
		}
		return subUsageProbeResult{Account: acct}
	}

	modelsBody, _ := json.Marshal(map[string]any{"project": projectID})
	req2, err := http.NewRequestWithContext(ctx, http.MethodPost, baseUsed+"/v1internal:fetchAvailableModels", bytes.NewReader(modelsBody))
	if err != nil {
		acct.Status = "error"
		acct.Error = "构造模型额度请求失败"
		return subUsageProbeResult{Account: acct}
	}
	req2.Header.Set("Authorization", "Bearer "+loc.accessToken)
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("User-Agent", ua)
	code2, hdr2, body2, err := c.doJSONWithHeaders(ctx, req2)
	if err != nil {
		acct.Status = "error"
		acct.Error = "拉取模型额度超时"
		return subUsageProbeResult{Account: acct}
	}
	if code2 == http.StatusUnauthorized || code2 == http.StatusForbidden {
		acct.Status = "expired"
		acct.Error = "等待本机 Agent 刷新"
		return subUsageProbeResult{Account: acct, AuthFailed: true}
	}
	if code2 == http.StatusTooManyRequests {
		acct.Status = "error"
		acct.Error = "用量接口请求频率受限，稍后自动重试"
		return subUsageProbeResult{Account: acct, RateLimit: true, RetryAfter: parseRetryAfterSeconds(hdr2, now)}
	}
	if code2 < 200 || code2 >= 300 {
		acct.Status = "error"
		acct.Error = fmt.Sprintf("模型额度接口返回 HTTP %d", code2)
		return subUsageProbeResult{Account: acct}
	}

	var mr agyFetchModelsResp
	if err := parseSubUsageJSON(body2, &mr); err != nil {
		acct.Status = "error"
		acct.Error = "模型额度返回格式无法解析"
		return subUsageProbeResult{Account: acct}
	}

	type bucket struct {
		found    bool
		maxUsed  float64
		earliest time.Time
		resetRaw string
	}
	var geminiB, partnerB bucket
	updateBucket := func(b *bucket, rem float64, resetStr string) {
		used := clampPct((1 - rem) * 100)
		if !b.found || used > b.maxUsed {
			b.maxUsed = used
		}
		if t, ok := parseSubTime(resetStr); ok {
			if b.earliest.IsZero() || t.Before(b.earliest) {
				b.earliest = t
				b.resetRaw = t.UTC().Format(time.RFC3339)
			}
		}
		b.found = true
	}

	for id, m := range mr.Models {
		if m.QuotaInfo == nil || m.QuotaInfo.RemainingFraction == nil {
			continue
		}
		lid := strings.ToLower(strings.TrimSpace(id))
		if strings.HasPrefix(lid, "tab_") {
			continue
		}
		rem := *m.QuotaInfo.RemainingFraction
		switch {
		case strings.HasPrefix(lid, "gemini-"):
			updateBucket(&geminiB, rem, m.QuotaInfo.ResetTime)
		case strings.HasPrefix(lid, "claude-") || strings.HasPrefix(lid, "gpt-oss-"):
			updateBucket(&partnerB, rem, m.QuotaInfo.ResetTime)
		}
	}

	checkedAt := now.UTC().Format(time.RFC3339)
	if geminiB.found {
		acct.Windows = append(acct.Windows, subUsageWindow{
			Kind:        "gemini_five_hour",
			Label:       "5H",
			Scope:       "model",
			ScopedModel: "gemini",
			UsedPercent: geminiB.maxUsed,
			ResetsAt:    geminiB.resetRaw,
			CheckedAt:   checkedAt,
		})
	}
	if partnerB.found {
		acct.Windows = append(acct.Windows, subUsageWindow{
			Kind:        "partner_five_hour",
			Label:       "Partner 5H",
			Scope:       "model",
			ScopedModel: "partner",
			UsedPercent: partnerB.maxUsed,
			ResetsAt:    partnerB.resetRaw,
			CheckedAt:   checkedAt,
		})
	}

	if geminiB.found && geminiB.maxUsed >= 99.9 && (!partnerB.found || partnerB.maxUsed >= 99.9) {
		acct.Status = "limited"
		if geminiB.resetRaw != "" {
			acct.AvailableAt = geminiB.resetRaw
		}
	}
	return subUsageProbeResult{Account: acct}
}

func readAgyLocalToken(home string) (token, email string, expiry time.Time, ok bool) {
	cliPath := filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	if b, err := os.ReadFile(cliPath); err == nil {
		var f agyCLIOAuthFile
		if json.Unmarshal(b, &f) == nil && f.Token != nil && strings.TrimSpace(f.Token.AccessToken) != "" {
			var exp time.Time
			if t, ok := parseSubTime(f.Token.Expiry); ok {
				exp = t
			}
			return strings.TrimSpace(f.Token.AccessToken), strings.TrimSpace(f.IDTokenEmail), exp, true
		}
	}
	geminiPath := filepath.Join(home, ".gemini", "oauth_creds.json")
	if b, err := os.ReadFile(geminiPath); err == nil {
		var f geminiOAuthCredsFile
		if json.Unmarshal(b, &f) == nil && strings.TrimSpace(f.AccessToken) != "" {
			var exp time.Time
			if f.ExpiryDate > 0 {
				exp = time.UnixMilli(f.ExpiryDate)
			}
			return strings.TrimSpace(f.AccessToken), parseJWTClaimEmail(f.IDToken), exp, true
		}
	}
	return "", "", time.Time{}, false
}

func parseJWTClaimEmail(jwt string) string {
	parts := strings.Split(strings.TrimSpace(jwt), ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return ""
	}
	return strings.TrimSpace(c.Email)
}

func parseAgyProjectID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return strings.TrimSpace(obj.ID)
	}
	return ""
}
