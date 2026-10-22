//go:build darwin

package proc

/*
#include <libproc.h>
#include <sys/proc_info.h>
#include <sys/resource.h>
#include <mach/mach_time.h>
#include <mach/mach.h>
#include <errno.h>
#include <stdlib.h>

static int sysx_taskall(int pid, struct proc_taskallinfo *out) {
	int n = proc_pidinfo(pid, PROC_PIDTASKALLINFO, 0, out, sizeof(*out));
	if (n == sizeof(*out)) return 0;
	return errno ? errno : -1;
}

static int sysx_bsdshort(int pid, struct proc_bsdshortinfo *out) {
	int n = proc_pidinfo(pid, PROC_PIDT_SHORTBSDINFO, 0, out, sizeof(*out));
	if (n == sizeof(*out)) return 0;
	return errno ? errno : -1;
}

static int sysx_rusage(int pid, struct rusage_info_v4 *out) {
	if (proc_pid_rusage(pid, RUSAGE_INFO_V4, (rusage_info_t *)out) == 0) return 0;
	return errno ? errno : -1;
}

static int sysx_cwd(int pid, char *buf, int len) {
	struct proc_vnodepathinfo vpi;
	int n = proc_pidinfo(pid, PROC_PIDVNODEPATHINFO, 0, &vpi, sizeof(vpi));
	if (n != sizeof(vpi)) return errno ? errno : -1;
	strlcpy(buf, vpi.pvi_cdir.vip_path, len);
	return 0;
}

static void sysx_timebase(uint32_t *numer, uint32_t *denom) {
	mach_timebase_info_data_t tb;
	mach_timebase_info(&tb);
	*numer = tb.numer;
	*denom = tb.denom;
}

static int sysx_vm(vm_statistics64_data_t *out, uint64_t *pagesize) {
	mach_msg_type_number_t count = HOST_VM_INFO64_COUNT;
	vm_size_t ps;
	host_page_size(mach_host_self(), &ps);
	*pagesize = ps;
	return host_statistics64(mach_host_self(), HOST_VM_INFO64, (host_info64_t)out, &count);
}

static int sysx_cpu(uint64_t *user, uint64_t *sys, uint64_t *idle, uint64_t *nice) {
	host_cpu_load_info_data_t info;
	mach_msg_type_number_t count = HOST_CPU_LOAD_INFO_COUNT;
	kern_return_t kr = host_statistics(mach_host_self(), HOST_CPU_LOAD_INFO, (host_info_t)&info, &count);
	if (kr != KERN_SUCCESS) return kr;
	*user = info.cpu_ticks[CPU_STATE_USER];
	*sys = info.cpu_ticks[CPU_STATE_SYSTEM];
	*idle = info.cpu_ticks[CPU_STATE_IDLE];
	*nice = info.cpu_ticks[CPU_STATE_NICE];
	return 0;
}
*/
import "C"

import (
	"fmt"
	"syscall"
	"unsafe"
)

// timebase converts mach absolute time units to nanoseconds. On Apple Silicon
// it is 125/3, so CPU times read from libproc are not nanoseconds as-is.
var tbNumer, tbDenom = func() (uint64, uint64) {
	var n, d C.uint32_t
	C.sysx_timebase(&n, &d)
	if d == 0 {
		return 1, 1
	}
	return uint64(n), uint64(d)
}()

func machToNs(v uint64) uint64 { return v * tbNumer / tbDenom }

// listPids returns every pid the kernel reports, including ones we cannot
// read in detail.
func listPids() ([]int, error) {
	n := C.proc_listallpids(nil, 0)
	if n <= 0 {
		return nil, fmt.Errorf("proc_listallpids: count failed")
	}
	// Room for processes started between the two calls.
	buf := make([]C.int, int(n)+64)
	n = C.proc_listallpids(unsafe.Pointer(&buf[0]), C.int(len(buf)*int(unsafe.Sizeof(buf[0]))))
	if n <= 0 {
		return nil, fmt.Errorf("proc_listallpids failed")
	}
	out := make([]int, 0, n)
	for _, p := range buf[:n] {
		out = append(out, int(p))
	}
	return out, nil
}

type rawTask struct {
	pid, ppid, uid, pgid int
	status               int
	comm                 string
	start                int64 // unix seconds
	startUs              int64
	nfiles               int
	rss, vsz             uint64
	cpuNs                uint64
	threads              int
	running              int
	nice                 int
	full                 bool // false: only the short BSD info was readable
}

