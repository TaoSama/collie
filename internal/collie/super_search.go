package collie

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const (
	defaultSearchLimit = 30
	maxSearchLimit     = 50
	searchSnippetRunes = 140
	searchLeadRunes    = 36
)

var (
	searchCodeFenceRe   = regexp.MustCompile("(?s)```[^\\n`]*\\n?(.*?)(?:```|$)")
	searchLinkKeepURLRe = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\n]+)\)`)
	searchQuoteLeadRe   = regexp.MustCompile(`^\s*[#>*•]+\s+`)
	searchBulletRe      = regexp.MustCompile(`^\s*-\s+`)
)

func searchableEntry(e *Entry, thread string) bool {
	if e == nil || e.ID == "" || (e.Role != "user" && e.Role != "collie") {
		return false
	}
	if thread == mainThread {
		return e.Thread == "" || e.Thread == mainThread
	}
	return e.Thread == thread
}

func toLowerRunes(s string) []rune {
	rs := []rune(s)
	out := make([]rune, len(rs))
	for i, r := range rs {
		out[i] = unicode.ToLower(r)
	}
	return out
}

func runesIndexFold(hayLow, needleLow []rune) int {
	n, m := len(hayLow), len(needleLow)
	if m == 0 {
		return 0
	}
	if m > n {
		return -1
	}
	first := needleLow[0]
	limit := n - m
	for i := 0; i <= limit; i++ {
		if hayLow[i] != first {
			continue
		}
		ok := true
		for j := 1; j < m; j++ {
			if hayLow[i+j] != needleLow[j] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// cleanSearchText turns raw Markdown entry text into clean single-line plain text
// while preserving code block bodies, numbers, paths, and (when includeURLs is set)
// link target URLs and quick-reply options.
func cleanSearchText(e *Entry, includeURLs bool) string {
	t := e.Text
	if e.Role == "user" {
		if i := strings.Index(t, "\n\n[The human picked"); i >= 0 {
			t = t[:i]
		}
		if i := strings.LastIndex(t, "附件：\n"); i >= 0 {
			head := strings.TrimSpace(t[:i])
			att := oneLine(t[i+len("附件：\n"):])
			if att != "" {
				t = strings.TrimSpace(head + " 📎 " + att)
			} else {
				t = head
			}
		}
	}
	t = searchCodeFenceRe.ReplaceAllString(t, " $1 ")
	if !includeURLs {
		t = pendingImgRe.ReplaceAllString(t, " ")
		t = pendingLinkRe.ReplaceAllString(t, "$1")
	} else {
		t = searchLinkKeepURLRe.ReplaceAllString(t, "$1 ($2)")
	}
	var parts []string
	for _, l := range strings.Split(t, "\n") {
		l = searchQuoteLeadRe.ReplaceAllString(l, "")
		l = searchBulletRe.ReplaceAllString(l, "")
		l = strings.TrimSpace(mdMarks.Replace(l))
		if l != "" {
			parts = append(parts, l)
		}
	}
	if includeURLs && len(e.Options) > 0 {
		parts = append(parts, strings.Join(e.Options, " / "))
	}
	out := oneLine(strings.Join(parts, " "))
	if out == "" {
		out = oneLine(e.Text)
	}
	return out
}

func matchTermsSnippet(text string, terms [][]rune) (string, bool) {
	if text == "" {
		return "", false
	}
	orig := []rune(text)
	low := make([]rune, len(orig))
	for i, r := range orig {
		low[i] = unicode.ToLower(r)
	}
	firstIdx := -1
	for ti, tr := range terms {
		idx := runesIndexFold(low, tr)
		if idx < 0 {
			return "", false
		}
		if ti == 0 {
			firstIdx = idx
		}
	}
	if len(orig) <= searchSnippetRunes {
		return text, true
	}
	start := max(0, firstIdx-searchLeadRunes)
	end := start + searchSnippetRunes
	if end > len(orig) {
		end = len(orig)
		start = max(0, end-searchSnippetRunes)
	}
	if termEnd := firstIdx + len(terms[0]); termEnd > end && termEnd <= len(orig) {
		if len(terms[0]) >= searchSnippetRunes {
			start = firstIdx
			end = min(len(orig), firstIdx+searchSnippetRunes)
		} else {
			end = termEnd
			start = max(0, end-searchSnippetRunes)
		}
	}
	out := string(orig[start:end])
	if start > 0 {
		out = "…" + strings.TrimLeft(out, " ")
	}
	if end < len(orig) {
		out = strings.TrimRight(out, " ") + "…"
	}
	return out, true
}

// Search queries full conversation history across in-memory and SQLite entries
// for case-insensitive multi-term AND matches on user/collie messages.
func (sp *Super) Search(thread, query string, limit int) SearchResult {
	if thread = strings.TrimSpace(thread); thread == "" {
		thread = mainThread
	}
	if limit <= 0 {
		limit = defaultSearchLimit
	} else if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return SearchResult{Query: "", Results: []SearchHit{}}
	}
	fields := strings.Fields(q)
	terms := make([][]rune, 0, len(fields))
	seenTerms := map[string]bool{}
	for _, f := range fields {
		lr := toLowerRunes(f)
		key := string(lr)
		if key != "" && !seenTerms[key] {
			seenTerms[key] = true
			terms = append(terms, lr)
		}
	}
	if len(terms) == 0 {
		return SearchResult{Query: "", Results: []SearchHit{}}
	}

	if sp.db != nil {
		sp.flush()
	}
	sp.mu.Lock()
	mem := make([]*Entry, len(sp.st.Entries))
	for i, e := range sp.st.Entries {
		if e != nil {
			cp := *e
			mem[i] = &cp
		}
	}
	sp.mu.Unlock()

	seenID := make(map[string]bool, len(mem))
	var candidates []*Entry
	for i := len(mem) - 1; i >= 0; i-- {
		e := mem[i]
		if e == nil || e.ID == "" || seenID[e.ID] {
			continue
		}
		seenID[e.ID] = true
		if searchableEntry(e, thread) {
			candidates = append(candidates, e)
		}
	}
	if sp.db != nil {
		for _, e := range threadEntries(sp.db, thread) {
			if e == nil || e.ID == "" || seenID[e.ID] {
				continue
			}
			seenID[e.ID] = true
			if searchableEntry(e, thread) {
				candidates = append(candidates, e)
			}
		}
	}

	type hitPair struct {
		e       *Entry
		snippet string
	}
	var hits []hitPair
	for _, e := range candidates {
		clean := cleanSearchText(e, false)
		snip, ok := matchTermsSnippet(clean, terms)
		if !ok {
			full := cleanSearchText(e, true)
			if full != clean {
				snip, ok = matchTermsSnippet(full, terms)
			}
		}
		if ok {
			hits = append(hits, hitPair{e: e, snippet: snip})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		ai, aj := hits[i].e.At, hits[j].e.At
		if !ai.Equal(aj) {
			return ai.After(aj)
		}
		return idNum(hits[i].e.ID) > idNum(hits[j].e.ID)
	})
	total := len(hits)
	hasMore := total > limit
	if hasMore {
		hits = hits[:limit]
	}
	res := make([]SearchHit, len(hits))
	for i, h := range hits {
		res[i] = SearchHit{
			ID:      h.e.ID,
			Role:    h.e.Role,
			At:      h.e.At,
			Snippet: h.snippet,
		}
	}
	return SearchResult{
		Query:   q,
		Results: res,
		Total:   total,
		HasMore: hasMore,
	}
}
