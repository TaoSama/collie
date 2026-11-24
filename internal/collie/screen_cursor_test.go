package collie

import "testing"

func TestMarkScreenCursor(t *testing.T) {
	cases := []struct {
		name, text, want string
		rows             int
		found            bool
	}{
		{
			// Claude Code's input row as herdr 0.9.5 reads it: the caret is
			// an inverse cell, which gives up its inverse to the real cursor.
			name: "claude painted caret",
			text: "● done\r\n\x1b[0m\x1b[38;2;153;153;153m❯\u00a0\x1b[0m\x1b[7m \x1b[0m\r\n  \x1b[38;5;8m⏵⏵ bypass\x1b[0m",
			want: "● done\r\n\x1b[0m\x1b[38;2;153;153;153m❯\u00a0\x1b[0m\x1b[7m\x1b7\x1b[27m \x1b[7m\x1b[0m\r\n  \x1b[38;5;8m⏵⏵ bypass\x1b[0m",
			rows: 40, found: true,
		},
		{
			name:  "caret inside typed text keeps the rest of the row as it was",
			text:  "❯ hel\x1b[7ml\x1b[0mo",
			want:  "❯ hel\x1b[7m\x1b7\x1b[27ml\x1b[7m\x1b[0mo",
			found: true,
		},
		{
			name:  "a colour index 7 isn't inverse, nor is a reversed status bar",
			text:  "\x1b[38;5;7m❯ hi\x1b[0m\r\n\x1b[7m NORMAL \x1b[0m",
			want:  "\x1b[38;5;7m❯ hi\x1b7\x1b[0m\r\n\x1b[7m NORMAL \x1b[0m",
			found: true,
		},
		{
			name:  "codex composer placeholder: the cursor is before it",
			text:  "› \x1b[2mAsk Codex to do anything\x1b[0m\r\n\r\n  ? for shortcuts",
			want:  "› \x1b[2m\x1b7Ask Codex to do anything\x1b[0m\r\n\r\n  ? for shortcuts",
			found: true,
		},
		{
			name:  "boxed prompt: after the text, not the border",
			text:  "╭────╮\r\n│ > fix it   │\r\n╰────╯",
			want:  "╭────╮\r\n│ > fix it\x1b7   │\r\n╰────╯",
			found: true,
		},
		{
			name:  "empty boxed prompt: one space after the mark",
			text:  "│ >                  │\r\n╰──╯",
			want:  "│ > \x1b7                 │\r\n╰──╯",
			found: true,
		},
		{
			name:  "empty prompt: after the space",
			text:  "› \r\n  status line",
			want:  "› \x1b7\r\n  status line",
			found: true,
		},
		{
			name:  "a menu's selected choice is not a prompt",
			text:  "\x1b[1m$ ls\x1b[0m\r\n❯ 1. Yes\r\n  2. No",
			want:  "\x1b[1m$ ls\x1b[0m\r\n❯ 1. Yes\r\n  2. No\x1b7",
			found: true,
		},
		{
			name:  "shell prompt whose trailing space was dropped",
			text:  "out\r\nme@mac ~ %\r\n   \r\n",
			want:  "out\r\nme@mac ~ % \x1b7\r\n   \r\n",
			found: true,
		},
		{
			name:  "shell mid-command",
			text:  "me@mac ~ % ls -la",
			want:  "me@mac ~ % ls -la\x1b7",
			found: true,
		},
		{
			name: "only the visible rows count",
			text: "\x1b[7mx\x1b[0m\r\nb\r\nc",
			want: "\x1b[7mx\x1b[0m\r\nb\r\nc\x1b7",
			rows: 2, found: true,
		},
		{
			name:  "OSC hyperlinks print nothing",
			text:  "\x1b]8;;https://x\x07link\x1b]8;;\x1b\\ \x1b[7mz\x1b[0m",
			want:  "\x1b]8;;https://x\x07link\x1b]8;;\x1b\\ \x1b[7m\x1b7\x1b[27mz\x1b[7m\x1b[0m",
			found: true,
		},
		{name: "empty", text: "", want: ""},
		{name: "blank screen", text: "\x1b[0m  \r\n", want: "\x1b[0m  \r\n"},
	}
	for _, c := range cases {
		got, found := markScreenCursor(c.text, c.rows)
		if got != c.want || found != c.found {
			t.Errorf("%s:\n got %q %v\nwant %q %v", c.name, got, found, c.want, c.found)
		}
	}
}
