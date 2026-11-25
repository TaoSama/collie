//go:build !linux

package collie

import "syscall"

// extSysProcAttr starts an extension in its own process group. Without
// Pdeathsig it ends on its stdin closing, which happens when collie goes.
func extSysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
