// Package outlier decides which processes deserve attention and says why.
//
// Every rule is a plain threshold on a value the user can see in the list or
// the detail pane, so a flag is never a mystery number: the reason text names
// the value and the window. Rules look at a short history (sustained use, not
// one spiky sample) and at groups (one fan-out of 40 small processes is an
// outlier even when no single member is).
package outlier

import (
	"fmt"
	"sort"
	"time"

	"github.com/keyolk/sysx/internal/proc"
)

// Sev is how loudly a finding is shown.
type Sev int

const (
	None Sev = iota
	Note
	Warn
	Alert
)

// Dim is the perspective a finding comes from; the list shows one column
// letter per dimension.
type Dim string

const (
	CPU    Dim = "cpu"
	Mem    Dim = "mem"
	Disk   Dim = "disk"
	Net    Dim = "net"
	Thread Dim = "thr"
	Group  Dim = "group" // fan-out and process-tree totals
	State  Dim = "state" // zombie, stopped, orphaned
)

// Dims is the column order.
var Dims = []Dim{CPU, Mem, Disk, Net, Thread, Group, State}

// Finding is one reason a process is flagged.
type Finding struct {
	Dim  Dim
	Sev  Sev
	Text string
	// Tag is a short word for the list's tag column ("leak", "×23",
	// "orph"); empty when the metric cell's color already says it.
	Tag string
}

// Report is the verdict for one process.
type Report struct {
	Findings []Finding
	Sev      Sev     // the loudest finding
	Score    float64 // sort key: severity first, then magnitude
}

// SevOf is the loudest finding in dimension d.
func (r Report) SevOf(d Dim) Sev {
	s := None
	for _, f := range r.Findings {
		if f.Dim == d && f.Sev > s {
			s = f.Sev
		}
	}
	return s
}

// Thresholds are the rule constants, in one place.
type Thresholds struct {
	Window time.Duration // sustained-use window

	CPUWarn, CPUAlert float64 // mean percent of one core over Window
	CPUSpike          float64 // a single sample this high is a note

	MemWarnFrac, MemAlertFrac float64 // footprint as a fraction of RAM
	GrowWindow                time.Duration
	GrowWarn, GrowAlert       uint64 // footprint growth over GrowWindow

	DiskWarn, DiskAlert float64 // bytes/s, mean over Window
	NetWarn, NetAlert   float64

	ThreadWarn, ThreadAlert int

	FanCount         int     // this many same-name processes of one user...
	FanCPU           float64 // ...using this much CPU together, or
	FanMemFrac       float64 // this fraction of RAM together
	FanCountAlone    int     // this many is flagged whatever they use
	TreeCPU          float64 // a subtree using this much CPU
	ZombieParentWarn int
	OrphanCPU        float64 // a spinning orphan
}

// Defaults suit a laptop or workstation.
func Defaults() Thresholds {
	return Thresholds{
		Window:  15 * time.Second,
		CPUWarn: 40, CPUAlert: 90, CPUSpike: 150,
		MemWarnFrac: 0.04, MemAlertFrac: 0.10,
		GrowWindow: 2 * time.Minute, GrowWarn: 300 << 20, GrowAlert: 1 << 30,
		DiskWarn: 20 << 20, DiskAlert: 100 << 20,
		NetWarn: 2 << 20, NetAlert: 20 << 20,
		ThreadWarn: 300, ThreadAlert: 1000,
		FanCount: 8, FanCPU: 200, FanMemFrac: 0.15, FanCountAlone: 40,
		TreeCPU:          300,
		ZombieParentWarn: 5,
		OrphanCPU:        30,
	}
}

type point struct {
	at        time.Time
	cpu       float64
	footprint uint64
	disk, net float64
}

// History keeps recent samples per process key. It is a plain value owned by
// the UI model's update path.
type History struct {
	series map[string][]point
	keep   time.Duration
}

// NewHistory keeps keep worth of samples per process.
func NewHistory(keep time.Duration) History {
	return History{series: map[string][]point{}, keep: keep}
}

// Add records a snapshot and forgets processes that are gone. It returns a
// new History; the old one is not modified (its slices are not appended to
// in place).
func (h History) Add(s proc.Snapshot) History {
	next := make(map[string][]point, len(s.Procs))
	cut := s.At.Add(-h.keep)
	for _, p := range s.Procs {
		old := h.series[p.Key()]
		i := 0
		for i < len(old) && old[i].at.Before(cut) {
			i++
		}
		pts := make([]point, 0, len(old)-i+1)
		pts = append(pts, old[i:]...)
		next[p.Key()] = append(pts, point{s.At, p.CPU, p.Footprint, p.DiskRead + p.DiskWrite, p.NetIn + p.NetOut})
	}
	return History{series: next, keep: h.keep}
}

