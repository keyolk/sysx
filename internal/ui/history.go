package ui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/keyolk/tuikit/chart"
	"github.com/keyolk/tuikit/list"
	"github.com/keyolk/tuikit/render"
	"github.com/keyolk/tuikit/toast"

	"github.com/keyolk/sysx/internal/history"
	"github.com/keyolk/sysx/internal/outlier"
)

// The history the live view cross-references is always the last day; the
// history view's own window is chosen with w.
var histWindows = []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour, 3 * 24 * time.Hour, 7 * 24 * time.Hour}

var histWindowNames = []string{"1h", "6h", "24h", "3d", "7d"}

const (
	defaultHistWindow = 1 // 6h
	histLiveWindow    = 24 * time.Hour
	histReload        = time.Minute
)

// histState is the history view and the loaded log.
type histState struct {
	on      bool
	rows    list.List[history.Episode]
	all     []history.Episode // the loaded window, newest first
	window  int               // index into histWindows
	sortBy  int               // index into history.SortKeys
	gen     uint64
	loading bool
	loaded  time.Time
	err     error

	recPID    int
	bytes     int64
	days      int
	sinceLoad time.Duration // the window the last load covered
}

type histLoadedMsg struct {
	gen    uint64
	since  time.Duration
	eps    []history.Episode
	err    error
	recPID int
	bytes  int64
	days   int
}

// loadHistory reads the log off the loop. It loads the larger of the view's
// window and the day the live view cross-references.
func (m Model) loadHistory() (Model, tea.Cmd) {
	m.past.gen++
	m.past.loading = true
	gen := m.past.gen
	since := max(histWindows[m.past.window], histLiveWindow)
	dir := m.histDir
	return m, func() tea.Msg {
		st := history.Store{Dir: dir}
		eps, err := st.Load(time.Now().Add(-since))
		pid, _ := history.RecorderPID(dir)
		bytes, days, _ := st.Size()
		return histLoadedMsg{gen: gen, since: since, eps: eps, err: err, recPID: pid, bytes: bytes, days: days}
	}
}

func (m Model) applyHistory(msg histLoadedMsg) Model {
	if msg.gen != m.past.gen {
		return m
	}
	h := m.past
	h.loading, h.loaded, h.err = false, time.Now(), msg.err
	h.all, h.recPID, h.bytes, h.days, h.sinceLoad = msg.eps, msg.recPID, msg.bytes, msg.days, msg.since
	m.past = h
	m.past.rows = m.past.rows.SetItems(m.histVisible())
	if msg.err != nil {
		m.logf("history: %v", msg.err)
	}
	return m
}

func (m Model) histVisible() []history.Episode {
	since := time.Now().Add(-histWindows[m.past.window])
	q := strings.ToLower(strings.TrimSpace(m.query))
	var out []history.Episode
	for _, e := range m.past.all {
		if e.End.Before(since) {
			continue
		}
		if q != "" && !history.Matches(e, q) {
			continue
		}
		out = append(out, e)
	}
	history.Sort(out, history.SortKeys[m.past.sortBy])
	return out
}

// pastOf sums what the log holds about a program (same user and name) over
// the last day, for the live view's detail.
func (m Model) pastOf(user, name string) (n int, cpuSec float64, busy time.Duration, last time.Time) {
	since := time.Now().Add(-histLiveWindow)
	for _, e := range m.past.all {
		if e.User != user || e.Name != name || e.End.Before(since) {
			continue
		}
		n++
		cpuSec += e.TotalCPUSec()
		busy += e.Duration()
		if e.End.After(last) {
			last = e.End
		}
	}
	return n, cpuSec, busy, last
}

// runHistory handles the actions the history view gives its own meaning.
func (m Model) runHistory(action string) (Model, tea.Cmd, bool) {
	h := m.past
	switch action {
	case "view.history":
		m.past.on, m.focusDetail, m.detailOff = true, false, 0
		m.past.rows = m.past.rows.SetHeight(m.geometry().listRows)
		m.past.rows = m.past.rows.SetItems(m.histVisible())
		m, cmd := m.loadHistory()
		return m, cmd, true
	case "view.live":
		m.past.on, m.focusDetail, m.detailOff = false, false, 0
		return m, nil, true
	}
	if !h.on {
		return m, nil, false
	}
	prev := h.rows.SelectedID()
	switch action {
	case "history.window":
		m.past.window = (h.window + 1) % len(histWindows)
		m.past.rows = m.past.rows.SetItems(m.histVisible())
		var save, load tea.Cmd
		m.saver, save = m.saver.Save(m.uiState())
		if histWindows[m.past.window] > h.sinceLoad {
			m, load = m.loadHistory()
		}
		return m, tea.Batch(save, load), true
	case "list.sort":
		m.past.sortBy = (h.sortBy + 1) % len(history.SortKeys)
		m.past.rows = m.past.rows.SetItems(m.histVisible())
		var save tea.Cmd
		m.saver, save = m.saver.Save(m.uiState())
		return m, save, true
	case "refresh":
		var cmd tea.Cmd
		m, cmd = m.loadHistory()
		m.toasts = toast.Add(m.toasts, toast.Toast{Source: "history", Text: "reloading history…", Level: toast.Progress})
		return m, cmd, true
	case "list.down":
		m.past.rows = h.rows.Move(1)
	case "list.up":
		m.past.rows = h.rows.Move(-1)
	case "list.top":
		m.past.rows = h.rows.Top()
	case "list.bottom":
		m.past.rows = h.rows.Bottom()
	case "list.halfdown":
		m.past.rows = h.rows.HalfDown()
	case "list.halfup":
		m.past.rows = h.rows.HalfUp()
	default:
		return m, nil, false
	}
	if m.past.rows.SelectedID() != prev {
		m.detailOff = 0
	}
	return m, nil, true
}

