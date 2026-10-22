package ui

import (
	"fmt"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/keyolk/tuikit/input"
	"github.com/keyolk/tuikit/keys"
	"github.com/keyolk/tuikit/mouse"
	"github.com/keyolk/tuikit/prefs"
	"github.com/keyolk/tuikit/split"
	"github.com/keyolk/tuikit/toast"
)

// Update starts the expiry tick of every toast the handlers added, in one
// place, so no handler threads a command out for it.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	before := m.toasts
	m, cmd := m.update(msg)
	return m, tea.Batch(cmd, toast.Ticks(before, m.toasts))
}

func (m Model) update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m = m.resizeLists()
	case histKickMsg:
		return m.loadHistory()
	case histLoadedMsg:
		m = m.applyHistory(msg)
		m.toasts = toast.Dismiss(m.toasts, "history")
		return m.resizeLists(), nil
	case toast.Expired:
		m.toasts = toast.Update(m.toasts, msg)
	case prefs.SaveTick:
		var cmd tea.Cmd
		m.saver, cmd = m.saver.Update(msg)
		return m, cmd
	case prefs.Saved:
		if msg.Err != nil {
			m.logf("save prefs: %v", msg.Err)
		}
		if msg.Final {
			return m, tea.Quit
		}
	case tickMsg:
		var load tea.Cmd
		if !m.past.loading && time.Since(m.past.loaded) >= histReload {
			m, load = m.loadHistory()
		}
		if m.paused {
			return m, load
		}
		m, cmd := m.startSample()
		return m, tea.Batch(cmd, load)
	case sampledMsg:
		m.sampling = m.sampling.Done(msg.id)
		next := m.nextTick()
		if msg.err != nil {
			m.logf("sample: %v", msg.err)
			m.toasts = toast.Add(m.toasts, toast.Toast{Source: "sample", Text: "sample failed: " + msg.err.Error(), Level: toast.Error})
			return m, next
		}
		for _, w := range msg.snap.Warnings {
			m.logf("sample warning: %s", w)
		}
		m = m.applySample(msg.snap)
		m = m.resizeLists()
		m, cmd := m.requestExtra(false)
		return m, tea.Batch(next, cmd)
	case extraStartMsg:
		if m.extraGen.Is(msg.gen) {
			return m, m.loadExtraCmd(msg)
		}
	case extraMsg:
		if m.extraGen.Is(msg.gen) {
			m.extra = msg.info
		}
	case killedMsg:
		lvl, text := toast.Success, fmt.Sprintf("%s sent to %s (%d)", sigName(msg.signal), msg.what, msg.ok)
		if len(msg.errs) > 0 {
			lvl = toast.Error
			text = fmt.Sprintf("%s: %d sent, %d failed — %s", sigName(msg.signal), msg.ok, len(msg.errs), msg.errs[0])
			for _, e := range msg.errs {
				m.logf("signal: %s", e)
			}
		}
		m.toasts = toast.Add(m.toasts, toast.Toast{Source: "kill", Text: text, Level: lvl})
		// Show the result now rather than at the next tick.
		return m.startSample()
	case jumpedMsg:
		if msg.err != nil {
			m.toasts = toast.Add(m.toasts, toast.Toast{Source: "tmux", Text: "tmux jump: " + msg.err.Error(), Level: toast.Error})
		}
	case tea.MouseMsg:
		return m.mouse(msg)
	case tea.KeyMsg:
		m.toasts = toast.OnKey(m.toasts)
		m.clicks = mouse.Clicks{}
		// Keys typed faster than a frame (tmux send-keys, a quick "/node")
		// arrive as one message with several runes. Outside a text field
		// each rune is its own key, so "/node" opens the filter and types.
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste && !m.filtering {
			var cmds []tea.Cmd
			for _, r := range msg.Runes {
				var cmd tea.Cmd
				m, cmd = m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: msg.Alt})
				cmds = append(cmds, cmd)
			}
			return m, tea.Batch(cmds...)
		}
		if m.filtering {
			return m.typeFilter(msg)
		}
		b, ok := m.km.Resolve(m.active(), msg, m.keyOpts())
		if !ok {
			return m, nil
		}
		return m.run(b, msg)
	}
	return m, nil
}

func (m Model) typeFilter(k tea.KeyMsg) (Model, tea.Cmd) {
	if b, ok := m.km.ResolveRaw([]keys.Ctx{ctxFilter}, k, m.keyOpts()); ok {
		return m.run(b, k)
	}
	if f, ok := m.filter.Update(k); ok {
		m.filter = f
		return m.applyQuery(f.Value())
	}
	if b, ok := m.km.ResolveRaw(m.active(), k, m.keyOpts()); ok {
		return m.run(b, k)
	}
	return m, nil
}

