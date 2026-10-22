package proc

import (
	"encoding/binary"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestParseNettopKeepsDottedNamesAndSkipsHeader(t *testing.T) {
	out := []byte(",bytes_in,bytes_out,\nkernel_task.0,10,20,\nGoogle Chrome H.1234,5,6,\ncom.apple.Web.99,7,8,\nbroken,1,2,\n")
	got := parseNettop(out)
	want := map[int]netBytes{0: {10, 20}, 1234: {5, 6}, 99: {7, 8}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("pid %d = %v, want %v", k, got[k], v)
		}
	}
}

func TestParseCPUTime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0:01.50":    1500 * time.Millisecond,
		"392:47.82":  392*time.Minute + 47820*time.Millisecond,
		"1:02:03.00": time.Hour + 2*time.Minute + 3*time.Second,
		"2-01:00:00": 49 * time.Hour,
		"garbage:xx": 0,
	} {
		if got := parseCPUTime(in); got != want {
			t.Errorf("%q = %v, want %v", in, got, want)
		}
	}
}

func TestParseLsofSplitsSocketsFromFiles(t *testing.T) {
	out := []byte("p42\nfcwd\ntDIR\nn/home\nf0\ntCHR\nn/dev/null\nf3\ntIPv4\nPTCP\nn127.0.0.1:8080\nTST=LISTEN\nf4\ntREG\nn/tmp/x.log\nf5\ntIPv6\nPUDP\nn*:5353\n")
	f := parseLsof(out)
	if f.Count != 4 {
		t.Errorf("count = %d, want 4 (cwd is not an fd)", f.Count)
	}
	if len(f.Conns) != 2 || f.Conns[0].State != "LISTEN" || f.Conns[1].Proto != "UDP" {
		t.Errorf("conns = %+v", f.Conns)
	}
	if len(f.Paths) != 1 || f.Paths[0] != "/tmp/x.log" {
		t.Errorf("paths = %v", f.Paths)
	}
}

func TestParseProcArgs(t *testing.T) {
	b := binary.LittleEndian.AppendUint32(nil, 2)
	b = append(b, "/bin/sleep\x00\x00\x00sleep\x0030\x00HOME=/h\x00TOKEN=s\x00\x00junk"...)
	a, err := parseProcArgs(b)
	if err != nil {
		t.Fatal(err)
	}
	if a.Exe != "/bin/sleep" || len(a.Argv) != 2 || a.Argv[1] != "30" || len(a.Env) != 2 || a.Env[1] != "TOKEN=s" {
		t.Errorf("args = %+v", a)
	}
}

// Signal must not hit a pid whose start time no longer matches: that is a
// different process that reused the pid.
func TestSignalRefusesAReusedPid(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	pid := cmd.Process.Pid
	rt, err := readTask(pid)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Unix(rt.start, rt.startUs*1000)
	if err := Signal(pid, start.Add(-time.Hour), syscall.SIGTERM); err == nil {
		t.Fatal("signalled a pid whose start time differs")
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("process died anyway: %v", err)
	}
	if err := Signal(pid, start, syscall.SIGTERM); err != nil {
		t.Fatalf("signal with the right start: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Error("sleep exited cleanly; SIGTERM did not land")
	}
}

func TestCollectorSeesItselfWithRates(t *testing.T) {
	c := NewCollector()
	if _, err := c.Sample(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	s, err := c.Sample(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if s.Interval <= 0 || s.Sys.MemTotal == 0 || s.Sys.NCPU == 0 {
		t.Errorf("system = %+v interval %v", s.Sys, s.Interval)
	}
	me := syscall.Getpid()
	for _, p := range s.Procs {
		if p.PID == me {
			if p.Partial || p.Footprint == 0 || p.Start.IsZero() {
				t.Errorf("self = %+v", p)
			}
			return
		}
	}
	t.Error("own process missing from the sample")
}
