package ui

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/keyolk/tuikit/render"

	"github.com/keyolk/sysx/internal/history"
	"github.com/keyolk/sysx/internal/outlier"
	"github.com/keyolk/sysx/internal/proc"
)

var tabNames = []string{"overview", "args+env", "files+net", "tree"}

func (m Model) viewDetail(g geom) string {
	w := g.detail.W
	var bar []string
	if m.past.on {
		bar = append(bar, m.th.Selected.Render(" episode "))
	}
	for i, n := range tabNames {
		if m.past.on {
			break
		}
		label := fmt.Sprintf("%d %s", i+1, n)
		if i == m.tab {
			bar = append(bar, m.th.Selected.Render(" "+label+" "))
		} else {
			bar = append(bar, m.th.Muted.Render(" "+label+" "))
		}
	}
	lines := m.detailLines(w)
	off := max(0, min(m.detailOff, len(lines)-g.detail.H))
	end := min(len(lines), off+g.detail.H)
	body := lines[off:end]
	if end < len(lines) && len(body) > 0 {
		body[len(body)-1] = m.th.Subtle.Render(render.MoreLine(off, len(lines)-end))
	}
	return render.Truncate(strings.Join(bar, ""), w) + "\n" + strings.Join(body, "\n")
}

// detailLines is the whole scrollable body of the current tab, already cut
// to w cells per line.
func (m Model) detailLines(w int) []string {
	if m.past.on {
		lines := m.episodeLines(w)
		for i := range lines {
			lines[i] = render.Truncate(lines[i], w)
		}
		return lines
	}
	r, ok := m.selected()
	if !ok || w <= 0 {
		return []string{m.th.Subtle.Render("no process selected")}
	}
	var lines []string
	switch m.tab {
	case 0:
		lines = m.overview(r, w)
	case 1:
		lines = m.argsEnv(r, w)
	case 2:
		lines = m.filesNet(r, w)
	case 3:
		lines = m.tree(r, w)
	}
	for i := range lines {
		lines[i] = render.Truncate(lines[i], w)
	}
	return lines
}

func (m Model) section(title string) string { return m.th.Accent.Bold(true).Render(title) }

// wrap hard-wraps plain text to w cells, indenting continuation lines.
func wrap(s string, w int, indent string) []string {
	if w <= len(indent)+4 {
		return []string{s}
	}
	out := strings.Split(ansi.Hardwrap(s, w-len(indent), true), "\n")
	for i := 1; i < len(out); i++ {
		out[i] = indent + out[i]
	}
	return out
}

