package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/keyolk/tuikit/chart"
	"github.com/keyolk/tuikit/input"
	"github.com/keyolk/tuikit/keys"
	"github.com/keyolk/tuikit/mouse"
	"github.com/keyolk/tuikit/prefs"
	"github.com/keyolk/tuikit/render"
	"github.com/keyolk/tuikit/split"
	"github.com/keyolk/tuikit/toast"

	"github.com/keyolk/sysx/internal/outlier"
	"github.com/keyolk/sysx/internal/proc"
)

const headerRows = 3

// geom is the layout, computed in one place so View, the key handlers and
// the mouse hit tests agree.
type geom struct {
	toasts            string
	body              mouse.Rect
	list, div, detail mouse.Rect
	listBody          mouse.Rect // the rows, under the column header
	listRows          int
	sp                split.Model
}

// laidOut is the split for this width: side by side when there is room,
// stacked otherwise. Orientation is derived, not saved, so a narrow tmux
// pane does not overwrite the wide layout's ratio semantics.
func (m Model) laidOut() split.Model {
	sp := m.split
	if m.w < 110 {
		sp.Orientation, sp.MinA, sp.MinB, sp.MaxA = split.Vertical, 6, 8, 0
	}
	return sp
}

func (m Model) geometry() geom {
	g := geom{toasts: toast.View(m.toasts, m.w, m.th.ToastStyles()), sp: m.laidOut()}
	bodyH := m.h - headerRows - 1
	if g.toasts != "" {
		bodyH -= lipgloss.Height(g.toasts)
	}
	g.body = mouse.Rect{X: 0, Y: headerRows, W: m.w, H: max(bodyH, 0)}
	g.list, g.div, g.detail = g.sp.Layout(g.body)
	_, g.listBody = g.list.SplitTop(1)
	g.listRows = max(g.listBody.H-1, 1) // the last row is the "↓ n more" line
	g.listBody.H = g.listRows
	if g.detail.H > 1 {
		_, g.detail = g.detail.SplitTop(1) // the tab bar
	}
	return g
}

// View draws the frame; overlays splice over it.
func (m Model) View() string {
	if m.w < 50 || m.h < 12 {
		return render.FitLines(fmt.Sprintf("sysx needs at least 50×12 (now %d×%d)", m.w, m.h), m.w, m.h)
	}
	g := m.geometry()
	header := render.Safe("header", m.w, headerRows, m.logf, m.viewHeader)
	listView := render.Safe("list", g.list.W, g.list.H, m.logf, func() string { return m.viewList(g) })
	detailView := render.Safe("detail", g.detail.W, g.detail.H+1, m.logf, func() string { return m.viewDetail(g) })
	divSt := m.th.Border
	if m.focusDetail {
		divSt = m.th.BorderFocus
	}
	body := g.sp.Join(g.body, listView, g.sp.DrawDivider(g.div, divSt), detailView)

	fs := m.th.FooterStyle()
	right := fmt.Sprintf("%d/%d", m.rows.Index()+1, m.rows.Len())
	if m.past.on {
		right = fmt.Sprintf("%d/%d", m.past.rows.Index()+1, m.past.rows.Len())
	}
	if m.paused {
		right = "paused " + right
	}
	fs.Right = m.th.Muted.Render(" " + right)
	footer := m.km.Footer(m.active(), m.w, m.keyOpts(), fs)

	parts := []string{header, body}
	if g.toasts != "" {
		parts = append(parts, g.toasts)
	}
	frame := strings.Join(append(parts, footer), "\n")
	switch {
	case m.help:
		return render.Center(render.Dim(frame), m.viewHelp(), m.w, m.h)
	case m.confirm != nil:
		return render.Center(render.Dim(frame), m.viewConfirm(), m.w, m.h)
	}
	return frame
}

