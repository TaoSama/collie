package collie

import (
	"bytes"
	"fmt"
	"os"
)

// procArgs reads the command line and environment pid was started with.
func procArgs(pid int) (argv []string, env map[string]string, err error) {
	cmd, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, nil, err
	}
	envb, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return nil, nil, err
	}
	for _, w := range bytes.Split(bytes.TrimSuffix(cmd, []byte{0}), []byte{0}) {
		argv = append(argv, string(w))
	}
	for _, w := range bytes.Split(envb, []byte{0}) {
		if len(w) > 0 {
			env = addEnv(env, string(w))
		}
	}
	return argv, env, nil
}
