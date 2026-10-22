package history

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/keyolk/sysx/internal/proc"
)

// Rules decide when a stretch starts, ends and is worth keeping.
type Rules struct {
	// A process is busy in a sample when any of these hold.
	BusyCPU  float64 // own + reaped children, percent of one core
	BusyDisk float64 // bytes/s
	BusyNet  float64 // bytes/s
	BusyMem  float64 // footprint fraction of RAM
	// Quiet ends a stretch: this long without a busy sample.
	Quiet time.Duration
	// Segment cuts a stretch still going on, so it reaches the log.
	Segment time.Duration
	// A finished stretch is kept when any of these hold.
	KeepCPUSec float64
	KeepMem    float64 // peak footprint fraction of RAM
	KeepDisk   uint64
	KeepNet    uint64
	// Groups: same user and name, at least GroupMin processes alive at
	// once, busy together at GroupCPU, kept at GroupKeepCPUSec.
	GroupMin        int
	GroupCPU        float64
	GroupKeepCPUSec float64
	// Around: processes started this long before a stretch began.
	Around time.Duration
}

// DefaultRules keep what a person would ask about later, and little else.
func DefaultRules() Rules {
	return Rules{
		BusyCPU: 25, BusyDisk: 4 << 20, BusyNet: 1 << 20, BusyMem: 0.10,
		Quiet: 30 * time.Second, Segment: 10 * time.Minute,
		KeepCPUSec: 30, KeepMem: 0.10, KeepDisk: 512 << 20, KeepNet: 256 << 20,
		GroupMin: 3, GroupCPU: 100, GroupKeepCPUSec: 120,
		Around: time.Minute,
	}
}

type tracker struct {
	ep       Episode
	lastBusy time.Time
	pids     map[int]bool // groups
	memTotal uint64
	// kept: an earlier segment of this stretch reached the log, so every
	// later one does too and the reader can join them.
	kept bool
}

// Recorder turns snapshots into finished episodes. It is fed by one
// goroutine.
type Recorder struct {
	rules  Rules
	procs  map[string]*tracker
	groups map[string]*tracker
	// args reads argv for a process when its stretch starts; nil skips it.
	args func(pid int) ([]string, error)
	path func(pid int) string
	cwd  func(pid int) string
}

// NewRecorder returns a recorder that reads argv, exe and cwd through proc.
func NewRecorder(r Rules) *Recorder {
	return &Recorder{
		rules: r, procs: map[string]*tracker{}, groups: map[string]*tracker{},
		args: func(pid int) ([]string, error) {
			a, err := proc.ReadArgs(pid)
			return a.Argv, err
		},
		path: proc.Path, cwd: proc.Cwd,
	}
}

func (r *Recorder) busy(p proc.Proc, memTotal uint64) []string {
	var why []string
	if p.CPU+p.ChildCPU >= r.rules.BusyCPU {
		why = append(why, "cpu")
	}
	if memTotal > 0 && float64(p.Footprint)/float64(memTotal) >= r.rules.BusyMem {
		why = append(why, "mem")
	}
	if p.DiskRead+p.DiskWrite >= r.rules.BusyDisk {
		why = append(why, "disk")
	}
	if p.NetIn+p.NetOut >= r.rules.BusyNet {
		why = append(why, "net")
	}
	return why
}

// Step feeds one snapshot and returns the episodes that finished or were
// cut. The first snapshot (no interval) only primes the collector's rates.
func (r *Recorder) Step(s proc.Snapshot) []Episode {
	if s.Interval <= 0 {
		return nil
	}
	now, secs := s.At, s.Interval.Seconds()
	byPID := make(map[int]proc.Proc, len(s.Procs))
	for _, p := range s.Procs {
		byPID[p.PID] = p
	}
	var out []Episode
	alive := make(map[string]bool, len(s.Procs))

	for _, p := range s.Procs {
		k := p.Key()
		alive[k] = true
		why := r.busy(p, s.Sys.MemTotal)
		t := r.procs[k]
		if t == nil {
			if len(why) == 0 {
				continue
			}
			t = r.open(p, s, byPID)
			r.procs[k] = t
		}
		r.observe(t, p, why, now, secs)
		if len(why) > 0 {
			t.lastBusy = now
		}
	}
	for k, t := range r.procs {
		switch {
		case !alive[k]:
			t.ep.Exited = true
			out = r.finish(out, t, false)
			delete(r.procs, k)
		case now.Sub(t.lastBusy) >= r.rules.Quiet:
			// End at the last busy sample: the quiet tail is not the stretch.
			t.ep.End = t.lastBusy
			out = r.finish(out, t, false)
			delete(r.procs, k)
		case now.Sub(t.ep.Start) >= r.rules.Segment:
			out = r.finish(out, t, true)
			r.procs[k] = r.reopen(t, now)
		}
	}
	out = append(out, r.stepGroups(s, byPID, now, secs)...)
	return out
}

