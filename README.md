# ccorral

Pins Claude Code CLI sessions, and everything they spawn (builds, tests, MCP servers), to a
chosen set of CPU cores, with a low CPU and I/O weight and a soft memory limit, so the rest of
the desktop stays responsive. Linux only.

It works through `AllowedCPUs`, `CPUWeight`, `IOWeight` and `MemoryHigh` on the systemd user
slice `claude.slice`, so a change applies live to running sessions and their children. A daemon sweeps every 5 s by default (`interval=`, 500 to 10000 ms) and moves Claude
sessions that were started outside the slice (tmux, other terminals, anything that doesn't use
the wrapper below) into it. Claude is recognised by its executable under
`~/.local/share/claude/versions/`.

## Modes

| Mode | Cores |
| --- | --- |
| green | all cores, no limit |
| yellow | about two thirds of the physical cores (the default) |
| red | about one third |

Groups are whole physical cores together with their SMT siblings, counted from the last core
down, so core 0 stays free for the desktop. On a 10-core / 20-thread machine, yellow is
`3-9,13-19` and red is `7-9,17-19`.

The choice is saved in `~/.config/ccorral/config`:

```
mode=yellow
yellow=3-9,13-19
red=7-9,17-19
interval=5000
theme=dark
cpuweight=idle
ioweight=10
memoryhigh=75%
```

`yellow=` and `red=` are optional cpulists you can set by hand; a missing or empty key means
the computed default, and an invalid list is ignored with a log line. `interval=` is the sweep
interval in milliseconds, 500 to 10000, default 5000; anything else is ignored with a log line. `theme=` is the tray icon's outline: `dark` (default,
white, for dark panels) or `light` (black, for light panels); anything else is ignored with a log line.
`cpuweight=` is the CPU weight of the Claude slice in every mode: `idle` (default) or a number from 1 to
10000; anything else is ignored with a log line. `idle` means Claude only gets CPU time nothing else
wants, so in green it can still use the whole machine while you are idle, but your own work always wins.
100 is the normal systemd weight. `idle` needs systemd 252 or newer.
`ioweight=` is the slice's I/O weight, 1 to 10000, default 10 (systemd's normal is 100), so your own disk
access wins over Claude's. It only has an effect on disks using the BFQ I/O scheduler
(`cat /sys/block/<dev>/queue/scheduler`); `none`, `mq-deadline` and `kyber` ignore it.
`memoryhigh=` is the slice's soft memory limit: `infinity` (no limit), a percentage of RAM like `75%`
(default) or a size like `16G` (suffixes K, M, G, T). Above it the kernel throttles the slice and reclaims its
memory instead of letting it push the desktop into swap; it does not kill anything.
Both are applied in every mode, with invalid values ignored with a log line.
After editing the file, run `ccorral reload` so the daemon picks it up. Or skip the editing and
use `ccorral settings` (below).

## Requirements

- Linux with systemd, cgroup v2 and the `cpuset` controller delegated to your user manager.
  Check that this prints a line containing `cpuset`:

  ```sh
  cat /sys/fs/cgroup/user.slice/user-$(id -u).slice/user@$(id -u).service/cgroup.subtree_control
  ```

- Go 1.27 or newer to build.

## Build

```sh
git clone https://github.com/Ritze03/ccorral && cd ccorral
CGO_ENABLED=0 go build -o ccorral .
```

One static binary, no cgo.

## Install

```sh
./ccorral install
```

Every run is a full reinstall: re-running `./ccorral install` from a fresh build updates everything (stops the service, replaces the binary and unit, starts it again). Running the *installed* `ccorral install` (`~/.local/bin/ccorral`) has no new binary to copy; it warns and only re-applies the same version.

It asks first and writes nothing until you confirm a summary of what will change. Then it:

