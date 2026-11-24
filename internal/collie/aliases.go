package collie

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// Launcher aliases: the shortcuts people keep in their shell rc file to start
// an agent with its own endpoint, model and keys, such as
//
//	alias ccf='ANTHROPIC_BASE_URL=https://… claude --model fable[1m]'
//
// Each computer reads its own by running its interactive shell, so whatever
// the rc file sources counts too; Collie can then launch through the alias
// instead of a bare `claude`, which would miss the alias's environment.

// LauncherAlias is one alias that starts claude, codex or agy. Secret-looking
// values never leave the computer: Command and Args show *** in their place.
type LauncherAlias struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`             // herdr kind: claude | codex | agy
	Model   string   `json:"model,omitempty"`  // "" when the alias leaves it to the agent
	Effort  string   `json:"effort,omitempty"` // likewise
	BaseURL string   `json:"base_url,omitempty"`
	Env     []string `json:"env,omitempty"`  // names of the variables it sets
	Args    []string `json:"args,omitempty"` // what follows the command
	Command string   `json:"command"`
	// The shell to run it through when it isn't the login shell a terminal
	// opens (bash at login, aliases only in ~/.zshrc).
	Via string `json:"via,omitempty"`

	raw []string // Args before redaction: only for use on this computer
}

// aliasReport is what one computer has: its shell, the rc file and the aliases.
type aliasReport struct {
	Shell   string          `json:"shell"`
	RC      string          `json:"rc"`
	Aliases []LauncherAlias `json:"aliases"`
}

const (
	aliasReadTimeout = 8 * time.Second
	// The shell's own output (rc files greet, warn, print fortunes) is
	// ignored outside these markers; fields and records use ASCII separators.
	aliasBegin = "\x02collie-aliases\x02"
	aliasEnd   = "\x03collie-aliases\x03"
	aliasField = "\x1f"
	aliasRec   = "\x1e"
	hiddenVal  = "***"
)

// aliasNameRe: names Collie types into a shell as a command.
var aliasNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:+@-]{0,63}$`)

// zsh hands over each alias's raw value; Go splits it like bash's.
const zshAliasScript = `print -rn -- $'\x02collie-aliases\x02'
for k v in "${(@kv)aliases}"; do print -rn -- "$k"$'\x1f'"$v"$'\x1e'; done
print -rn -- $'\x03collie-aliases\x03'`

// bash 3.2 (macOS) has no BASH_ALIASES: read alias -p, which quotes for us.
const bashAliasScript = `printf '\002collie-aliases\002'; alias -p; printf '\003collie-aliases\003'`

// ---------------------------------------------------------------- the shell

// loginShell: the shell the account logs in with, which is what herdr's
// panes run. The account database comes first: a service's $SHELL may be
// another shell (systemd --user hands out /bin/bash on some machines).
func loginShell() string {
	if sh := accountShell(); sh != "" {
		return sh
	}
	return os.Getenv("SHELL")
}

func accountShell() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	if b, err := os.ReadFile("/etc/passwd"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Split(line, ":"); len(f) >= 7 && f[0] == u.Username {
				return f[6]
			}
		}
	}
	if runtime.GOOS == "darwin" { // accounts live in Directory Services, not /etc/passwd
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "dscl", ".", "-read", "/Users/"+u.Username, "UserShell").Output()
		if err == nil {
			if f := strings.Fields(string(out)); len(f) == 2 {
				return f[1]
			}
		}
	}
	return ""
}

// binInShell finds name as the user's terminal would: their login shell,
// interactive (PATH from rc files, fnm/nvm shims), real files only. `command
// -v` answers with the alias when one shadows the program (alias codex=…).
func binInShell(ctx context.Context, home, name string) string {
	sh := loginShell()
	if sh == "" {
		sh = "/bin/bash"
	}
	lookup := "type -P " + name
	if path.Base(sh) == "zsh" {
		lookup = "whence -p " + name
	}
	out, err := runShell(ctx, home, sh, lookup+"; printf '\\003collie-aliases\\003'")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(strings.TrimSuffix(l, aliasEnd)); filepath.IsAbs(l) && isFile(l) {
			return l
		}
	}
	return ""
}

// aliasShell picks the shell whose rc file holds the aliases: zsh when that
// is the login shell, or when bash has no ~/.bashrc but ~/.zshrc exists.
func aliasShell(home, login string) (name, bin, rc string) {
	base := path.Base(login)
	name = "bash"
	if base == "zsh" || (!isFile(filepath.Join(home, ".bashrc")) && isFile(filepath.Join(home, ".zshrc"))) {
		name = "zsh"
	}
	bin = login
	if base != name {
		if bin, _ = exec.LookPath(name); bin == "" {
			bin = "/bin/" + name
		}
	}
	return name, bin, "~/." + name + "rc"
}

// discoverAliases reads this computer's launcher aliases.
func discoverAliases(ctx context.Context, home string) (aliasReport, error) {
	login := loginShell()
	name, bin, rc := aliasShell(home, login)
	rep := aliasReport{Shell: name, RC: rc, Aliases: []LauncherAlias{}}
	script := bashAliasScript
	if name == "zsh" {
		script = zshAliasScript
	}
	out, err := runShell(ctx, home, bin, script)
	if err != nil {
		return rep, err
	}
	defs, err := aliasDefs(name, out)
	if err != nil {
		return rep, err
	}
	for _, d := range defs {
		if a, ok := parseLauncher(d[0], d[1]); ok {
			if bin != login {
				a.Via = bin
			}
			rep.Aliases = append(rep.Aliases, a)
		}
	}
	slices.SortFunc(rep.Aliases, func(a, b LauncherAlias) int { return strings.Compare(a.Name, b.Name) })
	return rep, nil
}

