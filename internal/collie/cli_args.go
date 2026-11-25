package collie

import (
	"flag"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// flagToken is what parseArgs treats as a flag: -name, --name or --name=value.
// Anything else that starts with a dash ("-", "-5度", "->", "--- 分隔") is text.
var flagToken = regexp.MustCompile(`^--?[A-Za-z][\w-]*(=|$)`)

// parseArgs parses fs over args with flags allowed anywhere: before, after or
// between the words of the text. Everything after "--" is text as written.
// Words before "--" that still spell one of fs's flags (inside a quoted
// argument, say) are refused rather than sent. fs.Args() holds the text words.
func parseArgs(fs *flag.FlagSet, args []string) error {
	var pos, literal []string
	for i := 0; i < len(args); {
		a := args[i]
		if a == "--" {
			literal = args[i+1:]
			break
		}
		if !flagToken.MatchString(a) {
			pos = append(pos, a)
			i++
			continue
		}
		n := 1
		name, hasValue := flagName(a)
		if f := fs.Lookup(name); f != nil && !hasValue && !isBoolFlag(f) && i+1 < len(args) {
			n = 2
		}
		if err := fs.Parse(args[i : i+n]); err != nil {
			return err
		}
		i += n
	}
	if err := strayFlag(fs, pos); err != nil {
		return err
	}
	return fs.Parse(slices.Concat([]string{"--"}, pos, literal))
}

func flagName(tok string) (name string, hasValue bool) {
	name, _, hasValue = strings.Cut(strings.TrimLeft(tok, "-"), "=")
	return name, hasValue
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// strayFlag reports a word of text that spells a flag of fs, e.g. the
// "--opt" in `collie reply "要发吗 --opt 发"`.
func strayFlag(fs *flag.FlagSet, words []string) error {
	for _, w := range words {
		for _, f := range strings.Fields(w) {
			if !flagToken.MatchString(f) {
				continue
			}
			if name, _ := flagName(f); fs.Lookup(name) != nil {
				return fmt.Errorf("%q in the text looks like a %s flag: pass it as its own argument, or put literal text after --", f, fs.Name())
			}
		}
	}
	return nil
}