- stops the service if it is running, then copies the binary to `~/.local/bin/ccorral` (and tells you if that isn't on your `$PATH`)
- writes `~/.config/systemd/user/ccorral.service`
- writes `~/.config/systemd/user/claude.slice` with `AllowedCPUs=` set to the yellow group.
  The file starts with a marker line; ccorral rewrites a slice carrying it freely, but if you
  already have a hand-written `claude.slice` it shows it next to the replacement and asks
  before taking it over (decline and it is left alone)
- offers to remove an old `~/.local/bin/claude-cpus` script, if there is one
- runs `systemctl --user daemon-reload` and `enable --now ccorral.service` (restarting it if
  it was already running)

To remove it again:

```sh
ccorral uninstall
```

After a confirmation this disables and stops the service, removes `ccorral.service`, and
removes `~/.local/bin/ccorral`. The slice file and `~/.config/ccorral/config` stay. The slice
keeps its CPU limit until reboot; `systemctl --user revert claude.slice` drops it right away.

## Usage

```sh
ccorral                  # same as: ccorral status
ccorral green            # or: yellow / red
ccorral reload           # re-read the config file and re-apply it
ccorral settings         # terminal UI for the cores per mode, the sweep interval, the icon theme, the CPU and I/O weight and the memory limit
ccorral -- CMD [ARGS...] # run CMD inside the slice (same: ccorral run [--] CMD [ARGS...])
ccorral daemon           # what the service runs
ccorral help
```

The daemon logs to the journal:

```sh
journalctl --user -u ccorral
```

## Running commands in the slice

```sh
ccorral -- npm run dev
```

Runs the command inside `claude.slice`, so it gets the slice's cores and weights from its first
instruction. This is the one way to launch anything long-running (dev servers, builds,
background daemons) so it can't escape the slice; agents should use it for those. `ccorral run
CMD` is the same, with the `--` optional.

It replaces itself with the command (via `systemd-run --user --scope`), so stdin, the terminal,
signals and the exit code behave as if you had run the command directly. If it is already
inside the slice, it just runs the command, so nesting is harmless. A missing command exits 127.

## Instant placement (optional)

Without this, a new Claude session is moved into the slice by the next sweep, within one sweep interval (5 s by default, `interval=` 500–10000 ms). To
start it inside the slice from the first moment, wrap `claude` in your `.zshrc` or `.bashrc`:

```sh
claude() { ccorral -- claude "$@"; }
```

This doesn't recurse: the command is looked up in `PATH`, where shell functions don't count.

## Known limits

- A background daemon a session started before it was moved, and that has since reparented
  away (a gradle or sccache server, say), stays outside the slice. Start such things with
  `ccorral -- CMD` to avoid it.
- Claude sessions from the Claude desktop app are not handled.
- A process that set its own CPU affinity (e.g. with `taskset`) keeps it. Restart it.

## Tray icon

The daemon shows a tray icon: a round gauge, an outline with a transparent inside, filled from
the bottom in the colour of the current mode (green, yellow or red). The outline is white for
`theme=dark` (default) and black for `theme=light`; before the first apply it is just the empty outline. The filled
area is the share of CPUs Claude may use, so green is full, and with the example above yellow is 70% and red 30%. Hover
for the tooltip, for example `ccorral — Yellow: 3-9,13-19`.

- **Left click** cycles green, yellow, red, green, and so on.
- **Menu** (right click, or whatever your host does) has three radio rows, one per mode, each
  with its cores. Pick one to switch.

It is a StatusNotifierItem, so it needs a tray host that speaks that protocol: Quickshell, KDE
Plasma, waybar's tray module and the like. Without a host the daemon just logs it and carries
on. The service usually starts before the tray host does; ccorral watches for the host and
registers again when it appears or restarts. Switching the mode from the CLI or the menu
updates the icon either way.

## ccorral settings

```sh
ccorral settings
```

A full-screen terminal UI for the config file. It shows a grid with one column per physical
core and a row each for Yellow and Red; `[x]` means the core is in that group. SMT siblings
always toggle together, and the last core of a group can't be unchecked. The third row is the
sweep interval, 500 to 10000 ms in 500 ms steps. The fourth is the icon theme, `dark (white
outline)` (default) or `light (black outline)`. The fifth is the CPU weight: left/right steps through `idle`, 1, 10, 50, 100
(systemd's normal weight), 200, 500, 1000 and 10000. The sixth is the I/O weight, the same
steps without `idle`. The seventh is the memory limit: 25%, 50%, 60%, 75%, 90% and `infinity`. A value you
set by hand in the file is shown as is; stepping from a hand-set weight goes to the next preset, from a hand-set
size to 75%.

| Key | Action |
| --- | --- |
| arrows or `h` `j` `k` `l` | move; on the Interval row left/right changes the value, on the Theme row it toggles, on the CPU wt, IO wt and Mem high rows it steps |
| space or enter | toggle the core under the cursor, or the theme |
| `+` / `-` | change the interval |
| `d` | reset the row to its default |
| `q`, Esc | quit |

Every change is saved to `~/.config/ccorral/config` right away and the daemon is told to
reload, so it applies live; the status line says whether that worked. With no daemon running,
the change is saved and applies when it starts. It needs a terminal. Switching the mode is not
done here, use the tray or `ccorral green|yellow|red`.

## Layout

A flat `package main` at the repo root, one file per area: `cpus.go`, `config.go`,
`sweep.go`, `slice.go`, `ipc.go`, `tray.go`, `settings.go`, `run.go`, `install.go` and `main.go`. Tests sit next to them
(`go test ./...`).
