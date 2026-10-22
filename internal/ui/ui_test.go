package ui

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/keyolk/tuikit/render"
	"github.com/keyolk/tuikit/theme"

	"github.com/keyolk/sysx/internal/proc"
)

var t0 = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

func testModel(t *testing.T) Model {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	th := theme.NewWith(theme.NewRenderer(os.Stdout), theme.DefaultRoles(), theme.DetectGlyphs(App))
	return New(Keymap(), th, t.Logf, UIState{}, time.Second)
}

func pr(pid, ppid int, name string, cpu float64) proc.Proc {
	return proc.Proc{PID: pid, PPID: ppid, PGID: pid, Name: name, User: "me", UID: os.Getuid(),
		Start: t0.Add(-time.Duration(pid) * time.Second), CPU: cpu, Footprint: 100 << 20, Threads: 4}
}

// world is a small tree: launchd → shell → sysx-parent → [this test] and a
// busy build: shell → cargo → 4 × rustc.
func world(at time.Duration, extra ...proc.Proc) proc.Snapshot {
	me := os.Getpid()
	ps := []proc.Proc{
		pr(1, 0, "launchd", 0),
		pr(500, 1, "tmux", 1),
		pr(600, 500, "fish", 0),
		pr(os.Getppid(), 600, "go", 0),
		pr(me, os.Getppid(), "sysx", 1),
		pr(700, 600, "cargo", 2),
	}
	for i := range 4 {
		ps = append(ps, pr(710+i, 700, "rustc", 95))
	}
	ps = append(ps, extra...)
	return proc.Snapshot{At: t0.Add(at), Interval: time.Second,
		Sys: proc.System{NCPU: 8, MemTotal: 16 << 30, MemUsed: 8 << 30}, Procs: ps}
}

func feed(m Model, n int) Model {
	for i := range n {
		m = m.applySample(world(time.Duration(i) * 2 * time.Second))
	}
	return m
}

func key(s string) tea.KeyMsg {
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func press(m Model, keys ...string) Model {
	for _, k := range keys {
		tm, _ := m.Update(key(k))
		m = tm.(Model)
	}
	return m
}

func TestOutliersSortFirst(t *testing.T) {
	m := feed(testModel(t), 10)
	var order []string
	for i := range min(6, m.rows.Len()) {
		r := m.rows.At(i)
		order = append(order, fmt.Sprintf("%s:%v", r.p.Name, r.r.Sev))
	}
	if r := m.rows.At(0); r.p.Name != "rustc" {
		t.Errorf("rows = %v, want a busy rustc first", order)
	}
}

func TestCursorFollowsTheProcessAcrossSamples(t *testing.T) {
	m := feed(testModel(t), 10)
	m.rows, _ = m.rows.Select(pr(600, 500, "fish", 0).Key())
	m.moved = true // as if the user had picked it
	// cargo starts burning CPU: rows reorder, the cursor must stay on fish.
	for i := 10; i < 20; i++ {
		hot := pr(800, 600, "newhog", 300)
		m = m.applySample(world(time.Duration(i)*2*time.Second, hot))
	}
	if r, _ := m.rows.Selected(); r.p.PID != 600 {
		t.Errorf("cursor on %d %s, want fish 600", r.p.PID, r.p.Name)
	}
}

func TestKillPlanLeavesSysxAndItsAncestorsOut(t *testing.T) {
	m := feed(testModel(t), 3)
	fish := row{p: m.byPID[600]}
	plan, err := m.planFor(fish)
	if err == nil {
		t.Fatalf("fish runs sysx; a plan was offered: %+v", plan)
	}
	cargo := row{p: m.byPID[700]}
	plan, err = m.planFor(cargo)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.tree) != 5 {
		t.Errorf("cargo tree = %d targets, want cargo + 4 rustc", len(plan.tree))
	}
	rustc := row{p: m.byPID[710]}
	plan, _ = m.planFor(rustc)
	if len(plan.same) != 4 {
		t.Errorf("same-name = %d, want 4", len(plan.same))
	}
	if _, err := m.planFor(row{p: m.byPID[os.Getpid()]}); err == nil {
		t.Error("sysx offered to kill itself")
	}
}

