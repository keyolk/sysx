// Command sysx is a process monitor that puts outliers first: the processes
// using unusual CPU, memory, disk or network, or standing out as a group,
// with everything about the selected one (family, args, env, files, tmux
// pane) beside the list, and a confirmed kill one key away.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/keyolk/tuikit/keys"
	"github.com/keyolk/tuikit/mouse"
	"github.com/keyolk/tuikit/prefs"
	"github.com/keyolk/tuikit/theme"
	"github.com/keyolk/tuikit/toast"

	"github.com/keyolk/sysx/internal/ui"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		var err error
		switch os.Args[1] {
		case "record":
			err = runRecord(os.Args[2:])
		case "history":
			err = runHistory(os.Args[2:])
		default:
			goto tui
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "sysx:", err)
			os.Exit(1)
		}
		return
	}
tui:
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: sysx [-i 2s]          the monitor\n"+
			"       sysx record [--install|--uninstall] [-i 10s] [-keep 7] [-max-mb 64]\n"+
			"       sysx history [-since 24h] [-sort time|cpu|mem|disk|net] [-grep text] [-json]\n")
		flag.PrintDefaults()
	}
	interval := flag.Duration("i", 2*time.Second, "sampling interval")
	showVersion := flag.Bool("version", false, "print the version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *interval < 500*time.Millisecond {
		*interval = 500 * time.Millisecond
	}

	logger, closeLog := openLog("sysx.log")
	defer closeLog()

	saved, err := prefs.Load(ui.App, ui.UIState{})
	if err != nil {
		logger.Printf("load prefs: %v", err)
	}
	// A bad keys.yaml must not stop the app: keep the defaults and say why.
	km, kerr := ui.Keymap().LoadOverrides(keys.ConfigPath(ui.App), ui.Stacks...)
	if kerr != nil {
		logger.Printf("keys: %v", kerr)
	}
	th := theme.NewWith(theme.NewRenderer(os.Stdout), theme.DefaultRoles(), theme.DetectGlyphs(ui.App))
	m := ui.New(km, th, logger.Printf, saved, *interval)
	if kerr != nil {
		m = m.WithToast(toast.Toast{Source: "keys", Text: "keys.yaml ignored: " + kerr.Error(), Level: toast.Error})
	}
	opts := append([]tea.ProgramOption{tea.WithAltScreen()}, mouse.ProgramOptions()...)
	if _, err := tea.NewProgram(m, opts...).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// openLog writes to $XDG_STATE_HOME/sysx/<name>; stdout is the screen.
func openLog(name string) (*log.Logger, func()) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "state")
	}
	dir = filepath.Join(dir, ui.App)
	if err := os.MkdirAll(dir, 0o700); err == nil {
		if f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			return log.New(f, "", log.LstdFlags), func() { f.Close() }
		}
	}
	return log.New(os.Stderr, "", 0), func() {}
}
