package collie

import (
	"bytes"
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// procArgs reads the command line and environment pid was started with.
// KERN_PROCARGS2 is argc, the executable's path, padding, then argv and the
// environment, each string NUL-terminated.
func procArgs(pid int) (argv []string, env map[string]string, err error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, nil, err
	}
	if len(b) < 4 {
		return nil, nil, errShortProcArgs
	}
	argc := int(binary.LittleEndian.Uint32(b))
	b = b[4:]
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return nil, nil, errShortProcArgs
	}
	b = b[i:] // past the executable's path
	words := bytes.Split(bytes.TrimLeft(b, "\x00"), []byte{0})
	for i, w := range words {
		if i < argc {
			argv = append(argv, string(w))
			continue
		}
		if len(w) == 0 {
			break
		}
		env = addEnv(env, string(w))
	}
	return argv, env, nil
}
