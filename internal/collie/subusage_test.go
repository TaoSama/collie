package collie

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func makeFakeJWT(claims map[string]any) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	b, _ := json.Marshal(claims)
	pay := base64.RawURLEncoding.EncodeToString(b)
	return hdr + "." + pay + ".sig"
}

func TestSubUsageCollectorsOffline(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)

	// 1. Write fake local credentials for Claude, Codex, Antigravity.
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	claudeSecret := "sk-ant-oat01-super-secret-token-value"
	claudeCreds := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d,"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"}}`,
		claudeSecret, now.Add(2*time.Hour).UnixMilli())
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(claudeCreds), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"alice@example.com","accountUuid":"uuid-claude-1","organizationUuid":"org-1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	codexSecret := makeFakeJWT(map[string]any{
		"exp": now.Add(3 * time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type":  "pro",
			"chatgpt_account_id": "acct-codex-1",
		},
	})
	codexID := makeFakeJWT(map[string]any{
		"email": "bob@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type":  "pro",
			"chatgpt_account_id": "acct-codex-1",
		},
	})
	codexAuth := fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"access_token":%q,"id_token":%q,"account_id":"acct-codex-1"}}`, codexSecret, codexID)
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte(codexAuth), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(home, ".gemini", "antigravity-cli"), 0o700); err != nil {
		t.Fatal(err)
	}
	agySecret := "ya29.antigravity-secret-token-value"
	agyAuth := fmt.Sprintf(`{"token":{"access_token":%q,"expiry":%q},"auth_method":"consumer","id_token_email":"carol@example.com"}`,
		agySecret, now.Add(time.Hour).Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"), []byte(agyAuth), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))

	// 2. Mock upstream HTTP endpoints; verify Phase 1 inventory makes ZERO HTTP calls.
	var upstreamHits atomic.Int32
	var gotClaudeBeta, gotCodexAcct string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		switch r.URL.Path {
		case "/api/oauth/usage":
			gotClaudeBeta = r.Header.Get("anthropic-beta")
			if r.Header.Get("Authorization") != "Bearer "+claudeSecret {
				http.Error(w, "unauthorized", 401)
				return
			}
			fmt.Fprintf(w, `{"five_hour":{"utilization":28.5,"resets_at":%q},"seven_day":{"utilization":41.0,"resets_at":%q},"seven_day_sonnet":{"utilization":12.0,"resets_at":%q}}`,
				now.Add(2*time.Hour).Format(time.RFC3339),
				now.Add(72*time.Hour).Format(time.RFC3339),
				now.Add(96*time.Hour).Format(time.RFC3339))
		case "/backend-api/wham/usage":
			gotCodexAcct = r.Header.Get("ChatGPT-Account-Id")
			if r.Header.Get("Authorization") != "Bearer "+codexSecret {
				http.Error(w, "unauthorized", 401)
				return
			}
			fmt.Fprintf(w, `{"email":"bob@example.com","account_id":"acct-codex-1","plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_at":%d}},"credits":{"has_credits":true,"unlimited":false,"balance":"1200.5"}}`,
				now.Add(120*time.Hour).Unix())
		case "/v1internal:loadCodeAssist":
			if r.Header.Get("Authorization") != "Bearer "+agySecret {
				http.Error(w, "unauthorized", 401)
				return
			}
			fmt.Fprint(w, `{"paidTier":{"id":"g1-ultra-lite-tier","name":"Google AI Ultra"},"cloudaicompanionProject":"proj-123"}`)
		case "/v1internal:retrieveUserQuotaSummary":
			if r.Header.Get("Authorization") != "Bearer "+agySecret {
				http.Error(w, "unauthorized", 401)
				return
			}
			fmt.Fprintf(w, `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.82,"resetTime":%q},{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.42,"resetTime":%q}]},{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-5h","window":"5h","remainingFraction":0.95,"resetTime":%q},{"bucketId":"3p-weekly","window":"weekly","remainingFraction":1.0}]}]}`,
				now.Add(3*time.Hour).Format(time.RFC3339),
				now.Add(120*time.Hour).Format(time.RFC3339),
				now.Add(4*time.Hour).Format(time.RFC3339))
		case "/v1internal:fetchAvailableModels":
			fmt.Fprintf(w, `{"models":{"gemini-3-flash":{"quotaInfo":{"remainingFraction":0.82,"resetTime":%q}},"claude-opus-5-5-high":{"quotaInfo":{"remainingFraction":0.95,"resetTime":%q}},"tab_flash_lite_preview":{"quotaInfo":{"remainingFraction":1}}}}`,
				now.Add(3*time.Hour).Format(time.RFC3339),
				now.Add(4*time.Hour).Format(time.RFC3339))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newSubUsageCollector(home, nil)
	c.client = srv.Client()
	c.now = func() time.Time { return now }
	c.claudeURL = srv.URL + "/api/oauth/usage"
	c.codexURL = srv.URL + "/backend-api/wham/usage"
	c.agyURLs = []string{srv.URL}

	// Phase 1: Pure local inventory must make ZERO upstream calls and leak ZERO secrets.
	inv := c.inventory(context.Background())
	if upstreamHits.Load() != 0 {
		t.Fatalf("inventory must not make upstream network calls, got %d", upstreamHits.Load())
	}
	if len(inv.Accounts) != 3 {
		t.Fatalf("inventory got %d accounts, want 3: %+v", len(inv.Accounts), inv.Accounts)
	}
	invJSON, _ := json.Marshal(inv)
	for _, secret := range []string{claudeSecret, codexSecret, agySecret} {
		if strings.Contains(string(invJSON), secret) {
			t.Fatalf("inventory leaked secret %q", secret)
		}
	}
	for _, it := range inv.Accounts {
		if !it.TokenValid || it.AccountHash == "" || it.TokenExpiresAt == "" {
			t.Fatalf("inventory item missing local validity/hash/expiry: %+v", it)
		}
	}

	// Phase 2: Targeted probe.
	rep := c.collect(context.Background(), true)
	if len(rep.Accounts) != 3 {
		t.Fatalf("got %d accounts, want 3: %+v", len(rep.Accounts), rep.Accounts)
	}
	if gotClaudeBeta != claudeOAuthBetaHeader {
		t.Fatalf("anthropic-beta = %q, want %q", gotClaudeBeta, claudeOAuthBetaHeader)
	}
	if gotCodexAcct != "acct-codex-1" {
		t.Fatalf("ChatGPT-Account-Id = %q, want acct-codex-1", gotCodexAcct)
	}

	rawJSON, _ := json.Marshal(rep)
	s := string(rawJSON)
	for _, secret := range []string{claudeSecret, codexSecret, agySecret} {
		if strings.Contains(s, secret) {
			t.Fatalf("serialized report leaked secret %q", secret)
		}
	}

	byProv := map[string]subUsageAccount{}
	for _, a := range rep.Accounts {
		byProv[a.Provider] = a
	}
	if byProv["claude"].PlanDisplay != "Max 20x ($200)" || len(byProv["claude"].Windows) != 3 {
		t.Fatalf("claude account mismatch: %+v", byProv["claude"])
	}
	if byProv["codex"].PlanDisplay != "Pro 20x" || len(byProv["codex"].Windows) != 1 || byProv["codex"].Windows[0].Label != "7D" || byProv["codex"].Credit == nil {
		t.Fatalf("codex account mismatch: %+v", byProv["codex"])
	}
	if byProv["antigravity"].PlanDisplay != "G1 Ultra Lite" || len(byProv["antigravity"].Windows) != 4 || byProv["antigravity"].Windows[0].Label != "5H" || byProv["antigravity"].Windows[1].Label != "7D" {
		t.Fatalf("antigravity account mismatch: %+v", byProv["antigravity"])
	}
}

func TestSubUsageSanitizeSecrets(t *testing.T) {
	in := "upstream error Bearer abcdef1234567890 sk-ant-oat01-secret999 ya29.a0AfH6SMxxxx eyJhbGciOiJIUzI1NiJ9.payload123456.sig"
	out := sanitizeSubUsageText(in)
	for _, leak := range []string{"abcdef1234567890", "secret999", "a0AfH6SMxxxx", "payload123456"} {
		if strings.Contains(out, leak) {
			t.Fatalf("sanitizeSubUsageText leaked %q in %q", leak, out)
		}
	}
}

func TestSubUsageTwoPhaseDedupeFailoverAndByMachineUI(t *testing.T) {
	_, h, _ := newExtTestHub(t)
	curNow := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)

	cat, err := extCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var usageMan *ExtManifest
	for _, m := range cat {
		if m.ID == featureUsage {
			usageMan = m
		}
	}
	if usageMan == nil {
		t.Fatal("built-in usage extension missing from extCatalog")
	}
	h.mu.Lock()
	h.cat[featureUsage] = usageMan
	h.mu.Unlock()

	if !h.setting(featureUsage).Enabled || !h.featureOn(featureUsage) {
		t.Fatal("usage extension must be enabled by default")
	}
	if _, err := h.configure(featureUsage, func(x *ExtSetting) { x.Enabled = false }); err != nil {
		t.Fatal(err)
	}
	if slots := h.headerStatuses(context.Background(), "claude-opus-4", false); len(slots) != 0 {
		t.Fatalf("disabled usage extension must not return header slots, got: %+v", slots)
	}
	if _, err := h.do(context.Background(), featureUsage, "subscription_usage", nil); err == nil {
		t.Fatal("expected action call to be refused when usage extension is off")
	}

	// Track exact probe counts per machine & provider to verify dedupe-before-probe.
	var macProbes, devProbes atomic.Int32
	var macAgyAuthFail atomic.Bool

	macSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sub-usage/inventory":
			writeJSON(w, 200, subUsageInventory{
				CollectedAt: curNow.Format(time.RFC3339),
				Today: tokenDayStats{
					Tokens: 1_500_000, Requests: 320, CacheRead: 1_200_000, CacheWrite: 100_000, CacheInput: 1_400_000,
					ByProvider: map[string]tokenDayStats{
						"claude": {Tokens: 1_000_000, Requests: 220, CacheRead: 800_000, CacheWrite: 100_000, CacheInput: 950_000},
						"codex":  {Tokens: 500_000, Requests: 100, CacheRead: 400_000, CacheInput: 450_000},
					},
				},
				Accounts: []subUsageInventoryItem{
					{Provider: "claude", AccountHash: "hash-claude-1", Email: "alice@example.com", Plan: "max", PlanRaw: "default_claude_max_20x", PlanDisplay: "Max 20x ($200)", TokenValid: true, TokenExpiresAt: curNow.Add(2 * time.Hour).Format(time.RFC3339)},
					{Provider: "codex", AccountHash: "hash-codex-1", Email: "bob@example.com", Plan: "pro", PlanRaw: "pro", PlanDisplay: "Pro 20x", TokenValid: true, TokenExpiresAt: curNow.Add(4 * time.Hour).Format(time.RFC3339)},
					// Shared antigravity account with LATER expiry on macpro, so hub selects macpro first!
					{Provider: "antigravity", AccountHash: "hash-agy-shared", Email: "shared@example.com", TokenValid: true, TokenExpiresAt: curNow.Add(3 * time.Hour).Format(time.RFC3339)},
				},
			})
		case "/api/sub-usage/probe":
			macProbes.Add(1)
			prov := r.URL.Query().Get("provider")
			switch prov {
			case "claude":
				writeJSON(w, 200, subUsageProbeResult{Account: &subUsageAccount{
					Provider: "claude", AccountHash: "hash-claude-1", Email: "alice@example.com", Plan: "max", PlanRaw: "default_claude_max_20x", PlanDisplay: "Max 20x ($200)", Status: "ok",
					Windows: []subUsageWindow{
						{Kind: "five_hour", Label: "5H", UsedPercent: 30, ResetsAt: curNow.Add(2 * time.Hour).Format(time.RFC3339), CheckedAt: curNow.Format(time.RFC3339)},
						{Kind: "seven_day", Label: "7D", UsedPercent: 45, ResetsAt: curNow.Add(72 * time.Hour).Format(time.RFC3339), CheckedAt: curNow.Format(time.RFC3339)},
					},
				}})
			case "codex":
				writeJSON(w, 200, subUsageProbeResult{Account: &subUsageAccount{
					Provider: "codex", AccountHash: "hash-codex-1", Email: "bob@example.com", Plan: "pro", PlanRaw: "pro", PlanDisplay: "Pro 20x", Status: "ok",
					Credit: &subUsageCredit{Left: 48158, Unit: "credits"},
					Windows: []subUsageWindow{
						{Kind: "weekly", Label: "7D", UsedPercent: 25, ResetsAt: curNow.Add(100 * time.Hour).Format(time.RFC3339), CheckedAt: curNow.Format(time.RFC3339)},
					},
				}})
			case "antigravity":
				if macAgyAuthFail.Load() {
					writeJSON(w, 200, subUsageProbeResult{AuthFailed: true, Account: &subUsageAccount{Provider: "antigravity", AccountHash: "hash-agy-shared", Status: "expired", Error: "等待本机 Agent 刷新"}})
					return
				}
				writeJSON(w, 200, subUsageProbeResult{Account: &subUsageAccount{
					Provider: "antigravity", AccountHash: "hash-agy-shared", Email: "shared@example.com", Plan: "g1-ultra-lite-tier", PlanRaw: "g1-ultra-lite-tier", PlanDisplay: "G1 Ultra Lite", Status: "ok",
					Windows: []subUsageWindow{
						{Kind: "gemini_five_hour", Label: "5H", Scope: "model", ScopedModel: "gemini", UsedPercent: 12, ResetsAt: curNow.Add(3 * time.Hour).Format(time.RFC3339), CheckedAt: curNow.Format(time.RFC3339)},
						{Kind: "partner_five_hour", Label: "Partner 5H", Scope: "model", ScopedModel: "partner", UsedPercent: 0, ResetsAt: curNow.Add(4 * time.Hour).Format(time.RFC3339), CheckedAt: curNow.Format(time.RFC3339)},
					},
				}})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer macSrv.Close()

	devSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sub-usage/inventory":
			writeJSON(w, 200, subUsageInventory{
				CollectedAt: curNow.Format(time.RFC3339),
				Today: tokenDayStats{
					Tokens: 800_000, Requests: 140, CacheRead: 700_000, CacheInput: 750_000,
					ByProvider: map[string]tokenDayStats{
						// Claude used via gateway (no local claude subscription on devbox!)
						"claude":      {Tokens: 500_000, Requests: 90, CacheRead: 450_000, CacheInput: 480_000},
						"antigravity": {Tokens: 300_000, Requests: 50, CacheRead: 250_000, CacheInput: 270_000},
					},
				},
				Accounts: []subUsageInventoryItem{
					// Same shared antigravity account, earlier expiry -> MUST NOT be probed unless macpro fails!
					{Provider: "antigravity", AccountHash: "hash-agy-shared", Email: "shared@example.com", TokenValid: true, TokenExpiresAt: curNow.Add(1 * time.Hour).Format(time.RFC3339)},
				},
			})
		case "/api/sub-usage/probe":
			devProbes.Add(1)
			writeJSON(w, 200, subUsageProbeResult{Account: &subUsageAccount{
				Provider: "antigravity", AccountHash: "hash-agy-shared", Email: "shared@example.com", Plan: "g1-ultra-lite-tier", PlanRaw: "g1-ultra-lite-tier", PlanDisplay: "G1 Ultra Lite", Status: "ok",
				Windows: []subUsageWindow{
					{Kind: "gemini_five_hour", Label: "5H", Scope: "model", ScopedModel: "gemini", UsedPercent: 15, ResetsAt: curNow.Add(3 * time.Hour).Format(time.RFC3339), CheckedAt: curNow.Format(time.RFC3339)},
				},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer devSrv.Close()

	oldSrv := httptest.NewServer(http.NotFoundHandler())
	defer oldSrv.Close()

	rewriteClient := func(target string) *http.Client {
		return &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				u := *req.URL
				u.Scheme = "http"
				u.Host = strings.TrimPrefix(target, "http://")
				r2 := req.Clone(req.Context())
				r2.URL = &u
				return http.DefaultTransport.RoundTrip(r2)
			}),
		}
	}

	h.s.herdr.attach(&Machine{ID: "macpro", Name: "macpro", Online: true, client: rewriteClient(macSrv.URL)})
	h.s.herdr.attach(&Machine{ID: "devbox", Name: "devbox", Online: true, client: rewriteClient(devSrv.URL)})
	h.s.herdr.attach(&Machine{ID: "oldnode", Name: "oldnode", Online: true, client: rewriteClient(oldSrv.URL)})
	h.s.usageHub().now = func() time.Time { return curNow }
	h.s.localSubUsageCollector().home = t.TempDir()

	if _, err := h.configure(featureUsage, func(x *ExtSetting) { x.Enabled = true }); err != nil {
		t.Fatal(err)
	}
	if h.run(featureUsage) != nil {
		t.Fatal("usage feature extension must not spawn a subprocess")
	}

	// First fetch: 3 unique accounts across 2 machines -> macpro probed 3 times, devbox probed 0 times!
	slots := h.headerStatuses(context.Background(), "claude-opus-4", true)
	if len(slots) != 1 || slots[0].ID != featureUsage || slots[0].Status == nil {
		t.Fatalf("unexpected header status slots: %+v", slots)
	}
	if gotMac, gotDev := macProbes.Load(), devProbes.Load(); gotMac != 3 || gotDev != 0 {
		t.Fatalf("dedupe-before-probe failed: macpro probes=%d (want 3), devbox probes=%d (want 0)", gotMac, gotDev)
	}

	hs := slots[0].Status
	if hs.Title != "订阅用量" {
		t.Fatalf("title = %q, want 订阅用量", hs.Title)
	}
	if !strings.Contains(hs.Subline, "2h 后重置") || strings.Contains(hs.Subline, "5H 2h 后重置") {
		t.Fatalf("subline = %q, want '2h 后重置' without window prefix", hs.Subline)
	}
	if !strings.Contains(hs.Subline, "今日 2.30M") || !strings.Contains(hs.Subline, "缓存") {
		t.Fatalf("subline = %q, want aggregated today tokens 2.30M & cache hit", hs.Subline)
	}

	// Verify top "订阅额度" cards show Codex Credit and do NOT show machine names;
	// bottom sections are grouped BY MACHINE showing per-vendor today token usage (no quota meters, no credit, 0-token omitted).
	var sawSummary, sawToday, sawDevbox, sawMacpro, sawCodexTopCredit bool
	for _, sec := range hs.Sections {
		switch sec.Title {
		case "订阅额度":
			sawSummary = true
			for _, r := range sec.Rows {
				if r.Sublabel != "" {
					t.Fatalf("top summary row %q must not display machine sublabel, got %q", r.Label, r.Sublabel)
				}
				if r.Label == "Codex" && r.Value == "Credits 48,158" {
					if r.Tone != "muted" {
						t.Fatalf("top Codex credit tone = %q, want muted (same color as reset time)", r.Tone)
					}
					if r.Metric == nil || r.Metric.Label != "Credits" || r.Metric.Value != "48,158" || r.Metric.Tone != "muted" {
						t.Fatalf("top Codex credit metric = %+v, want {Label:Credits Value:48,158 Tone:muted}", r.Metric)
					}
					sawCodexTopCredit = true
				}
			}
		case "今日用量":
			sawToday = true
			if len(sec.Rows) != 3 || sec.Rows[0].Label != "今日" || len(sec.Rows[0].Badges) == 0 {
				t.Fatalf("today section should have total card with cache badge + 2 machine rows, got %+v", sec.Rows)
			}
		case "devbox":
			sawDevbox = true
			if len(sec.Rows) != 2 || sec.Rows[0].Label != "Claude" || sec.Rows[1].Label != "Gemini" {
				t.Fatalf("devbox section should show non-zero vendor token rows (Claude via gateway + Gemini), got %+v", sec.Rows)
			}
			if sec.Rows[0].Sublabel != "" || sec.Rows[1].Sublabel != "shared@example.com" {
				t.Fatalf("devbox vendor subscription metadata mismatch: %+v", sec.Rows)
			}
			for _, r := range sec.Rows {
				if len(r.Meters) != 0 || !strings.HasSuffix(r.Value, " tok") {
					t.Fatalf("machine vendor row must show token usage without quota meters, got %+v", r)
				}
			}
		case "macpro":
			sawMacpro = true
			// Antigravity has 0 tokens on macpro so only Claude and Codex are shown.
			if len(sec.Rows) != 2 || sec.Rows[0].Label != "Claude" || sec.Rows[1].Label != "Codex" {
				t.Fatalf("macpro section should show only non-zero vendor rows (Claude + Codex, omitting 0-token Gemini), got %+v", sec.Rows)
			}
			if strings.Contains(sec.Rows[1].Detail, "credit") || strings.Contains(sec.Rows[1].Value, "Credit") {
				t.Fatalf("macpro Codex row must not show credit below, got %+v", sec.Rows[1])
			}
		}
	}
	if !sawSummary || !sawToday || !sawDevbox || !sawMacpro || !sawCodexTopCredit {
		t.Fatalf("missing expected drawer sections/credit: summary=%v today=%v devbox=%v macpro=%v codexCredit=%v", sawSummary, sawToday, sawDevbox, sawMacpro, sawCodexTopCredit)
	}

	// Verify failover on 401: advance time past TTL, make macpro's Antigravity 401 -> hub fails over to devbox!
	curNow = curNow.Add(6 * time.Minute)
	macAgyAuthFail.Store(true)
	_ = h.headerStatuses(context.Background(), "claude-opus-4", false)
	if devProbes.Load() != 1 {
		t.Fatalf("expected hub to fail over to devbox on macpro 401, got devbox probes=%d", devProbes.Load())
	}

	// Verify set_header_focus("all") shows every available provider with a percentage bar.
	out, err := h.do(context.Background(), featureUsage, "set_header_focus", map[string]any{"focus": "all"})
	if err != nil || !strings.Contains(out.Text, "全部") {
		t.Fatalf("set_header_focus(all): %+v %v", out, err)
	}
	slotsAll := h.headerStatuses(context.Background(), "opus", false)
	if len(slotsAll[0].Status.Items) != 3 {
		t.Fatalf("all mode should show 3 provider items, got %+v", slotsAll[0].Status.Items)
	}
	for _, it := range slotsAll[0].Status.Items {
		if it.Percent == nil {
			t.Fatalf("expected every available provider in all mode to show a percentage bar, got %+v", it)
		}
	}

	// Verify subscription_usage text action.
	txt, err := h.do(context.Background(), featureUsage, "subscription_usage", map[string]any{"fresh": false})
	if err != nil || !strings.Contains(txt.Text, "Claude") || !strings.Contains(txt.Text, "Codex") || !strings.Contains(txt.Text, "Gemini") {
		t.Fatalf("subscription_usage action: %+v %v", txt, err)
	}

	// Verify built-in extension ordering puts usage first.
	h.mu.Lock()
	for _, m := range cat {
		h.cat[m.ID] = m
	}
	h.mu.Unlock()
	views := h.views()
	if len(views) < 2 || views[0].Manifest == nil || views[0].Manifest.ID != featureUsage {
		t.Fatalf("expected %q first in extension views, got %+v", featureUsage, views)
	}
}

func TestSubUsageRateLimitBackoffAndAllExpiredZeroRefresh(t *testing.T) {
	_, h, _ := newExtTestHub(t)
	curNow := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)

	cat, err := extCatalog()
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	for _, m := range cat {
		if m.ID == featureUsage {
			h.cat[m.ID] = m
		}
	}
	h.mu.Unlock()

	var m1Probes, m2Probes atomic.Int32
	var hit429 atomic.Bool
	var allExpired atomic.Bool

	makeNodeSrv := func(probes *atomic.Int32, expOffset time.Duration) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/sub-usage/inventory":
				valid := !allExpired.Load()
				exp := curNow.Add(expOffset)
				if !valid {
					exp = curNow.Add(-10 * time.Minute)
				}
				writeJSON(w, 200, subUsageInventory{
					CollectedAt: curNow.Format(time.RFC3339),
					Accounts: []subUsageInventoryItem{
						{Provider: "codex", AccountHash: "hash-shared", Email: "u@example.com", PlanDisplay: "Pro 20x", TokenValid: valid, TokenExpiresAt: exp.Format(time.RFC3339)},
					},
				})
			case "/api/sub-usage/probe":
				probes.Add(1)
				if hit429.Load() {
					writeJSON(w, 200, subUsageProbeResult{RateLimit: true, RetryAfter: 600, Account: &subUsageAccount{Provider: "codex", AccountHash: "hash-shared", Status: "error", Error: "限流"}})
					return
				}
				writeJSON(w, 200, subUsageProbeResult{Account: &subUsageAccount{
					Provider: "codex", AccountHash: "hash-shared", Email: "u@example.com", PlanDisplay: "Pro 20x", Status: "ok",
					Windows: []subUsageWindow{{Kind: "weekly", Label: "7D", UsedPercent: 20, ResetsAt: curNow.Add(72 * time.Hour).Format(time.RFC3339), CheckedAt: curNow.Format(time.RFC3339)}},
				}})
			default:
				http.NotFound(w, r)
			}
		}))
	}

	s1 := makeNodeSrv(&m1Probes, 3*time.Hour)
	defer s1.Close()
	s2 := makeNodeSrv(&m2Probes, 1*time.Hour)
	defer s2.Close()

	rewriteClient := func(target string) *http.Client {
		return &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				u := *req.URL
				u.Scheme = "http"
				u.Host = strings.TrimPrefix(target, "http://")
				r2 := req.Clone(req.Context())
				r2.URL = &u
				return http.DefaultTransport.RoundTrip(r2)
			}),
		}
	}

	h.s.herdr.attach(&Machine{ID: "m1", Name: "m1", Online: true, client: rewriteClient(s1.URL)})
	h.s.herdr.attach(&Machine{ID: "m2", Name: "m2", Online: true, client: rewriteClient(s2.URL)})
	h.s.usageHub().now = func() time.Time { return curNow }
	h.s.localSubUsageCollector().home = t.TempDir()
	if _, err := h.configure(featureUsage, func(x *ExtSetting) { x.Enabled = true }); err != nil {
		t.Fatal(err)
	}

	// 1. Initial probe succeeds on m1 only (0 probes on m2).
	_ = h.headerStatuses(context.Background(), "", true)
	if m1Probes.Load() != 1 || m2Probes.Load() != 0 {
		t.Fatalf("initial probes: m1=%d m2=%d, want 1, 0", m1Probes.Load(), m2Probes.Load())
	}

	// 2. Advance past TTL and return 429 on m1 -> MUST NOT fail over to m2, and MUST serve cached windows!
	curNow = curNow.Add(6 * time.Minute)
	hit429.Store(true)
	slots429 := h.headerStatuses(context.Background(), "", false)
	if m1Probes.Load() != 2 || m2Probes.Load() != 0 {
		t.Fatalf("429 must not fail over to m2: m1=%d m2=%d", m1Probes.Load(), m2Probes.Load())
	}
	if !strings.Contains(slots429[0].Status.Compact, "80%") {
		t.Fatalf("429 should retain last good cached 80%% window, got compact=%q", slots429[0].Status.Compact)
	}

	// 3. Within backoff window (even with fresh=true), no additional probe is sent.
	curNow = curNow.Add(2 * time.Minute)
	_ = h.headerStatuses(context.Background(), "", true)
	if m1Probes.Load() != 2 || m2Probes.Load() != 0 {
		t.Fatalf("during 429 backoff no probe should fire: m1=%d m2=%d", m1Probes.Load(), m2Probes.Load())
	}

	// 4. Advance past backoff + stale cache window and mark ALL tokens expired -> zero probes, shows 等待刷新!
	curNow = curNow.Add(45 * time.Minute)
	hit429.Store(false)
	allExpired.Store(true)
	slotsExp := h.headerStatuses(context.Background(), "", true)
	if m1Probes.Load() != 2 || m2Probes.Load() != 0 {
		t.Fatalf("all-expired accounts must not trigger any probe: m1=%d m2=%d", m1Probes.Load(), m2Probes.Load())
	}
	if !strings.Contains(slotsExp[0].Status.Compact, "等待刷新") {
		t.Fatalf("all-expired should show 等待刷新, got compact=%q", slotsExp[0].Status.Compact)
	}
}