// runShell runs script in an interactive shell with no terminal: its own
// session, so job control can't stop it waiting for one, and killed whole
// (rc files start helpers) when it takes too long.
func runShell(ctx context.Context, home, bin, script string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, aliasReadTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-ic", script)
	cmd.Dir = home
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second // a helper left holding stdout
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if b := out.Bytes(); bytes.Contains(b, []byte(aliasEnd)) {
		return b, nil // a nonzero exit from the rc file doesn't matter
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s 读取 alias 超时（%s）", path.Base(bin), aliasReadTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("%s -ic: %w", path.Base(bin), err)
	}
	return nil, errors.New("shell 没有输出 alias 列表")
}

// aliasDefs takes (name, value) pairs out of the shell's output.
func aliasDefs(shell string, out []byte) ([][2]string, error) {
	s := string(out)
	i := strings.Index(s, aliasBegin)
	j := strings.LastIndex(s, aliasEnd)
	if i < 0 || j < i {
		return nil, errors.New("shell 输出里没有 alias 列表")
	}
	s = s[i+len(aliasBegin) : j]
	var defs [][2]string
	if shell == "zsh" {
		for _, rec := range strings.Split(s, aliasRec) {
			if k, v, ok := strings.Cut(rec, aliasField); ok && k != "" {
				defs = append(defs, [2]string{k, v})
			}
		}
		return defs, nil
	}
	words, err := shellSplit(s)
	if err != nil {
		return nil, err
	}
	for i := 0; i+1 < len(words); i++ {
		if words[i].op || words[i].text != "alias" || words[i+1].op {
			continue
		}
		w := words[i+1].text
		if w == "--" && i+2 < len(words) {
			i++
			w = words[i+1].text
		}
		if k, v, ok := strings.Cut(w, "="); ok && k != "" {
			defs = append(defs, [2]string{k, v})
			i++
		}
	}
	return defs, nil
}

// ---------------------------------------------------------------- words

// shWord is one word of a command line; op marks an unquoted control
// operator (; & && | || or a newline).
type shWord struct {
	text string
	op   bool
}

// shellSplit splits a command line the way a POSIX shell does, without
// expanding anything: $VAR stays as written.
func shellSplit(s string) ([]shWord, error) {
	var out []shWord
	var cur strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			out = append(out, shWord{text: cur.String()})
			cur.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			flush()
		case c == '\n' || c == ';' || c == '&' || c == '|':
			flush()
			op := string(c)
			if (c == '&' || c == '|') && i+1 < len(s) && s[i+1] == c {
				op += string(c)
				i++
			}
			out = append(out, shWord{text: op, op: true})
		case c == '#' && !inWord:
			for i < len(s) && s[i] != '\n' {
				i++
			}
			i--
		case c == '\\':
			inWord = true
			if i+1 < len(s) {
				i++
				if s[i] != '\n' {
					cur.WriteByte(s[i])
				}
			}
		case c == '\'':
			inWord = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, errors.New("unterminated '")
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case c == '$' && i+1 < len(s) && s[i+1] == '\'':
			inWord = true
			j, err := ansiQuoted(s, i+2, &cur)
			if err != nil {
				return nil, err
			}
			i = j
		case c == '"':
			inWord = true
			j := i + 1
			for ; j < len(s) && s[j] != '"'; j++ {
				if s[j] == '\\' && j+1 < len(s) && strings.IndexByte("$`\"\\\n", s[j+1]) >= 0 {
					j++
					if s[j] == '\n' {
						continue
					}
				}
				cur.WriteByte(s[j])
			}
			if j >= len(s) {
				return nil, errors.New(`unterminated "`)
			}
			i = j
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	flush()
	return out, nil
}

// ansiQuoted reads a $'…' string from s[i:] into cur and returns the index
// of its closing quote.
var ansiEscapes = map[byte]byte{'n': '\n', 't': '\t', 'r': '\r', 'e': 0x1b, 'a': 7, 'b': 8, '\\': '\\', '\'': '\'', '"': '"'}

func ansiQuoted(s string, i int, cur *strings.Builder) (int, error) {
	for ; i < len(s); i++ {
		switch {
		case s[i] == '\'':
			return i, nil
		case s[i] == '\\' && i+1 < len(s):
			i++
			if b, ok := ansiEscapes[s[i]]; ok {
				cur.WriteByte(b)
			} else {
				cur.WriteByte('\\')
				cur.WriteByte(s[i])
			}
		default:
			cur.WriteByte(s[i])
		}
	}
	return 0, errors.New("unterminated $'")
}

// shellQuote writes one word so any POSIX shell (zsh too) reads it back as is.
func shellQuote(w string) string {
	// zsh expands a leading = to a command's path.
	if w != "" && w[0] != '=' && strings.Trim(w, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-.,/:=@%+") == "" {
		return w
	}
	return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
}

// shellLine quotes words into one line to type at a prompt. A control
// character would act as a key there (^U clears the line, tab completes).
func shellLine(words []string) (string, error) {
	q := make([]string, len(words))
	for i, w := range words {
		if strings.ContainsFunc(w, unicode.IsControl) {
			return "", fmt.Errorf("argument %q has a control character", w)
		}
		q[i] = shellQuote(w)
	}
	return strings.Join(q, " "), nil
}
