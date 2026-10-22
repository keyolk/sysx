//go:build darwin

package proc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// Args is a process's argv and environment.
type Args struct {
	Exe  string
	Argv []string
	Env  []string
}

// ReadArgs reads KERN_PROCARGS2. The kernel refuses other users' processes
// without root, and gives an empty environment for some hardened binaries.
func ReadArgs(pid int) (Args, error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EPERM) {
			return Args{}, fmt.Errorf("no access to args/env (another user's process; run sysx with sudo)")
		}
		return Args{}, err
	}
	return parseProcArgs(b)
}

// parseProcArgs reads: int32 argc, exec path, NUL padding, argc strings,
// then environment strings until an empty one or the end.
func parseProcArgs(b []byte) (Args, error) {
	if len(b) < 4 {
		return Args{}, fmt.Errorf("procargs: short buffer")
	}
	argc := int(binary.LittleEndian.Uint32(b[:4]))
	b = b[4:]
	var a Args
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return a, fmt.Errorf("procargs: no exec path")
	}
	a.Exe = string(b[:i])
	b = b[i:]
	for len(b) > 0 && b[0] == 0 {
		b = b[1:]
	}
	next := func() (string, bool) {
		if len(b) == 0 {
			return "", false
		}
		j := bytes.IndexByte(b, 0)
		if j < 0 {
			s := string(b)
			b = nil
			return s, true
		}
		s := string(b[:j])
		b = b[j+1:]
		return s, true
	}
	for range argc {
		s, ok := next()
		if !ok {
			break
		}
		a.Argv = append(a.Argv, s)
	}
	for {
		s, ok := next()
		if !ok || s == "" {
			break
		}
		a.Env = append(a.Env, s)
	}
	return a, nil
}
