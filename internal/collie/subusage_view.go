package collie

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

const (
	subRecentResetWindow = 2 * time.Hour
)

var (
	subNonAlnumRe   = regexp.MustCompile(`[^a-z0-9]+`)
	subMultiplierRe = regexp.MustCompile(`(\d+)\s*x(?:[^a-z0-9]|$)`)
	subOpenAIORe    = regexp.MustCompile(`^o[134](?:-|$)`)
)

// aggSubAccount is a normalized subscription entry after optional cross-machine
// deduplication by (provider, account_hash).
type aggSubAccount struct {
	Provider    string
	AccountHash string
	Machines    []string
	Email       string
	Tier        string // canonical weight key: Ultra | Max | Pro | Free
	TierDisplay string // e.g. Max 20x ($200), Pro 20x, G1 Ultra Lite
	Status      string // ok | limited | expired | error
	Error       string
	AvailableAt *time.Time
	Windows     []parsedSubWindow
	Credit      *subUsageCredit
}

type parsedSubWindow struct {
	Label    string
	Model    string // "" for account-wide, or "gemini" / "partner" / "sonnet" / ...
	PctUsed  float64
	ResetsAt *time.Time
	Checked  time.Time
	Rank     float64
}

type subPoolWindow struct {
	Label       string
	AvgUsed     float64
	Remaining   float64
	NextResetAt *time.Time
	JustReset   bool
}

type subPoolSummary struct {
	Provider        string
	Available       int
	Total           int
	SingleTier      string // populated when Total == 1
	SingleMachines  []string
	Windows         []subPoolWindow
	CreditLeft      float64 // summed open-ended credit balance (e.g. Codex credits)
	CreditUnit      string
	NextAvailableAt *time.Time
	SingleStatus    string // ok | limited | expired | error when Total == 1
}

func aggregateSubAccounts(nodes []subUsageMachineNode, dedupe bool, now time.Time) []aggSubAccount {
	var out []aggSubAccount
	byKey := map[string]int{}

	for _, n := range nodes {
		mName := subMachineDisplayName(n)
		for _, a := range n.Report.Accounts {
			prov := strings.ToLower(strings.TrimSpace(a.Provider))
			if prov == "" {
				continue
			}
			key := accountGroupKey(prov, a.AccountHash, n.ID, dedupe)

			cand := aggSubAccount{
				Provider:    prov,
				AccountHash: a.AccountHash,
				Machines:    []string{mName},
				Email:       strings.TrimSpace(a.Email),
				Tier:        resolveSubTier(prov, a.Plan, a.PlanRaw, a.PlanDisplay),
				TierDisplay: formatSubPlanDisplay(prov, a.Plan, a.PlanRaw, a.PlanDisplay),
				Status:      normalizeSubStatus(a.Status),
				Error:       strings.TrimSpace(a.Error),
				Windows:     parseSubWindows(a.Windows),
				Credit:      a.Credit,
			}
			if t, ok := parseSubTime(a.AvailableAt); ok {
				cand.AvailableAt = &t
			}

			if idx, exists := byKey[key]; exists {
				existing := &out[idx]
				existing.Machines = appendUniqueSorted(existing.Machines, mName)
				if existing.Email == "" && cand.Email != "" {
					existing.Email = cand.Email
				}
				if existing.TierDisplay == "" && cand.TierDisplay != "" {
					existing.TierDisplay = cand.TierDisplay
					existing.Tier = cand.Tier
				}
				if shouldPreferSubReading(cand, *existing, now) {
					machines := existing.Machines
					email := existing.Email
					if cand.Email != "" {
						email = cand.Email
					}
					tier, tierDisp := existing.Tier, existing.TierDisplay
					if cand.TierDisplay != "" {
						tier, tierDisp = cand.Tier, cand.TierDisplay
					}
					*existing = cand
					existing.Machines = machines
					existing.Email = email
					existing.Tier = tier
					existing.TierDisplay = tierDisp
				}
				continue
			}
			byKey[key] = len(out)
			out = append(out, cand)
		}
	}
	return out
}

func subMachineDisplayName(n subUsageMachineNode) string {
	mName := strings.TrimSpace(n.Name)
	if mName == "" {
		if n.Local {
			return "hub"
		}
		return n.ID
	}
	return mName
}

func appendUniqueSorted(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	list = append(list, v)
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i]) < strings.ToLower(list[j]) })
	return list
}

func normalizeSubStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ok", "limited", "expired", "error":
		return strings.ToLower(strings.TrimSpace(s))
	default:
		return "ok"
	}
}

func subReadingQuality(a aggSubAccount) int {
	score := 0
	switch a.Status {
	case "ok":
		score += 40
	case "limited":
		score += 30
	case "expired":
		score += 10
	case "error":
		score += 5
	}
	if len(a.Windows) > 0 {
		score += 20
	}
	if a.Error == "" {
		score += 5
	}
	return score
}

func newestWindowTime(ws []parsedSubWindow) time.Time {
	var best time.Time
	for _, w := range ws {
		if w.Checked.After(best) {
			best = w.Checked
		}
	}
	return best
}

func shouldPreferSubReading(cand, cur aggSubAccount, _ time.Time) bool {
	qc, qcur := subReadingQuality(cand), subReadingQuality(cur)
	if qc != qcur {
		return qc > qcur
	}
	return newestWindowTime(cand.Windows).After(newestWindowTime(cur.Windows))
}

