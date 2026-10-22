package outlier

import (
	"strings"
	"testing"
	"time"

	"github.com/keyolk/sysx/internal/proc"
)

var t0 = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

func snap(at time.Duration, ps ...proc.Proc) proc.Snapshot {
	return proc.Snapshot{At: t0.Add(at), Sys: proc.System{NCPU: 8, MemTotal: 32 << 30}, Procs: ps}
}

func run(t *testing.T, steps int, every time.Duration, mk func(i int) []proc.Proc) map[string]Report {
	t.Helper()
	h := NewHistory(5 * time.Minute)
	var s proc.Snapshot
	for i := range steps {
		s = snap(time.Duration(i)*every, mk(i)...)
		h = h.Add(s)
	}
	return Analyze(s, h, Defaults())
}

func p(pid int, name string) proc.Proc {
	return proc.Proc{PID: pid, PPID: 100, PGID: pid, Name: name, User: "me", Start: t0.Add(-time.Hour)}
}

func TestSustainedCPUAlertsButOneSpikeOnlyNotes(t *testing.T) {
	r := run(t, 10, 2*time.Second, func(i int) []proc.Proc {
		busy := p(10, "busy")
		busy.CPU = 99
		spiky := p(11, "spiky")
		if i == 9 {
			spiky.CPU = 180
		}
		return []proc.Proc{busy, spiky}
	})
	if got := r[p(10, "busy").Key()].SevOf(CPU); got != Alert {
		t.Errorf("busy cpu sev = %v, want Alert", got)
	}
	if got := r[p(11, "spiky").Key()].SevOf(CPU); got != Note {
		t.Errorf("spiky cpu sev = %v, want Note", got)
	}
}

func TestNewProcessIsNotJudgedOnOneSample(t *testing.T) {
	r := run(t, 1, 2*time.Second, func(int) []proc.Proc {
		x := p(10, "x")
		x.CPU = 400
		return []proc.Proc{x}
	})
	if got := r[p(10, "x").Key()].SevOf(CPU); got != None {
		t.Errorf("sev = %v, want None before the window fills", got)
	}
}

func TestSteadyGrowthIsALeakButASawtoothIsNot(t *testing.T) {
	r := run(t, 61, 2*time.Second, func(i int) []proc.Proc {
		leak := p(10, "leak")
		leak.Footprint = uint64(100<<20) + uint64(i)*(25<<20) // +1.5G over 2m
		saw := p(11, "saw")
		saw.Footprint = uint64(100 << 20)
		if i%2 == 0 {
			saw.Footprint += uint64(i) * (25 << 20)
		}
		return []proc.Proc{leak, saw}
	})
	lr := r[p(10, "leak").Key()]
	if lr.SevOf(Mem) != Alert || !strings.Contains(lr.Findings[0].Text, "steadily") {
		t.Errorf("leak = %+v, want a steady-growth Alert", lr)
	}
	for _, f := range r[p(11, "saw").Key()].Findings {
		if strings.Contains(f.Text, "grew") {
			t.Errorf("sawtooth flagged as growth: %s", f.Text)
		}
	}
}

func TestFanOutFlagsEveryMemberWhenTheyAddUp(t *testing.T) {
	r := run(t, 2, 2*time.Second, func(int) []proc.Proc {
		var ps []proc.Proc
		for i := range 10 {
			c := p(200+i, "rustc")
			c.CPU = 30 // 300% together, none alone over CPUWarn
			ps = append(ps, c)
		}
		lone := p(300, "rustc2")
		return append(ps, lone)
	})
	warned := 0
	for i := range 10 {
		got := r[p(200+i, "rustc").Key()]
		if !strings.Contains(got.Findings[0].Text, "10 × rustc") || got.Findings[0].Tag != "×10" {
			t.Errorf("member %d = %+v", i, got)
		}
		if got.SevOf(Group) == Warn {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("%d members carry the group's Warn, want only the leader", warned)
	}
	if r[p(300, "rustc2").Key()].Sev != None {
		t.Errorf("a different name joined the group")
	}
}

func TestTreeFlagsTheDriverNotItsAncestors(t *testing.T) {
	r := run(t, 2, 2*time.Second, func(int) []proc.Proc {
		shell := p(50, "fish")
		cargo := p(60, "cargo")
		cargo.PPID = 50
		ps := []proc.Proc{shell, cargo}
		for i := range 4 {
			c := p(70+i, "rustc"+string(rune('a'+i))) // distinct names: not a fan-out
			c.PPID, c.CPU = 60, 100
			ps = append(ps, c)
		}
		return ps
	})
	if r[p(60, "cargo").Key()].SevOf(Group) != Warn {
		t.Errorf("cargo not flagged: %+v", r[p(60, "cargo").Key()])
	}
	if r[p(50, "fish").Key()].SevOf(Group) != None {
		t.Errorf("the shell above the driver was flagged too")
	}
}

func TestOrphanNeedsADeadGroupLeaderNotJustParentOne(t *testing.T) {
	r := run(t, 10, 2*time.Second, func(int) []proc.Proc {
		app := p(10, "Slack") // launchd child leading its own group
		app.PPID, app.CPU = 1, 80
		orphan := p(11, "node")
		orphan.PPID, orphan.PGID, orphan.CPU = 1, 9999, 80
		return []proc.Proc{app, orphan}
	})
	if r[p(10, "Slack").Key()].SevOf(State) != None {
		t.Errorf("an app started by launchd was called an orphan")
	}
	if r[p(11, "node").Key()].SevOf(State) != Alert {
		t.Errorf("spinning orphan = %+v", r[p(11, "node").Key()])
	}
}

func TestOwnOutlierOutranksGroupLeader(t *testing.T) {
	r := run(t, 10, 2*time.Second, func(int) []proc.Proc {
		var ps []proc.Proc
		for i := range 10 {
			c := p(200+i, "node")
			c.CPU = 30 // under CPUWarn alone; 300% together makes a group Warn
			ps = append(ps, c)
		}
		lone := p(300, "spin")
		lone.CPU = 50
		return append(ps, lone)
	})
	lone := r[p(300, "spin").Key()]
	for i := range 10 {
		if m := r[p(200+i, "node").Key()]; m.Score >= lone.Score {
			t.Errorf("node %d score %.0f >= loner %.0f", i, m.Score, lone.Score)
		}
	}
}