func (m Model) applyQuery(q string) (Model, tea.Cmd) {
	m.query = q
	m.rows = m.rows.SetItems(m.visible())
	m.past.rows = m.past.rows.SetItems(m.histVisible())
	return m.requestExtra(false)
}

// relist re-filters after a view toggle and saves the toggle.
func (m Model) relist() (Model, tea.Cmd) {
	m.rows = m.rows.SetItems(m.visible())
	var save tea.Cmd
	m.saver, save = m.saver.Save(m.uiState())
	m, extra := m.requestExtra(false)
	return m, tea.Batch(save, extra)
}

func (m Model) run(b keys.Binding, k tea.KeyMsg) (Model, tea.Cmd) {
	if mm, cmd, ok := m.runHistory(b.Action); ok {
		return mm.resizeLists(), cmd
	}
	g := m.geometry()
	prevSel := m.rows.SelectedID()
	var cmd tea.Cmd
	switch b.Action {
	case "quit":
		m.saver, cmd = m.saver.FlushWith(m.uiState())
		return m, cmd
	case "help":
		m.help, m.helpOff = true, 0
		m.toasts = toast.Hold(m.toasts)
	case "help.close":
		m.help = false
		m.toasts, _ = toast.Release(m.toasts)
	case "help.down":
		m.helpOff = m.clampHelp(m.helpOff + 1)
	case "help.up":
		m.helpOff = m.clampHelp(m.helpOff - 1)
	case "pause":
		m.paused = !m.paused
		if !m.paused {
			return m.startSample()
		}
	case "refresh":
		return m.startSample()

	case "list.down":
		m.rows = m.rows.Move(1)
	case "list.up":
		m.rows = m.rows.Move(-1)
	case "list.top":
		m.rows = m.rows.Top()
	case "list.bottom":
		m.rows = m.rows.Bottom()
	case "list.halfdown":
		m.rows = m.rows.HalfDown()
	case "list.halfup":
		m.rows = m.rows.HalfUp()
	case "list.parent":
		r, _ := m.selected()
		if pp, ok := m.byPID[r.p.PPID]; ok {
			var found bool
			if m.rows, found = m.rows.Select(pp.Key()); !found {
				m.toasts = toast.Add(m.toasts, toast.Toast{Source: "nav", Text: fmt.Sprintf("parent %s (%d) is hidden by the filter", pp.Name, pp.PID), Level: toast.Info})
			}
		}
	case "list.focus":
		m.focusDetail = true
	case "detail.close":
		m.focusDetail = false
	case "list.filter":
		m.filtering = true
		m.filter = input.New(m.query)
	case "filter.apply":
		m.filtering = false
	case "filter.clear":
		m.filtering = false
		m.filter = input.Field{}
		return m.applyQuery("")
	case "list.sort":
		m.sortBy = m.sortBy.next()
		return m.relist()
	case "list.flagged":
		m.flagged = !m.flagged
		return m.relist()
	case "list.mine":
		m.mine = !m.mine
		return m.relist()

	case "tab.next", "tab.prev", "tab.jump":
		switch b.Action {
		case "tab.next":
			m.tab = (m.tab + 1) % len(tabNames)
		case "tab.prev":
			m.tab = (m.tab + len(tabNames) - 1) % len(tabNames)
		default:
			m.tab = b.KeyIndex(k)
		}
		m.detailOff = 0
		m.saver, cmd = m.saver.Save(m.uiState())
		return m, cmd
	case "secrets":
		m.secrets = !m.secrets
	case "detail.down":
		m.detailOff = m.clampDetail(m.detailOff+1, g)
	case "detail.up":
		m.detailOff = m.clampDetail(m.detailOff-1, g)
	case "detail.halfdown":
		m.detailOff = m.clampDetail(m.detailOff+g.detail.H/2, g)
	case "detail.halfup":
		m.detailOff = m.clampDetail(m.detailOff-g.detail.H/2, g)
	case "detail.top":
		m.detailOff = 0
	case "detail.bottom":
		m.detailOff = m.clampDetail(1<<30, g)

	case "tmux.jump":
		if !m.extra.inPane {
			m.toasts = toast.Add(m.toasts, toast.Toast{Source: "tmux", Text: "not running under a tmux pane", Level: toast.Info})
			return m, nil
		}
		return m, jumpCmd(m.extra.pane.ID)
	case "kill":
		r, _ := m.selected()
		plan, err := m.planFor(r)
		if err != nil {
			m.toasts = toast.Add(m.toasts, toast.Toast{Source: "kill", Text: err.Error(), Level: toast.Warn})
			return m, nil
		}
		m.confirm = plan
		m.toasts = toast.Hold(m.toasts)
	case "confirm.cancel", "confirm.term", "confirm.kill", "confirm.tree", "confirm.same", "confirm.stop":
		plan := m.confirm
		m.confirm = nil
		m.toasts, _ = toast.Release(m.toasts)
		switch b.Action {
		case "confirm.term":
			cmd = signalCmd(plan.self.name, []target{plan.self}, syscall.SIGTERM)
		case "confirm.kill":
			cmd = signalCmd(plan.self.name, []target{plan.self}, syscall.SIGKILL)
		case "confirm.tree":
			cmd = signalCmd(plan.self.name+" tree", plan.tree, syscall.SIGTERM)
		case "confirm.same":
			cmd = signalCmd("every "+plan.self.name, plan.same, syscall.SIGTERM)
		case "confirm.stop":
			sig := syscall.SIGSTOP
			if plan.stopped {
				sig = syscall.SIGCONT
			}
			cmd = signalCmd(plan.self.name, []target{plan.self}, sig)
		}
		return m, cmd

	case split.ActionGrow, split.ActionShrink, split.ActionReset:
		var res split.Result
		m.split, res = m.split.Handle(b.Action, g.body)
		if res.Changed {
			m.saver, cmd = m.saver.Save(m.uiState())
		}
		return m.resizeLists(), cmd
	}
	if m.rows.SelectedID() != prevSel {
		m.moved = true
		m.detailOff = 0
		return m.requestExtra(false)
	}
	return m, nil
}

