package collie

import (
	"cmp"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Reading what an alias launches: the agent, and the model, effort and
// endpoint it pins.

// Environment variables an alias may set that Collie shows as they are.
const (
	envAnthropicModel = "ANTHROPIC_MODEL"
	envClaudeEffort   = "CLAUDE_CODE_EFFORT_LEVEL"
	envAnthropicURL   = "ANTHROPIC_BASE_URL"
	envOpenAIURL      = "OPENAI_BASE_URL"
)

// codex config keys taken from -c key=value.
const (
	codexModelKey  = "model"
	codexEffortKey = "model_reasoning_effort"
)

// Commands that run the next word as the command.
var aliasWrappers = []string{"command", "exec", "noglob", "nocorrect", "builtin", "nohup"}

var (
	assignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	// Names whose values are credentials.
	secretNameRe = regexp.MustCompile(`(?i)(key|token|secret|passw|credential|cookie|auth|session)`)
	// Values that look like credentials whatever they are called.
	secretValRe = regexp.MustCompile(`^(sk-|sk_|ghp_|gho_|github_pat_|xox[abp]-|AKIA|AIza|eyJ)|^[A-Za-z0-9_+/=.-]{32,}$`)
	// Names whose values are settings, never credentials, however random they look.
	plainNameRe = regexp.MustCompile(`(?i)(model|url|window|effort|dir|path|pct)`)
	// An endpoint carrying a credential in its query.
	secretQueryRe = regexp.MustCompile(`[?&](key|token|secret|auth)[^=]*=`)
)

// parseLauncher reads one alias; ok is false unless it starts an agent.
func parseLauncher(name, value string) (LauncherAlias, bool) {
	words, err := shellSplit(value)
	if err != nil || !aliasNameRe.MatchString(name) {
		return LauncherAlias{}, false
	}
	env := map[string]string{}
	var envNames []string
	setEnv := func(w string) {
		k, v, _ := strings.Cut(w, "=")
		if _, seen := env[k]; !seen {
			envNames = append(envNames, k)
		}
		env[k] = v
	}
	for _, seg := range segments(words) {
		if seg[0] == "export" {
			for _, w := range seg[1:] {
				if assignRe.MatchString(w) {
					setEnv(w)
				}
			}
			continue
		}
		i := commandAt(seg, setEnv)
		if i < 0 || !slices.Contains(launcherCmds, path.Base(seg[i])) {
			continue
		}
		kind := path.Base(seg[i])
		a := LauncherAlias{Name: name, Kind: kind, Env: envNames}
		args := seg[i+1:]
		a.Model, a.Effort = launchModel(kind, args)
		if kind == "claude" {
			a.Model = cmp.Or(a.Model, env[envAnthropicModel])
			a.Effort = cmp.Or(a.Effort, env[envClaudeEffort])
		}
		a.BaseURL = cmp.Or(env[envAnthropicURL], env[envOpenAIURL])
		if secretVal(a.BaseURL) {
			a.BaseURL = hiddenVal
		}
		if !modelNameRe.MatchString(a.Model) {
			a.Model = "" // "$MODEL" and the like: only the shell knows
		}
		if !modelNameRe.MatchString(a.Effort) {
			a.Effort = ""
		}
		a.Args = redactArgs(args)
		a.Command = redactCommand(words)
		return a, true
	}
	return LauncherAlias{}, false
}

// commandAt: where seg's command is, past assignments and wrappers; -1 if none.
func commandAt(seg []string, setEnv func(string)) int {
	for i := 0; i < len(seg); {
		w := seg[i]
		switch {
		case assignRe.MatchString(w):
			setEnv(w)
			i++
		case w == "env":
			i = skipEnvOpts(seg, i+1, setEnv)
		case slices.Contains(aliasWrappers, w):
			i++
			for i < len(seg) && strings.HasPrefix(seg[i], "-") { // command -p, exec -c
				i++
			}
		default:
			return i
		}
	}
	return -1
}

// segments splits words at control operators, dropping empty commands.
func segments(words []shWord) [][]string {
	var out [][]string
	var cur []string
	for _, w := range words {
		if w.op {
			if len(cur) > 0 {
				out = append(out, cur)
			}
			cur = nil
			continue
		}
		cur = append(cur, w.text)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// skipEnvOpts steps over env's options and assignments from seg[i].
func skipEnvOpts(seg []string, i int, setEnv func(string)) int {
	for i < len(seg) {
		w := seg[i]
		switch {
		case w == "--":
			return i + 1
		case w == "-u" || w == "--unset" || w == "-C" || w == "--chdir":
			i += 2
		case strings.HasPrefix(w, "-"): // -i, --ignore-environment, -u=NAME…
			i++
		case assignRe.MatchString(w):
			setEnv(w)
			i++
		default:
			return i
		}
	}
	return i
}

// launchModel reads the model and effort a launcher's flags pin.
func launchModel(kind string, args []string) (model, effort string) {
	for i := 0; i < len(args); i++ {
		flag, val, inline := strings.Cut(args[i], "=")
		next := func() string {
			if inline {
				return val
			}
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case flag == "--model" || (kind == "codex" && flag == "-m"):
			model = next()
		case kind == "codex" && strings.HasPrefix(args[i], "-m") && len(args[i]) > 2 && !inline:
			model = args[i][2:] // -mgpt-5
		case kind != "codex" && flag == "--effort":
			effort = next()
		case kind == "codex" && (flag == "-c" || flag == "--config"):
			k, v, _ := strings.Cut(next(), "=")
			switch strings.TrimSpace(k) {
			case codexModelKey:
				model = tomlString(v)
			case codexEffortKey:
				effort = tomlString(v)
			}
		}
	}
	return model, effort
}

// tomlString: the value of a -c override, quoted or bare.
func tomlString(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

func secretVal(v string) bool {
	if v == "" || strings.HasPrefix(v, "$") {
		return false // a reference to a variable, not the value
	}
	low := strings.ToLower(v)
	if strings.Contains(low, "://") {
		// An endpoint: secret only when it carries credentials.
		return strings.Contains(v, "@") || secretQueryRe.MatchString(low)
	}
	return secretValRe.MatchString(v)
}

// redactWord hides a NAME=value or --flag=value whose value is a secret.
func redactWord(w string) string {
	if k, v, ok := strings.Cut(w, "="); ok && (assignRe.MatchString(w) || strings.HasPrefix(k, "-")) {
		if plainNameRe.MatchString(k) && !secretNameRe.MatchString(k) && !(strings.Contains(v, "://") && secretVal(v)) {
			return w
		}
		if (secretNameRe.MatchString(k) && !strings.HasPrefix(v, "$") && v != "") || secretVal(v) {
			return k + "=" + hiddenVal
		}
		return w
	}
	if secretVal(w) {
		return hiddenVal
	}
	return w
}

// modelFlags take a model or effort, never a secret, however random it looks.
var modelFlags = []string{"--model", "-m", "--effort"}

// redactArgs hides secret values, including the one after a --api-key style flag.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, w := range args {
		if i > 0 && slices.Contains(modelFlags, args[i-1]) {
			out[i] = w
			continue
		}
		out[i] = redactWord(w)
		if i > 0 && strings.HasPrefix(args[i-1], "-") && !strings.Contains(args[i-1], "=") &&
			secretNameRe.MatchString(args[i-1]) && !strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "$") {
			out[i] = hiddenVal
		}
	}
	return out
}

// redactCommand rebuilds the alias's value for showing, secrets hidden.
func redactCommand(words []shWord) string {
	var plain []string
	for _, w := range words {
		plain = append(plain, w.text)
	}
	red := redactArgs(plain)
	var b strings.Builder
	for i, w := range words {
		if i > 0 && !(w.op && w.text == ";") {
			b.WriteByte(' ')
		}
		switch {
		case w.op && w.text == "\n":
			b.WriteString(";")
		case w.op, red[i] == hiddenVal, strings.HasSuffix(red[i], "="+hiddenVal):
			b.WriteString(red[i])
		case assignRe.MatchString(red[i]):
			k, v, _ := strings.Cut(red[i], "=")
			b.WriteString(k + "=" + showQuote(v))
		default:
			b.WriteString(showQuote(red[i]))
		}
	}
	return b.String()
}

// showQuote quotes a word for reading: "$VAR" keeps the double quotes that
// say it is expanded.
func showQuote(w string) string {
	if q := shellQuote(w); q == w || !strings.Contains(w, "$") || strings.ContainsAny(w, "\"`\\") {
		return q
	}
	return `"` + w + `"`
}

// ---------------------------------------------------------------- launching

// valueFlags: launch flags that take a value, by herdr kind.
var valueFlags = map[string][]string{
	"claude": {"--model", "--effort", "--allowedTools", "--permission-mode", "--resume"},
	"agy":    {"--model", "--effort", "--conversation"},
	"codex":  {"-m", "--model", "-c", "--config"},
}

// aliasArgs: what Collie adds after the alias's name. A flag the alias
// already has stays out (codex refuses a flag given twice), and so do a
// model or effort equal to the alias's own.
func aliasArgs(a *LauncherAlias, args []string) []string {
	has := map[string]bool{}
	for _, w := range a.Args {
		f, _, _ := strings.Cut(w, "=")
		if a.Kind == "codex" && strings.HasPrefix(f, "-m") && !strings.HasPrefix(f, "--") {
			f = "-m" // -mgpt-5
		}
		has[f] = true
	}
	var out []string
	for i := 0; i < len(args); i++ {
		w := args[i]
		f, _, inline := strings.Cut(w, "=")
		pair := []string{w}
		if !inline && slices.Contains(valueFlags[a.Kind], w) && i+1 < len(args) {
			pair = append(pair, args[i+1])
			i++
		}
		model, effort := launchModel(a.Kind, pair)
		switch {
		case !strings.HasPrefix(w, "-"):
			// a subcommand or its argument (codex resume <id>)
		case model != "" && (model == a.Model || (a.Kind == "codex" && (has["-m"] || has["--model"]))):
			continue
		case effort != "" && effort == a.Effort:
			continue
		case len(pair) == 1 && has[f]:
			// The alias's own value wins; ours, if it has one, goes too.
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			continue
		}
		out = append(out, pair...)
	}
	return out
}
