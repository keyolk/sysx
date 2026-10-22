package proc

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// The kernel reports reaped children's CPU time on the parent; that is how
// a build driver gets credit for compilers that already exited. This test
// also pins the unit: own time from rusage must agree with the task info.
func TestChildCPUIsCreditedAfterReapInNanoseconds(t *testing.T) {
	before, err := readUsage(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	// A child that burns a fixed amount of work, then exits and is reaped
	// by Wait. A count, not a clock: a deadline loop started near the end
	// of a second barely runs.
	cmd := exec.Command("/bin/sh", "-c", `i=0; while [ $i -lt 300000 ]; do i=$((i+1)); done`)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	after, err := readUsage(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	child := time.Duration(after.childCPUNs - before.childCPUNs)
	if child < 50*time.Millisecond || child > 30*time.Second {
		t.Errorf("child cpu credited = %v, want the loop's CPU (0.05-30s)", child)
	}
	rt, _ := readTask(os.Getpid())
	own := time.Duration(after.cpuNs)
	task := time.Duration(rt.cpuNs)
	if d := own - task; d > 200*time.Millisecond || d < -200*time.Millisecond {
		t.Errorf("rusage own cpu %v vs task info %v: units disagree", own, task)
	}
}

// A child the collector already sampled must not be charged again to its
// parent when the parent reaps it: what a sample saw is the child's.
func TestReapedChildSeenAliveIsNotChargedTwice(t *testing.T) {
	c := NewCollector()
	cmd := exec.Command("/bin/sh", "-c", `i=0; while [ $i -lt 600000 ]; do i=$((i+1)); done`)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Sample(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	s2, err := c.Sample(t.Context()) // sees the child busy
	if err != nil {
		t.Fatal(err)
	}
	var seen time.Duration
	for _, p := range s2.Procs {
		if p.PID == cmd.Process.Pid {
			seen = p.CPUTime
		}
	}
	if seen < 100*time.Millisecond {
		t.Skipf("child barely ran before the sample (%v); nothing to subtract", seen)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	life := cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	s3, err := c.Sample(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s3.Procs {
		if p.PID != os.Getpid() {
			continue
		}
		charged := time.Duration(p.ChildCPU / 100 * float64(s3.Interval))
		// Unseen is life - seen; the sampler's own nettop and ps are
		// children of this process too, so allow some slack for them.
		if charged > life-seen+700*time.Millisecond {
			t.Errorf("charged %v of child CPU; child life %v, already seen %v", charged, life, seen)
		}
		return
	}
	t.Fatal("own process missing")
}
