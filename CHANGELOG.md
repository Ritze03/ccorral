# Changelog

All notable user-facing changes, newest first. Categories:

**Added** (new features),
**Fixed** (bug and behaviour fixes),
**Removed** (things taken out),
**Info** (notes worth knowing).

## 2026-10-28

### Fixed
- **Pinned threads follow slice changes**: Threads that were pinned to a slice's old CPUs
  are moved when its allowed CPUs change, so a changed core selection takes effect
  everywhere.

## 2026-10-25

### Added
- **CPU split**: The desktop slices (`app`, `session`, `background`) get the cores Claude
  does not, so Claude and your desktop never share cores. `ccorral` can run outside the
  split and release it.
- **Split switch**: `split=on|off` in the config and a Split row in settings turn the
  split off. The tray shows no desktop part when the desktop already has every core.

## 2026-10-24

### Removed
- **Built-in installer**: `ccorral install` and `uninstall` are gone. `install.sh` (or
  `ritz-get install ccorral`) builds, links and installs the systemd unit, and the daemon
  creates `claude.slice` if it is missing.

### Fixed
- **First install**: A running old install is restarted on first install, and you are told
  when the service is disabled.

## 2026-10-22

### Added
- **Install options**: `install --yes` and `uninstall --yes` answer every question yes
  without reading stdin.

### Info
- **Agent hint**: The README tells Claude Code sessions to launch commands with
  `ccorral --` via `CLAUDE.md`.

## 2026-10-21

### Added
- **Run in the slice**: `ccorral -- CMD` runs a command inside `claude.slice`.

## 2026-10-12

### Info
- **README**: The intro mentions the weights and the memory limit.

## 2026-10-10

### Added
- **CPU weight**: A CPU weight setting for the slice (default idle).
- **I/O weight and memory limit**: Settings for I/O weight and a soft memory limit
  (`MemoryHigh`).

## 2026-10-07

### Added
- **First release**: Pins Claude Code CLI sessions, and everything they spawn, to a chosen
  set of CPU cores in the systemd user slice `claude.slice`, so your desktop stays
  responsive. Linux only.
- **Sweeper**: A daemon moves new Claude processes into the slice, every 5 s by default
  (setting: sweep interval).
- **Tray and settings**: A tray icon with radio modes (`dark` icon theme by default) and a
  terminal settings UI with per-core groups and the sweep interval.
- **CLI**: `ccorral status`, `reload`, `green`, `yellow` and `red` talk to the running
  daemon, and tell you to install when the service is missing.
