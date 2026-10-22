// Package history keeps a log of processes that used a lot of a resource,
// so a heavy build or a busy agent can be found after it is gone.
//
// The log holds episodes, not samples: one record per stretch of time a
// process (or a swarm of same-named processes) was busy, with what it cost
// over that stretch. Long stretches are cut into segments so a process that
// never stops (an endpoint agent) still shows up, and the reader joins them.
package history

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Kind is what an episode describes.
type Kind string

const (
	// KindProc is one process.
	KindProc Kind = "proc"
	// KindGroup is several processes of one user and name, busy together
	// while none had to be busy alone (a compiler swarm).
	KindGroup Kind = "group"
)

// Episode is one record in the log.
type Episode struct {
	Kind  Kind      `json:"kind"`
	Key   string    `json:"key"` // proc.Key for a process; user/name for a group
	PID   int       `json:"pid,omitempty"`
	PPID  int       `json:"ppid,omitempty"`
	Name  string    `json:"name"`
	User  string    `json:"user"`
	Exe   string    `json:"exe,omitempty"`
	Args  []string  `json:"args,omitempty"`
	Cwd   string    `json:"cwd,omitempty"`
	Chain []string  `json:"chain,omitempty"` // ancestors, outermost first: "tmux 812"
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// Cont is set on a segment cut from a stretch still going on; the next
	// segment of the same key starts where this one ends.
	Cont bool `json:"cont,omitempty"`
	// Exited is set when the stretch ended because the process went away.
	Exited bool `json:"exited,omitempty"`

	CPUSec      float64 `json:"cpu_sec"`                 // own CPU time used in the stretch
	ChildCPUSec float64 `json:"child_cpu_sec,omitempty"` // reaped children's CPU in the stretch
	PeakCPU     float64 `json:"peak_cpu"`                // percent of one core
	PeakMem     uint64  `json:"peak_mem"`
	DiskBytes   uint64  `json:"disk_bytes,omitempty"`
	NetBytes    uint64  `json:"net_bytes,omitempty"`
	Procs       int     `json:"procs,omitempty"` // distinct pids in a group
	PeakProcs   int     `json:"peak_procs,omitempty"`
	// Why lists what made the stretch worth keeping: cpu, mem, disk, net.
	Why []string `json:"why"`
	// Series is CPU percent (own plus reaped children) over the stretch,
	// at most seriesCap points.
	Series []float32 `json:"series,omitempty"`
	// Around names processes the same user started just before the stretch
	// began: the docker or make command behind load that runs elsewhere.
	Around []string `json:"around,omitempty"`
	// Partial: another user's process; disk bytes and args are unknown.
	Partial bool `json:"partial,omitempty"`
}

// Duration is how long the stretch lasted.
func (e Episode) Duration() time.Duration { return e.End.Sub(e.Start) }

// TotalCPUSec is own plus reaped children's CPU.
func (e Episode) TotalCPUSec() float64 { return e.CPUSec + e.ChildCPUSec }

// AvgCPU is the mean CPU percent over the stretch, children included.
func (e Episode) AvgCPU() float64 {
	if d := e.Duration().Seconds(); d > 0 {
		return e.TotalCPUSec() / d * 100
	}
	return 0
}

// ID makes Episode a list item. A process key plus start is unique.
func (e Episode) ID() string { return fmt.Sprintf("%s/%d", e.Key, e.Start.UnixMilli()) }

// Command is argv joined for display, or the name when argv is unknown.
func (e Episode) Command() string {
	if len(e.Args) == 0 {
		return e.Name
	}
	return strings.Join(e.Args, " ")
}

// join merges segment b into a, the earlier one.
func join(a, b Episode) Episode {
	a.End, a.Cont, a.Exited = b.End, b.Cont, b.Exited
	a.CPUSec += b.CPUSec
	a.ChildCPUSec += b.ChildCPUSec
	a.PeakCPU = max(a.PeakCPU, b.PeakCPU)
	a.PeakMem = max(a.PeakMem, b.PeakMem)
	a.DiskBytes += b.DiskBytes
	a.NetBytes += b.NetBytes
	a.Procs = max(a.Procs, b.Procs) // distinct pids per segment; a lower bound
	a.PeakProcs = max(a.PeakProcs, b.PeakProcs)
	for _, w := range b.Why {
		if !slices.Contains(a.Why, w) {
			a.Why = append(a.Why, w)
		}
	}
	a.Series = downsample(append(slices.Clone(a.Series), b.Series...), seriesCap)
	return a
}

// Merge joins consecutive segments of the same stretch: same key, the
// earlier one marked Cont, and the later starting where it ended (within
// slack, for a missed sample). Input order does not matter; the result is
// newest first.
func Merge(eps []Episode, slack time.Duration) []Episode {
	in := slices.Clone(eps)
	slices.SortStableFunc(in, func(a, b Episode) int { return a.Start.Compare(b.Start) })
	open := map[string]int{} // key -> index in out of a stretch still continuing
	var out []Episode
	for _, e := range in {
		if i, ok := open[e.Key]; ok {
			prev := out[i]
			if gap := e.Start.Sub(prev.End); gap >= -slack && gap <= slack {
				out[i] = join(prev, e)
				if !e.Cont {
					delete(open, e.Key)
				}
				continue
			}
			delete(open, e.Key)
		}
		out = append(out, e)
		if e.Cont {
			open[e.Key] = len(out) - 1
		}
	}
	slices.SortStableFunc(out, func(a, b Episode) int { return b.Start.Compare(a.Start) })
	return out
}

const seriesCap = 120

// downsample keeps at most n points by bucket max, so a spike survives.
func downsample(xs []float32, n int) []float32 {
	if len(xs) <= n {
		return xs
	}
	out := make([]float32, n)
	for i := range n {
		lo, hi := i*len(xs)/n, (i+1)*len(xs)/n
		m := xs[lo]
		for _, v := range xs[lo:hi] {
			m = max(m, v)
		}
		out[i] = m
	}
	return out
}

// Matches is a case-insensitive substring match (q lower-cased) on what a
// person remembers about a process: its name, command, chain, user, and
// what was started around it.
func Matches(e Episode, q string) bool {
	hay := strings.ToLower(strings.Join([]string{e.Name, e.User, e.Command(), e.Exe, e.Cwd,
		strings.Join(e.Chain, " "), strings.Join(e.Around, " "), strings.Join(e.Why, " ")}, "\n"))
	return strings.Contains(hay, q)
}

// SortKeys are the orders Sort knows.
var SortKeys = []string{"time", "cpu", "mem", "disk", "net"}

// Sort orders episodes in place: newest first, or largest cost first.
func Sort(eps []Episode, by string) {
	key := func(e Episode) float64 {
		switch by {
		case "cpu":
			return e.TotalCPUSec()
		case "mem":
			return float64(e.PeakMem)
		case "disk":
			return float64(e.DiskBytes)
		case "net":
			return float64(e.NetBytes)
		}
		return float64(e.Start.UnixNano())
	}
	slices.SortStableFunc(eps, func(a, b Episode) int {
		ka, kb := key(a), key(b)
		switch {
		case ka > kb:
			return -1
		case ka < kb:
			return 1
		}
		return b.Start.Compare(a.Start)
	})
}

// Dur is a short duration: 45s, 12m, 3h05m, 2d4h.
func Dur(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
