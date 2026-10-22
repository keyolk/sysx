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