// Series returns the recorded values of one metric for a key, oldest first.
func (h History) Series(key string, metric Dim) []float64 {
	pts := h.series[key]
	out := make([]float64, len(pts))
	for i, p := range pts {
		switch metric {
		case CPU:
			out[i] = p.cpu
		case Mem:
			out[i] = float64(p.footprint)
		case Disk:
			out[i] = p.disk
		case Net:
			out[i] = p.net
		}
	}
	return out
}

// mean averages the metric over the last w; ok is false until the history
// covers most of the window, so a process that just started is not judged on
// one sample.
func (h History) mean(key string, w time.Duration, now time.Time, f func(point) float64) (float64, bool) {
	pts := h.series[key]
	if len(pts) < 2 {
		return 0, false
	}
	cut := now.Add(-w)
	var sum float64
	n := 0
	for i := len(pts) - 1; i >= 0 && !pts[i].at.Before(cut); i-- {
		sum += f(pts[i])
		n++
	}
	// The first sample has no rate; require two thirds of the window.
	if n < 2 || now.Sub(pts[len(pts)-n].at) < w*2/3 {
		return 0, false
	}
	return sum / float64(n), true
}

// sustainedFor is how long the metric has stayed at or above v, counting back
// from the newest sample.
func (h History) sustainedFor(key string, v float64, f func(point) float64) time.Duration {
	pts := h.series[key]
	i := len(pts) - 1
	for i >= 0 && f(pts[i]) >= v {
		i--
	}
	if i == len(pts)-1 {
		return 0
	}
	return pts[len(pts)-1].at.Sub(pts[i+1].at)
}

// growth is the footprint change over w and whether it rose steadily (at
// least 80% of steps non-negative), the shape of a leak rather than a burst.
func (h History) growth(key string, w time.Duration, now time.Time) (delta int64, span time.Duration, steady bool) {
	pts := h.series[key]
	cut := now.Add(-w)
	i := 0
	for i < len(pts) && pts[i].at.Before(cut) {
		i++
	}
	pts = pts[i:]
	if len(pts) < 4 {
		return 0, 0, false
	}
	up := 0
	for j := 1; j < len(pts); j++ {
		if pts[j].footprint >= pts[j-1].footprint {
			up++
		}
	}
	first, last := pts[0], pts[len(pts)-1]
	return int64(last.footprint) - int64(first.footprint), last.at.Sub(first.at), up*5 >= (len(pts)-1)*4
}

