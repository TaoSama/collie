package collie

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

// Real screens from claude 2.1 on herdr 0.8, which reports both as "idle".
const idleClaude = `           Claude Code v2.1.325
 ▐▛███▛█   llmbox-2/seed-evolving[1m]
▝▜██████▀  API Usage Billing
 ▝▝   ▝▝   /…/home/u/.collie/router

  ⎿  SessionStart:startup hook error
  ⎿  Failed with non-blocking status code:
     node:internal/modules/cjs/loader:1433

● 已回复：自我介绍 + hostname（n37-125-077），两条测试
  消息内容相同，合并为一条回复。

✻ Worked for 14s · done 5:11 AM

──────────────────────────────────────────────────────
❯ Try "edit <filepath> to..."
──────────────────────────────────────────────────────
  ⏸ manual mode on · ? for shortcuts
`

const trustDialog = ` Accessing workspace:
 /data00/home/u/.collie
 Quick safety check: Is this a project you created or
 one you trust? (Like your own code, a well-known
 open source project, or work from your team). If
 not, take a moment to review what's in this folder
 first.
 Claude Code'll be able to read, edit, and execute
 files here.
 Security guide
 ❯ 1. Yes, I trust this folder
   2. No, exit
 Enter to confirm · Esc to cancel
`

const wrappedMenu = `─────────────────────────────────────────────────────
 Read file
 Symlink target: /data00/home/u/.collie/up
 loads/20261117/0d224b-number.png

╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 Read(/home/u/.collie/uploads/20261117/0d2
 24b-number.png)
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, allow reading from
      /home/u/.collie/uploads/20261117 and
      /data00/home/u/.collie/uploads/2026
      1117 during this session
   3. No

 Esc to cancel · Tab to amend`

func TestWrappedChoices(t *testing.T) {
	a := parseApproval(wrappedMenu)
	if len(a.Choices) != 3 || !a.Choices[0].Current || !strings.HasPrefix(a.Choices[1].Label, "Yes, allow reading from /home/u") || a.Choices[2].Label != "No" {
		t.Fatalf("choices: %+v", a.Choices)
	}
	if !strings.HasSuffix(a.Question, "Do you want to proceed?") || !strings.Contains(a.Question, "Read file") {
		t.Fatalf("question: %q", a.Question)
	}
}

func TestScreenAsks(t *testing.T) {
	if screenAsks(idleClaude) {
		t.Error("idle prompt taken for a question")
	}
	if !screenAsks(trustDialog) {
		t.Error("trust dialog not seen")
	}
	if !screenAsks(cursorTrust) {
		t.Error("cursor-style trust dialog not seen")
	}
}

const cursorTrust = ` Claude Code'll be able to read, edit, and execute
 files here.
 Security guide
 ❯ No, exit
   Yes, I trust this folder
 Enter to confirm · Esc to cancel
`

const trustFull = `u@host:~/collie-card-test$ claude

──────────────────────────────────────────────────────
 Accessing workspace:

 /data00/home/u/collie-card-test

 Quick safety check: Is this a project you created or
 one you trust? (Like your own code, a well-known
 open source project, or work from your team). If
 not, take a moment to review what's in this folder
 first.

 Claude Code'll be able to read, edit, and execute
 files here.

 Security guide

 ❯ No, exit
   Yes, I trust this folder

 Enter to confirm · Esc to cancel
`

func TestApprovalKeepsParagraphs(t *testing.T) {
	a := parseApproval(trustFull)
	if len(a.Choices) != 2 {
		t.Fatalf("choices: %+v", a.Choices)
	}
	if strings.Contains(a.Question, "$ claude") {
		t.Errorf("question starts above the dialog:\n%s", a.Question)
	}
	if !strings.HasPrefix(a.Question, " Accessing workspace:\n\n") || !strings.Contains(a.Question, "\n\n Quick safety check:") {
		t.Errorf("paragraphs lost:\n%q", a.Question)
	}
}

// The router quoted a worker's trust dialog, then went back to its prompt.
const quotedDialog = `> [collie] 1 new event

  #t132 · chat:main · worker · from paper-ref-checker · 20:20
    worker paper-ref-checker is BLOCKED waiting for a human approval/answer in its
  terminal. Screen:
    Accessing workspace:
    /data00/home/u/agent-workspace
    Do you trust the contents of this project?
    Antigravity CLI requires permission to read, edit, and execute files here.
    > Yes, I trust this folder
      No, exit
      ↑/↓ Navigate · enter Confirm
                                                     auto · Gemini 4 Carbon ·
  high

  Reply to the human only via ` + "`collie reply`" + `.

○ Thought for 5.8s (I realized I was wrong in telling the user that no prompt was expected)
● Ran (C=/data00/home/u/.local/bin/collie; $C reply "说错了") (ctrl+o to expand)

  paper-ref-checker is stuck waiting for you to approve folder access, so it hasn't started
  the check yet.

───────────────────────────────────────────────────────────────────────────────
>
───────────────────────────────────────────────────────────────────────────────
? for shortcuts                                         auto · Claude Opus 5.5 · medium`

