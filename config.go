package main

// config.go is the on-disk settings file, shared by everything that reads or
// writes it: the daemon, the tray, the settings UI and the CLI, which run as
// separate processes.
//
// The file is a flat list of "key=value" lines:
//
//	mode=green|yellow|red
//	yellow=<cpulist>
//	red=<cpulist>
//	interval=<ms>
//	theme=dark|light
//	cpuweight=idle|<1-10000>
//	ioweight=<1-10000>
//	memoryhigh=infinity|<1-100>%|<bytes>[K|M|G|T]
//
// green means no limit, yellow and red mean "pin to that group". A missing or
// empty group key means the default computed from the topology (cpuDefaults).
// cpuweight, ioweight and memoryhigh are the slice's CPUWeight, IOWeight and
// MemoryHigh in every mode; cpuweight=idle means it only gets CPU time nothing
// else wants.
// Blank lines, "#" comments and unknown keys are kept as they are on rewrite.

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	modeGreen  = "green"
	modeYellow = "yellow"
	modeRed    = "red"
)

// Tray icon theme: the outline colour. light is for light panels.
const (
	themeLight = "light" // black outline
	themeDark  = "dark"  // white outline (default)
)

// Sweep interval in milliseconds: the allowed range and the default.
const (
	intervalMin     = 500
	intervalMax     = 10000
	intervalDefault = 5000
)

// CPUWeight of claude.slice: "idle" or an integer weightMin..weightMax (systemd's
// range; 100 is its normal weight). idle needs systemd 252+. IOWeight takes the
// same range without idle.
const (
	weightIdle = "idle"
	weightMin  = 1
	weightMax  = 10000

	ioWeightDefault    = "10"
	memoryHighInfinity = "infinity"
	memoryHighDefault  = "75%"
)

// ioWeightValid says whether w is a plain integer in the allowed range.
func ioWeightValid(w string) bool {
	n, err := strconv.Atoi(w)
	return err == nil && n >= weightMin && n <= weightMax && strconv.Itoa(n) == w
}

// cpuWeightValid says whether w is "idle" or a valid IO-style weight.
func cpuWeightValid(w string) bool { return w == weightIdle || ioWeightValid(w) }

// memoryHighValid says whether v is "infinity", a percentage 1-100 with "%", or
// a plain positive integer with an optional K, M, G or T suffix (systemd's
// 1024-based units). It is passed to systemd verbatim.
func memoryHighValid(v string) bool {
	if v == memoryHighInfinity {
		return true
	}
	if p, ok := strings.CutSuffix(v, "%"); ok {
		n, err := strconv.Atoi(p)
		return err == nil && n >= 1 && n <= 100 && strconv.Itoa(n) == p
	}
	if len(v) > 0 && strings.Contains("KMGT", v[len(v)-1:]) {
		v = v[:len(v)-1]
	}
	n, err := strconv.ParseUint(v, 10, 63)
	return err == nil && n >= 1 && strconv.FormatUint(n, 10) == v
}

// configMu serializes read-modify-write within this process; the rename in
// configSet keeps other processes from ever seeing a half-written file.
var configMu sync.Mutex

// configPath is $XDG_CONFIG_HOME/ccorral/config, falling back to ~/.config.
func configPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "ccorral", "config")
}

// Config is the settings with the groups already resolved.
type Config struct {
	Mode           string // modeGreen, modeYellow or modeRed
	Yellow, Red    []int  // explicit or default
	YellowExplicit bool   // false: Yellow is the computed default
	RedExplicit    bool
	IntervalMs     int    // sweep interval, intervalMin..intervalMax
	Theme          string // themeLight or themeDark
	CPUWeight      string // weightIdle or "1".."10000"
	IOWeight       string // "1".."10000"
	MemoryHigh     string // memoryHighInfinity, "<n>%" or "<n>[KMGT]"
}

// Group is the CPU set a mode pins to; nil for green (no limit).
func (c Config) Group(mode string) []int {
	switch mode {
	case modeYellow:
		return c.Yellow
	case modeRed:
		return c.Red
	}
	return nil
}

// configReadLines is the file split into lines; a missing file is no lines.
func configReadLines(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// configCut splits a "key=value" line; comments and garbage are not ok.
func configCut(line string) (k, v string, ok bool) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "#") {
		return "", "", false
	}
	k, v, ok = strings.Cut(line, "=")
	return strings.TrimSpace(k), strings.TrimSpace(v), ok
}

// configKV reads the key/value pairs; later lines win, anything else is ignored.
func configKV(path string) map[string]string {
	out := map[string]string{}
	for _, line := range configReadLines(path) {
		if k, v, ok := configCut(line); ok {
			out[k] = v
		}
	}
	return out
}

