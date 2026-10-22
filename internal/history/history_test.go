package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/keyolk/sysx/internal/proc"
)

var t0 = time.Date(2026, 10, 22, 9, 0, 0, 0, time.Local)

const step = 10 * time.Second

func pp(pid int, name string, cpu float64) proc.Proc {
	return proc.Proc{PID: pid, PPID: 50, Name: name, User: "me", UID: 501,
		Start: t0.Add(-time.Hour), CPU: cpu, Footprint: 100 << 20}
}

// sim keeps cumulative counters consistent with the rates, as the collector
// does.
type sim struct {
	r     *Recorder
	at    time.Duration
	cpu   map[int]time.Duration
	child map[int]time.Duration
	out   []Episode
}

func newSim() *sim {
	r := NewRecorder(DefaultRules())
	r.args = func(int) ([]string, error) { return []string{"docker", "build", "--secret-token", "abc", "."}, nil }
	r.path = func(int) string { return "" }
	r.cwd = func(int) string { return "/src" }
	return &sim{r: r, cpu: map[int]time.Duration{}, child: map[int]time.Duration{}}
}

func (s *sim) tick(ps ...proc.Proc) {
	s.at += step
	for i := range ps {
		p := &ps[i]
		s.cpu[p.PID] += time.Duration(p.CPU / 100 * float64(step))
		s.child[p.PID] += time.Duration(p.ChildCPU / 100 * float64(step))
		p.CPUTime, p.ChildCPUTime = s.cpu[p.PID], s.child[p.PID]
	}
	snap := proc.Snapshot{At: t0.Add(s.at), Interval: step, Sys: proc.System{MemTotal: 32 << 30, NCPU: 8}, Procs: ps}
	s.out = append(s.out, s.r.Step(snap)...)
}

func TestBusyStretchIsRecordedWithItsCost(t *testing.T) {
	s := newSim()
	s.tick(pp(1, "launchd", 0))
	for range 6 { // 60s at 100%
		s.tick(pp(1, "launchd", 0), pp(10, "hog", 100))
	}
	for range 4 { // quiet for 40s, still alive
		s.tick(pp(1, "launchd", 0), pp(10, "hog", 1))
	}
	if len(s.out) != 1 {
		t.Fatalf("episodes = %+v", s.out)
	}
	e := s.out[0]
	if e.Name != "hog" || e.CPUSec < 59 || e.CPUSec > 61 || e.Duration() != 60*time.Second || e.Exited {
		t.Errorf("episode = %+v (dur %v)", e, e.Duration())
	}
	if strings.Join(e.Args, " ") != "docker build --secret-token ****** ." {
		t.Errorf("args not masked: %q", e.Args)
	}
}

func TestShortSpikeIsNotKept(t *testing.T) {
	s := newSim()
	s.tick()
	s.tick(pp(10, "blip", 150)) // 15 cpu-s, under KeepCPUSec
	s.tick()                    // exits
	if len(s.out) != 0 {
		t.Errorf("kept a short spike: %+v", s.out)
	}
}

func TestBuildDriverGetsItsReapedChildrensCPU(t *testing.T) {
	s := newSim()
	s.tick()
	for range 6 {
		make := pp(20, "make", 1)
		make.ChildCPU = 400 // compilers that came and went between samples
		s.tick(make)
	}
	s.tick() // make exits
	if len(s.out) != 1 || !s.out[0].Exited || s.out[0].ChildCPUSec < 230 {
		t.Fatalf("episodes = %+v", s.out)
	}
}

func TestLongStretchIsCutAndMergedBack(t *testing.T) {
	s := newSim()
	s.tick()
	for range 150 { // 25 minutes busy: two cuts at 10m
		s.tick(pp(30, "cyberhaven", 60))
	}
	s.tick() // exits
	if len(s.out) != 3 || !s.out[0].Cont || !s.out[1].Cont || s.out[2].Cont {
		t.Fatalf("segments = %d %+v", len(s.out), s.out)
	}
	m := Merge(s.out, 30*time.Second)
	if len(m) != 1 {
		t.Fatalf("merged into %d", len(m))
	}
	if d := m[0].Duration(); d < 24*time.Minute || d > 26*time.Minute {
		t.Errorf("merged duration %v", d)
	}
	if c := m[0].CPUSec; c < 880 || c > 920 {
		t.Errorf("merged cpu %.0f, want ~900", c)
	}
}

func TestSwarmOfShortLivedProcessesIsOneGroupEpisode(t *testing.T) {
	s := newSim()
	s.tick()
	pid := 1000
	for range 12 { // 2 minutes; each rustc lives one sample at 90%
		var ps []proc.Proc
		for range 4 {
			pid++
			ps = append(ps, pp(pid, "rustc", 90))
		}
		s.tick(ps...)
	}
	s.tick()
	var groups []Episode
	for _, e := range s.out {
		if e.Kind == KindGroup {
			groups = append(groups, e)
		}
	}
	if len(groups) != 1 || groups[0].Procs != 48 || groups[0].PeakProcs != 4 || groups[0].CPUSec < 400 {
		t.Fatalf("groups = %+v", groups)
	}
}