func (m Model) viewHeader() string {
	s := m.snap.Sys
	look := m.look
	bar := func(frac float64, w int, label string) string {
		return chart.Bar(frac, w, chart.BarOpts{Look: look, Warn: chart.Fixed(0.75), Error: chart.Fixed(0.92), Percent: true, Label: label})
	}
	spark := func(xs []float64, w int, hi float64) string { return spark(xs, w, hi, look) }
	barW := min(30, max(14, m.w/5))
	sparkW := min(40, max(8, m.w/6))

	load := fmt.Sprintf("load %.1f %.1f %.1f / %d cpu", s.Load[0], s.Load[1], s.Load[2], s.NCPU)
	switch {
	case s.NCPU > 0 && s.Load[0] >= float64(s.NCPU)*2:
		load = m.th.Error.Render(load)
	case s.NCPU > 0 && s.Load[0] >= float64(s.NCPU):
		load = m.th.Warn.Render(load)
	}
	cpu := m.th.Title.Render("sysx") + "  cpu " + bar((100-s.CPUIdle)/100, barW, "") + " " + spark(m.sys.cpu, sparkW, 100) + "  " + load

	memFrac := 0.0
	if s.MemTotal > 0 {
		memFrac = float64(s.MemUsed) / float64(s.MemTotal)
	}
	pressure := "pressure " + s.Pressure.String()
	switch s.Pressure {
	case proc.PressureWarn:
		pressure = m.th.Warn.Render(pressure)
	case proc.PressureCritical:
		pressure = m.th.Error.Render(pressure)
	}
	swap := fmt.Sprintf("swap %s/%s", outlier.Bytes(s.SwapUsed), outlier.Bytes(s.SwapTotal))
	if s.SwapOuts > 0 {
		swap += m.th.Warn.Render(fmt.Sprintf(" out %.0f/s", s.SwapOuts))
	}
	mem := "      mem " + bar(memFrac, barW, "") + " " + spark(m.sys.mem, sparkW, 100) +
		fmt.Sprintf("  %s/%s  ", outlier.Bytes(s.MemUsed), outlier.Bytes(s.MemTotal)) + pressure + "  " + swap +
		m.th.Muted.Render(fmt.Sprintf("  compressed %s", outlier.Bytes(s.MemCompressed)))

	var vols []string
	for _, v := range s.Volumes {
		if v.Total == 0 {
			continue
		}
		used := 1 - float64(v.Free)/float64(v.Total)
		name := strings.TrimPrefix(strings.TrimPrefix(v.Mount, "/System/Volumes/"), "/Volumes/")
		t := fmt.Sprintf("%s %.0f%% (%s free)", name, used*100, outlier.Bytes(v.Free))
		switch {
		case used >= 0.95:
			t = m.th.Error.Render(t)
		case used >= 0.85:
			t = m.th.Warn.Render(t)
		}
		vols = append(vols, t)
	}
	alerts, warns := 0, 0
	for _, r := range m.reports {
		switch r.Sev {
		case outlier.Alert:
			alerts++
		case outlier.Warn:
			warns++
		}
	}
	flags := m.th.Muted.Render("no outliers")
	if alerts+warns > 0 {
		flags = m.th.Error.Render(fmt.Sprintf("%d alert", alerts)) + m.th.Muted.Render(" · ") + m.th.Warn.Render(fmt.Sprintf("%d warn", warns))
	}
	io := fmt.Sprintf("     disk r %s w %s ", rateStr(s.DiskRead), rateStr(s.DiskWrite)) + spark(m.sys.diskIO, sparkW/2, 0) +
		fmt.Sprintf("  net ↓%s ↑%s ", rateStr(s.NetIn), rateStr(s.NetOut)) + spark(m.sys.netIO, sparkW/2, 0) +
		"  " + strings.Join(vols, "  ") + "  " + flags
	if m.snap.At.IsZero() {
		io = m.th.Muted.Render("     sampling…")
	}
	return strings.Join([]string{render.Truncate(cpu, m.w), render.Truncate(mem, m.w), render.Truncate(io, m.w)}, "\n")
}

func rateStr(v float64) string {
	if v < 1 {
		return "0"
	}
	return outlier.Bytes(uint64(v)) + "/s"
}

// column is one list column; drop is the order columns leave a narrow pane
// (higher first, 0 never).
type column struct {
	title string
	w     int
	right bool
	drop  int
}

func (m Model) columns(w int) (cols []column, nameW int) {
	all := []column{
		{"PID", 6, true, 0},
		{"CPU%", 6, true, 0},
		{"MEM", 6, true, 0},
		{"DISK", 7, true, 3},
		{"NET", 7, true, 4},
		{"THR", 4, true, 5},
		{"USER", 9, false, 6},
		{"TAGS", 9, false, 2},
	}
	const markW, minName = 2, 12
	keep := func(level int) (cs []column, used int) {
		for _, c := range all {
			if c.drop != 0 && c.drop >= level {
				continue
			}
			cs = append(cs, c)
			used += c.w + 1
		}
		return cs, used
	}
	for level := 99; level >= 1; level-- {
		cs, used := keep(level)
		if level == 1 || w-markW-used >= minName {
			return cs, max(w-markW-used, 4)
		}
	}
	return nil, w
}

func inputStyle(m Model) input.Style { return input.Style{Placeholder: m.th.Subtle} }