// Analyze judges every process in s against the history (which must already
// include s).
func Analyze(s proc.Snapshot, h History, t Thresholds) map[string]Report {
	out := make(map[string]Report, len(s.Procs))
	add := func(key string, f Finding) {
		r := out[key]
		r.Findings = append(r.Findings, f)
		out[key] = r
	}
	byPID := make(map[int]*proc.Proc, len(s.Procs))
	for i := range s.Procs {
		byPID[s.Procs[i].PID] = &s.Procs[i]
	}
	memTotal := float64(s.Sys.MemTotal)
	now := s.At
	cpuOf := func(p point) float64 { return p.cpu }

	zombiesOf := map[int]int{}
	for _, p := range s.Procs {
		k := p.Key()
		if avg, ok := h.mean(k, t.Window, now, cpuOf); ok {
			switch {
			case avg >= t.CPUAlert:
				add(k, Finding{CPU, Alert, fmt.Sprintf("CPU %.0f%% on average over %s", avg, dur(t.Window)), ""})
			case avg >= t.CPUWarn:
				add(k, Finding{CPU, Warn, fmt.Sprintf("CPU %.0f%% on average over %s", avg, dur(t.Window)), ""})
			case p.CPU >= t.CPUSpike:
				add(k, Finding{CPU, Note, fmt.Sprintf("CPU spike %.0f%% (average %.0f%%)", p.CPU, avg), ""})
			}
		}
		if memTotal > 0 {
			frac := float64(p.Footprint) / memTotal
			switch {
			case frac >= t.MemAlertFrac:
				add(k, Finding{Mem, Alert, fmt.Sprintf("memory %s, %.0f%% of RAM", Bytes(p.Footprint), frac*100), ""})
			case frac >= t.MemWarnFrac:
				add(k, Finding{Mem, Warn, fmt.Sprintf("memory %s, %.1f%% of RAM", Bytes(p.Footprint), frac*100), ""})
			}
		}
		if d, span, steady := h.growth(k, t.GrowWindow, now); steady && d > 0 && span >= t.GrowWindow/2 {
			switch {
			case uint64(d) >= t.GrowAlert:
				add(k, Finding{Mem, Alert, fmt.Sprintf("memory grew +%s in %s, steadily (leak?)", Bytes(uint64(d)), dur(span)), "leak"})
			case uint64(d) >= t.GrowWarn:
				add(k, Finding{Mem, Warn, fmt.Sprintf("memory grew +%s in %s, steadily", Bytes(uint64(d)), dur(span)), "grow"})
			}
		}
		if avg, ok := h.mean(k, t.Window, now, func(p point) float64 { return p.disk }); ok {
			switch {
			case avg >= t.DiskAlert:
				add(k, Finding{Disk, Alert, fmt.Sprintf("disk I/O %s/s on average over %s", Bytes(uint64(avg)), dur(t.Window)), ""})
			case avg >= t.DiskWarn:
				add(k, Finding{Disk, Warn, fmt.Sprintf("disk I/O %s/s on average over %s", Bytes(uint64(avg)), dur(t.Window)), ""})
			}
		}
		if avg, ok := h.mean(k, t.Window, now, func(p point) float64 { return p.net }); ok {
			switch {
			case avg >= t.NetAlert:
				add(k, Finding{Net, Alert, fmt.Sprintf("network %s/s on average over %s", Bytes(uint64(avg)), dur(t.Window)), ""})
			case avg >= t.NetWarn:
				add(k, Finding{Net, Warn, fmt.Sprintf("network %s/s on average over %s", Bytes(uint64(avg)), dur(t.Window)), ""})
			}
		}
		switch {
		case p.Threads >= t.ThreadAlert:
			add(k, Finding{Thread, Alert, fmt.Sprintf("%d threads", p.Threads), "thr"})
		case p.Threads >= t.ThreadWarn:
			add(k, Finding{Thread, Warn, fmt.Sprintf("%d threads", p.Threads), "thr"})
		}
		switch p.State {
		case proc.StateZombie:
			zombiesOf[p.PPID]++
			add(k, Finding{State, Note, fmt.Sprintf("zombie: parent %d has not reaped it", p.PPID), "zomb"})
		case proc.StateStop:
			add(k, Finding{State, Note, "stopped (SIGSTOP or job control)", "stop"})
		}
		// An app started by launchd also has parent 1, but leads its own
		// process group. A process whose group leader is gone was left
		// behind by a shell or a tool that exited.
		if p.PPID == 1 && p.PGID != p.PID && p.PGID > 1 && byPID[p.PGID] == nil {
			if d := h.sustainedFor(k, t.OrphanCPU, cpuOf); d >= t.Window {
				add(k, Finding{State, Alert, fmt.Sprintf("orphan spinning: its group leader %d exited, CPU ≥%.0f%% for %s", p.PGID, t.OrphanCPU, dur(d)), "orph"})
			} else {
				add(k, Finding{State, Note, fmt.Sprintf("orphan: its process group leader %d exited", p.PGID), "orph"})
			}
		}
	}
	for ppid, n := range zombiesOf {
		if p := byPID[ppid]; p != nil && n >= t.ZombieParentWarn {
			add(p.Key(), Finding{State, Warn, fmt.Sprintf("%d zombie children not reaped", n), "reap"})
		}
	}

	fanOut(s, t, add)
	treeTotals(s, byPID, t, add)

	byKey := make(map[string]*proc.Proc, len(s.Procs))
	for i := range s.Procs {
		byKey[s.Procs[i].Key()] = &s.Procs[i]
	}
	for k, r := range out {
		mag := 0.0
		if p := byKey[k]; p != nil {
			mag = p.CPU + float64(p.Footprint)/float64(1<<30)*10
		}
		own, group := None, None
		for _, f := range r.Findings {
			if f.Sev > r.Sev {
				r.Sev = f.Sev
			}
			if f.Dim == Group {
				group = max(group, f.Sev)
			} else {
				own = max(own, f.Sev)
			}
		}
		sort.SliceStable(r.Findings, func(i, j int) bool { return r.Findings[i].Sev > r.Findings[j].Sev })
		// Severity first; at the same severity a process abnormal on its own
		// ranks above the leader of an abnormal group.
		r.Score = float64(r.Sev)*1e6 + float64(len(r.Findings))*1e4 + mag
		if own >= group {
			r.Score += 5e5
		}
		out[k] = r
	}
	return out
}

