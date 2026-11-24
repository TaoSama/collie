package collie

import "testing"

func TestParseFrontmatter(t *testing.T) {
	fm, body := parseFrontmatter(`---
name: doc-writer
description: |
  多类型文档编写与交付技能。
  根据读者撰写文档。
argument-hint: "[file] [mode]"
quoted: 'it''s'
folded: >
  one
  two
plain: starts here
  and goes on
user-invocable: false # hidden
metadata:
  short-description: Short one
tags:
  - a
  - b: c
---
# Body
text
`)
	want := map[string]string{
		"name":                       "doc-writer",
		"description":                "多类型文档编写与交付技能。\n根据读者撰写文档。",
		"argument-hint":              "[file] [mode]",
		"quoted":                     "it's",
		"folded":                     "one two",
		"plain":                      "starts here and goes on",
		"user-invocable":             "false",
		"metadata.short-description": "Short one",
	}
	for k, v := range want {
		if fm[k] != v {
			t.Errorf("%s = %q, want %q", k, fm[k], v)
		}
	}
	if len(fm) != len(want) {
		t.Errorf("extra fields: %v", fm)
	}
	if body != "# Body\ntext\n" {
		t.Errorf("body = %q", body)
	}
	if fm, body := parseFrontmatter("no header\n---\n"); len(fm) != 0 || body != "no header\n---\n" {
		t.Errorf("no header: %v %q", fm, body)
	}
	if fm, _ := parseFrontmatter("---\nname: x\n"); len(fm) != 0 {
		t.Errorf("unclosed header read: %v", fm)
	}
}

func TestMenuLine(t *testing.T) {
	for in, want := range map[string]string{
		"\n\n# Review PR\nmore": "Review PR",
		"abcdefghij":            "abcdefghij",
		"abcdefghijkl":          "abcdefghi…",
		"":                      "",
	} {
		if got := menuLine(in, 10); got != want {
			t.Errorf("menuLine(%q) = %q, want %q", in, got, want)
		}
	}
}
