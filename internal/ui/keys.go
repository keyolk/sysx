package ui

import (
	"strings"

	"github.com/keyolk/tuikit/keys"
	"github.com/keyolk/tuikit/split"
)

const (
	ctxGlobal  keys.Ctx = "global"
	ctxList    keys.Ctx = "list"
	ctxDetail  keys.Ctx = "detail"
	ctxFilter  keys.Ctx = "filter"
	ctxConfirm keys.Ctx = "confirm"
	ctxHelp    keys.Ctx = "help"
	ctxHistory keys.Ctx = "history"
)

// Stacks are every context stack the app can be in, innermost first.
var Stacks = [][]keys.Ctx{
	{ctxList, ctxGlobal},
	{ctxDetail, ctxList, ctxGlobal},
	{ctxFilter, ctxGlobal},
	{ctxConfirm, ctxGlobal},
	{ctxHelp, ctxGlobal},
	{ctxHistory, ctxGlobal},
	{ctxDetail, ctxHistory, ctxGlobal},
}

var tabKeys = []string{"1", "2", "3", "4"}

// Keymap is the one table behind dispatch, the footer and help.
func Keymap() keys.Keymap {
	bs := []keys.Binding{
		{Action: "quit", Keys: []string{"q", "ctrl+c"}, Ctx: ctxGlobal, Help: "quit", Pinned: true, Footer: 2},
		{Action: "help", Keys: []string{"?"}, Ctx: ctxGlobal, Help: "help", Pinned: true, Footer: 1},
		{Action: "pause", Keys: []string{"p"}, Ctx: ctxGlobal, Group: "Sampling", Help: "pause sampling",
			Labels: map[string]string{"on": "resume"}, Footer: 9},
		{Action: "refresh", Keys: []string{"r"}, Ctx: ctxGlobal, Group: "Sampling", Help: "sample now"},

		{Action: "list.down", Keys: []string{"j", "down"}, Ctx: ctxList, Group: "Move", Section: "list.move",
			Help: "down", Footer: 3, FooterGroup: "move"},
		{Action: "list.up", Keys: []string{"k", "up"}, Ctx: ctxList, Group: "Move", Section: "list.move",
			Help: "up", Footer: 3, FooterGroup: "move"},
		{Action: "list.top", Keys: []string{"g", "home"}, Ctx: ctxList, Group: "Move", Section: "list.move", Help: "first row"},
		{Action: "list.bottom", Keys: []string{"G", "end"}, Ctx: ctxList, Group: "Move", Section: "list.move", Help: "last row"},
		{Action: "list.halfdown", Keys: []string{"ctrl+d", "pgdown"}, Ctx: ctxList, Group: "Move", Section: "list.move", Help: "half page down", NoPalette: true},
		{Action: "list.halfup", Keys: []string{"ctrl+u", "pgup"}, Ctx: ctxList, Group: "Move", Section: "list.move", Help: "half page up", NoPalette: true},
		{Action: "list.parent", Keys: []string{"u"}, Ctx: ctxList, Group: "Move", Section: "list.move", Help: "select the parent process"},

		{Action: "list.focus", Keys: []string{"enter", "l"}, Ctx: ctxList, Group: "View", Help: "focus detail", Short: "detail", Footer: 4},
		{Action: "list.filter", Keys: []string{"/"}, Ctx: ctxList, Group: "View", Help: "filter by name, user, pid or tag", Short: "filter", Footer: 5},
		{Action: "list.sort", Keys: []string{"s"}, Ctx: ctxList, Group: "View", Help: "cycle sort", Short: "sort", Footer: 6},
		{Action: "list.flagged", Keys: []string{"o"}, Ctx: ctxList, Group: "View", Help: "only flagged processes",
			Short: "flagged", Labels: map[string]string{"on": "all"}, Footer: 7},
		{Action: "list.mine", Keys: []string{"m"}, Ctx: ctxList, Group: "View", Help: "only my processes",
			Short: "mine", Labels: map[string]string{"on": "everyone"}, Footer: 8},
		{Action: "tab.next", Keys: []string{"tab"}, Ctx: ctxList, Group: "Detail", Help: "next detail tab", Short: "tab", NoPalette: true},
		{Action: "tab.prev", Keys: []string{"shift+tab"}, Ctx: ctxList, Group: "Detail", Help: "previous detail tab", NoPalette: true},
		{Action: "tab.jump", Keys: tabKeys, KeyLabel: "1-4", Ctx: ctxList, Group: "Detail",
			Help: "overview / args+env / files+net / tree", Short: "tab", NoPalette: true},
		{Action: "secrets", Keys: []string{"v"}, Ctx: ctxList, Group: "Detail", Help: "reveal secret-looking env values",
			Labels: map[string]string{"on": "mask secrets"}},
		{Action: "tmux.jump", Keys: []string{"t"}, Ctx: ctxList, Group: "Act", Help: "jump to the tmux pane running it"},
		{Action: "kill", Keys: []string{"x", "delete"}, Ctx: ctxList, Group: "Act", Help: "kill / stop… (asks first)", Short: "kill", Footer: 2},

		// In detail, j/k scroll the pane instead of moving the list.
		{Action: "detail.down", Keys: []string{"j", "down"}, Ctx: ctxDetail, Group: "Scroll", Section: "detail.scroll",
			Help: "scroll down", Footer: 3, FooterGroup: "scroll", Shadows: []string{"list.down"}},
		{Action: "detail.up", Keys: []string{"k", "up"}, Ctx: ctxDetail, Group: "Scroll", Section: "detail.scroll",
			Help: "scroll up", Footer: 3, FooterGroup: "scroll", Shadows: []string{"list.up"}},
		{Action: "detail.halfdown", Keys: []string{"ctrl+d", "pgdown"}, Ctx: ctxDetail, Group: "Scroll", Section: "detail.scroll",
			Help: "half page down", NoPalette: true, Shadows: []string{"list.halfdown"}},
		{Action: "detail.halfup", Keys: []string{"ctrl+u", "pgup"}, Ctx: ctxDetail, Group: "Scroll", Section: "detail.scroll",
			Help: "half page up", NoPalette: true, Shadows: []string{"list.halfup"}},
		{Action: "detail.top", Keys: []string{"g", "home"}, Ctx: ctxDetail, Group: "Scroll", Section: "detail.scroll",
			Help: "top", Shadows: []string{"list.top"}},
		{Action: "detail.bottom", Keys: []string{"G", "end"}, Ctx: ctxDetail, Group: "Scroll", Section: "detail.scroll",
			Help: "bottom", Shadows: []string{"list.bottom"}},
		{Action: "detail.close", Keys: []string{"esc", "h"}, Ctx: ctxDetail, Group: "Scroll", Help: "back to the list",
			Short: "back", Footer: 1},

		{Action: "filter.apply", Keys: []string{"enter"}, Ctx: ctxFilter, Help: "apply", Footer: 1},
		{Action: "filter.clear", Keys: []string{"esc"}, Ctx: ctxFilter, Help: "clear", Footer: 2},
		{Action: "quit", Keys: []string{"ctrl+c"}, Ctx: ctxFilter, Help: "quit", Pinned: true, Footer: 2},

		{Action: "confirm.term", Keys: []string{"t"}, Ctx: ctxConfirm, Help: "SIGTERM this process", Footer: 1},
		{Action: "confirm.kill", Keys: []string{"k"}, Ctx: ctxConfirm, Help: "SIGKILL this process", Footer: 2},
		{Action: "confirm.tree", Keys: []string{"g"}, Ctx: ctxConfirm, Help: "SIGTERM it and its descendants", Footer: 3},
		{Action: "confirm.same", Keys: []string{"n"}, Ctx: ctxConfirm, Help: "SIGTERM every process with this name", Footer: 4},
		{Action: "confirm.stop", Keys: []string{"s"}, Ctx: ctxConfirm, Help: "SIGSTOP / SIGCONT (pause or resume)", Footer: 5},
		{Action: "confirm.cancel", Keys: []string{"esc", "q"}, Ctx: ctxConfirm, Help: "cancel", Footer: 1, Shadows: []string{"quit"}},

		{Action: "help.close", Keys: []string{"esc", "q", "?"}, Ctx: ctxHelp, Help: "close help", Footer: 1, Shadows: []string{"quit", "help"}},
		{Action: "help.down", Keys: []string{"j", "down"}, Ctx: ctxHelp, Group: "Move", Section: "help.move",
			Help: "scroll down", Footer: 2, FooterGroup: "scroll"},
		{Action: "help.up", Keys: []string{"k", "up"}, Ctx: ctxHelp, Group: "Move", Section: "help.move",
			Help: "scroll up", Footer: 2, FooterGroup: "scroll"},
	}
	// The history view moves, filters, sorts and focuses with the list's
	// own actions, so the detail pane's shadows and the handlers serve both.
	for _, b := range bs {
		if b.Ctx == ctxList && (strings.HasPrefix(b.Action, "list.") && b.Action != "list.parent" &&
			b.Action != "list.flagged" && b.Action != "list.mine") {
			b.Ctx = ctxHistory
			bs = append(bs, b)
		}
	}
	bs = append(bs,
		keys.Binding{Action: "view.history", Keys: []string{"H", "f2"}, Ctx: ctxList, Group: "View",
			Help: "history of past heavy processes", Short: "history", Footer: 7},
		keys.Binding{Action: "view.live", Keys: []string{"H", "f2", "esc"}, Ctx: ctxHistory, Group: "View",
			Help: "back to live processes", Short: "live", Footer: 1},
		keys.Binding{Action: "history.window", Keys: []string{"w"}, Ctx: ctxHistory, Group: "View",
			Help: "cycle time window", Short: "window", Footer: 6},
	)
	bs = append(bs, split.BindingsWith(ctxList, "View", 0)...)
	hs := split.BindingsWith(ctxHistory, "View", 0)
	bs = append(bs, hs...)
	return keys.New(bs...)
}