func parseSubWindows(raws []subUsageWindow) []parsedSubWindow {
	out := make([]parsedSubWindow, 0, len(raws))
	for _, rw := range raws {
		lbl := strings.TrimSpace(rw.Label)
		if lbl == "" {
			continue
		}
		var rst *time.Time
		if t, ok := parseSubTime(rw.ResetsAt); ok {
			rst = &t
		}
		checked, _ := parseSubTime(rw.CheckedAt)
		out = append(out, parsedSubWindow{
			Label:    lbl,
			Model:    strings.ToLower(strings.TrimSpace(rw.ScopedModel)),
			PctUsed:  clampPct(rw.UsedPercent),
			ResetsAt: rst,
			Checked:  checked,
			Rank:     subWindowRank(lbl, rw.ScopedModel),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rank != out[j].Rank {
			return out[i].Rank < out[j].Rank
		}
		return out[i].Label < out[j].Label
	})
	return out
}

func subWindowRank(label, scopedModel string) float64 {
	if scopedModel == "partner" || strings.HasPrefix(label, "Partner") {
		if strings.Contains(label, "7D") {
			return 2.3
		}
		return 2.2
	}
	switch label {
	case "3H":
		return -0.2
	case "5H":
		return 0
	case "1D":
		return 0.5
	case "7D":
		return 1
	case "月":
		return 1.5
	case "加量":
		return 3.5
	default:
		if scopedModel != "" {
			return 2.5
		}
		return 2
	}
}

func parseSubTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func resolveSubTier(provider, plan, planRaw, planDisplay string) string {
	txt := strings.ToLower(planRaw + " " + planDisplay + " " + plan)
	switch provider {
	case "", "claude":
		if m := subMultiplierRe.FindStringSubmatch(txt); len(m) == 2 {
			if m[1] == "20" {
				return "Ultra"
			}
			if m[1] == "5" {
				return "Max"
			}
		}
		if strings.Contains(txt, "ultra") {
			return "Ultra"
		}
		if strings.Contains(txt, "max") {
			return "Max"
		}
	case "codex":
		if strings.Contains(txt, "prolite") || strings.Contains(txt, "pro_lite") || strings.Contains(txt, "pro-lite") {
			return "Max"
		}
		if strings.Contains(txt, "pro") {
			return "Pro"
		}
	case "antigravity":
		if strings.Contains(txt, "ultra") {
			return "Ultra"
		}
		if strings.Contains(txt, "free") {
			return "Free"
		}
		if strings.Contains(txt, "pro") {
			return "Pro"
		}
	}
	p := strings.TrimSpace(plan)
	if p == "" {
		return ""
	}
	return strings.ToUpper(p[:1]) + p[1:]
}

func formatSubPlanDisplay(provider, plan, planRaw, planName string) string {
	raw := strings.ToLower(strings.TrimSpace(planRaw))
	p := strings.ToLower(strings.TrimSpace(plan))
	txt := strings.ToLower(strings.TrimSpace(planRaw + " " + planName + " " + plan))

	switch provider {
	case "", "claude":
		if strings.Contains(txt, "enterprise") {
			return "Enterprise"
		}
		if strings.Contains(txt, "team") {
			return "Team"
		}
		if m := subMultiplierRe.FindStringSubmatch(txt); len(m) == 2 {
			switch m[1] {
			case "20":
				return "Max 20x ($200)"
			case "5":
				return "Max 5x ($100)"
			default:
				return "Max " + m[1] + "x"
			}
		}
		if strings.Contains(txt, "ultra") {
			return "Max 20x ($200)"
		}
		if raw == "default_claude_ai" || raw == "claude_pro" || p == "pro" {
			return "Pro ($20)"
		}
		if strings.Contains(raw, "claude_max") || p == "max" {
			return "Max"
		}
	case "codex":
		if m := subMultiplierRe.FindStringSubmatch(txt); len(m) == 2 {
			return "Pro " + m[1] + "x"
		}
		switch {
		case raw == "prolite" || strings.Contains(raw, "prolite") || strings.Contains(raw, "pro_lite") || strings.Contains(raw, "pro-lite"):
			return "ProLite 5x"
		case raw == "pro" || p == "pro":
			return "Pro 20x"
		case raw == "plus" || p == "plus":
			return "Plus"
		case strings.Contains(txt, "team"):
			return "Team"
		case strings.Contains(txt, "enterprise"):
			return "Enterprise"
		case raw == "free" || p == "free":
			return "Free"
		}
	case "antigravity":
		switch raw {
		case "g1-ultra-lite-tier", "g1_ultra_lite_tier":
			return "G1 Ultra Lite"
		case "g1-ultra-tier", "g1_ultra_tier":
			return "G1 Ultra"
		case "g1-pro-tier", "g1_pro_tier":
			return "Pro"
		case "free-tier", "g1-free-tier", "g1_free_tier":
			return "Free"
		}
		if strings.TrimSpace(planName) != "" {
			return strings.TrimSpace(planName)
		}
		if raw != "" {
			return subTitleRaw(raw)
		}
	}
	if strings.TrimSpace(planName) != "" {
		return strings.TrimSpace(planName)
	}
	if raw != "" && p == "" {
		return subTitleRaw(raw)
	}
	if p != "" {
		return strings.ToUpper(p[:1]) + p[1:]
	}
	return ""
}

func subTitleRaw(raw string) string {
	parts := subNonAlnumRe.Split(raw, -1)
	var out []string
	for _, s := range parts {
		if s == "" {
			continue
		}
		if s[0] >= '0' && s[0] <= '9' {
			out = append(out, strings.ToUpper(s))
		} else {
			out = append(out, strings.ToUpper(s[:1])+s[1:])
		}
	}
	return strings.Join(out, " ")
}

func subAccountAvailable(a aggSubAccount, now time.Time) bool {
	if a.Status == "expired" || a.Status == "error" {
		return false
	}
	if a.Status == "limited" {
		return a.AvailableAt != nil && !a.AvailableAt.After(now)
	}
	return true
}

func subAccountWeight(a aggSubAccount) float64 {
	tier := strings.ToLower(a.Tier)
	switch a.Provider {
	case "antigravity":
		switch tier {
		case "ultra":
			return 6
		case "free":
			return 0.25
		default:
			return 1
		}
	case "codex":
		if tier == "pro" {
			return 16
		}
		if tier == "max" {
			return 5
		}
		return 1
	default:
		switch tier {
		case "ultra":
			return 12
		case "max":
			return 5
		default:
			return 1
		}
	}
}

func subLeafModel(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	if i := strings.IndexByte(id, '['); i >= 0 {
		id = id[:i]
	}
	return strings.ToLower(strings.TrimSpace(id))
}

func subProvidersForModel(model string) []string {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	leaf := subLeafModel(model)
	if leaf == "" {
		return nil
	}
	if segs := strings.Split(trimmed, "/"); len(segs) >= 2 {
		switch segs[len(segs)-2] {
		case "antigravity", "google":
			return []string{"antigravity"}
		case "codex", "openai":
			return []string{"codex"}
		case "claude", "anthropic":
			return []string{"claude"}
		}
	}
	switch {
	case strings.HasPrefix(leaf, "gemini-") || leaf == "antigravity" || leaf == "agy":
		return []string{"antigravity"}
	case strings.HasPrefix(leaf, "claude-") || strings.HasPrefix(leaf, "opus") || strings.HasPrefix(leaf, "sonnet") || strings.HasPrefix(leaf, "haiku"):
		return []string{"claude", "antigravity"}
	case strings.HasPrefix(leaf, "gpt-") || strings.HasPrefix(leaf, "codex") || subOpenAIORe.MatchString(leaf):
		return []string{"codex", "antigravity"}
	default:
		return nil
	}
}

func subProviderOrder(p string) int {
	switch p {
	case "claude":
		return 0
	case "codex":
		return 1
	case "antigravity":
		return 2
	default:
		return 10
	}
}

func subProviderLabel(provider, model string) string {
	switch strings.ToLower(provider) {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "antigravity":
		leaf := subLeafModel(model)
		if leaf == "" || strings.HasPrefix(leaf, "gemini-") || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "antigravity/") {
			return "Gemini"
		}
		return "Antigravity"
	default:
		if provider == "" {
			return "?"
		}
		return strings.ToUpper(provider[:1]) + provider[1:]
	}
}