func TestKillDialogIsModalAndEscCancels(t *testing.T) {
	m := feed(testModel(t), 3)
	m, _ = m.update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m.rows, _ = m.rows.Select(m.byPID[700].Key())
	m = press(m, "x")
	if m.confirm == nil {
		t.Fatal("x did not open the dialog")
	}
	if !strings.Contains(m.View(), "SIGKILL") {
		t.Error("dialog not drawn")
	}
	before := m.rows.SelectedID()
	m = press(m, "j") // not a dialog key: nothing moves underneath
	if m.rows.SelectedID() != before || m.confirm == nil {
		t.Error("a key leaked through the dialog")
	}
	m = press(m, "esc")
	if m.confirm != nil {
		t.Error("esc did not cancel")
	}
}

func TestEveryLineFitsTheWidth(t *testing.T) {
	m := feed(testModel(t), 10)
	for _, sz := range [][2]int{{200, 50}, {120, 40}, {100, 30}, {80, 24}, {60, 20}, {50, 12}, {30, 8}} {
		tm, _ := m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		mm := tm.(Model)
		for _, tab := range []int{0, 1, 2, 3} {
			mm.tab = tab
			for _, v := range []string{mm.View(), press(mm, "?").View(), press(mm, "x").View()} {
				lines := strings.Split(v, "\n")
				if len(lines) > sz[1] {
					t.Errorf("%dx%d tab %d: %d lines", sz[0], sz[1], tab, len(lines))
				}
				for i, l := range lines {
					if w := render.Width(l); w > sz[0] {
						t.Errorf("%dx%d tab %d line %d: %d cells: %q", sz[0], sz[1], tab, i, w, l)
					}
				}
			}
		}
	}
}

func TestFilterMatchesTagsAndPid(t *testing.T) {
	m := feed(testModel(t), 10)
	m, _ = m.applyQuery("tree")
	if r, ok := m.rows.Selected(); !ok || r.p.Name != "cargo" {
		t.Errorf("tag filter 'tree' → %+v, want cargo", r.p.Name)
	}
	m, _ = m.applyQuery(fmt.Sprint(712))
	if m.rows.Len() != 1 {
		t.Errorf("pid filter rows = %d", m.rows.Len())
	}
}

func TestCursorFollowsTheTopUntilTheUserMoves(t *testing.T) {
	m := testModel(t)
	m, _ = m.update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m = m.applySample(world(0)) // no rates yet: order is arbitrary
	for i := 1; i < 10; i++ {
		m = m.applySample(world(time.Duration(i) * 2 * time.Second))
	}
	if r, _ := m.rows.Selected(); r.p.Name != "rustc" {
		t.Fatalf("cursor on %s before any key, want the top outlier", r.p.Name)
	}
	m = press(m, "j")
	picked := m.rows.SelectedID()
	m = m.applySample(world(20*time.Second, pr(900, 600, "hog", 400)))
	if m.rows.SelectedID() != picked {
		t.Error("after j the cursor jumped back to the top")
	}
}

// Fast typing arrives as one message with many runes; "/rustc" must still
// open the filter and type into it, not be dropped as an unknown key.
func TestBatchedRunesOpenTheFilterAndType(t *testing.T) {
	m := feed(testModel(t), 3)
	m, _ = m.update(tea.WindowSizeMsg{Width: 140, Height: 40})
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/rustc")})
	m = tm.(Model)
	if !m.filtering || m.query != "rustc" {
		t.Fatalf("filtering=%v query=%q", m.filtering, m.query)
	}
	if !strings.Contains(m.View(), "/ rustc") {
		t.Error("the filter field is not on screen")
	}
}