// configSet sets key to value (or removes it when value is empty), keeping every
// other line. The write goes to a temp file that is renamed over the config.
func configSet(path, key, value string) error {
	configMu.Lock()
	defer configMu.Unlock()
	var out []string
	done := false
	for _, line := range configReadLines(path) {
		if k, _, ok := configCut(line); ok && k == key {
			if !done && value != "" {
				out = append(out, key+"="+value)
			}
			done = true // also drops duplicates of the key
			continue
		}
		out = append(out, line)
	}
	if !done && value != "" {
		out = append(out, key+"="+value)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // a no-op once renamed
	text := ""
	if len(out) > 0 {
		text = strings.Join(out, "\n") + "\n"
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// configLoad reads the config at path and resolves the groups against the
// topology under sysRoot. A missing file, bad mode or bad group is not an error:
// it falls back to the default (and a bad group is logged). The error is only
// the topology failing, in which case the groups are nil.
func configLoad(path, sysRoot string) (Config, error) {
	kv := configKV(path)
	c := Config{Mode: modeYellow, IntervalMs: intervalDefault, Theme: themeDark, CPUWeight: weightIdle, IOWeight: ioWeightDefault, MemoryHigh: memoryHighDefault}
	switch kv["mode"] {
	case modeGreen, modeYellow, modeRed:
		c.Mode = kv["mode"]
	}
	if v := kv["interval"]; v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= intervalMin && ms <= intervalMax {
			c.IntervalMs = ms
		} else {
			log.Printf("config: ignoring interval=%q: want an integer from %d to %d ms", v, intervalMin, intervalMax)
		}
	}
	switch v := kv["theme"]; v {
	case "", themeDark:
	case themeLight:
		c.Theme = v
	default:
		log.Printf("config: ignoring theme=%q: want %s or %s", v, themeLight, themeDark)
	}
	if v := kv["cpuweight"]; v != "" {
		if cpuWeightValid(v) {
			c.CPUWeight = v
		} else {
			log.Printf("config: ignoring cpuweight=%q: want %s or an integer from %d to %d", v, weightIdle, weightMin, weightMax)
		}
	}
	if v := kv["ioweight"]; v != "" {
		if ioWeightValid(v) {
			c.IOWeight = v
		} else {
			log.Printf("config: ignoring ioweight=%q: want an integer from %d to %d", v, weightMin, weightMax)
		}
	}
	if v := kv["memoryhigh"]; v != "" {
		if memoryHighValid(v) {
			c.MemoryHigh = v
		} else {
			log.Printf("config: ignoring memoryhigh=%q: want %s, a percentage like 75%%, or a size like 16G", v, memoryHighInfinity)
		}
	}
	cores, err := cpuCores(sysRoot)
	if err != nil {
		return c, err
	}
	c.Yellow, c.Red = cpuDefaults(cores)
	for _, g := range []struct {
		key      string
		cpus     *[]int
		explicit *bool
	}{{"yellow", &c.Yellow, &c.YellowExplicit}, {"red", &c.Red, &c.RedExplicit}} {
		if kv[g.key] == "" {
			continue
		}
		list, err := cpuParse(kv[g.key])
		if err == nil {
			err = cpuCheck(cores, list)
		}
		if err != nil {
			log.Printf("config: ignoring %s=%q: %v", g.key, kv[g.key], err)
			continue
		}
		*g.cpus, *g.explicit = list, true
	}
	return c, nil
}

// configSetMode persists the mode.
func configSetMode(path, mode string) error {
	switch mode {
	case modeGreen, modeYellow, modeRed:
		return configSet(path, "mode", mode)
	}
	return fmt.Errorf("unknown mode %q", mode)
}

// configSetGroup persists the cpulist of a group ("yellow" or "red"), after
// checking it against the topology under sysRoot. An empty cpulist is rejected;
// configResetGroup goes back to the default.
func configSetGroup(path, sysRoot, group, cpulist string) error {
	if group != modeYellow && group != modeRed {
		return fmt.Errorf("unknown group %q", group)
	}
	list, err := cpuParse(cpulist)
	if err != nil {
		return err
	}
	cores, err := cpuCores(sysRoot)
	if err != nil {
		return err
	}
	if err := cpuCheck(cores, list); err != nil {
		return err
	}
	return configSet(path, group, cpuFormat(list))
}

// configResetGroup drops the explicit cpulist, so the group is the default again.
func configResetGroup(path, group string) error {
	if group != modeYellow && group != modeRed {
		return fmt.Errorf("unknown group %q", group)
	}
	return configSet(path, group, "")
}

// configSetInterval persists the sweep interval in milliseconds.
func configSetInterval(path string, ms int) error {
	if ms < intervalMin || ms > intervalMax {
		return fmt.Errorf("interval %d ms out of range %d-%d", ms, intervalMin, intervalMax)
	}
	return configSet(path, "interval", strconv.Itoa(ms))
}

// configSetTheme persists the tray icon theme.
func configSetTheme(path, theme string) error {
	switch theme {
	case themeLight, themeDark:
		return configSet(path, "theme", theme)
	}
	return fmt.Errorf("unknown theme %q", theme)
}

// configSetCPUWeight persists the slice's CPUWeight: "idle" or 1-10000.
func configSetCPUWeight(path, w string) error {
	if !cpuWeightValid(w) {
		return fmt.Errorf("cpuweight %q: want %s or an integer from %d to %d", w, weightIdle, weightMin, weightMax)
	}
	return configSet(path, "cpuweight", w)
}

// configSetIOWeight persists the slice's IOWeight: 1-10000.
func configSetIOWeight(path, w string) error {
	if !ioWeightValid(w) {
		return fmt.Errorf("ioweight %q: want an integer from %d to %d", w, weightMin, weightMax)
	}
	return configSet(path, "ioweight", w)
}

// configSetMemoryHigh persists the slice's MemoryHigh: infinity, 1-100% or a size.
func configSetMemoryHigh(path, v string) error {
	if !memoryHighValid(v) {
		return fmt.Errorf("memoryhigh %q: want %s, a percentage like 75%%, or a size like 16G", v, memoryHighInfinity)
	}
	return configSet(path, "memoryhigh", v)
}