func readTask(pid int) (rawTask, error) {
	var ti C.struct_proc_taskallinfo
	if rc := C.sysx_taskall(C.int(pid), &ti); rc == 0 {
		b, t := ti.pbsd, ti.ptinfo
		name := C.GoString(&b.pbi_name[0])
		if name == "" {
			name = C.GoString(&b.pbi_comm[0])
		}
		return rawTask{
			pid: int(b.pbi_pid), ppid: int(b.pbi_ppid), uid: int(b.pbi_uid), pgid: int(b.pbi_pgid),
			status: int(b.pbi_status), comm: name,
			start: int64(b.pbi_start_tvsec), startUs: int64(b.pbi_start_tvusec),
			nfiles: int(b.pbi_nfiles), nice: int(b.pbi_nice),
			rss: uint64(t.pti_resident_size), vsz: uint64(t.pti_virtual_size),
			cpuNs:   machToNs(uint64(t.pti_total_user) + uint64(t.pti_total_system)),
			threads: int(t.pti_threadnum), running: int(t.pti_numrunning),
			full: true,
		}, nil
	}
	// Other users' tasks refuse TASKALLINFO without root; the short BSD info
	// still names them and their parent, which the tree needs.
	var si C.struct_proc_bsdshortinfo
	if rc := C.sysx_bsdshort(C.int(pid), &si); rc != 0 {
		return rawTask{}, syscall.Errno(rc)
	}
	return rawTask{
		pid: int(si.pbsi_pid), ppid: int(si.pbsi_ppid), uid: int(si.pbsi_uid), pgid: int(si.pbsi_pgid),
		status: int(si.pbsi_status), comm: C.GoString(&si.pbsi_comm[0]),
	}, nil
}

type rawUsage struct {
	diskRead, diskWrite uint64
	footprint           uint64
	start               uint64 // mach absolute time; disambiguates pid reuse
}

func readUsage(pid int) (rawUsage, error) {
	var ri C.struct_rusage_info_v4
	if rc := C.sysx_rusage(C.int(pid), &ri); rc != 0 {
		return rawUsage{}, syscall.Errno(rc)
	}
	return rawUsage{
		diskRead: uint64(ri.ri_diskio_bytesread), diskWrite: uint64(ri.ri_diskio_byteswritten),
		footprint: uint64(ri.ri_phys_footprint), start: uint64(ri.ri_proc_start_abstime),
	}, nil
}

// Path is the executable path, or "" when the process is not readable.
func Path(pid int) string {
	buf := make([]byte, C.PROC_PIDPATHINFO_MAXSIZE)
	n := C.proc_pidpath(C.int(pid), unsafe.Pointer(&buf[0]), C.uint32_t(len(buf)))
	if n <= 0 {
		return ""
	}
	return string(buf[:n])
}

// Cwd is the working directory, or "" when the process is not readable.
func Cwd(pid int) string {
	buf := make([]byte, 1024)
	if C.sysx_cwd(C.int(pid), (*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf))) != 0 {
		return ""
	}
	return C.GoString((*C.char)(unsafe.Pointer(&buf[0])))
}

type rawVM struct {
	pageSize                                        uint64
	free, active, inactive, wired, compressed, spec uint64
	purgeable, external, internal                   uint64
	swapins, swapouts, pageins, pageouts            uint64
}

func readVM() (rawVM, error) {
	var vm C.vm_statistics64_data_t
	var ps C.uint64_t
	if kr := C.sysx_vm(&vm, &ps); kr != 0 {
		return rawVM{}, fmt.Errorf("host_statistics64: kern_return %d", int(kr))
	}
	return rawVM{
		pageSize: uint64(ps),
		free:     uint64(vm.free_count), active: uint64(vm.active_count), inactive: uint64(vm.inactive_count),
		wired: uint64(vm.wire_count), compressed: uint64(vm.compressor_page_count), spec: uint64(vm.speculative_count),
		purgeable: uint64(vm.purgeable_count), external: uint64(vm.external_page_count), internal: uint64(vm.internal_page_count),
		swapins: uint64(vm.swapins), swapouts: uint64(vm.swapouts), pageins: uint64(vm.pageins), pageouts: uint64(vm.pageouts),
	}, nil
}

type rawCPU struct{ user, sys, idle, nice uint64 }

func readCPU() (rawCPU, error) {
	var u, s, i, n C.uint64_t
	if kr := C.sysx_cpu(&u, &s, &i, &n); kr != 0 {
		return rawCPU{}, fmt.Errorf("host_statistics: kern_return %d", int(kr))
	}
	return rawCPU{uint64(u), uint64(s), uint64(i), uint64(n)}, nil
}