func subProviderGroupTitle(provider string) string {
	switch strings.ToLower(provider) {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "antigravity":
		return "Antigravity · Gemini"
	default:
		return subProviderLabel(provider, "")
	}
}

func subProviderRowName(provider string) string {
	switch strings.ToLower(provider) {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "antigravity":
		return "Antigravity"
	default:
		return subProviderLabel(provider, "")
	}
}

func subProviderIconKey(provider string) string {
	switch strings.ToLower(provider) {
	case "claude":
		return "claude"
	case "codex":
		return "codex"
	case "antigravity":
		return "agy"
	default:
		return ""
	}
}

func subProviderAccentKey(provider string) string {
	switch strings.ToLower(provider) {
	case "claude":
		return "claude"
	case "codex":
		return "codex"
	case "antigravity":
		return "gemini"
	default:
		return ""
	}
}

func subWindowHasReset(w parsedSubWindow, now time.Time) bool {
	return w.ResetsAt != nil && !w.ResetsAt.After(now)
}

func subWindowAppliesToModel(w parsedSubWindow, model string) bool {
	if w.Model == "" {
		return true
	}
	leaf := subLeafModel(model)
	if leaf == "" {
		return w.Model == "gemini"
	}
	if w.Model == "partner" {
		return strings.HasPrefix(leaf, "claude-") || strings.HasPrefix(leaf, "opus") || strings.HasPrefix(leaf, "sonnet") || strings.HasPrefix(leaf, "haiku") || strings.HasPrefix(leaf, "gpt-oss-")
	}
	return strings.Contains("-"+subSlug(leaf)+"-", "-"+w.Model+"-")
}

func subSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = subNonAlnumRe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

func summarizeSubPools(accounts []aggSubAccount, model string, now time.Time, includeAllScoped bool) []subPoolSummary {
	hasProvider := map[string]bool{}
	groups := map[string][]aggSubAccount{}
	for _, a := range accounts {
		if a.Provider == "" {
			continue
		}
		hasProvider[a.Provider] = true
		groups[a.Provider] = append(groups[a.Provider], a)
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		oi, oj := subProviderOrder(keys[i]), subProviderOrder(keys[j])
		if oi != oj {
			return oi < oj
		}
		return keys[i] < keys[j]
	})

	poolModelFor := func(prov string) string {
		for _, c := range subProvidersForModel(model) {
			if hasProvider[c] {
				if c == prov {
					return model
				}
				return ""
			}
		}
		return model
	}

	pools := make([]subPoolSummary, 0, len(keys))
	for _, prov := range keys {
		members := groups[prov]
		pm := poolModelFor(prov)
		type accPair struct{ weightedUsed, weight float64 }
		acc := map[string]*accPair{}
		earliestReset := map[string]time.Time{}
		var earliestAnyReset *time.Time
		availCount := 0
		sawExpired5H := false

		collectWins := func(useModel string) {
			for _, a := range members {
				avail := subAccountAvailable(a, now)
				wgt := subAccountWeight(a)
				for _, w := range a.Windows {
					if !includeAllScoped && !subWindowAppliesToModel(w, useModel) {
						continue
					}
					if subWindowHasReset(w, now) {
						if avail && w.Label == "5H" && w.ResetsAt != nil && now.Sub(*w.ResetsAt) < subRecentResetWindow {
							sawExpired5H = true
						}
						continue
					}
					if w.ResetsAt != nil && w.ResetsAt.After(now) {
						if prev, ok := earliestReset[w.Label]; !ok || w.ResetsAt.Before(prev) {
							earliestReset[w.Label] = *w.ResetsAt
						}
						if earliestAnyReset == nil || w.ResetsAt.Before(*earliestAnyReset) {
							t := *w.ResetsAt
							earliestAnyReset = &t
						}
					}
					if !avail {
						continue
					}
					p := acc[w.Label]
					if p == nil {
						p = &accPair{}
						acc[w.Label] = p
					}
					p.weightedUsed += w.PctUsed * wgt
					p.weight += wgt
				}
			}
		}

		var creditLeft float64
		var creditUnit string
		for _, a := range members {
			if subAccountAvailable(a, now) {
				availCount++
			}
			if a.Credit != nil && a.Credit.Left > 0 && a.Credit.Limit <= 0 {
				creditLeft += a.Credit.Left
				if creditUnit == "" && a.Credit.Unit != "" {
					creditUnit = a.Credit.Unit
				}
			}
		}
		collectWins(pm)
		if !includeAllScoped && len(acc) == 0 && pm != "" {
			collectWins("")
		}

		var nextAvail *time.Time
		if availCount == 0 {
			for _, a := range members {
				if a.AvailableAt != nil && a.AvailableAt.After(now) {
					if nextAvail == nil || a.AvailableAt.Before(*nextAvail) {
						t := *a.AvailableAt
						nextAvail = &t
					}
				}
			}
			if nextAvail == nil {
				nextAvail = earliestAnyReset
			}
		}

		labels := make([]string, 0, len(acc))
		for l := range acc {
			labels = append(labels, l)
		}
		sort.Slice(labels, func(i, j int) bool {
			ri, rj := subWindowRank(labels[i], ""), subWindowRank(labels[j], "")
			if ri != rj {
				return ri < rj
			}
			return labels[i] < labels[j]
		})

		wins := make([]subPoolWindow, 0, len(labels)+1)
		for _, l := range labels {
			p := acc[l]
			avgUsed := 0.0
			if p.weight > 0 {
				avgUsed = clampPct(p.weightedUsed / p.weight)
			}
			var rst *time.Time
			if t, ok := earliestReset[l]; ok {
				tt := t
				rst = &tt
			}
			wins = append(wins, subPoolWindow{
				Label:       l,
				AvgUsed:     avgUsed,
				Remaining:   math.Max(0, 100-avgUsed),
				NextResetAt: rst,
			})
		}
		if availCount > 0 && sawExpired5H && acc["5H"] == nil {
			wins = append([]subPoolWindow{{Label: "5H", AvgUsed: 0, Remaining: 100, JustReset: true}}, wins...)
		}

		ps := subPoolSummary{
			Provider:        prov,
			Available:       availCount,
			Total:           len(members),
			Windows:         wins,
			CreditLeft:      creditLeft,
			CreditUnit:      creditUnit,
			NextAvailableAt: nextAvail,
		}
		if len(members) == 1 {
			ps.SingleTier = members[0].TierDisplay
			ps.SingleMachines = members[0].Machines
			ps.SingleStatus = members[0].Status
		}
		pools = append(pools, ps)
	}
	return pools
}

