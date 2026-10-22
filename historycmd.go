package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/keyolk/tuikit/render"

	"github.com/keyolk/sysx/internal/history"
	"github.com/keyolk/sysx/internal/outlier"
)

func runHistory(args []string) error {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	since := fs.Duration("since", 24*time.Hour, "how far back")
	sortBy := fs.String("sort", "time", "time, cpu, mem, disk or net")
	asJSON := fs.Bool("json", false, "print episodes as JSON lines")
	grep := fs.String("grep", "", "only episodes whose name, command or chain contains this")
	fs.Parse(args)

	st := history.Store{Dir: history.DefaultDir()}
	eps, err := st.Load(time.Now().Add(-*since))
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err)
	}
	if *grep != "" {
		q := strings.ToLower(*grep)
		eps = slices.DeleteFunc(eps, func(e history.Episode) bool { return !history.Matches(e, q) })
	}
	history.Sort(eps, *sortBy)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		for _, e := range eps {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
		return nil
	}
	if pid, _ := history.RecorderPID(st.Dir); pid == 0 {
		fmt.Fprintln(os.Stderr, "note: no recorder is running; `sysx record --install` starts one")
	}
	if len(eps) == 0 {
		fmt.Printf("nothing recorded in the last %s\n", *since)
		return nil
	}
	fmt.Printf("%-11s %8s  %-24s %9s %6s %6s %7s %7s  %s\n", "WHEN", "FOR", "NAME", "CPU-TIME", "AVG%", "PEAK%", "MEM", "DISK", "COMMAND")
	for _, e := range eps {
		name := e.Name
		if e.Kind == history.KindGroup {
			name = fmt.Sprintf("%s ×%d", e.Name, e.Procs)
		}
		fmt.Printf("%-11s %8s  %-24s %9s %6.0f %6.0f %7s %7s  %s\n",
			e.Start.Local().Format("01-02 15:04"), history.Dur(e.Duration()),
			render.Truncate(name, 24), history.Dur(time.Duration(e.TotalCPUSec()*float64(time.Second))),
			e.AvgCPU(), e.PeakCPU, outlier.Bytes(e.PeakMem), outlier.Bytes(e.DiskBytes),
			render.Truncate(e.Command(), 80))
	}
	return nil
}
