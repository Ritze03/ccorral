package main

// cpus.go is everything about CPU numbers: reading the physical-core topology
// from sysfs, the cpulist syntax shared by the kernel, systemd and the config
// file ("3-9,13-19"), and the default Yellow / Red groups.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// cpuCores returns the physical cores under sysRoot ("/sys" in production): one
// sorted list of logical CPU ids per core, cores ordered by their lowest id.
// Siblings share the same thread_siblings_list, so one list is one core. CPUs
// without a topology dir (offline) are skipped.
func cpuCores(sysRoot string) ([][]int, error) {
	files, _ := filepath.Glob(filepath.Join(sysRoot, "devices/system/cpu/cpu[0-9]*/topology/thread_siblings_list"))
	seen := map[string]bool{}
	var cores [][]int
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		list, err := cpuParse(string(b))
		if err != nil || len(list) == 0 {
			continue
		}
		if k := cpuFormat(list); !seen[k] {
			seen[k] = true
			cores = append(cores, list)
		}
	}
	if len(cores) == 0 {
		return nil, fmt.Errorf("no CPU topology under %s", sysRoot)
	}
	sort.Slice(cores, func(i, j int) bool { return cores[i][0] < cores[j][0] })
	return cores, nil
}

// cpuParse reads a cpulist such as "3-9,13-19" into sorted unique ids. Spaces
// work as separators too, because `systemctl show` prints "3-9 13-19". An empty
// string is an empty list; garbage, negatives and reversed ranges are errors.
func cpuParse(s string) ([]int, error) {
	set := map[int]bool{}
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		lo, hi, isRange := strings.Cut(p, "-")
		a, err := strconv.Atoi(lo)
		if err != nil || a < 0 || lo != strconv.Itoa(a) {
			return nil, fmt.Errorf("bad cpu list %q", s)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a || hi != strconv.Itoa(b) {
				return nil, fmt.Errorf("bad cpu list %q", s)
			}
		}
		for i := a; i <= b; i++ {
			set[i] = true
		}
	}
	out := make([]int, 0, len(set))
	for i := range set {
		out = append(out, i)
	}
	sort.Ints(out)
	return out, nil
}

// cpuFormat is the inverse of cpuParse: comma-separated, runs collapsed to
// ranges, a lone CPU as plain "5". The input is sorted and deduplicated first.
func cpuFormat(cpus []int) string {
	s := append([]int(nil), cpus...)
	sort.Ints(s)
	var parts []string
	for i := 0; i < len(s); {
		j := i
		for j+1 < len(s) && s[j+1] <= s[j]+1 {
			j++
		}
		if s[i] == s[j] {
			parts = append(parts, strconv.Itoa(s[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", s[i], s[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// cpuDefaults are the default groups: whole physical cores, counted from the
// last one down so core 0 stays free. Yellow is two thirds of the cores, Red one
// third, each rounded to nearest and at least one core.
func cpuDefaults(cores [][]int) (yellow, red []int) {
	n := len(cores)
	pick := func(k int) []int {
		k = min(max(k, 1), n)
		var out []int
		for _, c := range cores[n-k:] {
			out = append(out, c...)
		}
		sort.Ints(out)
		return out
	}
	// round(2n/3) and round(n/3) in integers: round(x/3) = (2x+3)/6.
	return pick((4*n + 3) / 6), pick((2*n + 3) / 6)
}

// cpuCheck says whether cpus is a usable group: not empty, and every CPU is in
// the topology.
func cpuCheck(cores [][]int, cpus []int) error {
	if len(cpus) == 0 {
		return fmt.Errorf("empty cpu list")
	}
	have := map[int]bool{}
	for _, c := range cores {
		for _, id := range c {
			have[id] = true
		}
	}
	for _, id := range cpus {
		if !have[id] {
			return fmt.Errorf("cpu %d is not in the topology", id)
		}
	}
	return nil
}
