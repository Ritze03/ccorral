package main

// slice.go applies a mode to claude.slice and is the daemon's ipcBackend.
//
// The only knob is AllowedCPUs, set with `systemctl --user set-property
// --runtime`: the property lives in the user manager's runtime state, so it
// vanishes on reboot and the daemon re-applies the saved mode at start. Green
// sets the explicit list of all CPUs: an empty AllowedCPUs= drops the cpuset
// controller and leaves running processes on their old, narrowed mask.

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
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

// sliceArgs is the systemctl argv that sets claude.slice to cpus.
func sliceArgs(cpus []int) []string {
	return []string{"--user", "set-property", "--runtime", sweepSlice, "AllowedCPUs=" + cpuFormat(cpus)}
}

// sliceBackend is the daemon's state: the config file, the sysfs root and what
// was last applied to the slice.
type sliceBackend struct {
	path    string
	sysRoot string
	run     sliceRunner

	mu      sync.Mutex
	applied string // "<mode> <cpulist>" of the last successful apply, "" before
}

func newSliceBackend() *sliceBackend {
	return &sliceBackend{path: configPath(), sysRoot: "/sys", run: sliceSystemctl}
}

// apply sets the slice to cfg's mode. A topology failure (err from configLoad)
// applies nothing; a mode with no CPUs is refused too. b.mu held.
func (b *sliceBackend) apply(cfg Config, err error) error {
	if err != nil {
		return err
	}
	group := cfg.Group(cfg.Mode)
	if cfg.Mode == modeGreen { // explicit full list: AllowedCPUs= (empty) would not widen running processes
		cores, err := cpuCores(b.sysRoot)
		if err != nil {
			return err
		}
		group = nil
		for _, c := range cores {
			group = append(group, c...)
		}
		sort.Ints(group)
	}
	if len(group) == 0 {
		return fmt.Errorf("%s has no CPUs: refusing to clear the limit", cfg.Mode)
	}
	if err := b.run(sliceArgs(group)...); err != nil {
		return err
	}
	b.applied = cfg.Mode + " " + cpuFormat(group)
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
