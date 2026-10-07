package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// newTestSlice is a sliceBackend on a temp config and a fake 10-core SMT sysfs
// whose runner records every systemctl argv instead of exec'ing it.
func newTestSlice(t *testing.T) (*sliceBackend, *[][]string) {
	t.Helper()
	var calls [][]string
	b := &sliceBackend{
		path:    cfgPath(t),
		sysRoot: fakeSys(t, smt10()),
		nudge:   make(chan struct{}, 1),
		run: func(args ...string) error {
			calls = append(calls, args)
			return nil
		},
	}
	return b, &calls
}

func wantArgs(cpulist string) []string {
	return []string{"--user", "set-property", "--runtime", "claude.slice", "AllowedCPUs=" + cpulist}
}

func TestSliceArgs(t *testing.T) {
	if got := sliceArgs([]int{3, 4, 5, 13, 14, 15}); !reflect.DeepEqual(got, wantArgs("3-5,13-15")) {
		t.Errorf("group: %v", got)
	}
}

func TestSliceModes(t *testing.T) {
	// 10 cores: yellow is the top 7 cores (3-9,13-19), red the top 3 (7-9,17-19).
	tests := []struct {
		mode, cpulist, status string
	}{
		{modeYellow, "3-9,13-19", "yellow 3-9,13-19"},
		{modeRed, "7-9,17-19", "red 7-9,17-19"},
		{modeGreen, "0-19", "green 0-19"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			b, calls := newTestSlice(t)
			if err := b.SetMode(tt.mode); err != nil {
				t.Fatal(err)
			}
			if want := [][]string{wantArgs(tt.cpulist)}; !reflect.DeepEqual(*calls, want) {
				t.Errorf("argv = %v, want %v", *calls, want)
			}
			if got := b.Status(); got != tt.status {
				t.Errorf("Status = %q, want %q", got, tt.status)
			}
			if got := readCfg(t, b.path); got != "mode="+tt.mode+"\n" {
				t.Errorf("config = %q", got)
			}
		})
	}
}

func TestSliceStart(t *testing.T) {
	b, calls := newTestSlice(t)
	if got := b.Status(); !strings.HasPrefix(got, "not applied") {
		t.Errorf("Status before apply = %q", got)
	}
	writeCfg(t, b.path, "mode=red\n")
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	if want := [][]string{wantArgs("7-9,17-19")}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("argv = %v, want %v", *calls, want)
	}
}

func TestSliceReload(t *testing.T) {
	b, calls := newTestSlice(t)
	if err := b.SetMode(modeYellow); err != nil {
		t.Fatal(err)
	}
	if err := configSetGroup(b.path, b.sysRoot, modeYellow, "1,2,11,12"); err != nil {
		t.Fatal(err)
	}
	if err := b.Reload(); err != nil {
		t.Fatal(err)
	}
	want := [][]string{wantArgs("3-9,13-19"), wantArgs("1-2,11-12")}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("argv = %v, want %v", *calls, want)
	}
	if got := b.Status(); got != "yellow 1-2,11-12" {
		t.Errorf("Status = %q", got)
	}
}

func TestSliceRunnerError(t *testing.T) {
	b, _ := newTestSlice(t)
	if err := b.SetMode(modeYellow); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	b.run = func(...string) error { return boom }
	if err := b.SetMode(modeRed); !errors.Is(err, boom) {
		t.Errorf("SetMode err = %v, want boom", err)
	}
	if err := b.Reload(); !errors.Is(err, boom) {
		t.Errorf("Reload err = %v, want boom", err)
	}
	// The mode is saved even though applying failed; Status shows what is live.
	if got := readCfg(t, b.path); got != "mode=red\n" {
		t.Errorf("config = %q", got)
	}
	if got := b.Status(); got != "yellow 3-9,13-19" {
		t.Errorf("Status = %q, want the last applied", got)
	}
}

func TestSliceBadMode(t *testing.T) {
	b, calls := newTestSlice(t)
	if err := b.SetMode("blue"); err == nil {
		t.Error("SetMode(blue) succeeded")
	}
	if len(*calls) != 0 {
		t.Errorf("systemctl ran: %v", *calls)
	}
}

// A broken topology must not turn into AllowedCPUs= (which would clear the limit),
// green included.
func TestSliceNoTopologyApplyNothing(t *testing.T) {
	b, calls := newTestSlice(t)
	b.sysRoot = t.TempDir()
	if err := b.SetMode(modeRed); err == nil {
		t.Error("SetMode succeeded without topology")
	}
	if err := b.SetMode(modeGreen); err == nil {
		t.Error("SetMode(green) succeeded without topology")
	}
	if err := b.Start(); err == nil {
		t.Error("Start succeeded without topology")
	}
	if len(*calls) != 0 {
		t.Errorf("systemctl ran: %v", *calls)
	}
}