// The same dialog, live in an agy pane.
const agyTrust = ` Accessing workspace:
 /data00/home/u/agent-workspace
 Do you trust the contents of this project?
 Antigravity CLI requires permission to read, edit, and execute files here.
 > Yes, I trust this folder
   No, exit
   ↑/↓ Navigate · enter Confirm
                                                     auto · Gemini 4 Carbon · high`

func TestScreenAsksQuoted(t *testing.T) {
	if screenAsks(quotedDialog) {
		t.Error("a dialog quoted in the transcript counted as a question")
	}
	if !screenAsks(agyTrust) {
		t.Error("agy's trust dialog not seen")
	}
}

// Claude's AskUserQuestion box: descriptions under the options, and the last
// option set apart by a rule.
const askBox = `───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
 ☐ Color

Which color for the button?

❯ 1. Red
     warm
  2. Blue
     calm
  3. Green
     fresh
  4. Type something.
───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
  5. Chat about this

Enter to select · ↑/↓ to navigate · Esc to cancel`

func TestAskBox(t *testing.T) {
	if !screenAsks(askBox) {
		t.Fatal("not seen as a question")
	}
	a := parseApproval(askBox)
	if len(a.Choices) != 5 || a.Choices[0].Label != "Red" || a.Choices[0].Desc != "warm" || !a.Choices[0].Current ||
		a.Choices[2].Desc != "fresh" || a.Choices[4].Text != "5" || a.Choices[4].Label != "Chat about this" {
		t.Fatalf("choices: %+v", a.Choices)
	}
	if got := strings.Join(a.Choices[0].Keys, ","); got != "enter" {
		t.Fatalf("choice 0 keys: %q", got)
	}
	if got := strings.Join(a.Choices[4].Keys, ","); got != "down,down,down,down,enter" {
		t.Fatalf("choice 4 keys (across descriptions and rule): %q", got)
	}
	if !strings.HasSuffix(a.Question, "Which color for the button?") {
		t.Fatalf("question: %q", a.Question)
	}
}

func TestCodexFolderAccessApproval(t *testing.T) {
	codexFolder := "Folder access\n  /Users/bytedance/agent-workspace/huanyu1w1/collie\n  Trust this folder? Codex can read, edit, and run files here.\n› 1. Trust and continue\n  2. Back to Agent Command Center\n  enter continue · esc back"
	if !screenAsks(codexFolder) {
		t.Fatal("codex folder access dialog not seen as a question")
	}
	a := parseApproval(codexFolder)
	if len(a.Choices) != 2 || !a.Choices[0].Current || a.Choices[0].Text != "1" {
		t.Fatalf("choices: %+v", a.Choices)
	}
	if got := strings.Join(a.Choices[0].Keys, ","); got != "enter" {
		t.Fatalf("choice 1 keys = %q, want enter (bare '1' does not confirm Codex Folder access)", got)
	}
	if got := strings.Join(a.Choices[1].Keys, ","); got != "down,enter" {
		t.Fatalf("choice 2 keys = %q, want down,enter", got)
	}
}

func TestTrimScreenBlankTail(t *testing.T) {
	in := "\x1b[0m\x1b[32mline 1\x1b[0m\r\n\r\nline 3\r\n   \r\n\x1b[0m   \r\n"
	want := "\x1b[0m\x1b[32mline 1\x1b[0m\r\n\r\nline 3"
	if got := trimScreenBlankTail(in); got != want {
		t.Fatalf("trimScreenBlankTail = %q, want %q", got, want)
	}
	bgBar := "top\r\n\x1b[48;5;4m          \x1b[0m\r\n   "
	wantBar := "top\r\n\x1b[48;5;4m          \x1b[0m"
	if got := trimScreenBlankTail(bgBar); got != wantBar {
		t.Fatalf("background bar stripped: got %q, want %q", got, wantBar)
	}
	if got := trimScreenBlankTail("  \r\n\x1b[0m "); got != "" {
		t.Fatalf("all-blank = %q, want empty", got)
	}
}

