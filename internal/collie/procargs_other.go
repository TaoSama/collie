//go:build !darwin && !linux

package collie

import "errors"

func procArgs(pid int) ([]string, map[string]string, error) {
	return nil, nil, errors.New("reading a process's environment is not supported here")
}
