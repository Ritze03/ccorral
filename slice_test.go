package main

import (
	"errors"
	"os"
	"path/filepath"
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

// wantArgs is the argv with the default weights and memory limit.
func wantArgs(cpulist string) []string { return wantArgsW(cpulist, "idle", "10", "75%") }

func wantArgsW(cpulist, cpu, io, mem string) []string {
	return []string{"--user", "set-property", "--runtime", "claude.slice", "AllowedCPUs=" + cpulist,
		"CPUWeight=" + cpu, "IOWeight=" + io, "MemoryHigh=" + mem}
}

// wantOutside is the three desktop calls: AllowedCPUs only, one per slice.
func wantOutside(cpulist string) [][]string {
	var out [][]string
	for _, sl := range []string{"app.slice", "session.slice", "background.slice"} {
		out = append(out, []string{"--user", "set-property", "--runtime", sl, "AllowedCPUs=" + cpulist})
	}
	return out
}

// wantCalls is the claude.slice call followed by the three desktop calls.
func wantCalls(claude [][]string, outside string) [][]string {
	return append(append([][]string{}, claude...), wantOutside(outside)...)
}

func TestSliceArgs(t *testing.T) {
	def := Config{CPUWeight: "idle", IOWeight: "10", MemoryHigh: "75%"}
	if got := sliceArgs([]int{3, 4, 5, 13, 14, 15}, def); !reflect.DeepEqual(got, wantArgs("3-5,13-15")) {
		t.Errorf("group: %v", got)
	}
	c := Config{CPUWeight: "100", IOWeight: "500", MemoryHigh: "16G"}
	if got := sliceArgs([]int{0, 1}, c); !reflect.DeepEqual(got, wantArgsW("0-1", "100", "500", "16G")) {
		t.Errorf("set values: %v", got)
	}
}

func TestSliceModes(t *testing.T) {
	// 10 cores: yellow is the top 7 cores (3-9,13-19), red the top 3 (7-9,17-19).
	// The desktop gets the complement; green shares all CPUs.
	tests := []struct {
		mode, cpulist, outside, status string
	}{
		{modeYellow, "3-9,13-19", "0-2,10-12", "yellow 3-9,13-19 desktop=0-2,10-12 cpu=idle io=10 mem=75%"},
		{modeRed, "7-9,17-19", "0-6,10-16", "red 7-9,17-19 desktop=0-6,10-16 cpu=idle io=10 mem=75%"},
		{modeGreen, "0-19", "0-19", "green 0-19 desktop=0-19 cpu=idle io=10 mem=75%"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			b, calls := newTestSlice(t)
			if err := b.SetMode(tt.mode); err != nil {
				t.Fatal(err)
			}
			if want := wantCalls([][]string{wantArgs(tt.cpulist)}, tt.outside); !reflect.DeepEqual(*calls, want) {
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

// The weights and memory limit from the config go out in every mode, green included.
func TestSliceLimits(t *testing.T) {
	for _, tt := range []struct{ mode, cpulist, outside string }{
		{modeGreen, "0-19", "0-19"}, {modeYellow, "3-9,13-19", "0-2,10-12"}, {modeRed, "7-9,17-19", "0-6,10-16"},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			b, calls := newTestSlice(t)
			writeCfg(t, b.path, "cpuweight=50\nioweight=500\nmemoryhigh=16G\n")
			if err := b.SetMode(tt.mode); err != nil {
				t.Fatal(err)
			}
			if want := wantCalls([][]string{wantArgsW(tt.cpulist, "50", "500", "16G")}, tt.outside); !reflect.DeepEqual(*calls, want) {
				t.Errorf("argv = %v, want %v", *calls, want)
			}
			if got, want := b.Status(), tt.mode+" "+tt.cpulist+" desktop="+tt.outside+" cpu=50 io=500 mem=16G"; got != want {
				t.Errorf("Status = %q, want %q", got, want)
			}
			// Changed limits are picked up by Reload.
			for _, err := range []error{configSetCPUWeight(b.path, "idle"), configSetIOWeight(b.path, "10"), configSetMemoryHigh(b.path, "75%")} {
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := b.Reload(); err != nil {
				t.Fatal(err)
			}
			if got := (*calls)[4]; !reflect.DeepEqual(got, wantArgs(tt.cpulist)) {
				t.Errorf("after reload argv = %v", got)
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
	if want := wantCalls([][]string{wantArgs("7-9,17-19")}, "0-6,10-16"); !reflect.DeepEqual(*calls, want) {
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
	want := append(wantCalls([][]string{wantArgs("3-9,13-19")}, "0-2,10-12"), wantCalls([][]string{wantArgs("1-2,11-12")}, "0,3-10,13-19")...)
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("argv = %v, want %v", *calls, want)
	}
	if got := b.Status(); got != "yellow 1-2,11-12 desktop=0,3-10,13-19 cpu=idle io=10 mem=75%" {
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
	if got := b.Status(); got != "yellow 3-9,13-19 desktop=0-2,10-12 cpu=idle io=10 mem=75%" {
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
	want := sliceSnapshot{Mode: modeRed, Theme: themeDark, Desktop: "0-6,10-16", Cores: map[string]string{
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

func TestSliceEnsureFile(t *testing.T) {
	cfg := t.TempDir()
	sys := fakeSys(t, smt10())
	var calls [][]string
	run := func(args ...string) error { calls = append(calls, args); return nil }
	path := filepath.Join(cfg, "systemd", "user", "claude.slice")

	if err := sliceEnsureFile(cfg, sys, run); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "[Slice]\nAllowedCPUs=3-9,13-19\n") {
		t.Errorf("slice file:\n%s", got)
	}
	if !reflect.DeepEqual(calls, [][]string{{"--user", "daemon-reload"}}) {
		t.Errorf("calls = %v", calls)
	}

	// An existing file (here hand-written) is left alone and no reload runs.
	if err := os.WriteFile(path, []byte("[Slice]\nAllowedCPUs=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls = nil
	if err := sliceEnsureFile(cfg, sys, run); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "[Slice]\nAllowedCPUs=1\n" {
		t.Errorf("existing slice was rewritten:\n%s", b)
	}
	if len(calls) != 0 {
		t.Errorf("calls = %v, want none", calls)
	}
}

// A hand-set non-green group that covers every CPU leaves nothing for the
// desktop: it gets all CPUs instead of an empty list.
func TestSliceOutsideEmptyComplement(t *testing.T) {
	b, calls := newTestSlice(t)
	writeCfg(t, b.path, "mode=yellow\nyellow=0-19\n")
	if err := b.Reload(); err != nil {
		t.Fatal(err)
	}
	if want := wantCalls([][]string{wantArgs("0-19")}, "0-19"); !reflect.DeepEqual(*calls, want) {
		t.Errorf("argv = %v, want %v", *calls, want)
	}
	for _, c := range *calls {
		if strings.HasSuffix(c[len(c)-1], "AllowedCPUs=") {
			t.Errorf("empty AllowedCPUs sent: %v", c)
		}
	}
	if got := b.Status(); got != "yellow 0-19 desktop=0-19 cpu=idle io=10 mem=75%" {
		t.Errorf("Status = %q", got)
	}
}

// One outside slice failing does not stop the others, the error names it,
// claude.slice stays applied and the desktop is reported as failed.
func TestSliceOutsideFailure(t *testing.T) {
	b, calls := newTestSlice(t)
	b.run = func(args ...string) error {
		*calls = append(*calls, args)
		if len(args) > 3 && args[3] == "session.slice" {
			return errors.New("boom")
		}
		return nil
	}
	ch := b.Changed()
	err := b.SetMode(modeYellow)
	if err == nil || !strings.Contains(err.Error(), "session.slice") ||
		strings.Contains(err.Error(), "app.slice") || strings.Contains(err.Error(), "background.slice") {
		t.Fatalf("SetMode err = %v, want one naming only session.slice", err)
	}
	if want := wantCalls([][]string{wantArgs("3-9,13-19")}, "0-2,10-12"); !reflect.DeepEqual(*calls, want) {
		t.Errorf("argv = %v, want %v", *calls, want)
	}
	if got := b.Status(); got != "yellow 3-9,13-19 desktop=failed(session.slice) cpu=idle io=10 mem=75%" {
		t.Errorf("Status = %q", got)
	}
	if s := b.Snapshot(); s.Mode != modeYellow || s.Desktop != "" {
		t.Errorf("Snapshot = %+v, want yellow without desktop", s)
	}
	select {
	case <-ch:
	default:
		t.Error("claude.slice applied but no Changed signal")
	}
}

// If claude.slice itself fails, the desktop is not touched.
func TestSliceClaudeFailureSkipsOutside(t *testing.T) {
	b, calls := newTestSlice(t)
	b.run = func(args ...string) error {
		*calls = append(*calls, args)
		return errors.New("boom")
	}
	if err := b.SetMode(modeYellow); err == nil {
		t.Fatal("SetMode succeeded")
	}
	if len(*calls) != 1 {
		t.Errorf("calls = %v, want only the claude.slice call", *calls)
	}
}

// Uninstall: every outside slice back to all CPUs, nothing else, no revert.
func TestSliceRelease(t *testing.T) {
	var calls [][]string
	all, err := sliceRelease(fakeSys(t, smt10()), func(args ...string) error {
		calls = append(calls, args)
		return nil
	})
	if err != nil || all != "0-19" {
		t.Fatalf("sliceRelease = %q, %v", all, err)
	}
	if want := wantOutside("0-19"); !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
	// A failure keeps going and names the slice.
	calls = nil
	_, err = sliceRelease(fakeSys(t, smt10()), func(args ...string) error {
		calls = append(calls, args)
		if args[3] == "app.slice" {
			return errors.New("boom")
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "app.slice") || len(calls) != 3 {
		t.Errorf("err = %v, calls = %v", err, calls)
	}
	// No topology: nothing is sent.
	calls = nil
	if _, err := sliceRelease(t.TempDir(), func(args ...string) error { calls = append(calls, args); return nil }); err == nil || len(calls) != 0 {
		t.Errorf("no topology: err = %v, calls = %v", err, calls)
	}
}
