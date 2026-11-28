package collie

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const taskCLIUsage = "usage: collie task list | <start|progress|review|wait|resolve|close> TASK [flags]"

func cliTask(args []string) error {
	root := flag.NewFlagSet("task", flag.ContinueOnError)
	root.Usage = func() { fmt.Fprintln(root.Output(), taskCLIUsage) }
	if err := root.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	action := root.Arg(0)
	switch action {
	case "list":
		if root.NArg() != 1 {
			return fmt.Errorf("usage: collie task list")
		}
		b, err := cliCall(http.MethodGet, "/api/super/task", nil)
		if err == nil {
			printResult(b)
		}
		return err
	case "start", "progress", "review", "wait", "resolve", "close":
	default:
		return errors.New(taskCLIUsage)
	}

	update := TaskUpdate{Action: action}
	fs := flag.NewFlagSet("task "+action, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: collie task %s TASK [flags]\n", action)
		fs.PrintDefaults()
	}
	fs.Uint64Var(&update.Version, "version", 0, "task version to update")
	if action != "start" {
		fs.StringVar(&update.Detail, "detail", "", "progress evidence, review finding, decision, or closure reason")
	}
	var at string
	if action != "close" {
		fs.StringVar(&at, "at", "", "next check or reminder (e.g. 'in 45m' or an RFC3339 timestamp)")
	}
	switch action {
	case "start", "progress", "review", "resolve":
		fs.StringVar(&update.Worker, "worker", "", "name of the worker actually assigned to the task")
	}
	switch action {
	case "start":
		fs.BoolVar(&update.Long, "long", false, "use the long-task check schedule")
	case "review":
		fs.BoolVar(&update.Activity, "activity", false, "confirmed ongoing activity with no new verified result")
	case "wait":
		fs.StringVar(&update.Decision, "decision", "", "ID of the open decision (entry returned by collie reply)")
	}
	if err := parseArgs(fs, root.Args()[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		return fmt.Errorf("usage: collie task %s TASK [flags]", action)
	}
	update.Path = strings.TrimSpace(fs.Arg(0))
	if !strings.HasPrefix(update.Path, "tasks/") {
		update.Path = "tasks/" + update.Path
	}
	if !strings.HasSuffix(update.Path, ".md") {
		update.Path += ".md"
	}
	if at != "" {
		next, err := taskCLIAt(at, time.Now())
		if err != nil {
			return err
		}
		update.At = next
	}
	return cliPostResult("/api/super/task", update)
}

func taskCLIAt(at string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, at); err == nil {
		return t, nil
	}
	if parts := strings.Fields(at); len(parts) == 2 && parts[0] == "in" {
		return parseSpec(at, now)
	}
	return time.Time{}, fmt.Errorf("--at needs an RFC3339 timestamp or a duration like 'in 45m'")
}
