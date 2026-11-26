package collie

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The same binary doubles as the tool surface for router and worker sessions:
// `collie reply|spawn|send|report|show|list|timer|…` talk to the local server.

var cliCommands = map[string]func([]string) error{
	"reply": cliReply, "spawn": cliSpawn, "send": cliSend, "report": cliReport,
	"subscribe": cliSubscribe, "stop": cliStop, "answer": cliAnswer, "show": cliShow, "list": cliList,
	"timer": cliTimer, "say": cliSay, "hook": cliHook, "pair": cliPair, "wb": cliWB,
}

func collieDir() string {
	if d := os.Getenv("COLLIE_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".collie")
}

func runCLI(cmd string, args []string) {
	if err := cliCommands[cmd](args); err != nil {
		fmt.Fprintln(os.Stderr, "collie "+cmd+":", err)
		os.Exit(1)
	}
}

func cliCall(method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	return cliDo(method, path, rd, -1, "application/json", 30*time.Second)
}

func cliPostResult(path string, body any) error {
	b, err := cliCall(http.MethodPost, path, body)
	if err == nil {
		printResult(b)
	}
	return err
}

// cliDo makes one request to the hub; size is the body's length (-1 unknown).
func cliDo(method, path string, rd io.Reader, size int64, contentType string, wait time.Duration) ([]byte, error) {
	base, tok, err := cliTarget()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		return nil, err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", contentType)
	resp, err := (&http.Client{Timeout: wait}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("collie server unreachable: %w", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		if json.Unmarshal(out, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s", e.Error)
		}
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return out, nil
}

// cliTarget finds the hub: on a computer joined with `collie node`, its hub
// (with this computer's token); otherwise the hub running on this machine.
func cliTarget() (base, token string, err error) {
	if c, ok := loadNodeConfig(); ok {
		return strings.TrimRight(c.Hub, "/"), c.Token, nil
	}
	dir := collieDir()
	tok, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil {
		return "", "", fmt.Errorf("no token in %s (is the collie server set up?)", dir)
	}
	addr, err := os.ReadFile(filepath.Join(dir, "addr"))
	if err != nil {
		return "", "", fmt.Errorf("collie server is not running (no %s/addr)", dir)
	}
	a := strings.TrimSpace(string(addr))
	if !strings.Contains(a, "://") {
		a = "http://" + a
	}
	return a, strings.TrimSpace(string(tok)), nil
}

// cliPair prints a one-time code: a QR code for a phone, or the command that
// joins another computer.
func cliPair(args []string) error {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	computer := fs.Bool("computer", false, "pair another computer instead of a phone")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	kind := "browser"
	if *computer {
		kind = "machine"
	}
	b, err := cliCall("POST", "/api/pair/new", map[string]any{"kind": kind})
	if err != nil {
		return err
	}
	var r struct {
		Code      string   `json:"code"`
		Addresses []string `json:"addresses"`
	}
	json.Unmarshal(b, &r)
	if len(r.Addresses) == 0 {
		return fmt.Errorf("hub has no address")
	}
	if *computer {
		fmt.Printf("\n  在要加入的电脑上运行（10 分钟内有效，只能用一次）：\n\n    collie node --hub %s --code %s\n\n", r.Addresses[0], r.Code)
		if len(r.Addresses) > 1 {
			fmt.Printf("  连不上的话换一个地址：%s\n\n", strings.Join(r.Addresses[1:], "  "))
		}
		return nil
	}
	u := pairURL(r.Addresses[0], r.Code)
	fmt.Printf("\n  用手机扫码（10 分钟内有效，只能用一次）：\n\n")
	printQR(os.Stdout, u)
	fmt.Printf("\n  %s\n\n", u)
	return nil
}

// textArg joins positional args; "-" (or nothing, with piped stdin) reads stdin.
func textArg(fs *flag.FlagSet) (string, error) {
	t := strings.Join(fs.Args(), " ")
	if t == "-" || (t == "" && !isTerminal(os.Stdin)) {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		t = string(b)
	}
	if strings.TrimSpace(t) == "" {
		return "", fmt.Errorf("text is required")
	}
	return t, nil
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func printResult(b []byte) {
	var m map[string]any
	if json.Unmarshal(b, &m) == nil {
		if w, ok := m["warning"].(string); ok {
			fmt.Println("warning:", w)
		}
		delete(m, "warning")
		out, _ := json.Marshal(m)
		fmt.Println(string(out))
		return
	}
	os.Stdout.Write(b)
}

type flagList []string

func (l *flagList) String() string     { return strings.Join(*l, ", ") }
func (l *flagList) Set(v string) error { *l = append(*l, v); return nil }

func parseReply(args []string, in io.Reader, tty bool) (map[string]any, error) {
	fs := flag.NewFlagSet("reply", flag.ContinueOnError)
	thread := fs.String("thread", mainThread, "thread to reply in")
	re := fs.String("re", "", "the event (#t123) this answers, shown to the human as a quote")
	var opts flagList
	fs.Var(&opts, "opt", "quick-reply choice button for the human (repeatable)")
	check := fs.Bool("check", false, "mark as needing the human's review or action (without --opt)")
	to := fs.String("to", "", "send to an extension's own chat with the human (e.g. wechat), only when they asked for it")
	progress := fs.Bool("progress", false, "a short note while still working, in an extension conversation (no footer)")
	var files flagList
	fs.Var(&files, "file", "a picture or file on this computer to send with it, to an extension conversation (repeatable; over 40 MB it goes as a 24-hour download link)")
	if err := parseArgs(fs, args); err != nil {
		return nil, err
	}
	t := strings.Join(fs.Args(), " ")
	if t == "-" || (t == "" && !tty && in != nil) {
		b, err := io.ReadAll(in)
		if err != nil {
			return nil, err
		}
		t = string(b)
	}
	if strings.TrimSpace(t) == "" {
		return nil, fmt.Errorf("text is required")
	}
	body := map[string]any{"text": t, "thread": *thread, "re": *re}
	if len(opts) > 0 {
		body["options"] = []string(opts)
	}
	if *check {
		body["check"] = true
	}
	if *to != "" {
		body["to"] = *to
	}
	if *progress {
		body["progress"] = true
	}
	if len(files) > 0 {
		blobs, err := readReplyFiles(files)
		if err != nil {
			return nil, err
		}
		body["files"] = blobs
	}
	return body, nil
}

func cliReply(args []string) error {
	body, err := parseReply(args, os.Stdin, isTerminal(os.Stdin))
	if err == flag.ErrHelp {
		return nil
	}
	if err != nil {
		return err
	}
	if files, ok := body["files"].([]map[string]string); ok {
		if err := uploadReplyFiles(files); err != nil {
			return err
		}
	}
	return cliPostResult("/api/super/reply", body)
}

func cliSay(args []string) error {
	fs := flag.NewFlagSet("say", flag.ExitOnError)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	t, err := textArg(fs)
	if err != nil {
		return err
	}
	return cliPostResult("/api/super/say", map[string]any{"text": t})
}

func cliSpawn(args []string) error {
	fs := flag.NewFlagSet("spawn", flag.ExitOnError)
	kind := fs.String("kind", "", "claude | codex | antigravity (default from settings)")
	cwd := fs.String("cwd", "", "working directory (default from settings)")
	thread := fs.String("thread", mainThread, "thread this work belongs to")
	name := fs.String("name", "", "worker name (default kind-N)")
	machine := fs.String("machine", "", "which computer (see `collie list`; default from settings)")
	model := fs.String("model", "", "model to launch with (default from settings for the default kind)")
	effort := fs.String("effort", "", "thinking effort: low | medium | high | xhigh | max ...")
	alias := fs.String("alias", "", "start it through this shell alias on that computer (default from settings for the default kind)")
	noAlias := fs.Bool("no-alias", false, "start the agent's own command, not the default alias")
	sub := fs.Bool("subscribe", false, "route later events of --thread straight to this worker")
	resume := fs.String("resume", "", "a stopped worker to carry on from: its computer, directory, alias and conversation")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	t, err := textArg(fs)
	if err != nil {
		return err
	}
	// The shell here may have turned "~" into this computer's home; the
	// worker's computer reads "~" as its own.
	if home, err := os.UserHomeDir(); err == nil && home != "/" {
		*cwd = tildeIn(home, *cwd)
	}
	body := map[string]any{"kind": *kind, "cwd": *cwd, "name": *name, "task": t, "by": by(), "model": *model, "effort": *effort, "resume": *resume}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "subscribe" {
			body["subscribe"] = *sub
		}
		if f.Name == "machine" {
			body["machine"] = *machine
		}
		if f.Name == "thread" {
			body["thread"] = *thread
		}
		if f.Name == "alias" {
			body["alias"] = *alias
		}
	})
	if *noAlias {
		if _, ok := body["alias"]; ok {
			return fmt.Errorf("--alias and --no-alias don't go together")
		}
		body["alias"] = ""
	}
	return cliPostResult("/api/super/spawn", body)
}

func by() string {
	if os.Getenv("HERDR_PANE_ID") != "" {
		return routerName
	}
	return "cli"
}

func cliSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	thread := fs.String("thread", "", "thread (default: the worker's)")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: collie send <worker> <text>")
	}
	to := fs.Arg(0)
	return cliPostResult("/api/super/send", map[string]any{"to": to, "text": strings.Join(fs.Args()[1:], " "), "thread": *thread})
}

func cliReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	worker := fs.String("worker", "", "worker name (default: detected from the herdr pane)")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	t, err := textArg(fs)
	if err != nil {
		return err
	}
	return cliPostResult("/api/super/report", map[string]any{"worker": *worker, "pane": os.Getenv("HERDR_PANE_ID"), "text": t})
}

func cliSubscribe(args []string) error {
	fs := flag.NewFlagSet("subscribe", flag.ExitOnError)
	off := fs.Bool("off", false, "unsubscribe")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: collie subscribe [--off] <worker> <thread>")
	}
	return cliPostResult("/api/super/subscribe", map[string]any{"worker": fs.Arg(0), "thread": fs.Arg(1), "off": *off})
}

func cliStop(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: collie stop <worker>")
	}
	return cliPostResult("/api/super/stop", map[string]any{"worker": args[0]})
}

func cliAnswer(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: collie answer <worker> <choice number | label words | esc>")
	}
	return cliPostResult("/api/super/answer", map[string]any{"worker": args[0], "choice": strings.Join(args[1:], " ")})
}

func cliShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	before := fs.String("before", "", "for a thread: list the triggers older than this id (e.g. t3985)")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: collie show <trigger-id | thread | worker> [--before ID]")
	}
	path := "/api/super/show/" + url.PathEscape(fs.Arg(0))
	if *before != "" {
		path += "?before=" + url.QueryEscape(*before)
	}
	b, err := cliCall("GET", path, nil)
	if err == nil {
		os.Stdout.Write(b)
	}
	return err
}

