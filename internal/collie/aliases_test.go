package collie

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const fakeToken = "sk-ant-api03-Zx9Qw8Er7Ty6Ui5Op4As3Df2Gh1Jk0Lz"

func TestParseLauncher(t *testing.T) {
	cases := []struct {
		name, value string
		want        LauncherAlias // Args, Env, Command checked separately
		ok          bool
	}{
		{"ccf", `ANTHROPIC_BASE_URL="https://cf.example.com" ANTHROPIC_MODEL="fable[1m]" claude`, LauncherAlias{Kind: "claude", Model: "fable[1m]", BaseURL: "https://cf.example.com"}, true},
		{"ccl5", `claude --model claude-opus-5-5[1m] --effort high`, LauncherAlias{Kind: "claude", Model: "claude-opus-5-5[1m]", Effort: "high"}, true},
		{"cce", `CLAUDE_CODE_EFFORT_LEVEL=max claude --model=opus`, LauncherAlias{Kind: "claude", Model: "opus", Effort: "max"}, true},
		{"flag-wins", `ANTHROPIC_MODEL=sonnet claude --model opus`, LauncherAlias{Kind: "claude", Model: "opus"}, true},
		{"cx", `codex -m gpt-5.5 -c model_reasoning_effort=high`, LauncherAlias{Kind: "codex", Model: "gpt-5.5", Effort: "high"}, true},
		{"cxq", `codex -c model="gpt-6" -c 'model_reasoning_effort="xhigh"'`, LauncherAlias{Kind: "codex", Model: "gpt-6", Effort: "xhigh"}, true},
		{"cxl", `command codex --model=gpt-6.2-astra --config model_reasoning_effort=low --no-alt-screen`, LauncherAlias{Kind: "codex", Model: "gpt-6.2-astra", Effort: "low"}, true},
		{"cxm", `codex -mgpt-5`, LauncherAlias{Kind: "codex", Model: "gpt-5"}, true},
		{"ex", `export ANTHROPIC_BASE_URL=$ANTHROPIC_BASE_URL ANTHROPIC_MODEL=opus; noglob claude`, LauncherAlias{Kind: "claude", Model: "opus", BaseURL: "$ANTHROPIC_BASE_URL"}, true},
		{"envi", `env -i -u HOME HOME=/tmp -- /usr/local/bin/claude --model haiku`, LauncherAlias{Kind: "claude", Model: "haiku"}, true},
		{"chain", `cd ~/code && exec nocorrect agy --model gemini-3`, LauncherAlias{Kind: "agy", Model: "gemini-3"}, true},
		{"varmodel", `claude --model "$MODEL"`, LauncherAlias{Kind: "claude"}, true},
		{"ll", `ls -la`, LauncherAlias{}, false},
		{"gst", `git status && echo claude`, LauncherAlias{}, false},
		{"claudebin", `claude-code --help`, LauncherAlias{}, false},
		{"bad", `claude "unterminated`, LauncherAlias{}, false},
		{"bad name;", `claude`, LauncherAlias{}, false},
	}
	for _, c := range cases {
		got, ok := parseLauncher(c.name, c.value)
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		c.want.Name = c.name
		got.Args, got.Env, got.Command, got.raw, got.rawEnv = nil, nil, "", nil, nil
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestParseLauncherHidesSecrets(t *testing.T) {
	cases := []string{
		`ANTHROPIC_BASE_URL="https://cf.example.com" ANTHROPIC_AUTH_TOKEN="` + fakeToken + `" claude --model fable[1m] --dangerously-skip-permissions`,
		`OPENAI_API_KEY=plain-but-named claude --api-key ` + fakeToken,
		`claude --api-key=` + fakeToken,
		`FOO=` + fakeToken + ` codex`,
		`ANTHROPIC_BASE_URL='https://user:` + fakeToken + `@proxy.example.com' claude`,
	}
	for _, v := range cases {
		a, ok := parseLauncher("x", v)
		if !ok {
			t.Fatalf("not a launcher: %s", v)
		}
		b, _ := json.Marshal(a)
		if strings.Contains(string(b), fakeToken) || strings.Contains(string(b), "plain-but-named") {
			t.Errorf("secret leaked: %s", b)
		}
		if !strings.Contains(string(b), hiddenVal) {
			t.Errorf("no mark where the secret was: %s", b)
		}
	}
	a, _ := parseLauncher("ccf", cases[0])
	want := `ANTHROPIC_BASE_URL=https://cf.example.com ANTHROPIC_AUTH_TOKEN=*** claude --model 'fable[1m]' --dangerously-skip-permissions`
	if a.Command != want {
		t.Errorf("command = %q\nwant      %q", a.Command, want)
	}
	if !reflect.DeepEqual(a.Env, []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"}) {
		t.Errorf("env = %v", a.Env)
	}
	// Variable references and long settings stay readable.
	a, _ = parseLauncher("ccg", `ANTHROPIC_AUTH_TOKEN="$TOKEN" ANTHROPIC_DEFAULT_SONNET_MODEL=claude-opus-5-5-20260101-extended-thinking claude`)
	if !strings.Contains(a.Command, `ANTHROPIC_AUTH_TOKEN="$TOKEN"`) || !strings.Contains(a.Command, "20260101-extended-thinking") {
		t.Errorf("command = %s", a.Command)
	}
}

func TestShellSplit(t *testing.T) {
	ws, err := shellSplit(`a 'b c' "d \"e\" $F" g\ h $'i\tj' k;l&&m # comment`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range ws {
		if w.op {
			got = append(got, "<"+w.text+">")
		} else {
			got = append(got, w.text)
		}
	}
	want := []string{"a", "b c", `d "e" $F`, "g h", "i\tj", "k", "<;>", "l", "<&&>", "m"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestShellLineRoundTrip(t *testing.T) {
	words := []string{"ccf", "--model", "fable[1m]", "--allowedTools", "Bash(collie:*),Read(~/x/**)", "it's", "", "=claude"}
	line, err := shellLine(words)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := shellSplit(line)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range ws {
		got = append(got, w.text)
	}
	if !reflect.DeepEqual(got, words) {
		t.Errorf("got %q", got)
	}
}

func TestAliasDefs(t *testing.T) {
	bash := "welcome!\n" + aliasBegin + "alias cc='claude --model opus'\nalias it='echo it'\\''s'\nalias -- -x='y'\nalias multi='claude\n--effort high'\n" + aliasEnd + "bye"
	defs, err := aliasDefs("bash", []byte(bash))
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"cc", "claude --model opus"}, {"it", "echo it's"}, {"-x", "y"}, {"multi", "claude\n--effort high"}}
	if !reflect.DeepEqual(defs, want) {
		t.Errorf("bash: %q", defs)
	}
	zsh := "p10k says hi" + aliasBegin + "cc" + aliasField + "claude --model opus" + aliasRec + "g" + aliasField + "git" + aliasRec + aliasEnd
	defs, err = aliasDefs("zsh", []byte(zsh))
	if err != nil || !reflect.DeepEqual(defs, [][2]string{{"cc", "claude --model opus"}, {"g", "git"}}) {
		t.Errorf("zsh: %q %v", defs, err)
	}
	if _, err := aliasDefs("bash", []byte("no markers")); err == nil {
		t.Error("output without markers accepted")
	}
}

func TestAliasShell(t *testing.T) {
	home := t.TempDir()
	touch := func(f string) { os.WriteFile(filepath.Join(home, f), nil, 0o600) }
	check := func(login, want, wantRC string) {
		t.Helper()
		name, bin, rc := aliasShell(home, login)
		if name != want || rc != wantRC || bin == "" {
			t.Errorf("login %s: %s %s %s, want %s %s", login, name, bin, rc, want, wantRC)
		}
	}
	check("/bin/zsh", "zsh", "~/.zshrc")
	check("/opt/homebrew/bin/bash", "bash", "~/.bashrc") // neither file: bash as logged in
	touch(".zshrc")
	check("/bin/bash", "zsh", "~/.zshrc") // bash without ~/.bashrc
	touch(".bashrc")
	check("/bin/bash", "bash", "~/.bashrc")
	if _, bin, _ := aliasShell(home, "/opt/homebrew/bin/bash"); bin != "/opt/homebrew/bin/bash" {
		t.Errorf("login shell's own path not kept: %s", bin)
	}
}

// discovery runs the real shells against a home made up for the test.
func TestDiscoverAliases(t *testing.T) {
	for _, sh := range []string{"bash", "zsh"} {
		bin, err := exec.LookPath(sh)
		if err != nil {
			t.Logf("no %s here", sh)
			continue
		}
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("SHELL", bin)
		rc := "echo noisy rc file\n" +
			"alias ccf='ANTHROPIC_BASE_URL=\"$ANTHROPIC_BASE_URL\" ANTHROPIC_AUTH_TOKEN=\"" + fakeToken + "\" claude --model \"fable[1m]\"'\n" +
			"alias cx='command codex -m gpt-5.5 -c model_reasoning_effort=high'\n" +
			"alias ll='ls -l'\n"
		os.WriteFile(filepath.Join(home, "."+sh+"rc"), []byte(rc), 0o600)
		rep, err := discoverAliases(context.Background(), home)
		if err != nil {
			t.Fatalf("%s: %v", sh, err)
		}
		if rep.Shell != sh || rep.RC != "~/."+sh+"rc" || len(rep.Aliases) != 2 {
			t.Fatalf("%s: %+v", sh, rep)
		}
		ccf, cx := rep.Aliases[0], rep.Aliases[1]
		if ccf.Name != "ccf" || ccf.Model != "fable[1m]" || ccf.BaseURL != "$ANTHROPIC_BASE_URL" || strings.Contains(ccf.Command, fakeToken) {
			t.Errorf("%s ccf: %+v", sh, ccf)
		}
		if cx.Name != "cx" || cx.Kind != "codex" || cx.Model != "gpt-5.5" || cx.Effort != "high" {
			t.Errorf("%s cx: %+v", sh, cx)
		}
	}
}

func TestAliasArgs(t *testing.T) {
	cl, _ := parseLauncher("ccf", `ANTHROPIC_MODEL=opus claude --dangerously-skip-permissions`)
	got := aliasArgs(&cl, []string{"--model", "opus", "--effort", "high", "--dangerously-skip-permissions", "--allowedTools", "Bash(collie:*)"})
	if want := []string{"--effort", "high", "--allowedTools", "Bash(collie:*)"}; !reflect.DeepEqual(got, want) {
		t.Errorf("claude: %q", got)
	}
	got = aliasArgs(&cl, []string{"--model", "sonnet", "--effort", "low"}) // the alias's model, the effort chosen
	if want := []string{"--effort", "low"}; !reflect.DeepEqual(got, want) {
		t.Errorf("claude other model: %q", got)
	}
	plain, _ := parseLauncher("cc", `claude`)
	if got = aliasArgs(&plain, []string{"--model=fable", "--effort", "max"}); !reflect.DeepEqual(got, []string{"--effort", "max"}) {
		t.Errorf("alias pinning no model: %q", got) // it leaves the model to the agent
	}
	cxc, _ := parseLauncher("cxc", `codex`)
	if got = aliasArgs(&cxc, []string{"-c", `model="gpt-6"`, "-c", `model_reasoning_effort="high"`}); !reflect.DeepEqual(got, []string{"-c", `model_reasoning_effort="high"`}) {
		t.Errorf("codex -c model: %q", got)
	}
	cxm, _ := parseLauncher("cxm", `codex -mgpt-5 -a never`)
	got = aliasArgs(&cxm, []string{"-m", "gpt-6", "-a", "on-request", "--search"})
	if want := []string{"--search"}; !reflect.DeepEqual(got, want) {
		t.Errorf("codex -m glued, -a twice: %q", got)
	}
	cx, _ := parseLauncher("cx", `codex -m gpt-5.5 --no-alt-screen`)
	got = aliasArgs(&cx, []string{"resume", "abc", "-m", "gpt-6", "-c", `model_reasoning_effort="high"`, "--no-alt-screen", "--dangerously-bypass-approvals-and-sandbox"})
	if want := []string{"resume", "abc", "-c", `model_reasoning_effort="high"`, "--dangerously-bypass-approvals-and-sandbox"}; !reflect.DeepEqual(got, want) {
		t.Errorf("codex: %q", got) // codex refuses -m and --no-alt-screen twice
	}
}

func TestAliasViewsMergeAndStale(t *testing.T) {
	s := &Server{}
	mk := func(names ...string) aliasReport {
		rep := aliasReport{Shell: "zsh", RC: "~/.zshrc"}
		for _, n := range names {
			rep.Aliases = append(rep.Aliases, LauncherAlias{Name: n, Kind: "claude", Model: n + "-model"})
		}
		return rep
	}
	s.aliases.put("", mk("ccf", "ccx"), nil)
	s.aliases.put("devos", mk("ccf", "ccd"), nil)
	s.aliases.put("devos", aliasReport{}, errors.New("timeout")) // keeps the last list
	s.aliases.put("mac", aliasReport{Shell: "bash", RC: "~/.bashrc", Aliases: []LauncherAlias{{Name: "ccf", Kind: "claude", Model: "other"}}}, nil)
	machines := []Machine{{ID: "", Name: "hub", Local: true}, {ID: "devos", Name: "devos", Online: true}, {ID: "mac", Name: "mac"}, {ID: "new", Name: "new", Online: true}}
	b, _ := json.Marshal(s.aliasViews(machines))
	var v struct {
		Machines []aliasMachineView `json:"machines"`
		Aliases  []aliasView        `json:"aliases"`
	}
	json.Unmarshal(b, &v)
	if len(v.Machines) != 3 || v.Machines[0].Stale || !v.Machines[1].Stale || v.Machines[1].Error != "timeout" || len(v.Machines[1].Aliases) != 2 || !v.Machines[2].Stale {
		t.Fatalf("machines: %s", b)
	}
	// Each computer's own order (its rc files'), the computers in turn.
	if len(v.Aliases) != 3 || v.Aliases[0].Name != "ccf" || v.Aliases[1].Name != "ccx" || v.Aliases[2].Name != "ccd" {
		t.Fatalf("aliases: %s", b)
	}
	ccf := v.Aliases[0]
	if ccf.Model != "ccf-model" || ccf.RC != "~/.zshrc" || !reflect.DeepEqual(ccf.Machines, []string{"", "devos", "mac"}) {
		t.Errorf("ccf: %+v", ccf)
	}
}

func TestGetAliasesNodeOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/bash")
	os.WriteFile(filepath.Join(home, ".bashrc"), []byte("alias cc='claude --model opus'\n"), 0o600)
	hub := &Server{home: home}
	w := httptest.NewRecorder()
	hub.getAliases(w, httptest.NewRequest("GET", "/api/aliases", nil))
	if w.Code != 404 {
		t.Errorf("hub served the node route: %d", w.Code)
	}
	node := &Server{home: home, node: true}
	w = httptest.NewRecorder()
	node.getAliases(w, httptest.NewRequest("GET", "/api/aliases", nil))
	var rep aliasReport
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(rep.Aliases) != 1 || rep.Aliases[0].Name != "cc" || rep.Aliases[0].Model != "opus" || rep.RC != "~/.bashrc" {
		t.Errorf("%+v", rep)
	}
}

func TestSuperAliasesHub(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/bash")
	os.WriteFile(filepath.Join(home, ".bashrc"), []byte("alias cx='codex -m gpt-5.5'\nalias g=git\n"), 0o600)
	s := &Server{home: home, herdr: newHerdr(filepath.Join(home, "missing.sock"), "hub", home)}
	w := httptest.NewRecorder()
	s.superAliases(w, httptest.NewRequest("GET", "/api/super/aliases", nil))
	var v struct {
		Machines []aliasMachineView `json:"machines"`
		Aliases  []aliasView        `json:"aliases"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(v.Machines) != 1 || v.Machines[0].Stale || v.Machines[0].SyncedAt == nil || len(v.Aliases) != 1 || v.Aliases[0].Name != "cx" || !reflect.DeepEqual(v.Aliases[0].Machines, []string{""}) {
		t.Errorf("%s", w.Body)
	}
	if a, err := s.aliasOn(context.Background(), "", "cx", "codex"); err != nil || a.Kind != "codex" {
		t.Errorf("aliasOn cx: %v %v", a, err)
	}
	if _, err := s.aliasOn(context.Background(), "", "cx", "claude"); err == nil {
		t.Error("a codex alias was offered for claude")
	}
	if _, err := s.aliasOn(context.Background(), "", "g", "claude"); err == nil {
		t.Error("a non-launcher alias was offered for launching")
	}
	if _, err := s.aliasOn(context.Background(), "gone", "cx", "codex"); err == nil {
		t.Error("an unknown computer answered")
	}
}

func TestShellLineRejectsControlChars(t *testing.T) {
	for _, w := range []string{"a\tb", "x\x15", "line\nbreak", "\x1b[2J"} {
		if _, err := shellLine([]string{"ccf", w}); err == nil {
			t.Errorf("%q accepted", w)
		}
	}
	if q := shellQuote("=claude"); q != "'=claude'" {
		t.Errorf("leading = not quoted: %s", q)
	}
}

func TestAliasKeepsPathsAndRawArgs(t *testing.T) {
	dir := "/Users/someone/Workspaces/collie/very/long/project-path"
	a, ok := parseLauncher("ccd", `claude --add-dir `+dir+` --add-dir ~/notes/`+strings.Repeat("x", 40)+` --api-key `+fakeToken)
	if !ok {
		t.Fatal("not a launcher")
	}
	if a.Args[1] != dir || !strings.HasPrefix(a.Args[3], "~/notes/") || a.Args[5] != hiddenVal {
		t.Errorf("display args: %q", a.Args)
	}
	if raw := a.launchArgs(); raw[5] != fakeToken || raw[1] != dir {
		t.Errorf("raw args lost: %q", raw)
	}
	b, _ := json.Marshal(a)
	if strings.Contains(string(b), fakeToken) {
		t.Errorf("raw args serialized: %s", b)
	}
	var back LauncherAlias // what the hub holds for another computer
	json.Unmarshal(b, &back)
	if got := back.launchArgs(); !reflect.DeepEqual(got, a.Args) {
		t.Errorf("remote copy: %q", got)
	}
}

func TestRedactsHeaders(t *testing.T) {
	for _, v := range []string{
		`ANTHROPIC_CUSTOM_HEADERS="x-team: infra" claude`,
		`HTTP_AUTHORIZATION=abc claude`,
		`claude --header "Bearer abc.def"`,
		`FOO="x-api-key: abc" codex`,
		`FOO="Api-Key: abc" codex`,
	} {
		a, _ := parseLauncher("x", v)
		if strings.Contains(a.Command, "abc") || strings.Contains(a.Command, "infra") {
			t.Errorf("leaked: %s", a.Command)
		}
	}
}

func TestAliasArgsKeepsPositionals(t *testing.T) {
	cx, _ := parseLauncher("cx", `codex --search -a never`)
	// codex resume <id> after a repeated boolean flag: the id must stay.
	got := aliasArgs(&cx, []string{"--search", "resume", "abc", "--config=x=1", "--search=yes", "prompt"})
	if want := []string{"resume", "abc", "--config=x=1", "prompt"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
	cl, _ := parseLauncher("cl", `claude --add-dir /a --model opus`)
	got = aliasArgs(&cl, []string{"--add-dir", "/b", "--model", "sonnet", "--effort", "high"})
	if want := []string{"--add-dir", "/b", "--effort", "high"}; !reflect.DeepEqual(got, want) {
		t.Errorf("claude: %q", got)
	}
}

func TestAliasOnUsesListedAlias(t *testing.T) {
	s := &Server{home: t.TempDir(), herdr: newHerdr(filepath.Join(t.TempDir(), "missing.sock"), "hub", t.TempDir())}
	rep := aliasReport{RC: "~/.zshrc", Aliases: []LauncherAlias{{Name: "ccf", Kind: "claude"}}}
	s.aliases.put("devos", rep, nil) // devos is offline: any read would fail
	if a, err := s.aliasOn(context.Background(), "devos", "ccf", "claude"); err != nil || a.Name != "ccf" {
		t.Fatalf("listed alias not used: %v %v", a, err)
	}
	if snap, _ := s.aliases.get("devos"); snap.err != "" {
		t.Errorf("read again for a listed alias: %s", snap.err)
	}
	if _, err := s.aliasOn(context.Background(), "devos", "ccx", "claude"); err == nil {
		t.Fatal("missing alias found")
	}
	snap, _ := s.aliases.get("devos")
	if snap.err == "" || len(snap.rep.Aliases) != 1 {
		t.Fatalf("failed read not recorded or list lost: %+v", snap)
	}
	s.aliases.put("devos", rep, nil)
	s.aliases.put("devos", aliasReport{}, errors.New("offline"))
	snap, _ = s.aliases.get("devos")
	tried := snap.tried
	if _, err := s.aliasOn(context.Background(), "devos", "ccx", "claude"); err == nil {
		t.Fatal("missing alias found")
	}
	if snap, _ := s.aliases.get("devos"); !snap.tried.Equal(tried) {
		t.Error("read again right after a failed read")
	}
}

func TestNodeJSONStatusError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: rewriteHost{u.Host}}
	err := nodeJSON(context.Background(), client, "GET", "/api/aliases", nil, nil)
	var se *nodeStatusError
	if !errors.As(err, &se) || se.Code != http.StatusNotFound {
		t.Fatalf("err = %v", err)
	}
}

// The program is only on the PATH the rc file sets, and an alias of the same
// name shadows it there (devbox: alias codex='command codex --no-alt-screen').
func TestBinInShellSeesThroughAlias(t *testing.T) {
	sh := path.Base(loginShell())
	if sh != "bash" && sh != "zsh" {
		t.Skipf("login shell %q", sh)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", home)
	bin := filepath.Join(home, "tools")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	prog := filepath.Join(bin, "fakeagent")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rc := "export PATH=\"" + bin + ":$PATH\"\nalias fakeagent='command fakeagent --flag'\n"
	for _, f := range []string{".bashrc", ".zshrc"} {
		if err := os.WriteFile(filepath.Join(home, f), []byte(rc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := binInShell(context.Background(), home, "fakeagent"); got != prog {
		t.Fatalf("got %q, want %q", got, prog)
	}
	if got := binInShell(context.Background(), home, "no-such-agent-xyz"); got != "" {
		t.Fatalf("missing program: %q", got)
	}
}