func (m Model) viewList(g geom) string {
	if m.past.on {
		return m.viewHistory(g)
	}
	w := g.list.W
	cols, nameW := m.columns(w)
	head := "  " + render.Pad("NAME", nameW)
	for _, c := range cols {
		if c.right {
			head += " " + render.PadLeft(c.title, c.w)
		} else {
			head += " " + render.Pad(c.title, c.w)
		}
	}
	sortTitle := strings.ToUpper(m.sortBy.String())
	head = strings.Replace(head, sortTitle, sortTitle+"↓", 1)
	lines := []string{m.th.Bold.Render(render.Truncate(head, w))}
	if m.filtering {
		// The column header gives way to the field while typing.
		lines[0] = m.th.Key.Render("/ ") + m.filter.View(w-2, true, input.Style{Placeholder: m.th.Subtle})
	}
	if m.rows.Len() == 0 {
		msg := "no processes yet"
		if m.query != "" || m.flagged || m.mine {
			msg = "nothing matches — / filter, o flagged, m mine"
		}
		return strings.Join(append(lines, m.th.Subtle.Render(render.Truncate(msg, w))), "\n")
	}
	start, end := m.rows.Window()
	cur := m.rows.Index()
	for i := start; i < end; i++ {
		r := m.rows.At(i)
		sel := i == cur
		lines = append(lines, m.listLine(r, cols, nameW, w, sel))
	}
	lines = append(lines, m.th.Subtle.Render(m.rows.MoreLine()))
	return strings.Join(lines, "\n")
}

func (m Model) sevStyle(s outlier.Sev) func(string) string {
	switch s {
	case outlier.Alert:
		return func(t string) string { return m.th.Error.Bold(true).Render(t) }
	case outlier.Warn:
		return func(t string) string { return m.th.Warn.Render(t) }
	case outlier.Note:
		return func(t string) string { return m.th.Info.Render(t) }
	}
	return func(t string) string { return t }
}

// sevMark leads each row, so the severity reads without color.
func (m Model) sevMark(s outlier.Sev) string {
	g := m.look.Glyphs
	switch s {
	case outlier.Alert:
		return g.Error
	case outlier.Warn:
		return g.Warn
	case outlier.Note:
		return "."
	}
	return " "
}

func tags(r outlier.Report) string {
	var out []string
	seen := map[string]bool{}
	for _, f := range r.Findings {
		if f.Tag != "" && !seen[f.Tag] {
			seen[f.Tag] = true
			out = append(out, f.Tag)
		}
	}
	return strings.Join(out, " ")
}

func (m Model) listLine(r row, cols []column, nameW, w int, sel bool) string {
	p := r.p
	style := func(d outlier.Dim, t string) string {
		if sel {
			return t
		}
		return m.sevStyle(r.r.SevOf(d))(t)
	}
	name := p.Name
	if p.State == proc.StateZombie {
		name += " <defunct>"
	}
	nameCell := render.Pad(render.Truncate(name, nameW), nameW)
	if !sel {
		switch {
		case r.r.Sev >= outlier.Warn:
			nameCell = m.th.Bold.Render(nameCell)
		case r.r.Sev == outlier.None && p.CPU < 0.5:
			nameCell = m.th.Muted.Render(nameCell)
		}
	}
	mark := m.sevMark(r.r.Sev)
	if !sel {
		mark = m.sevStyle(r.r.Sev)(mark)
	}
	line := mark + " " + nameCell
	for _, c := range cols {
		var cell string
		var dim outlier.Dim
		switch c.title {
		case "PID":
			cell = fmt.Sprint(p.PID)
		case "CPU%":
			cell, dim = fmt.Sprintf("%.1f", p.CPU), outlier.CPU
		case "MEM":
			cell, dim = outlier.Bytes(p.Footprint), outlier.Mem
		case "DISK":
			cell, dim = rateCell(p.DiskRead+p.DiskWrite, p.Partial), outlier.Disk
		case "NET":
			cell, dim = rateCell(p.NetIn+p.NetOut, false), outlier.Net
		case "THR":
			cell, dim = fmt.Sprint(p.Threads), outlier.Thread
			if p.Partial {
				cell = "-"
			}
		case "USER":
			cell = p.User
		case "TAGS":
			cell = tags(r.r)
		}
		if c.right {
			cell = render.PadLeft(render.Truncate(cell, c.w), c.w)
		} else {
			cell = render.Pad(render.Truncate(cell, c.w), c.w)
		}
		switch {
		case dim != "":
			cell = style(dim, cell)
		case c.title == "TAGS":
			cell = style(outlier.Group, cell)
			if r.r.SevOf(outlier.State) > r.r.SevOf(outlier.Group) {
				cell = style(outlier.State, render.Pad(render.Truncate(tags(r.r), c.w), c.w))
			}
		case !sel && c.title != "PID":
			cell = m.th.Muted.Render(cell)
		}
		line += " " + cell
	}
	if sel {
		return m.th.Selected.Render(render.Pad(render.Truncate(line, w), w))
	}
	return render.Truncate(line, w)
}