func (r *Recorder) open(p proc.Proc, s proc.Snapshot, byPID map[int]proc.Proc) *tracker {
	t := &tracker{
		ep: Episode{
			Kind: KindProc, Key: p.Key(), PID: p.PID, PPID: p.PPID, Name: p.Name, User: p.User,
			// The interval that made it busy belongs to the stretch.
			Start:   s.At.Add(-s.Interval),
			Chain:   chain(p, byPID),
			Around:  around(p, s, byPID, r.rules.Around),
			Partial: p.Partial,
		},
		memTotal: s.Sys.MemTotal,
	}
	if r.args != nil {
		if a, err := r.args(p.PID); err == nil {
			t.ep.Args = maskArgs(capArgs(a))
		}
	}
	if r.path != nil {
		t.ep.Exe = r.path(p.PID)
	}
	if r.cwd != nil {
		t.ep.Cwd = r.cwd(p.PID)
	}
	return t
}

// reopen starts the next segment of a stretch from where the cut was made.
func (r *Recorder) reopen(t *tracker, now time.Time) *tracker {
	n := &tracker{ep: t.ep, lastBusy: t.lastBusy, memTotal: t.memTotal, pids: map[int]bool{}, kept: t.kept}
	n.ep.Start, n.ep.Cont = now, false
	n.ep.CPUSec, n.ep.ChildCPUSec, n.ep.PeakCPU, n.ep.PeakMem = 0, 0, 0, 0
	n.ep.DiskBytes, n.ep.NetBytes, n.ep.Series, n.ep.Why, n.ep.Around = 0, 0, nil, nil, nil
	n.ep.Procs, n.ep.PeakProcs = 0, 0
	return n
}

func (r *Recorder) observe(t *tracker, p proc.Proc, why []string, now time.Time, secs float64) {
	t.ep.End = now
	// Sums of per-interval rates, not differences of lifetime counters:
	// the collector only reports a rate it measured between two good
	// readings, so a sample where ps timed out costs that interval, not
	// a process's whole life.
	t.ep.CPUSec += p.CPU / 100 * secs
	t.ep.ChildCPUSec += p.ChildCPU / 100 * secs
	t.ep.DiskBytes += uint64((p.DiskRead + p.DiskWrite) * secs)
	t.ep.NetBytes += uint64((p.NetIn + p.NetOut) * secs)
	cpu := p.CPU + p.ChildCPU
	t.ep.PeakCPU = max(t.ep.PeakCPU, cpu)
	t.ep.PeakMem = max(t.ep.PeakMem, p.Footprint)
	t.ep.Series = append(t.ep.Series, float32(cpu))
	for _, w := range why {
		if !slices.Contains(t.ep.Why, w) {
			t.ep.Why = append(t.ep.Why, w)
		}
	}
}

// finish appends the tracker's episode when it is worth keeping. A cut
// segment of a stretch is always kept once the stretch qualified, so the
// reader can join the pieces.
func (r *Recorder) finish(out []Episode, t *tracker, cut bool) []Episode {
	e := t.ep
	e.Cont = cut
	e.Series = downsample(e.Series, seriesCap)
	if !t.kept && !r.keep(e, t.memTotal) {
		return out
	}
	t.kept = true
	return append(out, e)
}

func (r *Recorder) keep(e Episode, memTotal uint64) bool {
	if e.Kind == KindGroup {
		return e.TotalCPUSec() >= r.rules.GroupKeepCPUSec
	}
	switch {
	case e.TotalCPUSec() >= r.rules.KeepCPUSec:
		return true
	case memTotal > 0 && float64(e.PeakMem)/float64(memTotal) >= r.rules.KeepMem:
		return true
	case e.DiskBytes >= r.rules.KeepDisk, e.NetBytes >= r.rules.KeepNet:
		return true
	}
	return false
}

// Flush ends every open stretch, for shutdown.
func (r *Recorder) Flush() []Episode {
	var out []Episode
	for k, t := range r.procs {
		out = r.finish(out, t, false)
		delete(r.procs, k)
	}
	for k, t := range r.groups {
		out = r.finish(out, t, false)
		delete(r.groups, k)
	}
	return out
}

