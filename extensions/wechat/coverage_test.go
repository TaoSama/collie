package wechat

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The original suite is listed in testdata/original.txt, one "file: name"
// per test. testdata/ported.txt says what became of each one:
// "file: name -> TestGoName" or "file: name -> SKIP(reason)".

var skipPattern = regexp.MustCompile(`^SKIP\(.+\)$`)

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return out
}

// goTests lists the Test functions of this package and the original test
// each one names in its "// from:" comment.
func goTests(t *testing.T) (map[string]bool, map[string][]string) {
	t.Helper()
	names, from := map[string]bool{}, map[string][]string{}
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, file := range files {
		parsed, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Name.Name == "TestMain" {
				continue
			}
			names[fn.Name.Name] = true
			if fn.Doc == nil {
				continue
			}
			for _, c := range fn.Doc.List {
				if text, ok := strings.CutPrefix(c.Text, "// from: "); ok {
					from[fn.Name.Name] = append(from[fn.Name.Name], text)
				}
			}
		}
	}
	return names, from
}

// from: extensions/wechat/README.md: every original test is ported or skipped with a reason
func TestEveryOriginalTestIsAccountedFor(t *testing.T) {
	original := readLines(t, filepath.Join("testdata", "original.txt"))
	ported := readLines(t, filepath.Join("testdata", "ported.txt"))
	names, from := goTests(t)
	want := map[string]bool{}
	for _, o := range original {
		want[o] = true
	}
	seen := map[string]bool{}
	for _, line := range ported {
		i := strings.LastIndex(line, " -> ")
		if i < 0 {
			t.Errorf("ported.txt line without \" -> \": %q", line)
			continue
		}
		orig, target := line[:i], line[i+len(" -> "):]
		if !want[orig] {
			t.Errorf("ported.txt names a test the original suite does not have: %q", orig)
		}
		if seen[orig] {
			t.Errorf("ported.txt lists %q twice", orig)
		}
		seen[orig] = true
		switch {
		case skipPattern.MatchString(target):
		case names[target]:
			found := false
			for _, f := range from[target] {
				found = found || f == orig
			}
			if !found {
				t.Errorf("%s has no // from: %s", target, orig)
			}
		default:
			t.Errorf("%q maps to %q, which is neither a Go test of this package nor SKIP(reason)", orig, target)
		}
	}
	for _, o := range original {
		if !seen[o] {
			t.Errorf("original test not accounted for in ported.txt: %q", o)
		}
	}
}