func cliList(args []string) error {
	b, err := cliCall("GET", "/api/super/list", nil)
	if err == nil {
		os.Stdout.Write(b)
	}
	return err
}

func cliTimer(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: collie timer add|ls|rm …")
	}
	switch args[0] {
	case "ls", "list":
		b, err := cliCall("GET", "/api/super/timers", nil)
		if err != nil {
			return err
		}
		var r struct{ Timers []*Timer }
		json.Unmarshal(b, &r)
		for _, t := range r.Timers {
			state := "off"
			if t.Enabled {
				state = "next " + t.NextRun.Format("01-02 15:04")
			}
			fmt.Printf("%-5s %-18s %-18s → %s  %s\n", t.ID, t.Spec, state, orStr(t.Target, "router"), clip(t.Text, 70))
		}
		return nil
	case "rm", "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: collie timer rm <id>")
		}
		return cliPostResult("/api/super/timers/"+args[1]+"/delete", nil)
	case "add":
		fs := flag.NewFlagSet("timer add", flag.ExitOnError)
		thread := fs.String("thread", mainThread, "thread the timer fires into")
		to := fs.String("to", "", "worker to wake (default: router)")
		if err := parseArgs(fs, args[1:]); err != nil {
			return err
		}
		if fs.NArg() < 2 {
			return fmt.Errorf(`usage: collie timer add [--thread T] [--to worker] "<every 2h | daily 09:00 | weekdays 09:00 | weekly mon,thu 09:00 | in 30m | at 2026-11-18 09:00>" <text>`)
		}
		return cliPostResult("/api/super/timers", map[string]any{"spec": fs.Arg(0), "text": strings.Join(fs.Args()[1:], " "), "thread": *thread, "target": *to, "owner": by()})
	}
	return fmt.Errorf("unknown timer command %q", args[0])
}

func cliHook(args []string) error {
	fs := flag.NewFlagSet("hook", flag.ExitOnError)
	thread := fs.String("thread", "", "thread id")
	from := fs.String("from", "", "sender")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: collie hook <source> <text>")
	}
	return cliPostResult("/hooks/"+fs.Arg(0), map[string]any{"thread": *thread, "from": *from, "text": strings.Join(fs.Args()[1:], " ")})
}

func orStr(a, b string) string {
	if a == "" {
		return b
	}
	return a
}
