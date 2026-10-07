package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fakeSys builds a sysfs tree: siblings maps a cpu id to its
// thread_siblings_list; an empty string makes a cpu dir without topology.
func fakeSys(t *testing.T, siblings map[int]string) string {
	t.Helper()
	root := t.TempDir()
	for id, list := range siblings {
		dir := filepath.Join(root, "devices/system/cpu", fmt.Sprintf("cpu%d", id))
		if list != "" {
			dir = filepath.Join(dir, "topology")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if list != "" {
			if err := os.WriteFile(filepath.Join(dir, "thread_siblings_list"), []byte(list+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// smt10 is 10 cores where cpu n and n+10 are siblings, like the dev machine.
func smt10() map[int]string {
	m := map[int]string{}
	for i := 0; i < 10; i++ {
		m[i] = fmt.Sprintf("%d,%d", i, i+10)
		m[i+10] = m[i]
	}
	return m
}

func TestCpuParse(t *testing.T) {
	tests := []struct {
		in      string
		want    []int
		wantErr bool
	}{
		{"3-9,13-19", []int{3, 4, 5, 6, 7, 8, 9, 13, 14, 15, 16, 17, 18, 19}, false},
		{"3-9 13-19", []int{3, 4, 5, 6, 7, 8, 9, 13, 14, 15, 16, 17, 18, 19}, false},
		{"0,10\n", []int{0, 10}, false},
		{"5", []int{5}, false},
		{"5,5,3-5", []int{3, 4, 5}, false},
		{"9,1", []int{1, 9}, false},
		{"7-7", []int{7}, false},
		{"", []int{}, false},
		{"  ", []int{}, false},
		{"a", nil, true},
		{"-1", nil, true},
		{"1--3", nil, true},
		{"9-3", nil, true},
		{"1-", nil, true},
		{"1-2-3", nil, true},
		{"1.5", nil, true},
		{"0x3", nil, true},
		{"+3", nil, true},
		{"0-4000000000", nil, true},
		{"4096", nil, true},
		{"4095", []int{4095}, false},
	}
	for _, tt := range tests {
		got, err := cpuParse(tt.in)
		if (err != nil) != tt.wantErr || (!tt.wantErr && !reflect.DeepEqual(got, tt.want)) {
			t.Errorf("cpuParse(%q) = %v, %v; want %v, err=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestCpuFormat(t *testing.T) {
	tests := []struct {
		in   []int
		want string
	}{
		{[]int{3, 4, 5, 6, 7, 8, 9, 13, 14, 15, 16, 17, 18, 19}, "3-9,13-19"},
		{[]int{5}, "5"},
		{[]int{0, 10}, "0,10"},
		{[]int{1, 2}, "1-2"},
		{[]int{9, 1, 2, 2}, "1-2,9"},
		{nil, ""},
	}
	for _, tt := range tests {
		if got := cpuFormat(tt.in); got != tt.want {
			t.Errorf("cpuFormat(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCpuCores(t *testing.T) {
	tests := []struct {
		name string
		sys  map[int]string
		want [][]int
	}{
		{"10 cores smt", smt10(), [][]int{{0, 10}, {1, 11}, {2, 12}, {3, 13}, {4, 14}, {5, 15}, {6, 16}, {7, 17}, {8, 18}, {9, 19}}},
		{"4 cores no smt", map[int]string{0: "0", 1: "1", 2: "2", 3: "3"}, [][]int{{0}, {1}, {2}, {3}}},
		{"adjacent pairs", map[int]string{0: "0-1", 1: "0-1", 2: "2-3", 3: "2-3"}, [][]int{{0, 1}, {2, 3}}},
		{"offline cpu ignored", map[int]string{0: "0", 1: "", 2: "2"}, [][]int{{0}, {2}}},
		{"single", map[int]string{0: "0"}, [][]int{{0}}},
	}
	for _, tt := range tests {
		got, err := cpuCores(fakeSys(t, tt.sys))
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: cpuCores = %v, %v; want %v", tt.name, got, err, tt.want)
		}
	}
	if _, err := cpuCores(t.TempDir()); err == nil {
		t.Error("empty sysfs: want error")
	}
}

func TestCpuDefaults(t *testing.T) {
	tests := []struct {
		name        string
		sys         map[int]string
		yellow, red string
	}{
		{"10 cores smt", smt10(), "3-9,13-19", "7-9,17-19"},
		{"4 cores no smt", map[int]string{0: "0", 1: "1", 2: "2", 3: "3"}, "1-3", "3"},
		{"1 core", map[int]string{0: "0"}, "0", "0"},
		{"1 core smt", map[int]string{0: "0-1", 1: "0-1"}, "0-1", "0-1"},
		{"2 cores", map[int]string{0: "0", 1: "1"}, "1", "1"},
		{"3 cores", map[int]string{0: "0", 1: "1", 2: "2"}, "1-2", "2"},
		{"adjacent pairs", map[int]string{0: "0-1", 1: "0-1", 2: "2-3", 3: "2-3", 4: "4-5", 5: "4-5"}, "2-5", "4-5"},
		{"6 cores", map[int]string{0: "0", 1: "1", 2: "2", 3: "3", 4: "4", 5: "5"}, "2-5", "4-5"},
	}
	for _, tt := range tests {
		cores, err := cpuCores(fakeSys(t, tt.sys))
		if err != nil {
			t.Fatal(err)
		}
		y, r := cpuDefaults(cores)
		if cpuFormat(y) != tt.yellow || cpuFormat(r) != tt.red {
			t.Errorf("%s: yellow=%s red=%s; want %s, %s", tt.name, cpuFormat(y), cpuFormat(r), tt.yellow, tt.red)
		}
	}
}

func TestCpuCheck(t *testing.T) {
	cores, _ := cpuCores(fakeSys(t, smt10()))
	tests := []struct {
		cpus    []int
		wantErr bool
	}{
		{[]int{0}, false},
		{[]int{3, 19}, false},
		{nil, true},
		{[]int{20}, true},
		{[]int{1, 99}, true},
	}
	for _, tt := range tests {
		if err := cpuCheck(cores, tt.cpus); (err != nil) != tt.wantErr {
			t.Errorf("cpuCheck(%v) err=%v, wantErr %v", tt.cpus, err, tt.wantErr)
		}
	}
}

// TestCpuRealSys checks the real /sys against this dev machine's layout
// (10 cores, n and n+10 siblings) and skips anywhere else.
func TestCpuRealSys(t *testing.T) {
	cores, err := cpuCores("/sys")
	if err != nil {
		t.Skip("no /sys topology:", err)
	}
	y, r := cpuDefaults(cores)
	t.Logf("real /sys: %d physical cores, yellow=%s red=%s", len(cores), cpuFormat(y), cpuFormat(r))
	if len(cores) != 10 || cpuFormat(cores[0]) != "0,10" {
		t.Skip("not the 10-core dev machine")
	}
	if cpuFormat(y) != "3-9,13-19" || cpuFormat(r) != "7-9,17-19" {
		t.Errorf("yellow=%s red=%s", cpuFormat(y), cpuFormat(r))
	}
}