func rateCell(v float64, unknown bool) string {
	switch {
	case unknown:
		return "-"
	case v < 1024:
		return "."
	}
	return outlier.Bytes(uint64(v))
}

func (m Model) helpBox() (w, h int) { return min(70, m.w-4), min(30, m.h-4) }

func (m Model) helpSections() []keys.Section {
	secs := m.km.Help(m.keyOpts(), Stacks[0], Stacks[1])
	return append(secs, keys.Section{Title: "Reading the list", Notes: []string{
		"Rows are ordered by how abnormal a process looks. The mark leads each row: " +
			m.look.Glyphs.Error + " alert, " + m.look.Glyphs.Warn + " warn, . note; a metric cell in color is the reason.",
		"CPU% is one core = 100, averaged over the last sample. MEM is the footprint Activity Monitor shows.",
		"Rules look at about 15s of history, so a process that just started is not judged yet. " +
			"TAGS: ×N many copies of one program, tree a parent whose children add up, leak steady growth, orph orphaned, zomb zombie.",
		"Another user's process (root daemons) shows - for disk and threads; run with sudo to see everything.",
		"State is saved to " + prefs.Path(App) + ".",
	}})
}

func (m Model) clampHelp(off int) int {
	w, h := m.helpBox()
	cw, ch := render.BoxContent(w, h, m.th.BoxFocus)
	return max(0, min(off, keys.HelpLineCount(m.helpSections(), cw)-ch))
}

func (m Model) viewHelp() string {
	w, h := m.helpBox()
	cw, ch := render.BoxContent(w, h, m.th.BoxFocus)
	lines, _ := keys.RenderHelp(m.helpSections(), cw, ch, m.helpOff, m.th.HelpStyle())
	return render.Box("sysx keys", strings.Join(lines, "\n"), w, h, m.th.BoxFocus)
}

func (m Model) viewConfirm() string {
	p := m.confirm
	kn := m.th.KeyNames()
	key := func(action string) string {
		// Opts without Enabled: a disabled line still names its key.
		k, ok := m.km.KeyFor(Stacks[3], action, keys.Opts{})
		if !ok {
			return " "
		}
		return kn.Name(k)
	}
	line := func(action, text string) string {
		k := m.th.Key.Render(render.Pad(key(action), 4))
		if !m.enabled(action) {
			return m.th.Subtle.Render(render.Pad(key(action), 4) + text)
		}
		return k + text
	}
	stop := "SIGSTOP — pause it, resume later with the same key"
	if p.stopped {
		stop = "SIGCONT — resume it"
	}
	descendants := len(p.tree) - 1
	body := []string{
		m.th.Bold.Render(fmt.Sprintf("%s (pid %d, %s)", p.self.name, p.self.pid, p.user)),
		"",
		line("confirm.term", "SIGTERM — ask it to exit"),
		line("confirm.kill", "SIGKILL — force, cannot be caught"),
		line("confirm.tree", fmt.Sprintf("SIGTERM it and %d descendants", descendants)),
	}
	if descendants > 0 {
		body = append(body, m.th.Muted.Render("    "+names(p.tree[1:])))
	}
	body = append(body,
		line("confirm.same", fmt.Sprintf("SIGTERM all %d '%s' of %s", len(p.same), p.self.name, p.user)),
		line("confirm.stop", stop),
		"",
		line("confirm.cancel", "cancel"),
	)
	if p.excluded > 0 {
		body = append(body, "", m.th.Muted.Render(fmt.Sprintf("%d process(es) running sysx itself are left out.", p.excluded)))
	}
	w := min(64, m.w-4)
	h := len(body) + 2
	cw, _ := render.BoxContent(w, h, m.th.BoxFocus)
	for i := range body {
		body[i] = render.Truncate(body[i], cw)
	}
	return render.Box("signal", strings.Join(body, "\n"), w, h, m.th.BoxFocus)
}

// spark draws a zero-based sparkline. A series that never left zero sits
// on the floor; chart draws a constant series at mid height otherwise.
func spark(xs []float64, w int, hi float64, look chart.Look) string {
	o := chart.SparkOpts{Look: look, Agg: chart.Max, Min: chart.Fixed(0), Right: true}
	if hi <= 0 {
		for _, v := range xs {
			hi = max(hi, v)
		}
		if hi <= 0 {
			hi = 1
		}
	}
	o.Max = chart.Fixed(hi)
	return chart.Sparkline(xs, w, o)
}