func (m Model) recorderLine() string {
	h := m.past
	if h.recPID == 0 {
		return m.th.Warn.Render("recorder not running — sysx record --install")
	}
	return m.th.Muted.Render(fmt.Sprintf("recorder pid %d · %d days · %s on disk", h.recPID, h.days, outlier.Bytes(uint64(h.bytes))))
}

func (m Model) viewHistory(g geom) string {
	w := g.list.W
	h := m.past
	whenW, forW, cpuW, pctW, memW, ioW := 11, 6, 8, 5, 6, 6
	fixed := 2 + whenW + 1 + forW + 1 + cpuW + 1 + pctW + 1 + pctW + 1 + memW + 1 + ioW + 1 + ioW + 1
	// Disk and net leave first, then the name shrinks to 10.
	showIO := w >= fixed+14
	if !showIO {
		fixed -= 2 * (ioW + 1)
	}
	nameW := max(w-fixed, 10)
	head := "  " + render.Pad("WHEN", whenW) + " " + render.PadLeft("FOR", forW) + " " + render.Pad("NAME", nameW) + " " +
		render.PadLeft("CPU-TIME", cpuW) + " " + render.PadLeft("AVG%", pctW) + " " + render.PadLeft("PEAK", pctW) + " " +
		render.PadLeft("MEM", memW)
	if showIO {
		head += " " + render.PadLeft("DISK", ioW) + " " + render.PadLeft("NET", ioW)
	}
	sortTitle := map[string]string{"time": "WHEN", "cpu": "CPU-TIME", "mem": "MEM", "disk": "DISK", "net": "NET"}[history.SortKeys[h.sortBy]]
	head = strings.Replace(head, sortTitle, sortTitle+"↓", 1)
	lines := []string{m.th.Bold.Render(render.Truncate(head, w))}
	if m.filtering {
		lines[0] = m.th.Key.Render("/ ") + m.filter.View(w-2, true, inputStyle(m))
	}
	title := m.th.Accent.Bold(true).Render(fmt.Sprintf("history · last %s", histWindowNames[h.window]))
	lines = append([]string{render.Truncate(title+"  "+m.recorderLine(), w)}, lines...)
	if h.rows.Len() == 0 {
		msg := "nothing recorded in this window"
		switch {
		case h.loading && h.loaded.IsZero():
			msg = "loading…"
		case h.err != nil:
			msg = h.err.Error()
		case m.query != "":
			msg = "nothing matches — / to change the filter"
		}
		return strings.Join(append(lines, m.th.Subtle.Render(render.Truncate(msg, w))), "\n")
	}
	start, end := h.rows.Window()
	cur := h.rows.Index()
	for i := start; i < end; i++ {
		e := h.rows.At(i)
		name := e.Name
		if e.Kind == history.KindGroup {
			name = fmt.Sprintf("%s ×%d", e.Name, e.Procs)
		}
		when := e.Start.Local().Format("01-02 15:04")
		if time.Since(e.End) < 2*m.interval+time.Minute && e.Cont {
			when = "now  " + e.Start.Local().Format("15:04")
		}
		line := "  " + render.Pad(when, whenW) + " " + render.PadLeft(history.Dur(e.Duration()), forW) + " " +
			render.Pad(render.Truncate(name, nameW), nameW) + " " +
			render.PadLeft(history.Dur(time.Duration(e.TotalCPUSec()*float64(time.Second))), cpuW) + " " +
			render.PadLeft(fmt.Sprintf("%.0f", e.AvgCPU()), pctW) + " " + render.PadLeft(fmt.Sprintf("%.0f", e.PeakCPU), pctW) + " " +
			render.PadLeft(outlier.Bytes(e.PeakMem), memW)
		if showIO {
			line += " " + render.PadLeft(ioCell(e.DiskBytes), ioW) + " " + render.PadLeft(ioCell(e.NetBytes), ioW)
		}
		if i == cur {
			line = m.th.Selected.Render(render.Pad(render.Truncate(line, w), w))
		} else {
			line = render.Truncate(line, w)
		}
		lines = append(lines, line)
	}
	lines = append(lines, m.th.Subtle.Render(h.rows.MoreLine()))
	return strings.Join(lines, "\n")
}

func ioCell(b uint64) string {
	if b < 1<<20 {
		return "."
	}
	return outlier.Bytes(b)
}

