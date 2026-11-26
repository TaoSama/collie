package collie

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// herdr's pane.read doesn't say where the terminal cursor is, so the screen
// frame locates it from what the pane shows, best first:
//
//  1. in Claude Code, a single inverse-video character in the visible rows:
//     the caret it paints itself (it hides the real cursor);
//  2. the last prompt line (`❯ `, `› `, `> `), at the end of what's typed, or
//     before a dimmed placeholder (Codex's empty composer);
//  3. the end of the last non-blank row (a shell prompt).
//
// The spot is marked in the ANSI text with DECSC (ESC 7), so the browser's
// terminal works out the column with its own character widths and only has to
// restore it (ESC 8) and show its cursor. A painted caret loses its inverse
// there, or the blinking cursor would blink against an identical block.
const saveCursor = "\x1b7"

// defaultPaneRows is the height assumed until herdr reports the pane's.
const defaultPaneRows = 40

var (
	promptLineRe = regexp.MustCompile(`^\s*(?:[│┃|]\s*)?[❯›>][\s\x{a0}]`)
	menuLineRe   = regexp.MustCompile(`^\s*(?:[│┃|]\s*)?[❯›>][\s\x{a0}]*\d[.)]\s`)
	promptEndRe  = regexp.MustCompile(`[$%#>❯›]$`)
)

// screenRune is one printed character of an ANSI line: its bytes and the
// attributes it was drawn with.
type screenRune struct {
	start, end    int
	inverse, dim  bool
	space, border bool
}

// markScreenCursor returns text with the cursor marked, and whether one was
// found. rows is the pane height: the cursor is in the last rows lines. kind
// is the pane's agent ("" for a shell). Other than the mark, an agent's
// picture stays as it drew it: only Claude Code's painted caret gives up its
// inverse, to the cursor drawn in its place.
func markScreenCursor(text string, rows int, kind string) (string, bool) {
	if rows <= 0 {
		rows = defaultPaneRows
	}
	// Only the visible rows are split off and parsed.
	off := len(text)
	for n := 0; n < rows && off >= 0; n++ {
		off = strings.LastIndexByte(text[:off], '\n')
	}
	head := ""
	if off >= 0 {
		head, text = text[:off+1], text[off+1:]
	}
	lines := strings.Split(text, "\n")
	parsed := make([][]screenRune, len(lines))
	for i := range lines {
		parsed[i] = parseScreenLine(lines[i])
	}
	mark := func(i, at int, unInvert *screenRune) (string, bool) {
		l := lines[i]
		if unInvert != nil {
			rest := l[unInvert.end:]
			if strings.TrimRight(rest, "\r") != "" {
				rest = "\x1b[7m" + rest
			}
			l = l[:at] + saveCursor + "\x1b[27m" + l[at:unInvert.end] + rest
		} else {
			l = l[:at] + saveCursor + l[at:]
		}
		lines[i] = l
		return head + strings.Join(lines, "\n"), true
	}
	// 1. A painted caret. Anywhere else a lone inverse cell is the
	// program's own (a highlighted key, a selected choice).
	for i := len(lines) - 1; i >= 0 && kind == "claude"; i-- {
		rs := parsed[i]
		for j := len(rs) - 1; j >= 0; j-- {
			if rs[j].inverse && (j == 0 || !rs[j-1].inverse) && (j == len(rs)-1 || !rs[j+1].inverse) {
				return mark(i, rs[j].start, &rs[j])
			}
		}
	}
	// 2. A prompt line, not a menu's selected choice.
	for i := len(lines) - 1; i >= 0; i-- {
		plain := plainScreenLine(lines[i], parsed[i])
		if !promptLineRe.MatchString(plain) || menuLineRe.MatchString(plain) {
			continue
		}
		rs := parsed[i]
		k := 0
		for k < len(rs) && (rs[k].space || rs[k].border) {
			k++
		}
		prompt := k
		for k++; k < len(rs) && rs[k].space; k++ {
		}
		if k < len(rs) && rs[k].dim {
			return mark(i, rs[k].start, nil)
		}
		if at, ok := endOfTyped(rs, k); ok {
			return mark(i, at, nil)
		}
		// Nothing typed yet: one space after the mark.
		if prompt+1 < len(rs) && rs[prompt+1].space {
			prompt++
		}
		return mark(i, rs[prompt].end, nil)
	}
	// 3. The end of the last row with anything on it.
	for i := len(lines) - 1; i >= 0; i-- {
		rs := parsed[i]
		if at, ok := endOfTyped(rs, 0); ok {
			l := lines[i]
			// A shell prompt reads "… % "; the snapshot may have dropped
			// the space the cursor sits after.
			if kind == "" && promptEndRe.MatchString(plainScreenLine(l, rs)) && (len(rs) == 0 || !rs[len(rs)-1].space) {
				lines[i] = l[:at] + " " + l[at:]
				at++
			}
			return mark(i, at, nil)
		}
	}
	return head + text, false
}

