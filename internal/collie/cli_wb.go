package collie

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
)

const wbUsage = "usage: collie wb ls | collie wb read <module>"

// cliWB is `collie wb`: the workbench modules the human shares with agents.
// The pane tells the hub which agent asks (Collie, one of its workers, or
// another one it refuses).
func cliWB(args []string) error {
	if len(args) == 0 {
		return errors.New(wbUsage)
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Println(wbUsage + "\n\n  ls     the workbench modules the human shares with agents\n  read   a module's data now, as markdown")
		return nil
	}
	pane := url.Values{"pane": {os.Getenv("HERDR_PANE_ID")}}
	switch args[0] {
	case "ls", "list":
		fs := flag.NewFlagSet("wb ls", flag.ExitOnError)
		fs.Usage = func() {
			fmt.Fprintln(fs.Output(), "usage: collie wb ls\n\nLists the workbench modules shared with agents.")
		}
		fs.Parse(args[1:])
		b, err := cliCall("GET", "/api/wb/ls?"+pane.Encode(), nil)
		if err != nil {
			return err
		}
		var r struct {
			Modules []struct {
				ID, Title, Share string
				Sensitive        bool
				SnapAt           string `json:"snap_at"`
			}
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		if len(r.Modules) == 0 {
			fmt.Println("（没有共享给 Agent 的模块）")
		}
		for _, m := range r.Modules {
			line := m.ID + "\t" + m.Title + "\t" + m.Share
			if m.Sensitive {
				line += "\t敏感"
			}
			fmt.Println(line)
		}
		return nil
	case "read":
		fs := flag.NewFlagSet("wb read", flag.ExitOnError)
		fs.Usage = func() {
			fmt.Fprintln(fs.Output(), "usage: collie wb read <module>\n\nPrints the module's data now, as markdown.")
		}
		fs.Parse(args[1:])
		if fs.NArg() != 1 {
			return errors.New(wbUsage)
		}
		b, err := cliCall("GET", "/api/wb/read/"+url.PathEscape(fs.Arg(0))+"?"+pane.Encode(), nil)
		if err != nil {
			return err
		}
		var r struct{ Text string }
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		fmt.Print(r.Text)
		return nil
	}
	return errors.New(wbUsage)
}
