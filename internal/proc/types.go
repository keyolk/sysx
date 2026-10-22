// Package proc samples processes and system totals on macOS. A Collector
// keeps the previous sample so every rate (CPU, disk, network) is a delta
// over the real elapsed time, not a kernel-side decaying average.
package proc

import (
	"fmt"
	"time"
)

// State is the BSD run state of a process.
type State byte

const (
	StateUnknown State = '?'
	StateIdle    State = 'I'
	StateRun     State = 'R'
	StateSleep   State = 'S'
	StateStop    State = 'T'
	StateZombie  State = 'Z'
)

func stateOf(status int) State {
	switch status {
	case 1:
		return StateIdle
	case 2:
		return StateRun
	case 3:
		return StateSleep
	case 4:
		return StateStop
	case 5:
		return StateZombie
	}
	return StateUnknown
}

// Proc is one process in one sample.
type Proc struct {
	PID, PPID, UID, PGID int
	User                 string
	Name                 string
	Start                time.Time
	State                State
	Nice                 int

	CPU     float64 // percent of one core over the last interval
	CPUTime time.Duration
	// ChildCPU is the CPU of children that exited and were reaped during
	// the interval, in percent of one core: a build driver's compilers
	// that came and went between two samples.
	ChildCPU     float64
	ChildCPUTime time.Duration
	RSS          uint64
	Footprint    uint64 // what Activity Monitor calls Memory; RSS when unreadable
	VSZ          uint64
	Threads      int

	DiskRead, DiskWrite float64 // bytes per second
	DiskTotal           uint64  // bytes read+written since start
	NetIn, NetOut       float64 // bytes per second
	NetTotal            uint64

	// Partial is set when the kernel refused the detailed task info (another
	// user's process without root): CPU and RSS then come from ps, and disk
	// and threads are unknown.
	Partial bool
}

// Key identifies a process across samples. A pid alone is reused; pid plus
// start time is not.
func (p Proc) Key() string { return Key(p.PID, p.Start) }

// Key builds the identity of a process from its pid and start time.
func Key(pid int, start time.Time) string {
	return fmt.Sprintf("%d@%d", pid, start.UnixMicro())
}

// ID makes Proc a list item; the cursor follows the process, not the row.
func (p Proc) ID() string { return p.Key() }

// Volume is one mounted local filesystem.
type Volume struct {
	Mount       string
	Total, Free uint64
}

// System is the machine-wide part of a sample.
type System struct {
	NCPU                     int
	CPUUser, CPUSys, CPUIdle float64 // percent of all cores
	Load                     [3]float64

	MemTotal      uint64
	MemUsed       uint64 // app + wired + compressed, as Activity Monitor
	MemApp        uint64
	MemWired      uint64
	MemCompressed uint64
	MemCached     uint64
	SwapTotal     uint64
	SwapUsed      uint64
	SwapIns       float64 // pages per second
	SwapOuts      float64
	Pressure      Pressure

	DiskRead, DiskWrite float64 // sum over readable processes
	NetIn, NetOut       float64 // sum over every process nettop reports

	Volumes []Volume
}

// Pressure is the kernel's memory pressure level.
type Pressure int

const (
	PressureUnknown  Pressure = 0
	PressureNormal   Pressure = 1
	PressureWarn     Pressure = 2
	PressureCritical Pressure = 4
)

func (p Pressure) String() string {
	switch p {
	case PressureNormal:
		return "normal"
	case PressureWarn:
		return "warn"
	case PressureCritical:
		return "critical"
	}
	return "?"
}

// Snapshot is one sample.
type Snapshot struct {
	At       time.Time
	Interval time.Duration // since the previous sample; 0 on the first
	Sys      System
	Procs    []Proc
	// Warnings are partial failures (nettop missing, ps failed); the rest of
	// the sample is still good.
	Warnings []string
}
