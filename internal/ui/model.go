// Package ui is sysx's Bubble Tea front end: a process list ordered by how
// abnormal each process looks, and a detail pane about the selected one.
package ui

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/keyolk/tuikit/async"
	"github.com/keyolk/tuikit/chart"
	"github.com/keyolk/tuikit/input"
	"github.com/keyolk/tuikit/keys"
	"github.com/keyolk/tuikit/list"
	"github.com/keyolk/tuikit/mouse"
	"github.com/keyolk/tuikit/prefs"
	"github.com/keyolk/tuikit/split"
	"github.com/keyolk/tuikit/theme"
	"github.com/keyolk/tuikit/toast"

	"github.com/keyolk/sysx/internal/history"
	"github.com/keyolk/sysx/internal/outlier"
	"github.com/keyolk/sysx/internal/proc"
)

// App is the prefs and log name.
const App = "sysx"

// historyKeep is how far back trends and growth rules look.
const historyKeep = 5 * time.Minute

type row struct {
	p proc.Proc
	r outlier.Report
}

// ID keeps the cursor on the process across samples, not on the row index.
func (r row) ID() string { return r.p.Key() }

// UIState survives a restart.
type UIState struct {
	Sort    string      `json:"sort"`
	Flagged bool        `json:"flagged"`
	Mine    bool        `json:"mine"`
	Tab     int         `json:"tab"`
	Split   split.State `json:"split"`
	// HistWindow and HistSort are the history view's; 0 is a default.
	HistWindow string `json:"hist_window,omitempty"`
	HistSort   string `json:"hist_sort,omitempty"`
}

type (
	tickMsg    struct{}
	sampledMsg struct {
		id   uint64
		snap proc.Snapshot
		err  error
	}
)

// sysSeries is the machine-wide trend for the header sparklines.
type sysSeries struct{ cpu, mem, diskIO, netIO []float64 }

func (s sysSeries) add(snap proc.Snapshot, keep int) sysSeries {
	push := func(xs []float64, v float64) []float64 {
		out := append(slices.Clone(xs), v)
		if len(out) > keep {
			out = out[len(out)-keep:]
		}
		return out
	}
	used := 100 - snap.Sys.CPUIdle
	mem := 0.0
	if snap.Sys.MemTotal > 0 {
		mem = float64(snap.Sys.MemUsed) / float64(snap.Sys.MemTotal) * 100
	}
	return sysSeries{
		cpu:    push(s.cpu, used),
		mem:    push(s.mem, mem),
		diskIO: push(s.diskIO, snap.Sys.DiskRead+snap.Sys.DiskWrite),
		netIO:  push(s.netIO, snap.Sys.NetIn+snap.Sys.NetOut),
	}
}

// Model is the whole UI state; Bubble Tea copies it by value.
type Model struct {
	km     keys.Keymap
	th     theme.Theme
	look   chart.Look
	toasts toast.Queue
	saver  prefs.Saver[UIState]
	logf   func(string, ...any)

	coll     *proc.Collector // only touched by the one in-flight sample command
	sampling async.Flight
	interval time.Duration
	paused   bool
	thr      outlier.Thresholds

	snap     proc.Snapshot
	hist     outlier.History
	sys      sysSeries
	reports  map[string]outlier.Report
	byPID    map[int]proc.Proc
	children map[int][]int
	me       int

	rows list.List[row]
	// moved is set once the user picks a row. Until then the cursor follows
	// the top of the list, so the worst process is what the detail shows.
	moved   bool
	sortBy  sortKey
	flagged bool
	mine    bool

	filter    input.Field
	filtering bool
	query     string

	focusDetail bool
	tab         int
	detailOff   int
	secrets     bool
	extra       extraInfo
	extraGen    async.Gen

	confirm *killPlan
	help    bool
	helpOff int

	past    histState // the history view and the loaded log
	histDir string

	split  split.Model
	clicks mouse.Clicks
	w, h   int
}

// New builds the model. interval is the sampling period.
func New(km keys.Keymap, th theme.Theme, logf func(string, ...any), saved UIState, interval time.Duration) Model {
	m := Model{
		km: km, th: th, look: chart.LookFor(th), logf: logf,
		toasts:   toast.New(3),
		saver:    prefs.NewSaver[UIState](App, 0),
		coll:     proc.NewCollector(),
		interval: interval,
		thr:      outlier.Defaults(),
		hist:     outlier.NewHistory(historyKeep),
		me:       os.Getuid(),
		histDir:  history.DefaultDir(),
		past:     histState{window: defaultHistWindow},
		sortBy:   parseSort(saved.Sort),
		flagged:  saved.Flagged,
		mine:     saved.Mine,
		tab:      min(max(saved.Tab, 0), len(tabNames)-1),
		split: split.Model{
			// MaxA: past ~130 columns the list only grows its name column;
			// the detail pane uses the room better.
			MinA: 40, MinB: 36, MaxA: 130, Divider: 1, Collapse: 0, Primary: split.A,
			Default: 0.55, Lo: 0.3, Hi: 0.8,
		},
	}
	m.split, _ = m.split.Restore(saved.Split)
	if i := slices.Index(histWindowNames, saved.HistWindow); i >= 0 {
		m.past.window = i
	}
	if i := slices.Index(history.SortKeys, saved.HistSort); i >= 0 {
		m.past.sortBy = i
	}
	return m
}