func TestSubUsageProxyConfigAndForwarding(t *testing.T) {
	// 1. Verify resolveSubUsageProxy handles global URL, per-machine mapping, fallback, and direct.
	if got := resolveSubUsageProxy("http://127.0.0.1:7890", "node1", "host1", false); got != "http://127.0.0.1:7890" {
		t.Fatalf("global proxy = %q", got)
	}
	spec := "host1=socks5h://127.0.0.1:1080, host2=direct"
	if got := resolveSubUsageProxy(spec, "id1", "host1", false); got != "socks5h://127.0.0.1:1080" {
		t.Fatalf("host1 proxy = %q", got)
	}
	if got := resolveSubUsageProxy(spec, "id2", "host2", false); got != "direct" {
		t.Fatalf("host2 proxy = %q", got)
	}
	if got := resolveSubUsageProxy(spec, "id3", "host3", false); got != "" {
		t.Fatalf("unmatched host3 proxy = %q, want empty", got)
	}

	// 2. Verify probeOne routes HTTP requests through the explicit proxy URL.
	home := t.TempDir()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	claudeCreds := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"sk-ant-test-tok","expiresAt":%d,"subscriptionType":"pro"}}`, now.Add(time.Hour).UnixMilli())
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(claudeCreds), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))

	var proxyHits atomic.Int32
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"five_hour":{"utilization":20,"resets_at":%q}}`, now.Add(time.Hour).Format(time.RFC3339))
	}))
	defer proxySrv.Close()

	c := newSubUsageCollector(home, nil)
	c.now = func() time.Time { return now }
	c.claudeURL = "http://usage.invalid/api/oauth/usage"

	res := c.probeOne(context.Background(), "claude", "", proxySrv.URL)
	if proxyHits.Load() != 1 || res.Account == nil || res.Account.Status != "ok" || len(res.Account.Windows) != 1 {
		t.Fatalf("proxy probe failed: hits=%d res=%+v", proxyHits.Load(), res)
	}
}