func pickSubPrimaryPool(pools []subPoolSummary, model, focusOverride string) *subPoolSummary {
	if len(pools) == 0 {
		return nil
	}
	if focusOverride != "" && focusOverride != "auto" && focusOverride != "all" {
		for i := range pools {
			if pools[i].Provider == focusOverride {
				return &pools[i]
			}
		}
	}
	for _, c := range subProvidersForModel(model) {
		for i := range pools {
			if pools[i].Provider == c {
				return &pools[i]
			}
		}
	}
	return &pools[0]
}

func subToneForUsed(usedPct float64) string {
	remaining := 100 - usedPct
	switch {
	case remaining < 12:
		return "danger"
	case remaining < 30:
		return "hot"
	case remaining < 55:
		return "warn"
	default:
		return "ok"
	}
}

func subPctWhole(pct float64) int {
	if pct >= 100 {
		return 100
	}
	r := int(math.Round(pct))
	if r >= 100 {
		return 99
	}
	if r < 0 {
		return 0
	}
	return r
}

func subFormatClock(at, now time.Time) string {
	locAt := at.In(now.Location())
	y1, m1, d1 := locAt.Date()
	y2, m2, d2 := now.Date()
	if y1 == y2 && m1 == m2 && d1 == d2 {
		return locAt.Format("15:04")
	}
	return locAt.Format("01-02 15:04")
}

func subFormatRelCompact(at, now time.Time) string {
	d := at.Sub(now)
	if d <= 0 {
		return "now"
	}
	if d < time.Minute {
		return "<1m"
	}
	mins := int(math.Round(d.Minutes()))
	if mins < 60 {
		return fmt.Sprintf("%dm", mins)
	}
	hours := mins / 60
	remMin := mins % 60
	if hours < 24 {
		if remMin == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, remMin)
	}
	days := hours / 24
	remHr := hours % 24
	if remHr == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, remHr)
}

func subFormatResetDetail(at, now time.Time) string {
	rel := subFormatRelCompact(at, now)
	clk := subFormatClock(at, now)
	if rel == "now" {
		return "即将重置（" + clk + "）"
	}
	return fmt.Sprintf("%s 后（%s）", rel, clk)
}

