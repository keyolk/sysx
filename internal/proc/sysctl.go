//go:build darwin

package proc

import (
	"encoding/binary"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

func numCPU() int {
	n, err := unix.SysctlUint32("hw.logicalcpu")
	if err != nil || n == 0 {
		return 1
	}
	return int(n)
}

func readSysctls(s *System) error {
	var errs []string
	if v, err := unix.SysctlUint64("hw.memsize"); err == nil {
		s.MemTotal = v
	} else {
		errs = append(errs, "hw.memsize: "+err.Error())
	}
	// struct loadavg { fixpt_t ldavg[3]; long fscale; }
	if b, err := unix.SysctlRaw("vm.loadavg"); err == nil && len(b) >= 24 {
		scale := float64(binary.LittleEndian.Uint64(b[16:24]))
		if scale > 0 {
			for i := range 3 {
				s.Load[i] = float64(binary.LittleEndian.Uint32(b[i*4:])) / scale
			}
		}
	} else if err != nil {
		errs = append(errs, "vm.loadavg: "+err.Error())
	}
	// struct xsw_usage { u_int64_t total, avail, used; ... }
	if b, err := unix.SysctlRaw("vm.swapusage"); err == nil && len(b) >= 24 {
		s.SwapTotal = binary.LittleEndian.Uint64(b[0:8])
		s.SwapUsed = binary.LittleEndian.Uint64(b[16:24])
	} else if err != nil {
		errs = append(errs, "vm.swapusage: "+err.Error())
	}
	if v, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level"); err == nil {
		s.Pressure = Pressure(v)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// readVolumes lists local, user-visible filesystems. On APFS "/" is the
// sealed system snapshot; the space that fills up is the Data volume.
func readVolumes() []Volume {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil || n <= 0 {
		return nil
	}
	buf := make([]unix.Statfs_t, n)
	n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT)
	if err != nil {
		return nil
	}
	var out []Volume
	for _, st := range buf[:n] {
		mnt := unix.ByteSliceToString(st.Mntonname[:])
		if st.Flags&unix.MNT_LOCAL == 0 {
			continue
		}
		if mnt != "/System/Volumes/Data" && !strings.HasPrefix(mnt, "/Volumes/") {
			continue
		}
		bs := uint64(st.Bsize)
		out = append(out, Volume{Mount: mnt, Total: st.Blocks * bs, Free: st.Bavail * bs})
	}
	return out
}
