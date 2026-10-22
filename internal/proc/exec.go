package proc

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func isNotFound(err error) bool { return errors.Is(err, exec.ErrNotFound) }

type netBytes struct{ in, out uint64 }

// readNettop takes one cumulative sample of per-process bytes. Streaming
// mode (-L 0) block-buffers into a pipe, so one-shot runs are used instead.
func readNettop(ctx context.Context) (map[int]netBytes, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nettop", "-P", "-L", "1", "-n", "-x", "-J", "bytes_in,bytes_out").Output()
	if err != nil {
		return nil, err
	}
	return parseNettop(out), nil
}

// parseNettop reads lines like "Google Chrome H.1234,5678,910,". The name
// may contain dots and commas never; the pid is after the last dot.
func parseNettop(out []byte) map[int]netBytes {
	res := map[int]netBytes{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ",")
		if len(f) < 3 || f[0] == "" {
			continue
		}
		dot := strings.LastIndexByte(f[0], '.')
		if dot < 0 {
			continue
		}
		pid, err := strconv.Atoi(f[0][dot+1:])
		if err != nil {
			continue
		}
		in, err1 := strconv.ParseUint(f[1], 10, 64)
		o, err2 := strconv.ParseUint(f[2], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		res[pid] = netBytes{in, o}
	}
	return res
}

type psFill struct {
	rss uint64
	cpu time.Duration
}

// readPS fills RSS and CPU time for processes libproc refused. /bin/ps is
// setuid root, so it sees them.
func readPS(ctx context.Context, pids []int) (map[int]psFill, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ids := make([]string, len(pids))
	for i, p := range pids {
		ids[i] = strconv.Itoa(p)
	}
	out, err := exec.CommandContext(ctx, "ps", "-o", "pid=,rss=,time=", "-p", strings.Join(ids, ",")).Output()
	// ps exits 1 when one of the pids is gone; the rest of the output holds.
	var ee *exec.ExitError
	if err != nil && !(errors.As(err, &ee) && len(out) > 0) {
		return nil, err
	}
	return parsePS(out), nil
}

func parsePS(out []byte) map[int]psFill {
	res := map[int]psFill{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		kb, _ := strconv.ParseUint(f[1], 10, 64)
		res[pid] = psFill{rss: kb * 1024, cpu: parseCPUTime(f[2])}
	}
	return res
}

// parseCPUTime reads ps time: "1:02.34", "12:01:02.34" or "1-02:03:04.5".
func parseCPUTime(s string) time.Duration {
	var days int
	if d, rest, ok := strings.Cut(s, "-"); ok {
		days, _ = strconv.Atoi(d)
		s = rest
	}
	parts := strings.Split(s, ":")
	var secs float64
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return 0
		}
		secs = secs*60 + v
	}
	secs += float64(days) * 86400
	return time.Duration(secs * float64(time.Second))
}

// Conn is one socket from lsof.
type Conn struct {
	Proto string // TCP or UDP
	Name  string // local->remote, or the listening address
	State string // ESTABLISHED, LISTEN, ...
}

// Files is what lsof says one process holds open.
type Files struct {
	Count int    // every fd
	Conns []Conn // sockets only
	Paths []string
}

// OpenFiles runs lsof for one pid. It is slow (100ms–1s), so the UI calls it
// off the loop, for the selected process only.
func OpenFiles(ctx context.Context, pid int) (Files, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-F", "ftPnT", "-p", strconv.Itoa(pid)).Output()
	var ee *exec.ExitError
	if err != nil && !(errors.As(err, &ee) && len(out) > 0) {
		if errors.As(err, &ee) {
			return Files{}, fmt.Errorf("lsof: no access (another user's process; run sysx with sudo)")
		}
		return Files{}, err
	}
	return parseLsof(out), nil
}

// parseLsof reads lsof -F output: one field per line, the first character
// naming it (f fd, t type, P protocol, n name, T tcp info like TST=LISTEN).
func parseLsof(out []byte) Files {
	var res Files
	var cur struct {
		typ, proto, name, state string
		open                    bool
	}
	flush := func() {
		if !cur.open {
			return
		}
		res.Count++
		switch {
		case cur.typ == "IPv4" || cur.typ == "IPv6":
			res.Conns = append(res.Conns, Conn{Proto: cur.proto, Name: cur.name, State: cur.state})
		case cur.typ == "REG" && cur.name != "":
			res.Paths = append(res.Paths, cur.name)
		}
		cur = struct {
			typ, proto, name, state string
			open                    bool
		}{}
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		l := sc.Text()
		if l == "" {
			continue
		}
		v := l[1:]
		switch l[0] {
		case 'f':
			flush()
			// cwd/txt/mem entries are not file descriptors.
			if v == "cwd" || v == "txt" || v == "rtd" || strings.HasPrefix(v, "mem") {
				continue
			}
			cur.open = true
		case 't':
			cur.typ = v
		case 'P':
			cur.proto = v
		case 'n':
			cur.name = v
		case 'T':
			if s, ok := strings.CutPrefix(v, "ST="); ok {
				cur.state = s
			}
		}
	}
	flush()
	return res
}