// endOfTyped is the byte just past the last non-space character at or after
// rune k, ignoring a closing box border.
func endOfTyped(rs []screenRune, k int) (int, bool) {
	n := len(rs)
	for n > k && rs[n-1].space {
		n--
	}
	if n > k && rs[n-1].border {
		n--
		for n > k && rs[n-1].space {
			n--
		}
	}
	if n <= k {
		return 0, false
	}
	return rs[n-1].end, true
}

func plainScreenLine(line string, rs []screenRune) string {
	var b strings.Builder
	for _, r := range rs {
		b.WriteString(line[r.start:r.end])
	}
	return b.String()
}

// parseScreenLine walks one row of herdr's ANSI snapshot: SGR codes set the
// attributes, other escape sequences print nothing.
func parseScreenLine(line string) []screenRune {
	var out []screenRune
	inverse, dim := false, false
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case c == 0x1b && i+1 < len(line) && line[i+1] == '[':
			j := i + 2
			for j < len(line) && (line[j] < 0x40 || line[j] > 0x7e) {
				j++
			}
			if j < len(line) && line[j] == 'm' {
				inverse, dim = applySGR(line[i+2:j], inverse, dim)
			}
			i = j + 1
		case c == 0x1b && i+1 < len(line) && line[i+1] == ']':
			j := i + 2
			for j < len(line) && line[j] != 0x07 && !(line[j] == 0x1b && j+1 < len(line) && line[j+1] == '\\') {
				j++
			}
			if j < len(line) && line[j] == 0x1b {
				j++
			}
			i = j + 1
		case c == 0x1b:
			i += 2
		case c < 0x20 || c == 0x7f:
			i++
		default:
			r, size := utf8.DecodeRuneInString(line[i:])
			out = append(out, screenRune{
				start: i, end: i + size, inverse: inverse, dim: dim,
				space: r == ' ' || r == '\u00a0', border: r == '│' || r == '┃' || r == '|',
			})
			i += size
		}
	}
	return out
}

// applySGR follows the two attributes the cursor search reads. Colour
// arguments (38;5;n, 38;2;r;g;b) are skipped so their numbers aren't taken
// for attributes.
func applySGR(params string, inverse, dim bool) (bool, bool) {
	ps := strings.Split(params, ";")
	for k := 0; k < len(ps); k++ {
		p := ps[k]
		if strings.Contains(p, ":") {
			continue
		}
		n := 0
		if p != "" {
			v, err := strconv.Atoi(p)
			if err != nil {
				continue
			}
			n = v
		}
		switch n {
		case 0:
			inverse, dim = false, false
		case 2:
			dim = true
		case 22:
			dim = false
		case 7:
			inverse = true
		case 27:
			inverse = false
		case 38, 48, 58:
			if k+1 < len(ps) {
				switch ps[k+1] {
				case "5":
					k += 2
				case "2":
					k += 4
				}
			}
		}
	}
	return inverse, dim
}
