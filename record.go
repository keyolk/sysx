package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/keyolk/sysx/internal/history"
	"github.com/keyolk/sysx/internal/proc"
)

const agentLabel = "io.github.keyolk.sysx.record"

func runRecord(args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	interval := fs.Duration("i", 10*time.Second, "sampling interval")
	keep := fs.Int("keep", 7, "days of history to keep")
	maxMB := fs.Int64("max-mb", 64, "cap on the history's total size, oldest days dropped first")
	install := fs.Bool("install", false, "install and start a launchd agent that records in the background")
	uninstall := fs.Bool("uninstall", false, "stop and remove the launchd agent")
	fs.Parse(args)

	switch {
	case *install:
		return installAgent(*interval, *keep, *maxMB)
	case *uninstall:
		return uninstallAgent()
	}
	if *interval < 2*time.Second {
		*interval = 2 * time.Second
	}
	store := history.Store{Dir: history.DefaultDir(), Keep: *keep, MaxBytes: *maxMB << 20}
	lock, err := history.TryLock(store.Dir)
	if err != nil {
		return err
	}
	defer lock.Release()

	logger, closeLog := openLog("record.log")
	defer closeLog()
	logger.Printf("recording every %s into %s (keep %d days, %d MB)", *interval, store.Dir, *keep, *maxMB)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	prune := func() {
		removed, err := store.Prune(time.Now())
		if err != nil {
			logger.Printf("prune: %v", err)
		}
		for _, f := range removed {
			logger.Printf("pruned %s", f)
		}
	}
	prune()

	coll := proc.NewCollector()
	rec := history.NewRecorder(history.DefaultRules())
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	lastPrune := time.Now()
	for {
		snap, err := coll.Sample(ctx)
		if err != nil {
			logger.Printf("sample: %v", err)
		} else {
			if err := store.Append(rec.Step(snap)); err != nil {
				logger.Printf("append: %v", err)
			}
		}
		if time.Since(lastPrune) >= time.Hour {
			prune()
			lastPrune = time.Now()
		}
		select {
		case <-ctx.Done():
			// Close open stretches so the work in progress is not lost.
			if err := store.Append(rec.Flush()); err != nil {
				logger.Printf("append on exit: %v", err)
			}
			logger.Printf("stopped")
			return nil
		case <-tick.C:
		}
	}
}

func agentPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", agentLabel+".plist")
}

func installAgent(interval time.Duration, keep int, maxMB int64) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	logDir := filepath.Dir(history.DefaultDir())
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return err
	}
	// Background + Nice + LowPriorityIO: the recorder must never be the
	// thing it reports on.
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>record</string>
		<string>-i</string><string>%s</string>
		<string>-keep</string><string>%d</string>
		<string>-max-mb</string><string>%d</string>
	</array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>60</integer>
	<key>ProcessType</key><string>Background</string>
	<key>Nice</key><integer>10</integer>
	<key>LowPriorityIO</key><true/>
	<key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, agentLabel, exe, interval, keep, maxMB, filepath.Join(logDir, "record.err"))
	if err := os.MkdirAll(filepath.Dir(agentPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(agentPath(), []byte(plist), 0o644); err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	// bootout fails when the agent is not loaded yet; that is fine.
	if out, err := exec.Command("launchctl", "bootout", domain+"/"+agentLabel).CombinedOutput(); err != nil {
		log.Printf("launchctl bootout (not loaded is fine): %s", strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("launchctl", "bootstrap", domain, agentPath()).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Printf("installed %s\nrecording with %s every %s, keeping %d days (max %d MB) in %s\n",
		agentPath(), exe, interval, keep, maxMB, history.DefaultDir())
	return nil
}

func uninstallAgent() error {
	domain := "gui/" + strconv.Itoa(os.Getuid())
	out, err := exec.Command("launchctl", "bootout", domain+"/"+agentLabel).CombinedOutput()
	if err != nil {
		fmt.Printf("launchctl bootout: %s\n", strings.TrimSpace(string(out)))
	}
	if err := os.Remove(agentPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Printf("removed %s (history in %s is kept)\n", agentPath(), history.DefaultDir())
	return nil
}