// Whatever goes wrong upstream, a non-green mode with no CPUs never reaches systemctl.
func TestSliceApplyRefusesEmptyGroup(t *testing.T) {
	b, calls := newTestSlice(t)
	if err := b.apply(Config{Mode: modeRed}, nil); err == nil {
		t.Error("apply(red, no CPUs) succeeded")
	}
	if len(*calls) != 0 {
		t.Errorf("systemctl ran: %v", *calls)
	}
}

func TestSliceApplyRefusesGreenWithoutCPUs(t *testing.T) {
	b, calls := newTestSlice(t)
	b.sysRoot = t.TempDir()
	if err := b.apply(Config{Mode: modeGreen}, nil); err == nil {
		t.Error("apply(green, no topology) succeeded")
	}
	if len(*calls) != 0 {
		t.Errorf("systemctl ran: %v", *calls)
	}
}

// The real runner is never used by the tests above.
var _ sliceRunner = sliceSystemctl

func TestSliceSnapshot(t *testing.T) {
	b, _ := newTestSlice(t)
	if s := b.Snapshot(); s.Mode != "" || len(s.Cores) != 0 {
		t.Errorf("before apply: %+v", s)
	}
	if err := b.SetMode(modeRed); err != nil {
		t.Fatal(err)
	}
	want := sliceSnapshot{Mode: modeRed, Theme: themeDark, Cores: map[string]string{
		"green": "0-19", "yellow": "3-9,13-19", "red": "7-9,17-19",
	}}
	got := b.Snapshot()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Snapshot = %+v, want %+v", got, want)
	}
	got.Cores["red"] = "x" // a copy: must not leak back
	if b.Snapshot().Cores["red"] != "7-9,17-19" {
		t.Error("Snapshot shares its map")
	}
	// A failed apply leaves the last good snapshot.
	b.run = func(...string) error { return errors.New("boom") }
	_ = b.SetMode(modeGreen)
	if b.Snapshot().Mode != modeRed {
		t.Errorf("Mode after failed apply = %q", b.Snapshot().Mode)
	}
}

func TestSliceChanged(t *testing.T) {
	b, _ := newTestSlice(t)
	ch := b.Changed()
	pending := func() bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	if pending() {
		t.Fatal("fired before any apply")
	}
	if err := b.SetMode(modeRed); err != nil {
		t.Fatal(err)
	}
	if !pending() || pending() {
		t.Error("want exactly one signal after one apply")
	}
	// Two applies with nobody reading must not block; they coalesce.
	if err := b.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := b.SetMode(modeGreen); err != nil {
		t.Fatal(err)
	}
	if !pending() || pending() {
		t.Error("want one coalesced signal")
	}
	// Failed applies do not signal.
	b.run = func(...string) error { return errors.New("boom") }
	_ = b.Reload()
	if pending() {
		t.Error("signal after failed apply")
	}
}

func TestSliceInterval(t *testing.T) {
	b, _ := newTestSlice(t)
	if got := b.Interval(); got != 5*time.Second {
		t.Errorf("default Interval = %v", got)
	}
	writeCfg(t, b.path, "interval=1500\n")
	if err := b.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := b.Interval(); got != 1500*time.Millisecond {
		t.Errorf("Interval after reload = %v", got)
	}
	if err := configSetInterval(b.path, 800); err != nil {
		t.Fatal(err)
	}
	if err := b.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := b.Interval(); got != 800*time.Millisecond {
		t.Errorf("Interval after second reload = %v", got)
	}
}

func TestSliceIntervalChanged(t *testing.T) {
	b, _ := newTestSlice(t)
	nudged := func() bool {
		select {
		case <-b.IntervalChanged():
			return true
		default:
			return false
		}
	}
	writeCfg(t, b.path, "interval=1500\n")
	if err := b.Reload(); err != nil || !nudged() {
		t.Fatalf("first load: err %v, nudged %v, want nudge", err, nudged())
	}
	if err := b.Reload(); err != nil || nudged() {
		t.Error("nudge without an interval change")
	}
	writeCfg(t, b.path, "interval=800\n")
	if err := b.Reload(); err != nil || !nudged() {
		t.Error("no nudge after interval change")
	}
}

// TestSliceThemeReload: the snapshot carries the theme from the config, and a
// reload that changes only the theme still signals Changed.
func TestSliceThemeReload(t *testing.T) {
	b, _ := newTestSlice(t)
	ch := b.Changed()
	if err := b.Reload(); err != nil {
		t.Fatal(err)
	}
	<-ch
	if got := b.Snapshot().Theme; got != themeDark {
		t.Errorf("default theme = %q", got)
	}
	if err := configSetTheme(b.path, themeLight); err != nil {
		t.Fatal(err)
	}
	if err := b.Reload(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	default:
		t.Error("no Changed after a theme-only reload")
	}
	if got := b.Snapshot().Theme; got != themeLight {
		t.Errorf("theme after reload = %q", got)
	}
}
