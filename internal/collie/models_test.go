package collie

import "testing"

func TestAliasLabel(t *testing.T) {
	for id, want := range map[string]string{
		"fable": "Fable", "opus": "Opus", "": "",
		"claude-opus-5-5": "", "opus[1m]": "", "gpt-5.1": "",
	} {
		if got := aliasLabel(id); got != want {
			t.Errorf("aliasLabel(%q) = %q, want %q", id, got, want)
		}
	}
}
