package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// newTestSlice is a sliceBackend on a temp config and a fake 10-core SMT sysfs
// whose runner records every systemctl argv instead of exec'ing it.
func newTestSlice(t *testing.T) (*sliceBackend, *[][]string) {
	t.Helper()
	var calls [][]string
	b := &sliceBackend{
		path:    cfgPath(t),
		sysRoot: fakeSys(t, smt10()),
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
