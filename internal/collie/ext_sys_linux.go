package collie

import "syscall"

// extSysProcAttr starts an extension in its own process group, killed with
// collie (extensions/README.md §5).
func extSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