func TestAroundNamesWhatTheUserStartedJustBefore(t *testing.T) {
	s := newSim()
	shell := pp(39, "fish", 0)
	docker := pp(40, "docker", 0)
	docker.PPID, docker.Start = 39, t0.Add(5*time.Second)
	daemon := pp(42, "mdworker", 0) // started too, but not from a shell
	daemon.PPID, daemon.Start = 1, t0.Add(6*time.Second)
	s.tick(shell, docker, daemon)
	vm := pp(41, "OrbStack Helper", 300)
	for range 6 {
		s.tick(shell, docker, daemon, vm)
	}
	for range 4 {
		s.tick(shell, docker, daemon, func() proc.Proc { v := vm; v.CPU = 0; return v }())
	}
	if len(s.out) == 0 || strings.Join(s.out[0].Around, ",") != "docker 40" {
		t.Fatalf("episodes = %+v", s.out)
	}
}

func TestStoreAppendLoadAndPrune(t *testing.T) {
	st := Store{Dir: t.TempDir(), Keep: 3, MaxBytes: 0}
	now := time.Now()
	var eps []Episode
	for d := range 6 {
		end := now.AddDate(0, 0, -d)
		eps = append(eps, Episode{Kind: KindProc, Key: "k" + string(rune('a'+d)), Name: "x", Start: end.Add(-time.Minute), End: end, CPUSec: 60})
	}
	if err := st.Append(eps); err != nil {
		t.Fatal(err)
	}
	// A torn last line, as if the recorder died mid-write.
	f, _ := os.OpenFile(st.file(now), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"kind":"proc","key":"torn`)
	f.Close()
	got, err := st.Load(now.AddDate(0, 0, -10))
	if err != nil || len(got) != 6 {
		t.Fatalf("load = %d %v", len(got), err)
	}
	removed, err := st.Prune(now)
	if err != nil || len(removed) != 3 {
		t.Fatalf("prune removed %v err %v", removed, err)
	}
	got, _ = st.Load(now.AddDate(0, 0, -10))
	if len(got) != 3 {
		t.Errorf("after prune %d episodes, want 3 days", len(got))
	}
	st.Keep, st.MaxBytes = 0, 1 // size cap: everything but today goes
	removed, _ = st.Prune(now)
	if _, err := os.Stat(st.file(now)); err != nil || len(removed) != 2 {
		t.Errorf("size prune removed %v; today's file must stay (%v)", removed, err)
	}
}

func TestOnlyOneRecorderHoldsTheLock(t *testing.T) {
	dir := t.TempDir()
	l, err := TryLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pid, _ := RecorderPID(dir); pid != os.Getpid() {
		t.Errorf("pid = %d", pid)
	}
	if _, err := TryLock(dir); err == nil {
		t.Error("a second recorder got the lock")
	}
	l.Release()
	if pid, _ := RecorderPID(filepath.Clean(dir)); pid != 0 {
		t.Errorf("after release pid = %d", pid)
	}
}

func TestAroundIsEmptyWhenTheProcessJustStarted(t *testing.T) {
	s := newSim()
	shell := pp(39, "fish", 0)
	other := pp(40, "git", 0)
	other.PPID, other.Start = 39, t0.Add(5*time.Second)
	s.tick(shell, other)
	hog := pp(41, "hog", 200)
	hog.PPID, hog.Start = 39, t0.Add(8*time.Second)
	for range 4 {
		s.tick(shell, other, hog)
	}
	s.tick(shell, other)
	if len(s.out) != 1 || len(s.out[0].Around) != 0 {
		t.Fatalf("episodes = %+v", s.out)
	}
}

// A sample where ps timed out reports no CPU time for a root daemon; the
// next good one must not charge its lifetime to the stretch (mDNSResponder
// 18069% on 2026-10-22). Snapshots are built by hand: the sim keeps its
// counters consistent, which is exactly what this case is not.
func TestMissingReadingCostsOneIntervalNotALifetime(t *testing.T) {
	r := newSim().r
	at := t0
	var out []Episode
	step := func(cpuTime time.Duration, cpu, netIn float64) {
		at = at.Add(10 * time.Second)
		d := proc.Proc{PID: 60, PPID: 1, Name: "mDNSResponder", User: "root", Start: t0.Add(-time.Hour),
			Partial: true, CPUTime: cpuTime, CPU: cpu, NetIn: netIn}
		out = append(out, r.Step(proc.Snapshot{At: at, Interval: 10 * time.Second,
			Sys: proc.System{MemTotal: 32 << 30, NCPU: 8}, Procs: []proc.Proc{d}})...)
	}
	step(0, 0, 10<<20) // busy by network (kept: 400M); ps timed out: no CPU time
	for range 3 {
		step(50*time.Hour, 3, 10<<20) // ps is back with the lifetime counter
	}
	for range 4 {
		step(50*time.Hour, 3, 0)
	}
	if len(out) == 0 {
		t.Fatal("the network stretch was not recorded")
	}
	for _, e := range out {
		if e.CPUSec > 60 {
			t.Errorf("%s charged %.0f cpu-s over %v", e.Name, e.CPUSec, e.Duration())
		}
	}
}
