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
	// A child that burns ~300ms of CPU, then exits and is reaped by Wait.
	cmd := exec.Command("/bin/sh", "-c", `i=0; end=$(( $(date +%s) + 1 )); while [ $(date +%s) -lt $end ]; do i=$((i+1)); done`)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	after, err := readUsage(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	child := time.Duration(after.childCPUNs - before.childCPUNs)
	if child < 100*time.Millisecond || child > 5*time.Second {
		t.Errorf("child cpu credited = %v, want ~0.3-1.5s", child)
	}
	rt, _ := readTask(os.Getpid())
	own := time.Duration(after.cpuNs)
	task := time.Duration(rt.cpuNs)
	if d := own - task; d > 200*time.Millisecond || d < -200*time.Millisecond {
		t.Errorf("rusage own cpu %v vs task info %v: units disagree", own, task)
	}
}