// mouse: the split takes divider presses first; a click selects a row and a
// second click on the selected row focuses the detail (the mouse Enter); the
// wheel scrolls whichever pane it is over.
func (m Model) mouse(msg tea.MouseMsg) (Model, tea.Cmd) {
	if m.help || m.confirm != nil {
		return m, nil // overlays own the mouse
	}
	g := m.geometry()
	sp, res := m.split.Update(msg, g.body)
	m.split = sp
	if res.Consumed {
		var cmd tea.Cmd
		if res.Changed {
			m.saver, cmd = m.saver.Save(m.uiState())
			m = m.resizeLists()
		}
		return m, cmd
	}
	if m.past.on {
		return m.mouseHistory(msg, g), nil
	}
	prevSel := m.rows.SelectedID()
	switch {
	case g.detail.Contains(msg.X, msg.Y):
		if d := mouse.Delta(msg); d != 0 {
			m.detailOff = m.clampDetail(m.detailOff+d, g)
		} else if mouse.IsLeftPress(msg) {
			m.focusDetail = true
		}
		return m, nil
	case g.listBody.Contains(msg.X, msg.Y):
		if l, ok := mouse.MoveByWheel(m.rows, msg); ok {
			m.rows = l
			break
		}
		if !mouse.IsLeftPress(msg) {
			return m, nil
		}
		hit := mouse.ListHit(m.rows, g.listBody, msg)
		if !hit.HasItem {
			return m, nil
		}
		id := m.rows.At(hit.Index).ID()
		var double bool
		m.clicks, double = m.clicks.Press(id, time.Now())
		m.rows = m.rows.SelectIndex(hit.Index)
		m.focusDetail = double && id == prevSel
	}
	if m.rows.SelectedID() != prevSel {
		m.moved = true
		m.detailOff = 0
		return m.requestExtra(false)
	}
	return m, nil
}

func (m Model) clampDetail(off int, g geom) int {
	n := len(m.detailLines(g.detail.W))
	return max(0, min(off, n-g.detail.H))
}

// resizeLists gives both lists the rows the layout leaves them; the history
// list has a title row above its column header.
func (m Model) resizeLists() Model {
	g := m.geometry()
	m.rows = m.rows.SetHeight(g.listRows)
	m.past.rows = m.past.rows.SetHeight(max(g.listRows-1, 1))
	return m
}

func (m Model) mouseHistory(msg tea.MouseMsg, g geom) Model {
	switch {
	case g.detail.Contains(msg.X, msg.Y):
		if d := mouse.Delta(msg); d != 0 {
			m.detailOff = m.clampDetail(m.detailOff+d, g)
		} else if mouse.IsLeftPress(msg) {
			m.focusDetail = true
		}
	case g.listBody.Contains(msg.X, msg.Y):
		prev := m.past.rows.SelectedID()
		if l, ok := mouse.MoveByWheel(m.past.rows, msg); ok {
			m.past.rows = l
		} else if mouse.IsLeftPress(msg) {
			_, body := g.listBody.SplitTop(1) // the title row
			hit := mouse.ListHit(m.past.rows, body, msg)
			if hit.HasItem {
				id := m.past.rows.At(hit.Index).ID()
				var double bool
				m.clicks, double = m.clicks.Press(id, time.Now())
				m.past.rows = m.past.rows.SelectIndex(hit.Index)
				m.focusDetail = double && id == prev
			}
		}
		if m.past.rows.SelectedID() != prev {
			m.detailOff = 0
		}
	}
	return m
}
