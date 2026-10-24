package main

// slice.go applies a mode to claude.slice and is the daemon's ipcBackend.
//
// Four properties, set together with `systemctl --user set-property --runtime`:
// AllowedCPUs (the mode) and CPUWeight, IOWeight and MemoryHigh (from the
// config, the same in every mode, green included; CPUWeight=idle needs systemd
// 252+, IOWeight only matters on disks using the BFQ scheduler). The properties live in the user
// manager's runtime state, so they vanish on reboot and the daemon re-applies
// the saved mode at start. Green sets the explicit list of all CPUs: an empty
// AllowedCPUs= drops the cpuset controller and leaves running processes on
// their old, narrowed mask.

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// sliceCallTimeout stays under the IPC client's ipcTimeout (set-property is fast).
const sliceCallTimeout = 4 * time.Second

// sliceRunner runs systemctl with args. Tests replace it so nothing is exec'd.
type sliceRunner func(args ...string) error

// sliceSystemctl is the real runner.
func sliceSystemctl(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), sliceCallTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sliceArgs is the systemctl argv that sets claude.slice to cpus and cfg's
// weights and memory limit.
func sliceArgs(cpus []int, cfg Config) []string {
	return []string{"--user", "set-property", "--runtime", sweepSlice, "AllowedCPUs=" + cpuFormat(cpus),
		"CPUWeight=" + cfg.CPUWeight, "IOWeight=" + cfg.IOWeight, "MemoryHigh=" + cfg.MemoryHigh}
}

// sliceBackend is the daemon's state: the config file, the sysfs root and what
// was last applied to the slice.
type sliceBackend struct {
	path    string
	sysRoot string
	run     sliceRunner

	mu      sync.Mutex
	applied string // "<mode> <cpulist> cpu=.. io=.. mem=.." of the last successful apply, "" before
	snap    sliceSnapshot
	changed chan struct{} // created on first use
	nudge   chan struct{} // signalled when the sweep interval changes

	ms atomic.Int64 // sweep interval in ms, 0 before the first config load; not under mu
}

// sliceSnapshot is what the tray shows: the live mode and each mode's cores.
type sliceSnapshot struct {
	Mode  string            // "green" | "yellow" | "red" ("" before first successful apply)
	Cores map[string]string // "green","yellow","red" -> cpulist, e.g. "3-9,13-19"
	Theme string            // themeLight | themeDark ("" before first successful apply)
}

// Snapshot is a copy of the state after the last successful apply.
func (b *sliceBackend) Snapshot() sliceSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := sliceSnapshot{Mode: b.snap.Mode, Theme: b.snap.Theme, Cores: map[string]string{}}
	for k, v := range b.snap.Cores {
		s.Cores[k] = v
	}
	return s
}

// chanLocked returns the change channel. b.mu held.
func (b *sliceBackend) chanLocked() chan struct{} {
	if b.changed == nil {
		b.changed = make(chan struct{}, 1)
	}
	return b.changed
}

// Changed receives a value after every successful apply (coalesced if nobody reads).
func (b *sliceBackend) Changed() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.chanLocked()
}

// Interval is the sweep interval from the last loaded config. It never waits
// for an in-flight apply (which holds b.mu across systemctl).
func (b *sliceBackend) Interval() time.Duration {
	if ms := b.ms.Load(); ms != 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return intervalDefault * time.Millisecond
}

// IntervalChanged receives a value after the sweep interval changes (coalesced).
func (b *sliceBackend) IntervalChanged() <-chan struct{} { return b.nudge }

func newSliceBackend() *sliceBackend {
	return &sliceBackend{path: configPath(), sysRoot: "/sys", run: sliceSystemctl, nudge: make(chan struct{}, 1)}
}

// apply sets the slice to cfg's mode. A topology failure (err from configLoad)
// applies nothing; a mode with no CPUs is refused too. b.mu held.
func (b *sliceBackend) apply(cfg Config, err error) error {
	if ms := int64(cfg.IntervalMs); ms != 0 && ms != b.ms.Swap(ms) {
		log.Printf("ccorral: sweep interval %d ms", ms)
		select {
		case b.nudge <- struct{}{}:
		default:
		}
	}
	if err != nil {
		return err
	}
	group := cfg.Group(cfg.Mode)
	var all []int // explicit full list: AllowedCPUs= (empty) would not widen running processes
	if cores, err := cpuCores(b.sysRoot); err == nil {
		for _, c := range cores {
			all = append(all, c...)
		}
		sort.Ints(all)
	} else if cfg.Mode == modeGreen {
		return err
	}
	if cfg.Mode == modeGreen {
		group = all
	}
	if len(group) == 0 {
		return fmt.Errorf("%s has no CPUs: refusing to clear the limit", cfg.Mode)
	}
	if err := b.run(sliceArgs(group, cfg)...); err != nil {
		return err
	}
	b.applied = cfg.Mode + " " + cpuFormat(group) + " cpu=" + cfg.CPUWeight + " io=" + cfg.IOWeight + " mem=" + cfg.MemoryHigh
	b.snap = sliceSnapshot{Mode: cfg.Mode, Theme: cfg.Theme, Cores: map[string]string{
		modeGreen: cpuFormat(all), modeYellow: cpuFormat(cfg.Yellow), modeRed: cpuFormat(cfg.Red),
	}}
	select {
	case b.chanLocked() <- struct{}{}:
	default:
	}
	return nil
}

// Start applies the saved mode (the runtime property is gone after a reboot).
func (b *sliceBackend) Start() error { return b.Reload() }

func (b *sliceBackend) Status() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.applied == "" {
		return "not applied (see the daemon log)"
	}
	return b.applied
}

func (b *sliceBackend) SetMode(mode string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := configSetMode(b.path, mode); err != nil {
		return err
	}
	return b.apply(configLoad(b.path, b.sysRoot))
}

// Reload re-reads the config and applies its mode.
func (b *sliceBackend) Reload() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.apply(configLoad(b.path, b.sysRoot))
}

// sliceFileTemplate is the claude.slice written when none exists; %s is the
// yellow cpulist. It is user config: ccorral never rewrites or removes it.
const sliceFileTemplate = `# Created by ccorral on first start; yours to edit, ccorral never rewrites it.
[Unit]
Description=Claude Code and everything it spawns (created by ccorral)

[Slice]
AllowedCPUs=%s
`

// configDir is $XDG_CONFIG_HOME, or ~/.config.
func configDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config")
}

// sliceEnsureFile writes <cfgDir>/systemd/user/claude.slice, limited to the
// yellow cores, if no such file exists, and asks the user manager to reload.
// An existing file (or a dangling symlink) is never touched. The daemon's
// runtime properties apply on top of it either way.
func sliceEnsureFile(cfgDir, sysRoot string, run sliceRunner) error {
	path := filepath.Join(cfgDir, "systemd", "user", sweepSlice)
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	cores, err := cpuCores(sysRoot)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	yellow, _ := cpuDefaults(cores)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	// O_EXCL: if something appeared meanwhile, leave it alone.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return fmt.Errorf("creating %s: %w", path, err)
	}
	_, werr := fmt.Fprintf(f, sliceFileTemplate, cpuFormat(yellow))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(path)
		return fmt.Errorf("creating %s: %w", path, werr)
	}
	log.Printf("ccorral: created %s", path)
	return run("--user", "daemon-reload")
}