func TestSubUsageDefaultEnabledAndPersistedFalse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COLLIE_HOME", dir)
	db, err := openDB(filepath.Join(dir, "herd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := loadState(db)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{herdr: newHerdr("", "test", dir), home: dir}
	sp := &Super{s: srv, db: db, dir: dir, bin: "collie", st: st, subs: map[chan struct{}]bool{}}
	srv.sup = sp

	// 1. Fresh install (no ext_settings saved): usage is enabled by default, others stay off.
	h := newExtHost(srv, sp, dir)
	srv.ext = h
	srv.localSubUsageCollector().home = t.TempDir()
	if !h.setting(featureUsage).Enabled || !h.featureOn(featureUsage) {
		t.Fatalf("fresh install: expected usage enabled by default, got %+v", h.setting(featureUsage))
	}
	if v := h.view(featureUsage); !v.Setting.Enabled {
		t.Fatalf("fresh install: expected usage view enabled by default, got %+v", v.Setting)
	}
	for _, id := range []string{featureWorkbench, "larkx", "wechat"} {
		if h.manifest(id) != nil && h.setting(id).Enabled {
			t.Fatalf("fresh install: expected %q disabled by default", id)
		}
	}

	// 2. Explicitly persist enabled=false with existing config via standard HTTP API.
	const wantProxy = "host1=socks5h://127.0.0.1:1080"
	postExt := func(payload string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/api/ext/usage", strings.NewReader(payload))
		req.SetPathValue("id", featureUsage)
		rec := httptest.NewRecorder()
		srv.extConfigure(rec, req)
		if rec.Code != 200 {
			t.Fatalf("POST /api/ext/usage %s: status %d body=%s", payload, rec.Code, rec.Body.String())
		}
	}
	postExt(fmt.Sprintf(`{"enabled":false,"config":{"proxy_url":%q}}`, wantProxy))

	// 3. Reload a fresh extHost from the same DB: persisted false MUST NOT be overwritten by default-on.
	h2 := newExtHost(srv, sp, dir)
	srv.ext = h2
	got := h2.setting(featureUsage)
	if got.Enabled || h2.featureOn(featureUsage) {
		t.Fatalf("reloaded host: persisted enabled=false was overwritten: %+v", got)
	}
	if got.Config["proxy_url"] != wantProxy {
		t.Fatalf("reloaded host: proxy_url = %v, want %q", got.Config["proxy_url"], wantProxy)
	}
	if slots := h2.headerStatuses(context.Background(), "claude-opus-4", false); len(slots) != 0 {
		t.Fatalf("reloaded host: disabled usage returned header slots: %+v", slots)
	}
	if _, err := h2.do(context.Background(), featureUsage, "subscription_usage", nil); err == nil {
		t.Fatal("reloaded host: disabled usage should refuse actions")
	}

	// 4. Config-only update without enabled field preserves persisted false.
	postExt(fmt.Sprintf(`{"config":{"proxy_url":%q,"refresh_s":180}}`, wantProxy))
	gotAfterCfg := h2.setting(featureUsage)
	if gotAfterCfg.Enabled {
		t.Fatalf("config-only update flipped persisted false to true: %+v", gotAfterCfg)
	}
	if gotAfterCfg.Config["proxy_url"] != wantProxy {
		t.Fatalf("config-only update lost proxy_url: %+v", gotAfterCfg.Config)
	}

	// 5. Non-feature extensions cannot set defaults.enabled=true.
	bad := fakeManifest("badext", "actions")
	bad.Defaults.Enabled = true
	if err := bad.validate(); err == nil {
		t.Fatal("expected non-feature manifest with defaults.enabled=true to fail validation")
	}
}

func TestSubUsageAgyKeychainDecodeMetaAndCaching(t *testing.T) {
	// 1. Parse macOS `security find-generic-password` metadata output (no -w).
	sampleMeta := `keychain: "/Users/me/Library/Keychains/login.keychain-db"
class: "genp"
attributes:
    "acct"<blob>="antigravity"
    "cdat"<timedate>=0x32303236313132353037353830325A00  "20261125075802Z\000"
    "desc"<blob>=<NULL>
    "mdat"<timedate>=0x32303236313132373134353235385A00  "20261127145258Z\000"
    "svce"<blob>="gemini"
`
	meta := parseKeychainMetaOutput(sampleMeta)
	if meta.cdat != "20261125075802Z" || meta.mdat != "20261127145258Z" {
		t.Fatalf("parseKeychainMetaOutput = %+v", meta)
	}
	if got := formatKeychainTimeRFC3339(meta.mdat); got != "2026-11-27T14:52:58Z" {
		t.Fatalf("formatKeychainTimeRFC3339 = %q", got)
	}

	// 2. Decode plain JSON, go-keyring-base64: (including writer_id + RFC3339Nano timezone), and hex-wrapped go-keyring-base64:.
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	plainJSON := fmt.Sprintf(`{"token":{"access_token":"ya29.kc-secret-1","token_type":"Bearer","refresh_token":"1//refresh-secret","expiry":"2026-11-27T23:52:57.504735+08:00","expires_in":3599},"auth_method":"consumer","id_token_email":"user@example.com","writer_id":"agy-1"}`)
	wantExp, _ := time.Parse(time.RFC3339Nano, "2026-11-27T23:52:57.504735+08:00")
	b64Payload := "go-keyring-base64:" + base64.StdEncoding.EncodeToString([]byte(plainJSON))
	hexPayload := hex.EncodeToString([]byte(b64Payload))
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"plain", plainJSON},
		{"b64", b64Payload},
		{"hex+b64", hexPayload},
	} {
		tok, em, exp, ok := parseAgyTokenJSON(decodeKeychainSecret([]byte(tc.raw)))
		if !ok || tok != "ya29.kc-secret-1" || em != "user@example.com" || !exp.Equal(wantExp) {
			t.Fatalf("%s decode/parse mismatch: ok=%v tok=%q em=%q exp=%v", tc.name, ok, tok, em, exp)
		}
	}

	// 3. Verify inventory caching minimizes -w secret reads across repeated polls, token refreshes, and process restarts.
	home := t.TempDir()
	cacheDir := t.TempDir()
	c := newSubUsageCollector(home, nil)
	c.cacheDir = cacheDir
	c.now = func() time.Time { return now }

	curMeta := keychainMeta{cdat: "20261125075802Z", mdat: "20261127145258Z"}
	curSecret := b64Payload
	metaOK := true
	secretOK := true
	var metaReads, secretReads atomic.Int32

	c.readKeychainMetaFn = func(_ context.Context, svc, acct string) (keychainMeta, bool) {
		if svc != agyKeychainService || acct != agyKeychainAccount {
			t.Errorf("unexpected keychain lookup svc=%q acct=%q", svc, acct)
		}
		metaReads.Add(1)
		return curMeta, metaOK
	}
	c.readKeychainSecretFn = func(_ context.Context, svc, acct string) ([]byte, bool) {
		secretReads.Add(1)
		if !secretOK {
			return nil, false
		}
		return []byte(curSecret), true
	}

	// Cold inventory reads -w once, flags FromKeychain, and persists non-secret metadata to disk.
	inv1 := c.inventory(context.Background())
	if len(inv1.Accounts) != 1 || !inv1.Accounts[0].FromKeychain || inv1.Accounts[0].Email != "user@example.com" || !inv1.Accounts[0].TokenValid {
		t.Fatalf("cold keychain inventory = %+v", inv1.Accounts)
	}
	if secretReads.Load() != 1 {
		t.Fatalf("cold inventory secretReads = %d, want 1", secretReads.Load())
	}
	diskBytes, err := os.ReadFile(filepath.Join(cacheDir, agyKeychainCacheFile))
	if err != nil {
		t.Fatalf("expected non-secret disk cache file: %v", err)
	}
	if strings.Contains(string(diskBytes), "ya29.") || strings.Contains(string(diskBytes), "refresh-secret") {
		t.Fatalf("disk cache leaked secret: %s", string(diskBytes))
	}

	// Simulated process restart/upgrade: a fresh collector with the same cacheDir
	// and unchanged cdat must NOT touch -w during inventory.
	cRestart := newSubUsageCollector(home, nil)
	cRestart.cacheDir = cacheDir
	cRestart.now = func() time.Time { return now }
	cRestart.readKeychainMetaFn = c.readKeychainMetaFn
	cRestart.readKeychainSecretFn = c.readKeychainSecretFn
	invRestart := cRestart.inventory(context.Background())
	if len(invRestart.Accounts) != 1 || invRestart.Accounts[0].Email != "user@example.com" || !invRestart.Accounts[0].FromKeychain {
		t.Fatalf("post-restart inventory = %+v", invRestart.Accounts)
	}
	if secretReads.Load() != 1 {
		t.Fatalf("post-restart inventory must reuse disk metadata without -w, got %d", secretReads.Load())
	}

	// Repeated inventory with unchanged mdat must NOT touch -w.
	_ = c.inventory(context.Background())
	_ = c.inventory(context.Background())
	if secretReads.Load() != 1 {
		t.Fatalf("cached inventory must not re-read -w, got %d", secretReads.Load())
	}

	// In-place token refresh (same cdat, new mdat) must NOT touch -w during inventory,
	// while updating TokenExpiresAt so hub can clear any prior auth block.
	curMeta.mdat = "20261127155500Z"
	invRefreshed := c.inventory(context.Background())
	if secretReads.Load() != 1 {
		t.Fatalf("mdat-updated inventory must not re-read -w, got %d", secretReads.Load())
	}
	if len(invRefreshed.Accounts) != 1 || !invRefreshed.Accounts[0].TokenValid || invRefreshed.Accounts[0].TokenExpiresAt != "2026-11-27T15:55:00Z" {
		t.Fatalf("refreshed inventory = %+v", invRefreshed.Accounts)
	}

	// Account recreation (new cdat) invalidates cache and reads -w once.
	curMeta.cdat = "20261127160000Z"
	curMeta.mdat = "20261127160000Z"
	newJSON := fmt.Sprintf(`{"token":{"access_token":"ya29.kc-secret-2","expiry":%q},"id_token_email":"other@example.com"}`, now.Add(2*time.Hour).Format(time.RFC3339))
	curSecret = "go-keyring-base64:" + base64.StdEncoding.EncodeToString([]byte(newJSON))
	invNew := c.inventory(context.Background())
	if secretReads.Load() != 2 || len(invNew.Accounts) != 1 || invNew.Accounts[0].Email != "other@example.com" {
		t.Fatalf("new cdat inventory: secretReads=%d accounts=%+v", secretReads.Load(), invNew.Accounts)
	}

	// Denied/failed -w read is negative-cached for same (cdat, mdat) so we don't spam dialogs.
	curMeta.cdat = "20261127170000Z"
	curMeta.mdat = "20261127170000Z"
	secretOK = false
	if invFail1 := c.inventory(context.Background()); len(invFail1.Accounts) != 0 {
		t.Fatalf("expected empty inventory on denied keychain, got %+v", invFail1.Accounts)
	}
	if invFail2 := c.inventory(context.Background()); len(invFail2.Accounts) != 0 {
		t.Fatalf("expected empty inventory on negative cache, got %+v", invFail2.Accounts)
	}
	if secretReads.Load() != 3 {
		t.Fatalf("negative cache must prevent repeated -w prompts, got secretReads=%d want 3", secretReads.Load())
	}
}

