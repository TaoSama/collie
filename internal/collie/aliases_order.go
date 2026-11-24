package collie

import (
	"cmp"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Aliases are listed in the order the human wrote them: the shell hands its
// alias table over in no order, so the rc files it reads are scanned for
// where each alias is defined, following the files they source.

// rcSourceMax bounds how deep sourced files are followed.
const rcSourceMax = 4

var (
	rcAliasRe  = regexp.MustCompile(`(?:^|[\s;&|(])alias\s+(.+)`)
	rcSourceRe = regexp.MustCompile(`^\s*(?:source|\.)\s+(\S+)\s*$`)
	rcHomeRe   = regexp.MustCompile(`^(?:\$HOME|\$\{HOME\})/`)
)

// rcFiles: the files an interactive, non-login shell reads, in its order.
func rcFiles(home, shell string) []string {
	if shell != "zsh" {
		return []string{filepath.Join(home, ".bashrc")}
	}
	dir := cmp.Or(os.Getenv("ZDOTDIR"), home)
	return []string{filepath.Join(dir, ".zshenv"), filepath.Join(dir, ".zshrc")}
}

// rcAliasOrder maps each alias name to where it is first defined across the
// shell's rc files, read in load order with sourced files read in place.
func rcAliasOrder(home, shell string) map[string]int {
	order := map[string]int{}
	seen := map[string]bool{}
	var read func(file string, depth int)
	read = func(file string, depth int) {
		if depth > rcSourceMax || seen[file] {
			return
		}
		seen[file] = true
		b, err := os.ReadFile(file)
		if err != nil {
			return // an rc file that isn't there, or can't be read, adds nothing
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if m := rcSourceRe.FindStringSubmatch(line); m != nil {
				read(expandIn(home, rcHomeRe.ReplaceAllString(strings.Trim(m[1], `"'`), "~/")), depth+1)
				continue
			}
			m := rcAliasRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			words, err := shellSplit(m[1])
			if err != nil {
				continue // a definition spanning lines: placed with the unknown ones
			}
			for _, w := range words {
				if w.op {
					break
				}
				if k, _, ok := strings.Cut(w.text, "="); ok && aliasNameRe.MatchString(k) {
					if _, dup := order[k]; !dup {
						order[k] = len(order)
					}
				}
			}
		}
	}
	for _, f := range rcFiles(home, shell) {
		read(f, 0)
	}
	return order
}

// sortAliasesByRC puts aliases in rc file order; ones not found there (made
// by a loop or a plugin) follow by name.
func sortAliasesByRC(list []LauncherAlias, order map[string]int) {
	slices.SortFunc(list, func(a, b LauncherAlias) int {
		ia, oka := order[a.Name]
		ib, okb := order[b.Name]
		switch {
		case oka && okb:
			return cmp.Compare(ia, ib)
		case oka != okb:
			if oka {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
}