// fanOut flags many copies of one program run by one user.
func fanOut(s proc.Snapshot, t Thresholds, add func(string, Finding)) {
	type agg struct {
		keys  []string
		cpu   float64
		mem   uint64
		lead  string // the heaviest member carries the group's severity
		leadW float64
	}
	groups := map[string]*agg{}
	for _, p := range s.Procs {
		if p.PID <= 1 {
			continue
		}
		g := p.User + "\x00" + p.Name
		a := groups[g]
		if a == nil {
			a = &agg{}
			groups[g] = a
		}
		a.keys = append(a.keys, p.Key())
		a.cpu += p.CPU
		a.mem += p.Footprint
		if w := p.CPU + float64(p.Footprint)/float64(1<<30)*25; a.lead == "" || w > a.leadW {
			a.lead, a.leadW = p.Key(), w
		}
	}
	memTotal := float64(s.Sys.MemTotal)
	machine := float64(max(s.Sys.NCPU, 1)) * 100
	for g, a := range groups {
		n := len(a.keys)
		if n < t.FanCount {
			continue
		}
		memFrac := 0.0
		if memTotal > 0 {
			memFrac = float64(a.mem) / memTotal
		}
		sev := None
		switch {
		case a.cpu >= machine/2 || memFrac >= t.FanMemFrac*2:
			sev = Alert
		case a.cpu >= t.FanCPU || memFrac >= t.FanMemFrac || n >= t.FanCountAlone:
			sev = Warn
		}
		if sev == None {
			continue
		}
		text := fmt.Sprintf("%d × %s together: CPU %.0f%%, memory %s", n, nameOf(g), a.cpu, Bytes(a.mem))
		// One row speaks for the group; the rest get a note, so forty copies
		// of node do not push a single runaway process off the screen. The
		// tag stays on every member, so filtering by it finds them all.
		for _, k := range a.keys {
			s := Note
			if k == a.lead {
				s = sev
			}
			add(k, Finding{Group, s, text, fmt.Sprintf("×%d", n)})
		}
	}
}

func nameOf(g string) string {
	for i := range len(g) {
		if g[i] == 0 {
			return g[i+1:]
		}
	}
	return g
}

// treeTotals flags the deepest process whose subtree uses TreeCPU or more
// while none of its children's subtrees does alone: the build driver behind
// a swarm of compilers, not every shell above it.
func treeTotals(s proc.Snapshot, byPID map[int]*proc.Proc, t Thresholds, add func(string, Finding)) {
	children := map[int][]int{}
	for _, p := range s.Procs {
		if p.PID != p.PPID {
			children[p.PPID] = append(children[p.PPID], p.PID)
		}
	}
	type tot struct {
		cpu float64
		mem uint64
		n   int
	}
	memo := map[int]tot{}
	var sum func(pid int, depth int) tot
	sum = func(pid int, depth int) tot {
		if v, ok := memo[pid]; ok {
			return v
		}
		var r tot
		if p := byPID[pid]; p != nil {
			r = tot{p.CPU, p.Footprint, 1}
		}
		if depth < 64 { // cycle guard; ppid chains are short
			for _, c := range children[pid] {
				ct := sum(c, depth+1)
				r.cpu += ct.cpu
				r.mem += ct.mem
				r.n += ct.n
			}
		}
		memo[pid] = r
		return r
	}
	for _, p := range s.Procs {
		if p.PID <= 1 || len(children[p.PID]) < 2 {
			continue
		}
		total := sum(p.PID, 0)
		if total.cpu < t.TreeCPU || total.cpu-p.CPU < t.TreeCPU/2 {
			continue
		}
		deeper := false
		for _, c := range children[p.PID] {
			if sum(c, 0).cpu >= t.TreeCPU {
				deeper = true
				break
			}
		}
		if deeper {
			continue
		}
		add(p.Key(), Finding{Group, Warn, fmt.Sprintf("its %d descendants use CPU %.0f%%, memory %s together", total.n-1, total.cpu-p.CPU, Bytes(total.mem-p.Footprint)), "tree"})
	}
}

// Bytes formats a size with a binary unit.
func Bytes(b uint64) string {
	const u = 1024
	switch {
	case b >= u*u*u*u:
		return fmt.Sprintf("%.1fT", float64(b)/(u*u*u*u))
	case b >= u*u*u:
		return fmt.Sprintf("%.1fG", float64(b)/(u*u*u))
	case b >= u*u:
		return fmt.Sprintf("%.0fM", float64(b)/(u*u))
	case b >= u:
		return fmt.Sprintf("%.0fK", float64(b)/u)
	}
	return fmt.Sprintf("%dB", b)
}

func dur(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0fm", d.Minutes())
	}
	return fmt.Sprintf("%.0fs", d.Seconds())
}
