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
//	theme=light|dark
//
// green means no limit, yellow and red mean "pin to that group". A missing or
// empty group key means the default computed from the topology (cpuDefaults).
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
	themeLight = "light" // black outline (default)
	themeDark  = "dark"  // white outline
)

// Sweep interval in milliseconds: the allowed range and the default.
const (
	intervalMin     = 500
	intervalMax     = 10000
	intervalDefault = 5000
)

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
	c := Config{Mode: modeYellow, IntervalMs: intervalDefault, Theme: themeLight}
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
	case "", themeLight:
	case themeDark:
		c.Theme = v
	default:
		log.Printf("config: ignoring theme=%q: want %s or %s", v, themeLight, themeDark)
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
