package ui

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/keyolk/sysx/internal/proc"
)

// target names a process by pid and start time, so a pid reused between the
// dialog and the signal is never hit (proc.Signal re-checks).
type target struct {
	pid   int
	start time.Time
	name  string
}

func targetOf(p proc.Proc) target { return target{p.PID, p.Start, p.Name} }

// killPlan is captured when the dialog opens; what it lists is exactly what
// a key in the dialog will signal.
type killPlan struct {
	self     target
	user     string
	stopped  bool
	tree     []target // self first, then descendants
	same     []target // same name and user, self included
	excluded int      // sysx itself and the processes it runs under
}

// protected are sysx and its ancestors: killing the shell or tmux server
// that sysx runs in, by a tree or same-name sweep, is never what was meant.
func (m Model) protected() map[int]bool {
	out := map[int]bool{0: true, 1: true}
	pid := os.Getpid()
	for range 64 {
		out[pid] = true
		p, ok := m.byPID[pid]
		if !ok || p.PPID == pid {
			break
		}
		pid = p.PPID
	}
	return out
}

func (m Model) planFor(r row) (*killPlan, error) {
	prot := m.protected()
	if prot[r.p.PID] {
		return nil, fmt.Errorf("%s (%d) runs sysx or is pid 0/1; not offered", r.p.Name, r.p.PID)
	}
	plan := &killPlan{self: targetOf(r.p), user: r.p.User, stopped: r.p.State == proc.StateStop}
	plan.tree = []target{plan.self}
	queue := []int{r.p.PID}
	seen := map[int]bool{r.p.PID: true}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, c := range m.children[pid] {
			if seen[c] {
				continue
			}
			seen[c] = true
			queue = append(queue, c)
			if prot[c] {
				plan.excluded++
				continue
			}
			plan.tree = append(plan.tree, targetOf(m.byPID[c]))
		}
	}
	for _, p := range m.snap.Procs {
		if p.Name != r.p.Name || p.UID != r.p.UID {
			continue
		}
		if prot[p.PID] {
			plan.excluded++
			continue
		}
		plan.same = append(plan.same, targetOf(p))
	}
	return plan, nil
}

type killedMsg struct {
	what   string
	ok     int
	errs   []string
	signal syscall.Signal
}

func signalCmd(what string, ts []target, sig syscall.Signal) tea.Cmd {
	return func() tea.Msg {
		res := killedMsg{what: what, signal: sig}
		for _, t := range ts {
			if err := proc.Signal(t.pid, t.start, sig); err != nil {
				res.errs = append(res.errs, err.Error())
				continue
			}
			res.ok++
		}
		return res
	}
}

func sigName(s syscall.Signal) string {
	switch s {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGSTOP:
		return "SIGSTOP"
	case syscall.SIGCONT:
		return "SIGCONT"
	}
	return s.String()
}

// names summarizes targets for the dialog: "rustc ×12, cargo".
func names(ts []target) string {
	count := map[string]int{}
	var order []string
	for _, t := range ts {
		if count[t.name] == 0 {
			order = append(order, t.name)
		}
		count[t.name]++
	}
	parts := make([]string, 0, len(order))
	for _, n := range order {
		if count[n] > 1 {
			parts = append(parts, fmt.Sprintf("%s ×%d", n, count[n]))
		} else {
			parts = append(parts, n)
		}
	}
	return strings.Join(parts, ", ")
}
