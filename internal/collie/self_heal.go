package collie

import (
	"errors"
	"fmt"
	"log"
	"strings"
)

// What goes wrong under Collie goes to whoever can deal with it. Collie
// retries what passes by itself (a computer reconnecting, a name in use);
// the Router hears what needs a decision (a worker that failed, a name it
// asked for that was taken); the human hears only what is theirs to fix, or
// what kept failing after the retries, once, in plain words.

// routerFailsToTell is how many Router launches in a row fail before the
// human hears; each later tick retries.
const routerFailsToTell = 3

// routerLaunchFailedLocked counts a failed Router launch and returns what to
// tell the human, or "" while Collie keeps trying by itself.
func (sp *Super) routerLaunchFailedLocked(machine string, err error) string {
	sp.routerFails++
	log.Printf("router: launch failed (%d in a row): %v", sp.routerFails, err)
	missing := strings.Contains(err.Error(), "没装") // see agentMissing: retrying won't help
	if sp.routerFailTold || (!missing && sp.routerFails < routerFailsToTell) {
		return ""
	}
	sp.routerFailTold = true
	if missing {
		return "Collie 起不来：" + err.Error() + "。装上它，或在 自动化 → 设置 → Collie 自己 里换一个 agent"
	}
	if machineOffline(err) {
		return fmt.Sprintf("Collie 所在的电脑 %s 一直没连上 hub，Collie 起不来；连上后会自动启动", machine)
	}
	return fmt.Sprintf("Collie 连着 %d 次没能启动，还在自动重试：%v", sp.routerFails, err)
}

// routerLaunchOKLocked ends a run of failed launches: a Router runs.
func (sp *Super) routerLaunchOKLocked() { sp.routerFails, sp.routerFailTold = 0, false }

func machineOffline(err error) bool {
	var he *HerdrError
	return errors.As(err, &he) && he.Code == "machine_offline"
}

// spareName is the first name-2, name-3, … not taken.
func spareName(name string, taken func(string) bool) string {
	for i := 2; ; i++ {
		suffix := fmt.Sprintf("-%d", i)
		n := strings.TrimRight(name[:min(len(name), maxNameLen-len(suffix))], "-_") + suffix
		if !taken(n) {
			return n
		}
	}
}