func age(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := time.Since(t)
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

func (m Model) extraFor(r row) (extraInfo, bool) {
	if m.extra.key != r.p.Key() || m.extra.loading {
		return extraInfo{}, false
	}
	return m.extra, true
}

func (m Model) overview(r row, w int) []string {
	p := r.p
	ex, loaded := m.extraFor(r)
	state := map[proc.State]string{proc.StateRun: "running", proc.StateSleep: "sleeping", proc.StateStop: "stopped",
		proc.StateZombie: "zombie", proc.StateIdle: "idle"}[p.State]
	if state == "" {
		state = "?"
	}
	lines := []string{
		m.th.Bold.Render(p.Name) + m.th.Muted.Render(fmt.Sprintf("  pid %d · ppid %d · %s · %s · up %s · nice %d",
			p.PID, p.PPID, p.User, state, age(p.Start), p.Nice)),
	}
	kv := func(k, v string) string { return m.th.Muted.Render(render.Pad(k, 6)) + v }
	if loaded {
		if ex.path != "" {
			lines = append(lines, wrap(kv("exe", ex.path), w, "      ")...)
		}
		if ex.cwd != "" {
			lines = append(lines, wrap(kv("cwd", ex.cwd), w, "      ")...)
		}
		switch {
		case ex.inPane:
			pn := ex.pane
			lines = append(lines, kv("tmux", fmt.Sprintf("%s:%d.%d %s  %s", pn.SessionName, pn.WindowIndex, pn.PaneIndex, pn.ID, pn.Title)+
				m.th.Muted.Render("  (t jumps there)")))
		case ex.paneErr != nil:
			lines = append(lines, kv("tmux", m.th.Muted.Render(ex.paneErr.Error())))
		}
	} else {
		lines = append(lines, m.th.Subtle.Render("loading paths, args, files…"))
	}

	if len(r.r.Findings) > 0 {
		lines = append(lines, "", m.section("why it stands out"))
		for _, f := range r.r.Findings {
			mark := m.sevStyle(f.Sev)(m.sevMark(f.Sev))
			lines = append(lines, wrap(mark+" "+f.Text, w, "  ")...)
		}
	}

	lines = append(lines, "", m.section("now"))
	lines = append(lines, kv("cpu", fmt.Sprintf("%.1f%%", p.CPU)+m.th.Muted.Render(fmt.Sprintf("  cpu time %s", p.CPUTime.Round(time.Second)))))
	memLine := outlier.Bytes(p.Footprint) + m.th.Muted.Render(fmt.Sprintf("  rss %s", outlier.Bytes(p.RSS)))
	if m.snap.Sys.MemTotal > 0 {
		memLine += m.th.Muted.Render(fmt.Sprintf(" · %.1f%% of RAM", float64(p.Footprint)/float64(m.snap.Sys.MemTotal)*100))
	}
	lines = append(lines, kv("mem", memLine))
	if p.Partial {
		lines = append(lines, kv("disk", m.th.Muted.Render("unknown (another user's process)")))
	} else {
		lines = append(lines, kv("disk", fmt.Sprintf("r %s w %s", rateStr(p.DiskRead), rateStr(p.DiskWrite))+
			m.th.Muted.Render("  total "+outlier.Bytes(p.DiskTotal))))
	}
	lines = append(lines, kv("net", fmt.Sprintf("↓%s ↑%s", rateStr(p.NetIn), rateStr(p.NetOut))+m.th.Muted.Render("  total "+outlier.Bytes(p.NetTotal))))
	thr := fmt.Sprintf("%d threads", p.Threads)
	if loaded && ex.filesErr == nil {
		thr += fmt.Sprintf(" · %d fds · %d sockets", ex.files.Count, len(ex.files.Conns))
	}
	lines = append(lines, kv("", thr))

	lines = append(lines, "", m.section(fmt.Sprintf("last %dm", int(historyKeep.Minutes()))))
	sw := max(w-18, 4)
	line := func(label string, xs []float64, hi float64, now string) string {
		return m.th.Muted.Render(render.Pad(label, 5)) + spark(xs, sw, hi, m.look) + " " + now
	}
	key := p.Key()
	cpuHi := 100.0
	for _, v := range m.hist.Series(key, outlier.CPU) {
		cpuHi = max(cpuHi, v)
	}
	lines = append(lines,
		line("cpu", m.hist.Series(key, outlier.CPU), cpuHi, fmt.Sprintf("%.0f%%", p.CPU)),
		line("mem", m.hist.Series(key, outlier.Mem), 0, outlier.Bytes(p.Footprint)),
		line("disk", m.hist.Series(key, outlier.Disk), 0, rateStr(p.DiskRead+p.DiskWrite)),
		line("net", m.hist.Series(key, outlier.Net), 0, rateStr(p.NetIn+p.NetOut)),
	)

	if n, cpu, busy, last := m.pastOf(p.User, p.Name); n > 0 {
		lines = append(lines, "", m.section("past day"))
		lines = append(lines, kv("", fmt.Sprintf("heavy %d times, busy %s, %s of CPU", n, history.Dur(busy),
			history.Dur(time.Duration(cpu*float64(time.Second)))))+
			m.th.Muted.Render("  last "+last.Local().Format("15:04")+" · H shows them"))
	}

	lines = append(lines, "", m.section("family"))
	lines = append(lines, wrap(m.th.Muted.Render("from  ")+m.lineage(p), w, "      ")...)
	desc := m.descendants(p.PID)
	if len(desc) > 0 {
		var cpu float64
		var mem uint64
		for _, d := range desc {
			cpu += d.CPU
			mem += d.Footprint
		}
		lines = append(lines, kv("kids", fmt.Sprintf("%d direct, %d in all · CPU %.0f%% · memory %s",
			len(m.children[p.PID]), len(desc), cpu, outlier.Bytes(mem))))
		slices.SortFunc(desc, func(a, b proc.Proc) int { return cmpF(b.CPU+float64(b.Footprint>>24), a.CPU+float64(a.Footprint>>24)) })
		for _, d := range desc[:min(len(desc), 6)] {
			lines = append(lines, m.procLine("      ", d, w))
		}
	}
	var same []proc.Proc
	for _, q := range m.snap.Procs {
		if q.Name == p.Name && q.UID == p.UID && q.PID != p.PID {
			same = append(same, q)
		}
	}
	if len(same) > 0 {
		var cpu float64
		var mem uint64
		for _, q := range same {
			cpu += q.CPU
			mem += q.Footprint
		}
		lines = append(lines, kv("same", fmt.Sprintf("%d other '%s' of %s · CPU %.0f%% · memory %s", len(same), p.Name, p.User, cpu, outlier.Bytes(mem))))
	}

	if loaded && len(ex.args.Argv) > 0 {
		lines = append(lines, "", m.section("command"))
		lines = append(lines, wrap(shellJoin(ex.args.Argv), w, "  ")...)
	} else if loaded && ex.argsErr != nil {
		lines = append(lines, "", m.section("command"), m.th.Muted.Render(ex.argsErr.Error()))
	}
	return lines
}

func cmpF(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// procLine is one related process: severity mark, name, pid, cpu, memory.
func (m Model) procLine(indent string, p proc.Proc, w int) string {
	rep := m.reports[p.Key()]
	mark := m.sevStyle(rep.Sev)(m.sevMark(rep.Sev))
	nameW := min(max(w-len(indent)-2-26, 6), 32)
	return indent + mark + " " + render.Pad(render.Truncate(p.Name, nameW), nameW) +
		m.th.Muted.Render(fmt.Sprintf(" %7d %6.1f%% %6s", p.PID, p.CPU, outlier.Bytes(p.Footprint)))
}

// lineage is the ancestor chain, outermost first: launchd 1 › tmux 812 › …
func (m Model) lineage(p proc.Proc) string {
	var chain []string
	cur := p
	for range 32 {
		pp, ok := m.byPID[cur.PPID]
		// pid 0 is the kernel; every chain ends there, so it says nothing.
		if !ok || pp.PID == cur.PID || pp.PID == 0 {
			break
		}
		chain = append(chain, fmt.Sprintf("%s %d", pp.Name, pp.PID))
		cur = pp
	}
	slices.Reverse(chain)
	if len(chain) == 0 {
		return m.th.Muted.Render("(no parent visible)")
	}
	sep := " › "
	if m.th.Glyphs.ASCII {
		sep = " > "
	}
	return strings.Join(chain, sep)
}

func (m Model) descendants(pid int) []proc.Proc {
	var out []proc.Proc
	seen := map[int]bool{pid: true}
	queue := []int{pid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range m.children[cur] {
			if seen[c] {
				continue
			}
			seen[c] = true
			queue = append(queue, c)
			out = append(out, m.byPID[c])
		}
	}
	return out
}

// shellJoin quotes arguments that need it, so the line can be pasted.
func shellJoin(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\n'\"$`\\|&;<>()*?[]{}!#~") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

// secretKey matches environment names whose values should not be on screen
// by default (screen sharing, screenshots).
var secretKey = regexp.MustCompile(`(?i)(TOKEN|SECRET|PASSW|PASSWD|API_?KEY|PRIVATE|CREDENTIAL|AUTH|SESSION|COOKIE|_KEY$|^KEY$|DSN)`)

func (m Model) argsEnv(r row, w int) []string {
	ex, loaded := m.extraFor(r)
	if !loaded {
		return []string{m.th.Subtle.Render("loading…")}
	}
	if ex.argsErr != nil {
		return []string{m.th.Warn.Render(ex.argsErr.Error())}
	}
	lines := []string{m.section(fmt.Sprintf("argv (%d)", len(ex.args.Argv)))}
	for i, a := range ex.args.Argv {
		lines = append(lines, wrap(m.th.Muted.Render(fmt.Sprintf("%3d ", i))+a, w, "    ")...)
	}
	env := slices.Clone(ex.args.Env)
	slices.Sort(env)
	masked := 0
	lines = append(lines, "", m.section(fmt.Sprintf("env (%d)", len(env))))
	if len(env) == 0 {
		lines = append(lines, m.th.Muted.Render("(empty — the kernel hides it for some hardened binaries)"))
	}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		if !m.secrets && secretKey.MatchString(k) && v != "" {
			v = m.th.Muted.Render(fmt.Sprintf("****** (%d chars)", len(v)))
			masked++
		}
		lines = append(lines, wrap(m.th.Accent.Render(k)+"="+v, w, "    ")...)
	}
	if masked > 0 {
		lines = append(lines, "", m.th.Muted.Render(fmt.Sprintf("%d secret-looking values masked; v reveals", masked)))
	}
	return lines
}

func (m Model) filesNet(r row, w int) []string {
	ex, loaded := m.extraFor(r)
	if !loaded {
		return []string{m.th.Subtle.Render("running lsof…")}
	}
	if ex.filesErr != nil {
		return []string{m.th.Warn.Render(ex.filesErr.Error())}
	}
	f := ex.files
	lines := []string{m.th.Muted.Render(fmt.Sprintf("%d open file descriptors", f.Count)), "", m.section(fmt.Sprintf("sockets (%d)", len(f.Conns)))}
	conns := slices.Clone(f.Conns)
	slices.SortStableFunc(conns, func(a, b proc.Conn) int { return strings.Compare(a.State, b.State) })
	for _, c := range conns {
		st := c.State
		if st == "" {
			st = "-"
		}
		lines = append(lines, fmt.Sprintf("%-4s %-12s %s", c.Proto, st, c.Name))
	}
	lines = append(lines, "", m.section(fmt.Sprintf("files (%d)", len(f.Paths))))
	for _, p := range f.Paths[:min(len(f.Paths), 300)] {
		lines = append(lines, p)
	}
	return lines
}

func (m Model) tree(r row, w int) []string {
	p := r.p
	var chain []proc.Proc
	cur := p
	for range 32 {
		pp, ok := m.byPID[cur.PPID]
		if !ok || pp.PID == cur.PID || pp.PID == 0 {
			break
		}
		chain = append(chain, pp)
		cur = pp
	}
	slices.Reverse(chain)
	var lines []string
	for i, a := range chain {
		lines = append(lines, m.procLine(strings.Repeat("  ", i), a, w))
	}
	depth := len(chain)
	self := m.procLine(strings.Repeat("  ", depth), p, w)
	lines = append(lines, m.th.Accent.Bold(true).Render(ansi.Strip(self)))
	var walk func(pid, d int)
	count := 0
	walk = func(pid, d int) {
		for _, c := range m.children[pid] {
			if count >= 500 {
				return
			}
			count++
			lines = append(lines, m.procLine(strings.Repeat("  ", d), m.byPID[c], w))
			if d < 40 {
				walk(c, d+1)
			}
		}
	}
	walk(p.PID, depth+1)
	if count >= 500 {
		lines = append(lines, m.th.Muted.Render("… cut at 500 descendants"))
	}
	return lines
}
