package collie

import (
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRCAliasOrder(t *testing.T) {
	home := t.TempDir()
	noZDOTDIR(t)
	write := func(f, s string) { os.WriteFile(filepath.Join(home, f), []byte(s), 0o600) }
	write(".zshenv", "alias zz='claude'\n")
	write(".zshrc", "# alias commented='codex'\n"+
		"alias ccx='claude --model claude-opus-5-5[1m]'\n"+
		"source ~/.aliases\n"+
		"[ -f $HOME/.more ] && alias cca='claude' ccb='claude'; echo hi\n"+
		"alias ccx='claude --model opus'\n"+ // redefined: stays where first written
		"alias -g G='| grep'\n"+
		". \"$HOME/.zshrc\"\n") // a loop back is read once
	write(".aliases", "alias codex62='codex -m gpt-6.2-sol'\nalias ab='agy'\n")
	got := rcAliasOrder(home, "zsh")
	want := map[string]int{"zz": 0, "ccx": 1, "codex62": 2, "ab": 3, "cca": 4, "ccb": 5, "G": 6}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order: %v, want %v", got, want)
	}
	list := []LauncherAlias{{Name: "ab"}, {Name: "looped2"}, {Name: "ccx"}, {Name: "looped1"}, {Name: "zz"}}
	sortAliasesByRC(list, got)
	var names []string
	for _, a := range list {
		names = append(names, a.Name)
	}
	if want := []string{"zz", "ccx", "ab", "looped1", "looped2"}; !reflect.DeepEqual(names, want) {
		t.Errorf("sorted: %v, want %v", names, want)
	}
	if o := rcAliasOrder(home, "bash"); len(o) != 0 {
		t.Errorf("bash reads ~/.bashrc only: %v", o)
	}
}

// The real shells hand their aliases over in their own order; the list
// follows the rc file.
func TestDiscoverAliasesInRCOrder(t *testing.T) {
	for _, sh := range []string{"bash", "zsh"} {
		if _, err := exec.LookPath(sh); err != nil {
			t.Logf("no %s here", sh)
			continue
		}
		home := t.TempDir()
		t.Setenv("HOME", home)
		noZDOTDIR(t)
		rc := "alias zcx='codex'\nalias ccx='claude --model opus'\nalias acc='agy'\nalias mcc='claude'\n"
		os.WriteFile(filepath.Join(home, "."+sh+"rc"), []byte(rc), 0o600)
		if !readsRC(t, home, sh) {
			continue
		}
		rep, err := discoverAliases(context.Background(), home)
		if err != nil {
			t.Fatalf("%s: %v", sh, err)
		}
		var names []string
		for _, a := range rep.Aliases {
			names = append(names, a.Name)
		}
		if want := []string{"zcx", "ccx", "acc", "mcc"}; !reflect.DeepEqual(names, want) {
			t.Errorf("%s: %v, want %v", sh, names, want)
		}
	}
}

// Collie reads aliases with the account's own login shell, which a test
// can't change ($SHELL doesn't count), so these follow whatever it is.

// accountRC: the rc file (".zshrc") Collie reads aliases from in home for
// this account. Skips the test when the login shell is neither bash nor zsh.
func accountRC(t *testing.T, home string) string {
	t.Helper()
	login := loginShell()
	if b := path.Base(login); b != "bash" && b != "zsh" {
		t.Skipf("login shell %q", login)
	}
	name, _, _ := aliasShell(home, login)
	return "." + name + "rc"
}

// readsRC: whether Collie reads sh's rc file in home as it is now. A zsh
// account never reads ~/.bashrc; a bash one reads ~/.zshrc when there is
// no ~/.bashrc.
func readsRC(t *testing.T, home, sh string) bool {
	t.Helper()
	if accountRC(t, home) != "."+sh+"rc" {
		t.Logf("%s: the account logs in with %s, which doesn't read ~/.%src", sh, loginShell(), sh)
		return false
	}
	return true
}

func noZDOTDIR(t *testing.T) {
	t.Setenv("ZDOTDIR", "") // restored afterwards
	os.Unsetenv("ZDOTDIR")
}