func TestSubUsageHubPrefersNonKeychainOverKeychain(t *testing.T) {
	_, h, _ := newExtTestHub(t)
	curNow := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)

	cat, err := extCatalog()
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	for _, m := range cat {
		if m.ID == featureUsage {
			h.cat[m.ID] = m
		}
	}
	h.mu.Unlock()

	var macProbes, devProbes atomic.Int32
	var devAuthFail atomic.Bool

	// macpro has LATER expiry and alphabetically earlier name, but FromKeychain=true.
	macSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sub-usage/inventory":
			writeJSON(w, 200, subUsageInventory{
				CollectedAt: curNow.Format(time.RFC3339),
				Accounts: []subUsageInventoryItem{
					{Provider: "antigravity", AccountHash: "hash-agy-1", Email: "shared@example.com", TokenValid: true, TokenExpiresAt: curNow.Add(4 * time.Hour).Format(time.RFC3339), FromKeychain: true},
				},
			})
		case "/api/sub-usage/probe":
			macProbes.Add(1)
			writeJSON(w, 200, subUsageProbeResult{Account: &subUsageAccount{
				Provider: "antigravity", AccountHash: "hash-agy-1", Email: "shared@example.com", PlanDisplay: "G1 Ultra Lite", Status: "ok",
				Windows: []subUsageWindow{{Kind: "gemini_five_hour", Label: "5H", Scope: "model", ScopedModel: "gemini", UsedPercent: 10, ResetsAt: curNow.Add(3 * time.Hour).Format(time.RFC3339), CheckedAt: curNow.Format(time.RFC3339)}},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer macSrv.Close()

	// devbox has EARLIER expiry, but FromKeychain=false (file-backed).
	devSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sub-usage/inventory":
			writeJSON(w, 200, subUsageInventory{
				CollectedAt: curNow.Format(time.RFC3339),
				Accounts: []subUsageInventoryItem{
					{Provider: "antigravity", AccountHash: "hash-agy-1", Email: "shared@example.com", TokenValid: true, TokenExpiresAt: curNow.Add(1 * time.Hour).Format(time.RFC3339), FromKeychain: false},
				},
			})
		case "/api/sub-usage/probe":
			devProbes.Add(1)
			if devAuthFail.Load() {
				writeJSON(w, 200, subUsageProbeResult{AuthFailed: true, Account: &subUsageAccount{Provider: "antigravity", AccountHash: "hash-agy-1", Status: "expired"}})
				return
			}
			writeJSON(w, 200, subUsageProbeResult{Account: &subUsageAccount{
				Provider: "antigravity", AccountHash: "hash-agy-1", Email: "shared@example.com", PlanDisplay: "G1 Ultra Lite", Status: "ok",
				Windows: []subUsageWindow{{Kind: "gemini_five_hour", Label: "5H", Scope: "model", ScopedModel: "gemini", UsedPercent: 12, ResetsAt: curNow.Add(3 * time.Hour).Format(time.RFC3339), CheckedAt: curNow.Format(time.RFC3339)}},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer devSrv.Close()

	rewriteClient := func(target string) *http.Client {
		return &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				u := *req.URL
				u.Scheme = "http"
				u.Host = strings.TrimPrefix(target, "http://")
				r2 := req.Clone(req.Context())
				r2.URL = &u
				return http.DefaultTransport.RoundTrip(r2)
			}),
		}
	}

	h.s.herdr.attach(&Machine{ID: "macpro", Name: "macpro", Online: true, client: rewriteClient(macSrv.URL)})
	h.s.herdr.attach(&Machine{ID: "devbox", Name: "devbox", Online: true, client: rewriteClient(devSrv.URL)})
	h.s.usageHub().now = func() time.Time { return curNow }
	h.s.localSubUsageCollector().home = t.TempDir()
	if _, err := h.configure(featureUsage, func(x *ExtSetting) { x.Enabled = true }); err != nil {
		t.Fatal(err)
	}

	// 1. Hub must pick devbox (!FromKeychain) and NOT probe macpro (FromKeychain).
	_ = h.headerStatuses(context.Background(), "", true)
	if devProbes.Load() != 1 || macProbes.Load() != 0 {
		t.Fatalf("expected hub to prefer file-backed devbox over keychain macpro: dev=%d mac=%d", devProbes.Load(), macProbes.Load())
	}

	// 2. When devbox returns 401, hub fails over to keychain-backed macpro.
	curNow = curNow.Add(6 * time.Minute)
	devAuthFail.Store(true)
	_ = h.headerStatuses(context.Background(), "", false)
	if devProbes.Load() != 2 || macProbes.Load() != 1 {
		t.Fatalf("expected failover to macpro on devbox 401: dev=%d mac=%d", devProbes.Load(), macProbes.Load())
	}
}
