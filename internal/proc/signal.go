package proc

import (
	"fmt"
	"syscall"
	"time"
)

// Signal sends sig to pid only if pid still belongs to the process that
// started at start; a pid reused since the user chose it is left alone.
func Signal(pid int, start time.Time, sig syscall.Signal) error {
	rt, err := readTask(pid)
	if err != nil {
		return fmt.Errorf("pid %d is gone", pid)
	}
	if rt.start != 0 && !start.IsZero() && Key(pid, time.Unix(rt.start, rt.startUs*1000)) != Key(pid, start) {
		return fmt.Errorf("pid %d now belongs to another process; not signalled", pid)
	}
	if err := syscall.Kill(pid, sig); err != nil {
		if err == syscall.EPERM {
			return fmt.Errorf("kill %d: not permitted (another user's process; run sysx with sudo)", pid)
		}
		return fmt.Errorf("kill %d: %w", pid, err)
	}
	return nil
}