func (m Model) uiState() UIState {
	return UIState{Sort: m.sortBy.String(), Flagged: m.flagged, Mine: m.mine, Tab: m.tab, Split: m.split.State(),
		HistWindow: histWindowNames[m.past.window], HistSort: history.SortKeys[m.past.sortBy]}
}

// Init takes the first sample right away; it has no rates, so the second
// follows after a short beat instead of a full interval.
func (m Model) Init() tea.Cmd {
	return tea.Batch(func() tea.Msg { return tickMsg{} }, func() tea.Msg { return histKickMsg{} })
}

// histKickMsg asks Update for the first history load; Init cannot bump
// the load generation on the model it returns nothing for.
type histKickMsg struct{}

func (m Model) sampleCmd(id uint64) tea.Cmd {
	c := m.coll
	return func() tea.Msg {
		snap, err := c.Sample(context.Background())
		return sampledMsg{id: id, snap: snap, err: err}
	}
}

func (m Model) startSample() (Model, tea.Cmd) {
	f, id, ok := m.sampling.Start()
	if !ok {
		return m, nil
	}
	m.sampling = f
	return m, m.sampleCmd(id)
}

func (m Model) nextTick() tea.Cmd {
	d := m.interval
	if m.snap.Interval == 0 {
		d = 700 * time.Millisecond
	}
	return tea.Tick(d, func(time.Time) tea.Msg { return tickMsg{} })
}

// applySample folds a new snapshot into history, reports and rows, keeping
// the cursor on its process.
func (m Model) applySample(s proc.Snapshot) Model {
	m.snap = s
	m.hist = m.hist.Add(s)
	m.sys = m.sys.add(s, 150)
	m.reports = outlier.Analyze(s, m.hist, m.thr)
	m.byPID = make(map[int]proc.Proc, len(s.Procs))
	m.children = map[int][]int{}
	for _, p := range s.Procs {
		m.byPID[p.PID] = p
		if p.PID != p.PPID {
			m.children[p.PPID] = append(m.children[p.PPID], p.PID)
		}
	}
	for _, kids := range m.children {
		slices.Sort(kids)
	}
	m.rows = m.rows.SetItems(m.visible())
	if !m.moved {
		m.rows = m.rows.Top()
	}
	return m
}

// visible is the filtered and sorted row set.
func (m Model) visible() []row {
	q := strings.ToLower(strings.TrimSpace(m.query))
	out := make([]row, 0, len(m.snap.Procs))
	for _, p := range m.snap.Procs {
		r := row{p, m.reports[p.Key()]}
		if m.flagged && r.r.Sev < outlier.Warn {
			continue
		}
		if m.mine && p.UID != m.me {
			continue
		}
		if q != "" && !matches(r, q) {
			continue
		}
		out = append(out, r)
	}
	slices.SortStableFunc(out, m.sortBy.cmp)
	return out
}

// matches is a plain substring match on the columns the user sees: name,
// user, pid, and the tags of its findings.
func matches(r row, q string) bool {
	if strings.Contains(strings.ToLower(r.p.Name), q) || strings.Contains(strings.ToLower(r.p.User), q) ||
		strconv.Itoa(r.p.PID) == q {
		return true
	}
	for _, f := range r.r.Findings {
		if strings.Contains(strings.ToLower(f.Tag), q) || string(f.Dim) == q {
			return true
		}
	}
	return false
}

func (m Model) selected() (row, bool) { return m.rows.Selected() }

func (m Model) active() []keys.Ctx {
	switch {
	case m.help:
		return Stacks[4]
	case m.confirm != nil:
		return Stacks[3]
	case m.filtering:
		return Stacks[2]
	case m.past.on && m.focusDetail:
		return Stacks[6]
	case m.past.on:
		return Stacks[5]
	case m.focusDetail:
		return Stacks[1]
	}
	return Stacks[0]
}

func (m Model) keyOpts() keys.Opts {
	return keys.Opts{Enabled: m.enabled, Visible: m.offered, State: m.keyState, Label: m.liveLabel}
}

func (m Model) enabled(action string) bool {
	switch action {
	case "kill", "tmux.jump", "list.parent":
		_, ok := m.selected()
		return ok
	case "confirm.same":
		return m.confirm != nil && len(m.confirm.same) > 1
	case "confirm.tree":
		return m.confirm != nil && len(m.confirm.tree) > 1
	}
	return true
}

func (m Model) offered(action string) bool {
	switch action {
	case "help", "refresh", "pause":
		return !m.filtering
	}
	return true
}

func (m Model) keyState(action string) string {
	on := false
	switch action {
	case "pause":
		on = m.paused
	case "list.flagged":
		on = m.flagged
	case "list.mine":
		on = m.mine
	case "secrets":
		on = m.secrets
	}
	if on {
		return "on"
	}
	return ""
}

func (m Model) liveLabel(action string) (string, bool) {
	switch {
	case action == "list.sort" && m.past.on:
		return "sort:" + history.SortKeys[m.past.sortBy], true
	case action == "list.sort":
		return "sort:" + m.sortBy.String(), true
	case action == "history.window":
		return "window:" + histWindowNames[m.past.window], true
	case action == "list.filter" && m.query != "":
		return "filter: " + m.query, true
	}
	return "", false
}

// WithToast queues a toast before the program starts (a bad keys.yaml).
func (m Model) WithToast(t toast.Toast) Model {
	m.toasts = toast.Add(m.toasts, t)
	return m
}
