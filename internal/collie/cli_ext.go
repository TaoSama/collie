package collie

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// `collie ext list` and `collie ext do <ext> <action> [--arg k=v]... [--json '{…}']`:
// an agent calling an extension's actions (extensions/README.md §3.6).

func init() { cliCommands["ext"] = cliExt }

const extUsage = `usage: collie ext list
       collie ext do <extension> <action> [--arg key=value]... [--json '{"key": …}'] [--raw]`

func cliExt(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", extUsage)
	}
	switch args[0] {
	case "list", "ls":
		return cliExtList()
	case "do":
		return cliExtDo(args[1:])
	case "-h", "--help", "help":
		fmt.Println(extUsage)
		return nil
	}
	return fmt.Errorf("%s", extUsage)
}

func cliExtList() error {
	b, err := cliCall("GET", "/api/ext-actions", nil)
	if err != nil {
		return err
	}
	var out struct {
		Extensions []actionView `json:"extensions"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return err
	}
	if len(out.Extensions) == 0 {
		fmt.Println("no extension with actions is on (自动化 → 接入)")
		return nil
	}
	for _, e := range out.Extensions {
		state := ""
		if !e.Running {
			state = " (not running)"
		}
		fmt.Printf("%s — %s, on %s%s\n", e.Ext, e.Name, e.Machine, state)
		for _, a := range e.Actions {
			var tags []string
			if !a.Readonly {
				tags = append(tags, "writes")
				if e.NoWrites {
					tags = append(tags, "turned off")
				}
			}
			if a.Untrusted {
				tags = append(tags, "third-party content")
			}
			if a.Slow {
				tags = append(tags, "slow")
			}
			tag := ""
			if len(tags) > 0 {
				tag = " [" + strings.Join(tags, ", ") + "]"
			}
			fmt.Printf("  %s%s — %s\n", a.Name, tag, a.Description)
			for k, p := range a.Params {
				req := ""
				if contains(a.Required, k) {
					req = ", required"
				}
				fmt.Printf("      --arg %s=<%s%s%s>  %s\n", k, p.Type, choices(p), req, p.Title)
			}
		}
	}
	return nil
}

func cliExtDo(args []string) error {
	fs := flag.NewFlagSet("ext do", flag.ContinueOnError)
	var kv flagList
	fs.Var(&kv, "arg", "a param, key=value (repeatable)")
	raw := fs.String("json", "", "params as a JSON object")
	asJSON := fs.Bool("raw", false, "print the whole result as JSON")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) != 2 {
		return fmt.Errorf("%s", extUsage)
	}
	params := map[string]any{}
	if *raw != "" {
		if err := json.Unmarshal([]byte(*raw), &params); err != nil {
			return fmt.Errorf("--json: %v", err)
		}
	}
	for _, a := range kv {
		k, v, ok := strings.Cut(a, "=")
		if !ok || k == "" {
			return fmt.Errorf("--arg wants key=value, got %q", a)
		}
		params[k] = v // a string: the hub turns it into the param's type
	}
	b, err := cliCall("POST", "/api/ext/"+url.PathEscape(pos[0])+"/do", map[string]any{"action": pos[1], "args": params})
	if err != nil {
		return err
	}
	if *asJSON {
		_, err = os.Stdout.Write(append(b, '\n'))
		return err
	}
	text, err := formatAction(b)
	if err != nil {
		return err
	}
	fmt.Println(text)
	return nil
}

// replyFileMost bounds a file sent with `collie reply --file`.
const replyFileMost = 50 << 20

// readReplyFiles reads files for `collie reply --file`, on the computer the
// CLI runs on.
func readReplyFiles(paths []string) ([]map[string]string, error) {
	var out []map[string]string
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if fi.IsDir() || fi.Size() > replyFileMost {
			return nil, fmt.Errorf("%s: not a file, or larger than %d MB", p, replyFileMost>>20)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		mime := http.DetectContentType(b)
		if i := strings.IndexByte(mime, ';'); i > 0 {
			mime = mime[:i]
		}
		out = append(out, map[string]string{"name": filepath.Base(p), "mime": mime, "data": base64.StdEncoding.EncodeToString(b)})
	}
	return out, nil
}