func (m Model) episodeLines(w int) []string {
	e, ok := m.past.rows.Selected()
	if !ok {
		return []string{m.th.Subtle.Render("no episode selected")}
	}
	kv := func(k, v string) string { return m.th.Muted.Render(render.Pad(k, 7)) + v }
	name := e.Name
	what := fmt.Sprintf("pid %d · %s", e.PID, e.User)
	if e.Kind == history.KindGroup {
		name = fmt.Sprintf("%s ×%d", e.Name, e.Procs)
		what = fmt.Sprintf("%d processes, at most %d at once · %s", e.Procs, e.PeakProcs, e.User)
	}
	end := e.End.Local().Format("15:04:05")
	switch {
	case e.Cont:
		end += " (still going)"
	case e.Exited:
		end += " (exited)"
	default:
		end += " (went quiet)"
	}
	lines := []string{
		m.th.Bold.Render(name) + m.th.Muted.Render("  "+what),
		kv("when", e.Start.Local().Format("2006-01-02 15:04:05")+" → "+end+m.th.Muted.Render("  "+history.Dur(e.Duration()))),
		kv("why", strings.Join(e.Why, ", ")),
		"",
		m.section("cost"),
		kv("cpu", fmt.Sprintf("%s of CPU time · average %.0f%% · peak %.0f%%",
			history.Dur(time.Duration(e.TotalCPUSec()*float64(time.Second))), e.AvgCPU(), e.PeakCPU)),
	}
	if e.ChildCPUSec >= 1 {
		lines = append(lines, kv("", m.th.Muted.Render(fmt.Sprintf("%s of it by children that already exited (a build driver's compilers)",
			history.Dur(time.Duration(e.ChildCPUSec*float64(time.Second)))))))
	}
	lines = append(lines, kv("mem", "peak "+outlier.Bytes(e.PeakMem)))
	if e.Partial {
		lines = append(lines, kv("disk", m.th.Muted.Render("unknown (another user's process)")))
	} else {
		lines = append(lines, kv("disk", outlier.Bytes(e.DiskBytes)))
	}
	lines = append(lines, kv("net", outlier.Bytes(e.NetBytes)))
	if len(e.Series) > 1 {
		xs := make([]float64, len(e.Series))
		for i, v := range e.Series {
			xs[i] = float64(v)
		}
		// Stretched: a short stretch has few samples, and they are the
		// whole story, not the newest end of a longer one.
		hi := 1.0
		for _, v := range xs {
			hi = max(hi, v)
		}
		sw := min(max(w-30, 4), 60)
		line := chart.Sparkline(xs, sw, chart.SparkOpts{Look: m.look, Agg: chart.Max, Min: chart.Fixed(0), Max: chart.Fixed(hi), Stretch: true})
		lines = append(lines, kv("", line+m.th.Muted.Render(" cpu over the stretch")))
	}
	if len(e.Around) > 0 {
		lines = append(lines, "", m.section("commands started from a shell just before"))
		lines = append(lines, wrap(strings.Join(e.Around, ", "), w, "  ")...)
		lines = append(lines, wrap(m.th.Muted.Render("  any shell, any session: a lead, not a cause. Load inside a VM (docker under OrbStack) shows on its helper, and the command that started it is usually here."), w, "  ")...)
	}
	lines = append(lines, "", m.section("where"))
	if len(e.Chain) > 0 {
		sep := " › "
		if m.th.Glyphs.ASCII {
			sep = " > "
		}
		lines = append(lines, wrap(m.th.Muted.Render("from   ")+strings.Join(e.Chain, sep), w, "       ")...)
	}
	if e.Exe != "" {
		lines = append(lines, wrap(kv("exe", e.Exe), w, "       ")...)
	}
	if e.Cwd != "" {
		lines = append(lines, wrap(kv("cwd", e.Cwd), w, "       ")...)
	}
	if len(e.Args) > 0 {
		lines = append(lines, "", m.section("command"))
		lines = append(lines, wrap(shellJoin(e.Args), w, "  ")...)
	}
	// Other stretches of the same program in the loaded window.
	var others []history.Episode
	for _, o := range m.past.all {
		if o.Name == e.Name && o.User == e.User && o.ID() != e.ID() {
			others = append(others, o)
		}
	}
	if len(others) > 0 {
		var cpu float64
		for _, o := range others {
			cpu += o.TotalCPUSec()
		}
		lines = append(lines, "", m.section(fmt.Sprintf("other times (%d, %s CPU)", len(others),
			history.Dur(time.Duration(cpu*float64(time.Second))))))
		slices.SortFunc(others, func(a, b history.Episode) int { return b.Start.Compare(a.Start) })
		for _, o := range others[:min(len(others), 8)] {
			lines = append(lines, m.th.Muted.Render(fmt.Sprintf("  %s  %7s  %8s CPU  peak %s",
				o.Start.Local().Format("01-02 15:04"), history.Dur(o.Duration()),
				history.Dur(time.Duration(o.TotalCPUSec()*float64(time.Second))), outlier.Bytes(o.PeakMem))))
		}
	}
	return lines
}