func (r *Recorder) stepGroups(s proc.Snapshot, byPID map[int]proc.Proc, now time.Time, secs float64) []Episode {
	type agg struct {
		cpu   float64
		mem   uint64
		pids  []int
		first proc.Proc
	}
	aggs := map[string]*agg{}
	for _, p := range s.Procs {
		if p.PID <= 1 {
			continue
		}
		k := p.User + "/" + p.Name
		a := aggs[k]
		if a == nil {
			a = &agg{first: p}
			aggs[k] = a
		}
		a.cpu += p.CPU
		a.mem += p.Footprint
		a.pids = append(a.pids, p.PID)
	}
	var out []Episode
	for k, a := range aggs {
		busy := len(a.pids) >= r.rules.GroupMin && a.cpu >= r.rules.GroupCPU
		t := r.groups[k]
		if t == nil {
			if !busy {
				continue
			}
			t = &tracker{
				ep: Episode{Kind: KindGroup, Key: k, Name: a.first.Name, User: a.first.User,
					Start: now.Add(-s.Interval), Chain: chain(a.first, byPID), Why: []string{"cpu"},
					Around: around(a.first, s, byPID, r.rules.Around)},
				pids: map[int]bool{}, memTotal: s.Sys.MemTotal,
			}
			r.groups[k] = t
		}
		t.ep.End = now
		t.ep.CPUSec += a.cpu / 100 * secs
		t.ep.PeakCPU = max(t.ep.PeakCPU, a.cpu)
		t.ep.PeakMem = max(t.ep.PeakMem, a.mem)
		t.ep.PeakProcs = max(t.ep.PeakProcs, len(a.pids))
		for _, pid := range a.pids {
			t.pids[pid] = true
		}
		t.ep.Procs = len(t.pids)
		t.ep.Series = append(t.ep.Series, float32(a.cpu))
		if busy {
			t.lastBusy = now
		}
	}
	for k, t := range r.groups {
		switch {
		case aggs[k] == nil:
			t.ep.Exited = true
			out = r.finish(out, t, false)
			delete(r.groups, k)
		case now.Sub(t.lastBusy) >= r.rules.Quiet:
			t.ep.End = t.lastBusy
			out = r.finish(out, t, false)
			delete(r.groups, k)
		case now.Sub(t.ep.Start) >= r.rules.Segment:
			out = r.finish(out, t, true)
			n := r.reopen(t, now)
			n.ep.Why = []string{"cpu"}
			r.groups[k] = n
		}
	}
	return out
}

// chain is the ancestor list, outermost first, kernel and launchd left out.
func chain(p proc.Proc, byPID map[int]proc.Proc) []string {
	var out []string
	cur := p
	for range 32 {
		pp, ok := byPID[cur.PPID]
		if !ok || pp.PID <= 1 || pp.PID == cur.PID {
			break
		}
		out = append(out, fmt.Sprintf("%s %d", pp.Name, pp.PID))
		cur = pp
	}
	slices.Reverse(out)
	return out
}

// around names commands the same user typed into a shell shortly before
// p's stretch began, newest first. Load that runs elsewhere (a docker build
// inside OrbStack's VM) shows up on a long-running helper; the docker
// command started a moment before says what it was. It is empty when p
// itself just started: then p is its own explanation.
func around(p proc.Proc, s proc.Snapshot, byPID map[int]proc.Proc, w time.Duration) []string {
	if w <= 0 {
		return nil
	}
	cut := s.At.Add(-s.Interval - w)
	if !p.Start.IsZero() && p.Start.After(cut) {
		return nil
	}
	var cands []proc.Proc
	for _, q := range s.Procs {
		if q.UID != p.UID || q.PID == p.PID || q.Start.Before(cut) || q.Start.After(s.At) {
			continue
		}
		if parent, ok := byPID[q.PPID]; !ok || !shells[parent.Name] {
			continue
		}
		cands = append(cands, q)
	}
	slices.SortFunc(cands, func(a, b proc.Proc) int { return b.Start.Compare(a.Start) })
	seen := map[string]bool{}
	var out []string
	for _, q := range cands {
		if seen[q.Name] {
			continue
		}
		seen[q.Name] = true
		out = append(out, fmt.Sprintf("%s %d", q.Name, q.PID))
		if len(out) == 5 {
			break
		}
	}
	return out
}

// shells are the parents of commands a person (or an agent's shell tool)
// typed.
var shells = map[string]bool{"fish": true, "zsh": true, "bash": true, "sh": true, "-fish": true, "-zsh": true, "-bash": true, "login": true}

// capArgs keeps argv readable and the log small.
func capArgs(a []string) []string {
	const maxArgs, maxLen = 64, 512
	if len(a) > maxArgs {
		a = append(slices.Clone(a[:maxArgs]), fmt.Sprintf("… %d more", len(a)-maxArgs))
	}
	out := make([]string, len(a))
	for i, s := range a {
		if len(s) > maxLen {
			s = s[:maxLen] + "…"
		}
		out[i] = s
	}
	return out
}

var secretFlag = regexp.MustCompile(`(?i)(token|secret|passw|api[-_]?key|private|credential|auth|cookie)`)

// maskArgs hides values that follow secret-looking flags (--token X,
// --password=X), since the log outlives the process.
func maskArgs(a []string) []string {
	out := slices.Clone(a)
	for i := 0; i < len(out); i++ {
		s := out[i]
		if !strings.HasPrefix(s, "-") || !secretFlag.MatchString(s) {
			continue
		}
		if k, _, ok := strings.Cut(s, "="); ok {
			out[i] = k + "=******"
			continue
		}
		if i+1 < len(out) && !strings.HasPrefix(out[i+1], "-") {
			out[i+1] = "******"
			i++
		}
	}
	return out
}
