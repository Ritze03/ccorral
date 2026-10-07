package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// cfgPath points the config at a fresh temp dir and returns the file path.
func cfgPath(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return configPath()
}

func writeCfg(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readCfg(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestConfigPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x/cfg")
	if got := configPath(); got != "/x/cfg/ccorral/config" {
		t.Errorf("xdg: %s", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/x/home")
	if got := configPath(); got != "/x/home/.config/ccorral/config" {
		t.Errorf("home: %s", got)
	}
}

func TestConfigLoad(t *testing.T) {
	sys := fakeSys(t, smt10())
	tests := []struct {
		name        string
		file        string // "-" means no file
		mode        string
		yellow, red string
		yExp, rExp  bool
	}{
		{"no file", "-", modeYellow, "3-9,13-19", "7-9,17-19", false, false},
		{"empty file", "", modeYellow, "3-9,13-19", "7-9,17-19", false, false},
		{"mode red", "mode=red\n", modeRed, "3-9,13-19", "7-9,17-19", false, false},
		{"mode green", "mode = green\n", modeGreen, "3-9,13-19", "7-9,17-19", false, false},
		{"bad mode", "mode=purple\n", modeYellow, "3-9,13-19", "7-9,17-19", false, false},
		{"explicit groups", "yellow=1-5\nred=2,12\n", modeYellow, "1-5", "2,12", true, true},
		{"spaces in list", "yellow=1-2 4\n", modeYellow, "1-2,4", "7-9,17-19", true, false},
		{"empty group is default", "yellow=\nred=\n", modeYellow, "3-9,13-19", "7-9,17-19", false, false},
		{"garbage group", "yellow=zzz\nred=5\n", modeYellow, "3-9,13-19", "5", false, true},
		{"cpu not in topology", "yellow=1-5,40\n", modeYellow, "3-9,13-19", "7-9,17-19", false, false},
		{"comments and junk", "# hi\nnonsense\n#yellow=1\nmode=red\n", modeRed, "3-9,13-19", "7-9,17-19", false, false},
	}
	for _, tt := range tests {
		path := cfgPath(t)
		if tt.file != "-" {
			writeCfg(t, path, tt.file)
		}
		c, err := configLoad(path, sys)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if c.Mode != tt.mode || cpuFormat(c.Yellow) != tt.yellow || cpuFormat(c.Red) != tt.red ||
			c.YellowExplicit != tt.yExp || c.RedExplicit != tt.rExp {
			t.Errorf("%s: got %+v", tt.name, c)
		}
	}
	if _, err := configLoad(cfgPath(t), t.TempDir()); err == nil {
		t.Error("no topology: want error")
	}
}

func TestConfigGroup(t *testing.T) {
	c := Config{Mode: modeRed, Yellow: []int{1, 2}, Red: []int{2}}
	if !reflect.DeepEqual(c.Group(modeYellow), []int{1, 2}) || !reflect.DeepEqual(c.Group(modeRed), []int{2}) || c.Group(modeGreen) != nil {
		t.Errorf("Group: %+v", c)
	}
}

func TestConfigSet(t *testing.T) {
	sys := fakeSys(t, smt10())
	path := cfgPath(t)

	// fresh: dir 0700, file 0600
	if err := configSetMode(path, modeRed); err != nil {
		t.Fatal(err)
	}
	if got := readCfg(t, path); got != "mode=red\n" {
		t.Errorf("fresh: %q", got)
	}
	for p, want := range map[string]os.FileMode{filepath.Dir(path): 0o700, path: 0o600} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %v", p, st, err, want)
		}
	}

	// unknown keys and comments survive, known keys are replaced in place
	writeCfg(t, path, "# my notes\nfuture=1\nmode=green\n\nmode=yellow\nred=1\n")
	if err := configSetMode(path, modeRed); err != nil {
		t.Fatal(err)
	}
	if err := configSetGroup(path, sys, "yellow", "5 3-4"); err != nil {
		t.Fatal(err)
	}
	if err := configSetGroup(path, sys, "red", "19"); err != nil {
		t.Fatal(err)
	}
	want := "# my notes\nfuture=1\nmode=red\n\nred=19\nyellow=3-5\n"
	if got := readCfg(t, path); got != want {
		t.Errorf("rewrite:\n got %q\nwant %q", got, want)
	}
	c, _ := configLoad(path, sys)
	if c.Mode != modeRed || cpuFormat(c.Yellow) != "3-5" || !c.YellowExplicit || cpuFormat(c.Red) != "19" {
		t.Errorf("reload: %+v", c)
	}

	// reset removes only that key
	if err := configResetGroup(path, "yellow"); err != nil {
		t.Fatal(err)
	}
	if got := readCfg(t, path); got != "# my notes\nfuture=1\nmode=red\n\nred=19\n" {
		t.Errorf("reset: %q", got)
	}

	// no temp files left behind
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "config-*")); len(m) != 0 {
		t.Errorf("leftover temp files: %v", m)
	}
}

