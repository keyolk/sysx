package proc

import (
	"context"
	"fmt"
	"os/user"
	"strconv"
	"sync"
	"time"
)

type counters struct {
	cpuNs   uint64
	diskR   uint64
	diskW   uint64
	netIn   uint64
	netOut  uint64
	hasDisk bool
}

// Collector samples the machine. It is not safe for concurrent use: one
// goroutine (the UI's sampling command) owns it.
type Collector struct {
	prev    map[string]counters
	prevCPU rawCPU
	prevVM  rawVM
	prevAt  time.Time
	users   map[int]string
	ncpu    int
	nettop  bool // false after nettop failed to start: stop trying
}

// NewCollector returns a collector; its first Sample has no rates.
func NewCollector() *Collector {
	return &Collector{prev: map[string]counters{}, users: map[int]string{}, ncpu: numCPU(), nettop: true}
}

func (c *Collector) userName(uid int) string {
	if n, ok := c.users[uid]; ok {
		return n
	}
	n := strconv.Itoa(uid)
	if u, err := user.LookupId(n); err == nil {
		n = u.Username
	}
	c.users[uid] = n
	return n
}

// Sample reads every process and the system totals.
func (c *Collector) Sample(ctx context.Context) (Snapshot, error) {
	now := time.Now()
	snap := Snapshot{At: now}
	if !c.prevAt.IsZero() {
		snap.Interval = now.Sub(c.prevAt)
	}
	secs := snap.Interval.Seconds()

	// nettop takes ~300ms; run it beside the libproc walk.
	var (
		wg     sync.WaitGroup
		net    map[int]netBytes
		netErr error
	)
	if c.nettop {
		wg.Add(1)
		go func() {
			defer wg.Done()
			net, netErr = readNettop(ctx)
		}()
	}

	pids, err := listPids()
	if err != nil {
		return snap, err
	}
	next := make(map[string]counters, len(pids))
	procs := make([]Proc, 0, len(pids))
	var partial []int
	for _, pid := range pids {
		rt, err := readTask(pid)
		if err != nil {
			continue // exited between the list and the read
		}
		p := Proc{
			PID: rt.pid, PPID: rt.ppid, UID: rt.uid, PGID: rt.pgid,
			User: c.userName(rt.uid), Name: rt.comm, State: stateOf(rt.status), Nice: rt.nice,
			RSS: rt.rss, VSZ: rt.vsz, Threads: rt.threads,
			CPUTime: time.Duration(rt.cpuNs),
			Partial: !rt.full,
		}
		if rt.start != 0 {
			p.Start = time.Unix(rt.start, rt.startUs*1000)
		}
		var cn counters
		if rt.full {
			cn.cpuNs = rt.cpuNs
			if u, err := readUsage(pid); err == nil {
				cn.diskR, cn.diskW, cn.hasDisk = u.diskRead, u.diskWrite, true
				p.Footprint = u.footprint
				p.DiskTotal = u.diskRead + u.diskWrite
			}
		} else {
			partial = append(partial, pid)
		}
		if p.Footprint == 0 {
			p.Footprint = p.RSS
		}
		procs = append(procs, p)
		next[p.Key()] = cn
	}

	// Fill what the kernel refused from ps, which is setuid root.
	if len(partial) > 0 {
		if fill, err := readPS(ctx, partial); err != nil {
			snap.Warnings = append(snap.Warnings, "ps: "+err.Error())
		} else {
			for i := range procs {
				if f, ok := fill[procs[i].PID]; ok && procs[i].Partial {
					procs[i].RSS, procs[i].Footprint, procs[i].CPUTime = f.rss, f.rss, f.cpu
					cn := next[procs[i].Key()]
					cn.cpuNs = uint64(f.cpu)
					next[procs[i].Key()] = cn
				}
			}
		}
	}

	wg.Wait()
	if netErr != nil {
		snap.Warnings = append(snap.Warnings, "nettop: "+netErr.Error())
		if isNotFound(netErr) {
			c.nettop = false
		}
	}
	for i := range procs {
		p := &procs[i]
		cn := next[p.Key()]
		if nb, ok := net[p.PID]; ok {
			cn.netIn, cn.netOut = nb.in, nb.out
			p.NetTotal = nb.in + nb.out
			next[p.Key()] = cn
		}
		old, seen := c.prev[p.Key()]
		if !seen || secs <= 0 {
			continue
		}
		p.CPU = rate(cn.cpuNs, old.cpuNs, secs) / 1e9 * 100
		if cn.hasDisk && old.hasDisk {
			p.DiskRead = rate(cn.diskR, old.diskR, secs)
			p.DiskWrite = rate(cn.diskW, old.diskW, secs)
		}
		p.NetIn = rate(cn.netIn, old.netIn, secs)
		p.NetOut = rate(cn.netOut, old.netOut, secs)
		snap.Sys.DiskRead += p.DiskRead
		snap.Sys.DiskWrite += p.DiskWrite
		snap.Sys.NetIn += p.NetIn
		snap.Sys.NetOut += p.NetOut
	}

	if err := c.system(&snap.Sys, secs); err != nil {
		snap.Warnings = append(snap.Warnings, err.Error())
	}
	c.prev, c.prevAt = next, now
	snap.Procs = procs
	return snap, nil
}

// rate is a per-second delta that treats a counter going backwards (a
// reset) as no activity rather than a huge unsigned wrap.
func rate(cur, old uint64, secs float64) float64 {
	if cur < old {
		return 0
	}
	return float64(cur-old) / secs
}

func (c *Collector) system(s *System, secs float64) error {
	s.NCPU = c.ncpu
	var errs []error
	if cpu, err := readCPU(); err != nil {
		errs = append(errs, err)
	} else {
		if !c.prevAt.IsZero() {
			du, ds := float64(cpu.user+cpu.nice-c.prevCPU.user-c.prevCPU.nice), float64(cpu.sys-c.prevCPU.sys)
			di := float64(cpu.idle - c.prevCPU.idle)
			if tot := du + ds + di; tot > 0 {
				s.CPUUser, s.CPUSys, s.CPUIdle = du/tot*100, ds/tot*100, di/tot*100
			}
		}
		c.prevCPU = cpu
	}
	if vm, err := readVM(); err != nil {
		errs = append(errs, err)
	} else {
		ps := vm.pageSize
		s.MemApp = (vm.internal - min(vm.purgeable, vm.internal)) * ps
		s.MemWired = vm.wired * ps
		s.MemCompressed = vm.compressed * ps
		s.MemCached = (vm.external + vm.purgeable) * ps
		s.MemUsed = s.MemApp + s.MemWired + s.MemCompressed
		if secs > 0 && c.prevVM.pageSize != 0 {
			s.SwapIns = rate(vm.swapins, c.prevVM.swapins, secs)
			s.SwapOuts = rate(vm.swapouts, c.prevVM.swapouts, secs)
		}
		c.prevVM = vm
	}
	if err := readSysctls(s); err != nil {
		errs = append(errs, err)
	}
	s.Volumes = readVolumes()
	if len(errs) > 0 {
		return fmt.Errorf("system: %v", errs)
	}
	return nil
}
