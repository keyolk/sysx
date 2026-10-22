package ui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/keyolk/tuikit/tmux"

	"github.com/keyolk/sysx/internal/proc"
)

// extraInfo is the slow part of the detail pane: args and env, paths,
// open files and sockets, and the tmux pane. It is loaded off the loop for
// the selected process only.
type extraInfo struct {
	key     string
	loading bool

	path, cwd string
	args      proc.Args
	argsErr   error
	files     proc.Files
	filesErr  error

	pane    tmux.Pane
	inPane  bool
	paneErr error
}

type (
	extraStartMsg struct {
		gen uint64
		key string
		pid int
	}
	extraMsg struct {
		gen  uint64
		info extraInfo
	}
)

// requestExtra debounces: holding j sends one load, for the row the cursor
// stops on. Calling it again for the key already loaded does nothing.
func (m Model) requestExtra(force bool) (Model, tea.Cmd) {
	r, ok := m.selected()
	if !ok {
		m.extra = extraInfo{}
		return m, nil
	}
	if !force && m.extra.key == r.p.Key() {
		return m, nil
	}
	var gen uint64
	m.extraGen, gen = m.extraGen.Next()
	m.extra = extraInfo{key: r.p.Key(), loading: true}
	key, pid := r.p.Key(), r.p.PID
	return m, tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return extraStartMsg{gen, key, pid} })
}

func (m Model) loadExtraCmd(msg extraStartMsg) tea.Cmd {
	// A plain copy of pid → parent for the pane walk, so the command never
	// reads the model after Update returns.
	table := make(tmux.Procs, len(m.snap.Procs))
	for _, p := range m.snap.Procs {
		table[p.PID] = tmux.Proc{PID: p.PID, PPID: p.PPID, Command: p.Name}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		info := extraInfo{key: msg.key}
		info.path = proc.Path(msg.pid)
		info.cwd = proc.Cwd(msg.pid)
		info.args, info.argsErr = proc.ReadArgs(msg.pid)
		info.files, info.filesErr = proc.OpenFiles(ctx, msg.pid)
		if tmux.InTmux(nil) {
			panes, err := tmux.New(nil).Panes(ctx, tmux.PaneFilter{})
			if err != nil {
				info.paneErr = err
			} else {
				info.pane, info.inPane = table.PaneOf(msg.pid, panes)
			}
		}
		return extraMsg{gen: msg.gen, info: info}
	}
}

type jumpedMsg struct{ err error }

func jumpCmd(pane string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return jumpedMsg{tmux.New(nil).Jump(ctx, pane)}
	}
}