func TestConfigSetRejects(t *testing.T) {
	sys := fakeSys(t, smt10())
	path := cfgPath(t)
	writeCfg(t, path, "mode=red\n")
	for _, tt := range []struct{ group, list string }{
		{"yellow", ""},
		{"yellow", "  "},
		{"yellow", "zzz"},
		{"yellow", "9-3"},
		{"yellow", "20"},
		{"red", "1,99"},
		{"blue", "1"},
	} {
		if err := configSetGroup(path, sys, tt.group, tt.list); err == nil {
			t.Errorf("configSetGroup(%q, %q): want error", tt.group, tt.list)
		}
	}
	if err := configSetMode(path, "purple"); err == nil {
		t.Error("configSetMode(purple): want error")
	}
	if err := configResetGroup(path, "mode"); err == nil {
		t.Error("configResetGroup(mode): want error")
	}
	if got := readCfg(t, path); got != "mode=red\n" {
		t.Errorf("file changed by rejected sets: %q", got)
	}
}

func TestConfigInterval(t *testing.T) {
	sys := fakeSys(t, smt10())
	for _, tt := range []struct {
		file    string
		want    int
		wantLog bool
	}{
		{"", 5000, false},
		{"interval=\n", 5000, false},
		{"interval=500\n", 500, false},
		{"interval = 10000\n", 10000, false},
		{"interval=2500\n", 2500, false},
		{"interval=499\n", 5000, true},
		{"interval=10001\n", 5000, true},
		{"interval=abc\n", 5000, true},
		{"interval=1.5\n", 5000, true},
	} {
		var buf bytes.Buffer
		log.SetOutput(&buf)
		path := cfgPath(t)
		writeCfg(t, path, tt.file)
		c, err := configLoad(path, sys)
		log.SetOutput(os.Stderr)
		if err != nil {
			t.Fatal(err)
		}
		if c.IntervalMs != tt.want {
			t.Errorf("%q: IntervalMs = %d, want %d", tt.file, c.IntervalMs, tt.want)
		}
		if got := strings.Contains(buf.String(), "config: ignoring interval="); got != tt.wantLog {
			t.Errorf("%q: logged = %v (%q)", tt.file, got, buf.String())
		}
	}
	// Also set when the topology is unreadable.
	if c, err := configLoad(cfgPath(t), t.TempDir()); err == nil || c.IntervalMs != 5000 {
		t.Errorf("no topology: %+v, %v", c, err)
	}
}

func TestConfigSetInterval(t *testing.T) {
	path := cfgPath(t)
	writeCfg(t, path, "mode=red\n")
	for _, ms := range []int{499, 10001, 0, -1} {
		err := configSetInterval(path, ms)
		if err == nil || !strings.Contains(err.Error(), "500-10000") {
			t.Errorf("configSetInterval(%d) = %v, want range error", ms, err)
		}
	}
	if got := readCfg(t, path); got != "mode=red\n" {
		t.Errorf("file changed by rejected sets: %q", got)
	}
	for _, ms := range []int{500, 10000} {
		if err := configSetInterval(path, ms); err != nil {
			t.Fatal(err)
		}
	}
	if got := readCfg(t, path); got != "mode=red\ninterval=10000\n" {
		t.Errorf("config = %q", got)
	}
}

func TestConfigTheme(t *testing.T) {
	sys := fakeSys(t, smt10())
	for _, tt := range []struct {
		file    string
		want    string
		wantLog bool
	}{
		{"", themeLight, false},
		{"theme=\n", themeLight, false},
		{"theme=light\n", themeLight, false},
		{"theme = dark\n", themeDark, false},
		{"theme=Dark\n", themeLight, true},
		{"theme=blue\n", themeLight, true},
	} {
		var buf bytes.Buffer
		log.SetOutput(&buf)
		path := cfgPath(t)
		writeCfg(t, path, tt.file)
		c, err := configLoad(path, sys)
		log.SetOutput(os.Stderr)
		if err != nil {
			t.Fatal(err)
		}
		if c.Theme != tt.want {
			t.Errorf("%q: Theme = %q, want %q", tt.file, c.Theme, tt.want)
		}
		if got := strings.Contains(buf.String(), "config: ignoring theme="); got != tt.wantLog {
			t.Errorf("%q: logged = %v (%q)", tt.file, got, buf.String())
		}
	}
	// Also set when the topology is unreadable.
	if c, err := configLoad(cfgPath(t), t.TempDir()); err == nil || c.Theme != themeLight {
		t.Errorf("no topology: %+v, %v", c, err)
	}
}

func TestConfigSetTheme(t *testing.T) {
	path := cfgPath(t)
	writeCfg(t, path, "mode=red\n")
	for _, th := range []string{"", "Dark", "blue"} {
		if err := configSetTheme(path, th); err == nil || !strings.Contains(err.Error(), "unknown theme") {
			t.Errorf("configSetTheme(%q) = %v, want error", th, err)
		}
	}
	if got := readCfg(t, path); got != "mode=red\n" {
		t.Errorf("file changed by rejected sets: %q", got)
	}
	for _, th := range []string{themeDark, themeLight, themeDark} {
		if err := configSetTheme(path, th); err != nil {
			t.Fatal(err)
		}
	}
	if got := readCfg(t, path); got != "mode=red\ntheme=dark\n" {
		t.Errorf("config = %q", got)
	}
}
