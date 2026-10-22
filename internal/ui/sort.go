package ui

import (
	"cmp"
	"strings"
)

type sortKey int

const (
	sortScore sortKey = iota
	sortCPU
	sortMem
	sortDisk
	sortNet
	sortThreads
	sortPID
	sortName
	sortCount
)

var sortNames = [...]string{"score", "cpu", "mem", "disk", "net", "threads", "pid", "name"}

func (s sortKey) String() string { return sortNames[s] }

func parseSort(s string) sortKey {
	for i, n := range sortNames {
		if n == s {
			return sortKey(i)
		}
	}
	return sortScore
}

func (s sortKey) next() sortKey { return (s + 1) % sortCount }

// cmp orders rows; every key falls back to CPU then pid so equal rows do not
// trade places between samples.
func (s sortKey) cmp(a, b row) int {
	var c int
	switch s {
	case sortScore:
		c = cmp.Compare(b.r.Score, a.r.Score)
	case sortCPU:
		c = cmp.Compare(b.p.CPU, a.p.CPU)
	case sortMem:
		c = cmp.Compare(b.p.Footprint, a.p.Footprint)
	case sortDisk:
		c = cmp.Compare(b.p.DiskRead+b.p.DiskWrite, a.p.DiskRead+a.p.DiskWrite)
	case sortNet:
		c = cmp.Compare(b.p.NetIn+b.p.NetOut, a.p.NetIn+a.p.NetOut)
	case sortThreads:
		c = cmp.Compare(b.p.Threads, a.p.Threads)
	case sortPID:
		c = cmp.Compare(a.p.PID, b.p.PID)
	case sortName:
		c = strings.Compare(strings.ToLower(a.p.Name), strings.ToLower(b.p.Name))
	}
	if c != 0 {
		return c
	}
	if c = cmp.Compare(b.p.CPU, a.p.CPU); c != 0 {
		return c
	}
	return cmp.Compare(a.p.PID, b.p.PID)
}