func TestStreamPaneScreenRecentANSI(t *testing.T) {
	var reads atomic.Int32
	var gotSource, gotFormat string
	var gotLines int
	var gotStrip bool

	h := &Herdr{
		local: &Machine{
			ID: "", Name: "test", Local: true, Online: true,
			conn: &herdrConn{dial: func(ctx context.Context) (net.Conn, error) {
				c1, c2 := net.Pipe()
				go func() {
					defer c2.Close()
					sc := bufio.NewScanner(c2)
					for sc.Scan() {
						var req struct {
							ID     string         `json:"id"`
							Method string         `json:"method"`
							Params map[string]any `json:"params"`
						}
						if json.Unmarshal(sc.Bytes(), &req) != nil {
							return
						}
						var resp map[string]any
						switch req.Method {
						case "pane.layout":
							resp = map[string]any{
								"id": req.ID,
								"result": map[string]any{
									"layout": map[string]any{
										"panes": []map[string]any{
											{"pane_id": "w1:p1", "rect": map[string]any{"width": 132, "height": 42}},
										},
									},
								},
							}
						case "pane.read":
							n := reads.Add(1)
							gotSource, _ = req.Params["source"].(string)
							gotFormat, _ = req.Params["format"].(string)
							if lf, ok := req.Params["lines"].(float64); ok {
								gotLines = int(lf)
							}
							gotStrip, _ = req.Params["strip_ansi"].(bool)
							switch n {
							case 1, 2:
								resp = map[string]any{
									"id": req.ID,
									"result": map[string]any{
										"read": map[string]any{
											"text":      "\x1b[32mfirst\x1b[0m\r\nsecond\r\n   \r\n\x1b[0m  ",
											"revision":  7,
											"truncated": true,
										},
									},
								}
							case 3:
								resp = map[string]any{
									"id": req.ID,
									"result": map[string]any{
										"read": map[string]any{
											"text":      "\x1b[32mfirst\x1b[0m\r\nsecond\r\nthird",
											"revision":  8,
											"truncated": false,
										},
									},
								}
							default:
								resp = map[string]any{
									"id":    req.ID,
									"error": map[string]any{"code": "pane_not_found", "message": "closed"},
								}
							}
						}
						b, _ := json.Marshal(resp)
						c2.Write(append(b, '\n'))
					}
				}()
				return c1, nil
			}},
		},
		remote: map[string]*Machine{},
		names:  map[string]string{},
		named:  map[string]string{},
		runs:   map[string][2]string{},
		asks:   map[string]askCheck{},
	}

	srv := &Server{herdr: h, token: "tok", web: fstest.MapFS{}}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/panes/w1:p1/stream?mode=screen", nil)
	req.Header.Set("Authorization", "Bearer tok")
	res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	type sseEvent struct {
		event string
		data  string
	}
	var events []sseEvent
	sc := bufio.NewScanner(res.Body)
	var cur sseEvent
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			cur.event = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			cur.data = strings.TrimPrefix(line, "data: ")
		} else if line == "" && cur.event != "" {
			events = append(events, cur)
			cur = sseEvent{}
		}
	}

	if gotSource != "recent" || gotFormat != "ansi" || gotLines != screenLines || gotStrip {
		t.Fatalf("pane.read params: source=%q format=%q lines=%d strip_ansi=%v", gotSource, gotFormat, gotLines, gotStrip)
	}
	if len(events) != 3 || events[0].event != "frame" || events[1].event != "frame" || events[2].event != "gone" {
		t.Fatalf("events = %+v, want [frame, frame, gone] (unchanged read #2 must be deduplicated)", events)
	}

	var f1, f2 struct {
		Text      string `json:"text"`
		Cols      int    `json:"cols"`
		Rows      int    `json:"rows"`
		Truncated bool   `json:"truncated"`
		Cursor    bool   `json:"cursor"`
	}
	json.Unmarshal([]byte(events[0].data), &f1)
	json.Unmarshal([]byte(events[1].data), &f2)
	if f1.Text != "\x1b[32mfirst\x1b[0m\r\nsecond\x1b7" || f1.Cols != 132 || f1.Rows != 42 || !f1.Truncated || !f1.Cursor {
		t.Fatalf("frame 1 = %+v", f1)
	}
	if f2.Text != "\x1b[32mfirst\x1b[0m\r\nsecond\r\nthird\x1b7" || f2.Truncated || !f2.Cursor {
		t.Fatalf("frame 2 = %+v", f2)
	}
}
