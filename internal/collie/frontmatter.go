package collie

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// frontmatterMax bounds how much of a file is read for its header: skills
// and prompts keep their metadata at the top, and the body can be long.
const frontmatterMax = 16 << 10

// readFrontmatter reads the YAML header ("---" … "---") at the top of a
// Markdown file as flat string fields. It covers what skill and prompt files
// use: plain and quoted scalars, `|` and `>` blocks, and nested maps
// flattened as "parent.child". Lists and anything fancier are skipped.
func readFrontmatter(path string) (map[string]string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, frontmatterMax))
	if err != nil {
		return nil, "", err
	}
	fm, body := parseFrontmatter(string(b))
	return fm, body, nil
}

// parseFrontmatter splits text into its header fields and the body after it.
func parseFrontmatter(text string) (map[string]string, string) {
	text = strings.TrimPrefix(text, "\ufeff")
	fm := map[string]string{}
	if !strings.HasPrefix(text, "---") {
		return fm, text
	}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 4096), frontmatterMax)
	sc.Scan() // the opening ---
	consumed := len(sc.Text()) + 1
	var lines []string
	closed := false
	for sc.Scan() {
		l := sc.Text()
		consumed += len(l) + 1
		if strings.TrimRight(l, " \t\r") == "---" {
			closed = true
			break
		}
		lines = append(lines, strings.TrimRight(l, "\r"))
	}
	if !closed {
		return fm, text
	}
	body := ""
	if consumed < len(text) {
		body = text[consumed:]
	}
	parent := ""
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		indent := len(l) - len(strings.TrimLeft(l, " "))
		key, val, ok := strings.Cut(strings.TrimSpace(l), ":")
		if !ok || strings.HasPrefix(key, "-") {
			continue
		}
		key = strings.Trim(strings.TrimSpace(key), `"'`)
		if indent == 0 {
			parent = ""
		} else if parent == "" {
			continue // a list item's fields, or a stray line
		}
		name := key
		if indent > 0 {
			name = parent + "." + key
		}
		val = strings.TrimSpace(val)
		switch {
		case val == "":
			if indent == 0 {
				parent = key
			}
		case val[0] == '|' || val[0] == '>':
			var block []string
			for i+1 < len(lines) {
				next := lines[i+1]
				if strings.TrimSpace(next) != "" && len(next)-len(strings.TrimLeft(next, " ")) <= indent {
					break
				}
				block = append(block, strings.TrimSpace(next))
				i++
			}
			sep := "\n"
			if val[0] == '>' {
				sep = " "
			}
			fm[name] = strings.TrimSpace(strings.Join(block, sep))
		default:
			// A plain scalar may go on over more-indented lines.
			for i+1 < len(lines) && val[0] != '"' && val[0] != '\'' {
				next := lines[i+1]
				if strings.TrimSpace(next) == "" || len(next)-len(strings.TrimLeft(next, " ")) <= indent {
					break
				}
				val += " " + strings.TrimSpace(next)
				i++
			}
			fm[name] = unquoteYAML(val)
		}
	}
	return fm, body
}

func unquoteYAML(v string) string {
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"') {
		r := strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, " ", `\t`, " ")
		return r.Replace(v[1 : len(v)-1])
	}
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

// menuLine is a description as one short line: the first non-empty line, or
// the first heading or sentence of a body, cut to fit a menu row.
func menuLine(s string, max int) string {
	s = strings.TrimSpace(s)
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "#>"))
		if l == "" {
			continue
		}
		s = l
		break
	}
	if r := []rune(s); len(r) > max {
		s = strings.TrimSpace(string(r[:max-1])) + "…"
	}
	return s
}