func subFormatCompactTokens(n int64) string {
	switch {
	case n >= 1_000_000_000_000:
		return fmt.Sprintf("%.2fT", float64(n)/1_000_000_000_000)
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", float64(n)/1_000_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.2fk", float64(n)/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}

func subFormatCommas(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	rem := len(s) % 3
	if rem > 0 {
		b.WriteString(s[:rem])
	}
	for i := rem; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func subEarliestResetText(ws []subPoolWindow, now time.Time) string {
	for _, w := range ws {
		if w.NextResetAt != nil {
			rel := subFormatRelCompact(*w.NextResetAt, now)
			if rel == "now" {
				return "即将重置"
			}
			return fmt.Sprintf("%s 后重置", rel)
		}
	}
	return ""
}

func subPtrFloat(v float64) *float64 { return &v }

func machineTodayStats(m subUsageMachineNode) tokenDayStats {
	st := m.Today
	if st.Tokens == 0 && m.Report.Today.Tokens > 0 {
		st = m.Report.Today
	}
	if st.Tokens == 0 && m.Report.TodayTokens > 0 {
		st.Tokens = m.Report.TodayTokens
	}
	return st
}

func buildSubUsageHeaderStatus(snap subUsageSnapshot, cfg subUsageConfig, model string, now time.Time) extproto.HeaderStatus {
	hs := extproto.HeaderStatus{
		Title:     "订阅用量",
		UpdatedAt: snap.CollectedAt.UTC().Format(time.RFC3339),
	}
	accounts := aggregateSubAccounts(snap.Machines, cfg.DedupeAccounts, now)

	var totalToday tokenDayStats
	for _, m := range snap.Machines {
		st := machineTodayStats(m)
		totalToday.Tokens += st.Tokens
		totalToday.Requests += st.Requests
		totalToday.CacheRead += st.CacheRead
		totalToday.CacheWrite += st.CacheWrite
		totalToday.CacheInput += st.CacheInput
	}

	if len(accounts) == 0 {
		hs.Compact = "用量 · 未检测到订阅"
		hs.Tone = "muted"
		hs.Items = []extproto.HeaderStatusItem{
			{Label: "订阅用量", Value: "未检测到登录", Tone: "muted"},
		}
		if len(snap.Machines) > 0 {
			hs.Subline = fmt.Sprintf("已扫描 %d 台在线电脑", len(snap.Machines))
			if cfg.ShowTokens && totalToday.Tokens > 0 {
				hs.Subline += " · 今日 " + subFormatCompactTokens(totalToday.Tokens)
				hs.Sections = append(hs.Sections, buildSubTodaySection(snap.Machines, totalToday))
			}
			machineSecs, otherNodes := buildSubMachineSections(snap.Machines)
			hs.Sections = append(hs.Sections, machineSecs...)
			if len(otherNodes) > 0 {
				hs.Sections = append(hs.Sections, buildSubOtherMachinesSection(otherNodes, len(machineSecs) > 0))
			}
		}
		return hs
	}

	focus := strings.ToLower(strings.TrimSpace(cfg.FocusOverride))
	isAllMode := focus == "all"
	poolModel := model
	if isAllMode {
		poolModel = ""
	}
	pools := summarizeSubPools(accounts, poolModel, now, false)
	pinnedProvider := ""
	if focus != "" && focus != "auto" && focus != "all" {
		pinnedProvider = focus
	}
	isAutoMode := !isAllMode && pinnedProvider == ""
	primary := pickSubPrimaryPool(pools, model, pinnedProvider)
	usedMode := cfg.PercentMode == "used"

	displayPct := func(avgUsed, remaining float64) float64 {
		if usedMode {
			return avgUsed
		}
		return remaining
	}
	pctWord := "剩"
	if usedMode {
		pctWord = "用"
	}

	var compactParts []string
	var subParts []string
	worstTone := ""
	bumpTone := func(t string) {
		rank := map[string]int{"": 0, "ok": 1, "info": 1, "warn": 2, "hot": 3, "danger": 4}
		if rank[t] > rank[worstTone] {
			worstTone = t
		}
	}

	switch {
	case isAllMode:
		for _, p := range pools {
			lbl := subProviderLabel(p.Provider, "")
			ico := subProviderIconKey(p.Provider)
			acc := subProviderAccentKey(p.Provider)
			if p.Available == 0 {
				val := subUnavailableShort(p.SingleStatus, p.Total)
				hs.Items = append(hs.Items, extproto.HeaderStatusItem{
					Label:   lbl,
					Value:   val,
					Icon:    ico,
					Accent:  acc,
					Tone:    "danger",
					Tooltip: fmt.Sprintf("%s %s", lbl, val),
				})
				compactParts = append(compactParts, fmt.Sprintf("%s %s", lbl, val))
				bumpTone("danger")
				continue
			}
			var tight *subPoolWindow
			for i := range p.Windows {
				if tight == nil || p.Windows[i].AvgUsed >= tight.AvgUsed {
					tight = &p.Windows[i]
				}
			}
			if tight == nil {
				val := "可用"
				if p.Total > 1 {
					val = fmt.Sprintf("%d/%d", p.Available, p.Total)
				}
				hs.Items = append(hs.Items, extproto.HeaderStatusItem{
					Label:  lbl,
					Value:  val,
					Icon:   ico,
					Accent: acc,
					Tone:   "muted",
				})
				compactParts = append(compactParts, lbl+" "+val)
				continue
			}
			dp := displayPct(tight.AvgUsed, tight.Remaining)
			tone := subToneForUsed(tight.AvgUsed)
			bumpTone(tone)
			hs.Items = append(hs.Items, extproto.HeaderStatusItem{
				Label:   fmt.Sprintf("%s %s", lbl, tight.Label),
				Value:   fmt.Sprintf("%d%%", subPctWhole(dp)),
				Icon:    ico,
				Accent:  acc,
				Percent: subPtrFloat(dp),
				Tone:    tone,
				Tooltip: fmt.Sprintf("%s · %s 剩余 %d%%（已用 %d%%）", lbl, tight.Label, subPctWhole(tight.Remaining), subPctWhole(tight.AvgUsed)),
			})
			compactParts = append(compactParts, fmt.Sprintf("%s %s%d%%", lbl, pctWord, subPctWhole(dp)))
		}
		if len(snap.Machines) > 0 {
			subParts = append(subParts, fmt.Sprintf("%d 台电脑 · %d 个订阅", len(snap.Machines), len(accounts)))
		}
	default:
		if primary != nil {
			lbl := subProviderLabel(primary.Provider, model)
			ico := subProviderIconKey(primary.Provider)
			acc := subProviderAccentKey(primary.Provider)
			availTone := "ok"
			headVal := ""
			if primary.Total > 1 {
				headVal = fmt.Sprintf("%d/%d", primary.Available, primary.Total)
				if primary.Available == 0 {
					availTone = "danger"
					bumpTone("danger")
				} else if primary.Available < primary.Total {
					availTone = "warn"
				}
			} else if primary.Available == 0 {
				headVal = subUnavailableShort(primary.SingleStatus, 1)
				availTone = "danger"
				bumpTone("danger")
			}
			tip := lbl
			if primary.SingleTier != "" {
				tip += " · " + primary.SingleTier
			}
			hs.Items = append(hs.Items, extproto.HeaderStatusItem{
				Label:   lbl,
				Value:   headVal,
				Icon:    ico,
				Accent:  acc,
				Tone:    availTone,
				Tooltip: tip,
			})
			if headVal != "" {
				compactParts = append(compactParts, lbl+" "+headVal)
			} else {
				compactParts = append(compactParts, lbl)
			}

			if primary.Available == 0 {
				if primary.NextAvailableAt != nil {
					msg := "预计 " + subFormatClock(*primary.NextAvailableAt, now) + " 恢复"
					hs.Items = append(hs.Items, extproto.HeaderStatusItem{Value: msg, Tone: "danger"})
					compactParts = append(compactParts, msg)
				}
			} else {
				for i, w := range primary.Windows {
					if i >= 3 {
						break
					}
					if w.JustReset {
						hs.Items = append(hs.Items, extproto.HeaderStatusItem{Label: w.Label, Value: "已重置", Tone: "ok"})
						compactParts = append(compactParts, w.Label+" 已重置")
						continue
					}
					dp := displayPct(w.AvgUsed, w.Remaining)
					tone := subToneForUsed(w.AvgUsed)
					bumpTone(tone)
					wtip := fmt.Sprintf("%s %s：剩余 %d%%（已用 %d%%）", lbl, w.Label, subPctWhole(w.Remaining), subPctWhole(w.AvgUsed))
					if w.NextResetAt != nil {
						wtip += " · 重置 " + subFormatClock(*w.NextResetAt, now)
					}
					hs.Items = append(hs.Items, extproto.HeaderStatusItem{
						Label:   w.Label,
						Value:   fmt.Sprintf("%d%%", subPctWhole(dp)),
						Percent: subPtrFloat(dp),
						Tone:    tone,
						Tooltip: wtip,
					})
					compactParts = append(compactParts, fmt.Sprintf("%s %d%%", w.Label, subPctWhole(dp)))
				}
				if rst := subEarliestResetText(primary.Windows, now); rst != "" {
					subParts = append(subParts, rst)
				}
			}
		}
	}

	if cfg.ShowTokens && totalToday.Tokens > 0 {
		todayTok := "今日 " + subFormatCompactTokens(totalToday.Tokens)
		subParts = append(subParts, todayTok)
		compactParts = append(compactParts, todayTok)
		if totalToday.CacheInput > 0 {
			hit := clampPct(float64(totalToday.CacheRead) * 100 / float64(totalToday.CacheInput))
			subParts = append(subParts, fmt.Sprintf("缓存 %.1f%%", hit))
		}
	}
	hs.Subline = strings.Join(subParts, " · ")
	hs.Compact = strings.Join(compactParts, " · ")
	if worstTone == "danger" || worstTone == "hot" {
		hs.Tone = worstTone
	}

	// Drawer sections.
	detailPools := summarizeSubPools(accounts, model, now, true)
	detailMeter := func(label string, avgUsed, remaining float64, resetAt *time.Time) extproto.HeaderStatusItem {
		dp := displayPct(avgUsed, remaining)
		remInt := subPctWhole(remaining)
		usedInt := 100 - remInt
		if usedMode {
			usedInt = subPctWhole(avgUsed)
			remInt = 100 - usedInt
		}
		val := fmt.Sprintf("剩 %d%%", remInt)
		if usedMode {
			val = fmt.Sprintf("用 %d%%", usedInt)
		}
		sub := ""
		if resetAt != nil {
			sub = subFormatResetDetail(*resetAt, now)
		}
		return extproto.HeaderStatusItem{
			Label:   label,
			Value:   val,
			Sub:     sub,
			Percent: subPtrFloat(dp),
			Tone:    subToneForUsed(avgUsed),
		}
	}

	// 1. Top interactive subscription summary cards (NO machine names here).
	if len(detailPools) > 0 {
		sec := extproto.HeaderStatusSection{
			Title: "订阅额度",
			Meta:  "点击卡片切换顶栏显示",
			Chips: []extproto.HeaderStatusChip{
				{
					Label:    "跟随模型",
					Selected: isAutoMode,
					Action: &extproto.HeaderStatusAction{
						Name:  "set_header_focus",
						Args:  map[string]any{"focus": "auto"},
						Label: "顶栏自动跟随当前会话模型",
					},
				},
				{
					Label:    "全部",
					Selected: isAllMode,
					Action: &extproto.HeaderStatusAction{
						Name:  "set_header_focus",
						Args:  map[string]any{"focus": "all"},
						Label: "顶栏同时展示全部订阅",
					},
				},
			},
		}
		for _, p := range detailPools {
			lbl := subProviderLabel(p.Provider, model)
			isPinned := pinnedProvider != "" && p.Provider == pinnedProvider
			isCurrent := primary != nil && p.Provider == primary.Provider && !isAllMode
			nextFocus := p.Provider
			actLabel := fmt.Sprintf("点击在顶栏固定显示 %s", lbl)
			if isPinned {
				nextFocus = "auto"
				actLabel = fmt.Sprintf("点击取消固定 %s，恢复跟随当前会话模型", lbl)
			}

			var badges []extproto.HeaderStatusBadge
			if p.Total == 1 {
				if p.SingleTier != "" {
					badges = append(badges, extproto.HeaderStatusBadge{Text: p.SingleTier, Tone: subTierBadgeTone(p.SingleTier)})
				}
				if p.Available == 1 {
					badges = append(badges, extproto.HeaderStatusBadge{Text: "可用", Tone: "ok"})
				} else {
					badges = append(badges, extproto.HeaderStatusBadge{Text: subUnavailableShort(p.SingleStatus, 1), Tone: "danger"})
				}
			} else {
				availTone := "ok"
				if p.Available == 0 {
					availTone = "danger"
				} else if p.Available < p.Total {
					availTone = "warn"
				}
				badges = append(badges, extproto.HeaderStatusBadge{Text: fmt.Sprintf("%d/%d 可用", p.Available, p.Total), Tone: availTone})
			}

			row := extproto.HeaderStatusRow{
				Label:    lbl,
				Icon:     subProviderIconKey(p.Provider),
				Accent:   subProviderAccentKey(p.Provider),
				Badges:   badges,
				Current:  isCurrent,
				Selected: isPinned,
				Action: &extproto.HeaderStatusAction{
					Name:  "set_header_focus",
					Args:  map[string]any{"focus": nextFocus},
					Label: actLabel,
				},
			}
			creditNum := ""
			creditText := ""
			if p.CreditLeft > 0 {
				creditNum = subFormatCommas(int64(math.Round(p.CreditLeft)))
				creditText = "Credits " + creditNum
			}
			if p.Available == 0 {
				row.Value = subUnavailableShort(p.SingleStatus, p.Total)
				row.Tone = "danger"
				var dets []string
				if p.NextAvailableAt != nil {
					dets = append(dets, "预计 "+subFormatResetDetail(*p.NextAvailableAt, now)+" 恢复")
				} else if p.SingleStatus == "expired" {
					dets = append(dets, "等待本机 Agent 刷新")
				}
				if creditText != "" {
					dets = append(dets, "剩余 "+creditText)
				}
				row.Detail = strings.Join(dets, " · ")
			} else {
				if creditText != "" {
					row.Metric = &extproto.HeaderStatusItem{
						Label: "Credits",
						Value: creditNum,
						Tone:  "muted",
					}
					row.Value = creditText
					row.Tone = "muted"
				}
				for _, w := range p.Windows {
					if w.JustReset {
						row.Meters = append(row.Meters, extproto.HeaderStatusItem{Label: w.Label, Value: "已重置", Sub: "等待刷新", Tone: "ok"})
						continue
					}
					row.Meters = append(row.Meters, detailMeter(w.Label, w.AvgUsed, w.Remaining, w.NextResetAt))
				}
			}
			sec.Rows = append(sec.Rows, row)
		}
		hs.Sections = append(hs.Sections, sec)
	}

	// 2. Today's token & cache stats (total + per-machine cards).
	if cfg.ShowTokens && totalToday.Tokens > 0 {
		hs.Sections = append(hs.Sections, buildSubTodaySection(snap.Machines, totalToday))
	}

	// 3. Per-machine today token breakdown by model vendor (grouped by machine).
	machineSecs, otherNodes := buildSubMachineSections(snap.Machines)
	hs.Sections = append(hs.Sections, machineSecs...)

	// 4. Online machines with zero today usage or legacy/error state (only when non-empty).
	if len(otherNodes) > 0 {
		hs.Sections = append(hs.Sections, buildSubOtherMachinesSection(otherNodes, true))
	}

	return hs
}

func buildSubTokenRow(label string, st tokenDayStats, valTone string) extproto.HeaderStatusRow {
	var badges []extproto.HeaderStatusBadge
	var detailParts []string
	if st.Requests > 0 {
		detailParts = append(detailParts, fmt.Sprintf("%s 次请求", subFormatCommas(st.Requests)))
	}
	if st.CacheInput > 0 {
		hit := clampPct(float64(st.CacheRead) * 100 / float64(st.CacheInput))
		hitTone := "ok"
		if hit < 50 {
			hitTone = "warn"
		}
		badges = append(badges, extproto.HeaderStatusBadge{Text: fmt.Sprintf("缓存 %.1f%%", hit), Tone: hitTone})
		if st.CacheRead > 0 && st.CacheWrite > 0 {
			detailParts = append(detailParts, fmt.Sprintf("读 %s / 写 %s", subFormatCompactTokens(st.CacheRead), subFormatCompactTokens(st.CacheWrite)))
		} else if st.CacheRead > 0 {
			detailParts = append(detailParts, fmt.Sprintf("读 %s", subFormatCompactTokens(st.CacheRead)))
		} else if st.CacheWrite > 0 {
			detailParts = append(detailParts, fmt.Sprintf("写 %s", subFormatCompactTokens(st.CacheWrite)))
		}
	}
	return extproto.HeaderStatusRow{
		Label:  label,
		Badges: badges,
		Value:  subFormatCompactTokens(st.Tokens) + " tok",
		Tone:   valTone,
		Detail: strings.Join(detailParts, " · "),
	}
}

func buildSubTodaySection(nodes []subUsageMachineNode, total tokenDayStats) extproto.HeaderStatusSection {
	type mTok struct {
		name string
		st   tokenDayStats
	}
	var perMachine []mTok
	for _, n := range nodes {
		st := machineTodayStats(n)
		if st.Tokens > 0 {
			perMachine = append(perMachine, mTok{name: subMachineDisplayName(n), st: st})
		}
	}
	sort.SliceStable(perMachine, func(i, j int) bool {
		if perMachine[i].st.Tokens != perMachine[j].st.Tokens {
			return perMachine[i].st.Tokens > perMachine[j].st.Tokens
		}
		return strings.ToLower(perMachine[i].name) < strings.ToLower(perMachine[j].name)
	})

	sec := extproto.HeaderStatusSection{
		Title: "今日用量",
		Meta:  fmt.Sprintf("%d 台在线电脑", len(nodes)),
	}
	sec.Rows = append(sec.Rows, buildSubTokenRow("今日", total, "info"))
	if len(perMachine) > 1 {
		for _, pm := range perMachine {
			sec.Rows = append(sec.Rows, buildSubTokenRow(pm.name, pm.st, ""))
		}
	}
	return sec
}

func buildSubMachineSections(nodes []subUsageMachineNode) (sections []extproto.HeaderStatusSection, others []subUsageMachineNode) {
	for _, n := range nodes {
		if n.Error != "" || n.Unsupported {
			others = append(others, n)
			continue
		}
		st := machineTodayStats(n)
		subsByProv := map[string]subUsageAccount{}
		for _, a := range n.Report.Accounts {
			p := strings.ToLower(strings.TrimSpace(a.Provider))
			if p == "" {
				continue
			}
			if _, exists := subsByProv[p]; !exists {
				subsByProv[p] = a
			}
		}

		var rows []extproto.HeaderStatusRow
		if len(st.ByProvider) > 0 {
			provs := make([]string, 0, len(st.ByProvider))
			for p, pst := range st.ByProvider {
				if pst.Tokens > 0 {
					provs = append(provs, strings.ToLower(strings.TrimSpace(p)))
				}
			}
			sort.Slice(provs, func(i, j int) bool {
				oi, oj := subProviderOrder(provs[i]), subProviderOrder(provs[j])
				if oi != oj {
					return oi < oj
				}
				return provs[i] < provs[j]
			})
			for _, prov := range provs {
				pst := st.ByProvider[prov]
				if pst.Tokens <= 0 {
					continue
				}
				sub, hasSub := subsByProv[prov]
				rows = append(rows, buildSubMachineVendorTokenRow(prov, pst, sub, hasSub))
			}
		} else if st.Tokens > 0 {
			// Fallback for older nodes that report aggregate today tokens without by_provider.
			rows = append(rows, buildSubTokenRow("今日用量", st, ""))
		}

		if len(rows) == 0 {
			others = append(others, n)
			continue
		}

		var metaParts []string
		if len(n.Report.Accounts) > 0 {
			metaParts = append(metaParts, fmt.Sprintf("%d 个订阅", len(n.Report.Accounts)))
		}
		if st.Tokens > 0 {
			metaParts = append(metaParts, "今日 "+subFormatCompactTokens(st.Tokens))
		}
		sections = append(sections, extproto.HeaderStatusSection{
			Title:       subMachineDisplayName(n),
			Meta:        strings.Join(metaParts, " · "),
			Icon:        "machine",
			Collapsible: true,
			Rows:        rows,
		})
	}
	return sections, others
}

func buildSubMachineVendorTokenRow(prov string, st tokenDayStats, sub subUsageAccount, hasSub bool) extproto.HeaderStatusRow {
	row := buildSubTokenRow(subProviderLabel(prov, ""), st, "")
	row.Icon = subProviderIconKey(prov)
	row.Accent = subProviderAccentKey(prov)
	if hasSub {
		if e := strings.TrimSpace(sub.Email); e != "" {
			row.Sublabel = e
		}
		if tierDisp := formatSubPlanDisplay(prov, sub.Plan, sub.PlanRaw, sub.PlanDisplay); tierDisp != "" {
			tierBadge := extproto.HeaderStatusBadge{Text: tierDisp, Tone: subTierBadgeTone(tierDisp)}
			row.Badges = append([]extproto.HeaderStatusBadge{tierBadge}, row.Badges...)
		}
	}
	return row
}

func buildSubOtherMachinesSection(nodes []subUsageMachineNode, collapsed bool) extproto.HeaderStatusSection {
	sec := extproto.HeaderStatusSection{
		Title:       "其他在线电脑",
		Meta:        fmt.Sprintf("%d 台今日无用量", len(nodes)),
		Icon:        "machine",
		Collapsible: true,
		Collapsed:   collapsed,
	}
	for _, n := range nodes {
		row := extproto.HeaderStatusRow{Label: subMachineDisplayName(n)}
		if n.Local {
			row.Badges = append(row.Badges, extproto.HeaderStatusBadge{Text: "hub", Tone: "muted"})
		}
		for _, a := range n.Report.Accounts {
			prov := strings.ToLower(strings.TrimSpace(a.Provider))
			lbl := subProviderLabel(prov, "")
			if tierDisp := formatSubPlanDisplay(prov, a.Plan, a.PlanRaw, a.PlanDisplay); tierDisp != "" {
				lbl += " " + tierDisp
			}
			row.Badges = append(row.Badges, extproto.HeaderStatusBadge{Text: lbl, Tone: "muted"})
		}
		switch {
		case n.Unsupported:
			row.Value = "需升级节点"
			row.Tone = "muted"
			row.Detail = n.Error
		case n.Error != "":
			row.Value = "采集失败"
			row.Tone = "warn"
			row.Detail = n.Error
		default:
			row.Value = "今日无用量"
			row.Tone = "muted"
		}
		sec.Rows = append(sec.Rows, row)
	}
	return sec
}

func subUnavailableShort(status string, total int) string {
	if total > 1 {
		return fmt.Sprintf("0/%d", total)
	}
	switch status {
	case "expired":
		return "等待刷新"
	case "limited":
		return "已限流"
	case "error":
		return "读取失败"
	default:
		return "暂不可用"
	}
}

func subTierBadgeTone(tier string) string {
	lt := strings.ToLower(tier)
	switch {
	case strings.Contains(lt, "20x") || strings.Contains(lt, "ultra"):
		return "info"
	case strings.Contains(lt, "free"):
		return "muted"
	default:
		return ""
	}
}

func formatSubUsageText(snap subUsageSnapshot, cfg subUsageConfig, model string, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "订阅用量（%s，共 %d 台在线电脑）\n", snap.CollectedAt.In(now.Location()).Format("2006-01-02 15:04:05"), len(snap.Machines))
	accounts := aggregateSubAccounts(snap.Machines, cfg.DedupeAccounts, now)
	if len(accounts) == 0 {
		b.WriteString("- 暂未检测到已登录的本地订阅\n")
		return strings.TrimSpace(b.String())
	}
	pools := summarizeSubPools(accounts, model, now, true)
	for _, p := range pools {
		lbl := subProviderLabel(p.Provider, model)
		var parts []string
		for _, w := range p.Windows {
			if w.JustReset {
				parts = append(parts, w.Label+" 已重置")
				continue
			}
			seg := fmt.Sprintf("%s 剩 %d%%（已用 %d%%）", w.Label, subPctWhole(w.Remaining), subPctWhole(w.AvgUsed))
			if w.NextResetAt != nil {
				seg += " 重置 " + subFormatClock(*w.NextResetAt, now)
			}
			parts = append(parts, seg)
		}
		if p.CreditLeft > 0 {
			parts = append(parts, "Credits "+subFormatCommas(int64(math.Round(p.CreditLeft))))
		}
		if len(parts) == 0 {
			parts = append(parts, subUnavailableShort(p.SingleStatus, p.Total))
		}
		fmt.Fprintf(&b, "- %s (%d/%d)：%s\n", lbl, p.Available, p.Total, strings.Join(parts, " · "))
	}
	var totalTokens int64
	for _, m := range snap.Machines {
		totalTokens += machineTodayStats(m).Tokens
	}
	if totalTokens > 0 {
		fmt.Fprintf(&b, "- 今日合计：%s tokens\n", subFormatCompactTokens(totalTokens))
	}
	return strings.TrimSpace(b.String())
}
