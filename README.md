# sysx — outliers first

A macOS process monitor for the moment you would otherwise open Activity
Monitor: the processes that look wrong are at the top of the list, each one
says why, and the selected process shows its family, command line,
environment, open files and sockets, and the tmux pane it runs in. Killing it
(or its whole tree, or every copy of it) is one confirmed key away.

```
make install   # builds and copies to ~/.local/bin/sysx
sysx           # sample every 2s
sysx -i 1s     # faster
sudo sysx      # also read root daemons' disk I/O, args and env
```

## History: what was heavy while you were not looking

`sysx record` samples every 10s in the background and keeps an episode for
each stretch a process (or a swarm of same-named processes) was busy, so a
docker build, an endpoint agent or a runaway script can be found after it
is gone.

```
sysx record --install     # launchd agent: starts now and at login (low priority)
sysx history              # last 24h, newest first
sysx history -sort cpu -since 3d
sysx history -grep docker -json
sysx record --uninstall   # stops recording; the history stays
```

In the TUI, `H` opens the same history (`w` cycles 1h/6h/24h/3d/7d, `s`
sorts, `/` filters, `enter` shows an episode), and the live overview says
how often the selected program was heavy in the past day.

An episode records what it cost (CPU time, peak memory, disk and network
bytes), how CPU moved over it, the command line (values after secret-looking
flags masked), working directory, ancestors, and the commands typed into a
shell just before it began — load inside a VM shows up on its helper
(OrbStack, Docker Desktop), and the `docker build` started a moment before is
usually the reason. A build driver (make, cargo, a shell loop) is credited
with the CPU of children that came and went between two samples; a child
that a sample already saw is counted once, on itself.

| Kept when | Default |
|---|---|
| busy | CPU ≥25% (own + exited children), disk ≥4 MB/s, net ≥1 MB/s, or memory ≥10% of RAM |
| ended | 30s without a busy sample, or the process exited; stretches over 10m are written in segments and joined when read |
| worth keeping | ≥30 CPU-seconds, peak memory ≥10% of RAM, ≥512 MB disk or ≥256 MB network; a swarm (≥3 alive, ≥100% together) at ≥120 CPU-seconds |
| retention | 7 days and 64 MB, oldest days dropped first, checked hourly |

Files are one JSONL per day in `$XDG_STATE_HOME/sysx/history` (mode 0600);
the recorder logs to `$XDG_STATE_HOME/sysx/record.log` and costs about 1% of
one core. Root daemons' CPU comes from `ps`; disk bytes and arguments of
other users' processes need `sudo`.

## What counts as an outlier

Every rule is a threshold on a value you can see, judged over a short history
so one spiky sample is not a verdict. The reason is printed in the detail pane.

| Perspective | Rule (defaults) |
|---|---|
| CPU | ≥40% warn, ≥90% alert, averaged over 15s; a single ≥150% sample is a note |
| Memory | footprint ≥4% of RAM warn, ≥10% alert; steady growth ≥300M/≥1G over 2m (`leak`) |
| Disk | ≥20 MB/s warn, ≥100 MB/s alert, averaged over 15s |
| Network | ≥2 MB/s warn, ≥20 MB/s alert (per-process bytes from `nettop`) |
| Threads | ≥300 warn, ≥1000 alert (`thr`) |
| Fan-out | ≥8 copies of one program by one user using ≥200% CPU or ≥15% RAM together, or ≥40 copies (`×N`); the heaviest copy carries the severity, the rest a note |
| Tree | the deepest process whose descendants use ≥300% CPU together (`tree`), e.g. the build driver behind a swarm of compilers |
| State | zombie (`zomb`), stopped (`stop`), a parent not reaping ≥5 zombies (`reap`), an orphan whose process group leader exited (`orph`), alerted when it keeps spinning |

Rows lead with a mark that reads without color (`✗` alert, `!` warn, `.`
note) and the metric cell that caused it is colored. Until you move the
cursor it follows the top row, so the detail pane always shows the worst
process.

## Keys

`?` lists every key. The common ones:

| Key | |
|---|---|
| `j` `k` `g` `G` | move |
| `enter` / `l`, `esc` / `h` | focus the detail pane (j/k scroll it), back |
| `1`–`4`, `tab` | overview · args+env · files+net · tree |
| `/` | filter by name, user, pid or tag (`leak`, `orph`, `×`, `cpu`) |
| `s` `o` `m` | cycle sort, only flagged, only mine |
| `u` | select the parent |
| `x` | signal dialog: `t` SIGTERM, `k` SIGKILL, `g` the tree, `n` every copy, `s` stop/continue |
| `t` | jump to the tmux pane running the process |
| `v` | reveal secret-looking environment values (masked by default) |
| `<` `>` `=` | resize the split |
| `H` | history of past heavy processes |
| `p`, `r` | pause sampling, sample now (reload in history) |

Signals are sent by pid **and** start time: if the pid was reused between the
dialog and the key, nothing is sent. sysx and the processes it runs under are
never part of a tree or same-name sweep.

## Data sources

- libproc (`proc_pidinfo`, `proc_pid_rusage`) through cgo: CPU time, footprint,
  disk bytes, threads, parent, start time, cwd, executable path.
- `ps` (setuid) for other users' CPU time and RSS when libproc refuses them.
- `nettop -P -L 1` for per-process network bytes.
- `kern.procargs2` for argv and environment; `lsof` (selected process only)
  for open files and sockets.
- `host_statistics64`, `vm.swapusage`, `kern.memorystatus_vm_pressure_level`,
  `getfsstat` for the header.

UI state (sort, filters, tab, split) is saved to
`$XDG_STATE_HOME/sysx/ui.json`; keys can be overridden in
`~/.config/sysx/keys.yaml`; logs go to `$XDG_STATE_HOME/sysx/sysx.log`.
