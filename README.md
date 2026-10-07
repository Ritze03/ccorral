# ccorral

Pins Claude Code CLI sessions, and everything they spawn (builds, tests, MCP servers), to a
chosen set of CPU cores, so the rest of the desktop stays responsive. Linux only.

It works through `AllowedCPUs` on the systemd user slice `claude.slice`, so a change applies
live to running sessions and their children. A daemon sweeps every 5 s by default (`interval=`, 500 to 10000 ms) and moves Claude
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
```

`yellow=` and `red=` are optional cpulists you can set by hand; a missing or empty key means
the computed default, and an invalid list is ignored with a log line. `interval=` is the sweep
interval in milliseconds, 500 to 10000, default 5000; anything else is ignored with a log line.
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

It asks first and writes nothing until you confirm a summary of what will change. Then it:

- copies the binary to `~/.local/bin/ccorral` (and tells you if that isn't on your `$PATH`)
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
ccorral settings         # terminal UI for the cores per mode and the sweep interval
ccorral daemon           # what the service runs
ccorral help
```

The daemon logs to the journal:

```sh
journalctl --user -u ccorral
```

## Instant placement (optional)

Without this, a new Claude session is moved into the slice by the next sweep, within one sweep interval (5 s by default, `interval=` 500–10000 ms). To
start it inside the slice from the first moment, wrap `claude` in your `.zshrc` or `.bashrc`:

```sh
claude() { systemd-run --user --scope --slice=claude.slice -q -- claude "$@"; }
```

## Known limits

- A background daemon a session started before it was moved, and that has since reparented
  away (a gradle or sccache server, say), stays outside the slice.
- Claude sessions from the Claude desktop app are not handled.
- Only cores are limited. There is no CPU or IO priority.
- A process that set its own CPU affinity (e.g. with `taskset`) keeps it. Restart it.

## Tray icon

The daemon shows a tray icon: a round gauge, grey with a black outline, filled from the bottom
in the colour of the current mode (green, yellow or red). The filled area is the share of CPUs
Claude may use, so green is full, and with the example above yellow is 70% and red 30%. Hover
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
sweep interval, 500 to 10000 ms in 500 ms steps.

| Key | Action |
| --- | --- |
| arrows or `h` `j` `k` `l` | move; on the Interval row left/right changes the value |
| space or enter | toggle the core under the cursor |
| `+` / `-` | change the interval |
| `d` | reset the row to its default |
| `q`, Esc | quit |

Every change is saved to `~/.config/ccorral/config` right away and the daemon is told to
reload, so it applies live; the status line says whether that worked. With no daemon running,
the change is saved and applies when it starts. It needs a terminal. Switching the mode is not
done here, use the tray or `ccorral green|yellow|red`.

## Layout

A flat `package main` at the repo root, one file per area: `cpus.go`, `config.go`,
`sweep.go`, `slice.go`, `ipc.go`, `tray.go`, `settings.go`, `install.go` and `main.go`. Tests sit next to them
(`go test ./...`).
