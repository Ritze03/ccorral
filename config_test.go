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
		{"", themeDark, false},
		{"theme=\n", themeDark, false},
		{"theme=light\n", themeLight, false},
		{"theme = dark\n", themeDark, false},
		{"theme=Dark\n", themeDark, true},
		{"theme=blue\n", themeDark, true},
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
	if c, err := configLoad(cfgPath(t), t.TempDir()); err == nil || c.Theme != themeDark {
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

func TestConfigCPUWeight(t *testing.T) {
	sys := fakeSys(t, smt10())
	for _, tt := range []struct {
		file    string
		want    string
		wantLog bool
	}{
		{"", "idle", false},
		{"cpuweight=\n", "idle", false},
		{"cpuweight=idle\n", "idle", false},
		{"cpuweight = 100\n", "100", false},
		{"cpuweight=1\n", "1", false},
		{"cpuweight=10000\n", "10000", false},
		{"cpuweight=0\n", "idle", true},
		{"cpuweight=10001\n", "idle", true},
		{"cpuweight=-5\n", "idle", true},
		{"cpuweight=1.5\n", "idle", true},
		{"cpuweight=Idle\n", "idle", true},
		{"cpuweight=garbage\n", "idle", true},
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
		if c.CPUWeight != tt.want {
			t.Errorf("%q: CPUWeight = %q, want %q", tt.file, c.CPUWeight, tt.want)
		}
		if got := strings.Contains(buf.String(), "config: ignoring cpuweight="); got != tt.wantLog {
			t.Errorf("%q: logged = %v (%q)", tt.file, got, buf.String())
		}
	}
	// Also set when the topology is unreadable.
	if c, err := configLoad(cfgPath(t), t.TempDir()); err == nil || c.CPUWeight != "idle" {
		t.Errorf("no topology: %+v, %v", c, err)
	}
}

func TestConfigSetCPUWeight(t *testing.T) {
	path := cfgPath(t)
	writeCfg(t, path, "mode=red\n")
	for _, w := range []string{"", "0", "10001", "-1", "Idle", "abc", "1.5", "010", " 5"} {
		if err := configSetCPUWeight(path, w); err == nil || !strings.Contains(err.Error(), "want idle or an integer from 1 to 10000") {
			t.Errorf("configSetCPUWeight(%q) = %v, want error", w, err)
		}
	}
	if got := readCfg(t, path); got != "mode=red\n" {
		t.Errorf("file changed by rejected sets: %q", got)
	}
	for _, w := range []string{"1", "10000", "idle", "100"} {
		if err := configSetCPUWeight(path, w); err != nil {
			t.Fatal(err)
		}
	}
	if got := readCfg(t, path); got != "mode=red\ncpuweight=100\n" {
		t.Errorf("config = %q", got)
	}
}

// parseTable runs configLoad over table rows, each a config file and the value
// of one field wanted, and checks the "ignoring <key>=" log line.
func parseTable(t *testing.T, key string, get func(Config) string, rows [][3]string) {
	t.Helper()
	sys := fakeSys(t, smt10())
	for _, r := range rows {
		file, want, wantLog := r[0], r[1], r[2] == "log"
		var buf bytes.Buffer
		log.SetOutput(&buf)
		path := cfgPath(t)
		writeCfg(t, path, file)
		c, err := configLoad(path, sys)
		log.SetOutput(os.Stderr)
		if err != nil {
			t.Fatal(err)
		}
		if got := get(c); got != want {
			t.Errorf("%q: %s = %q, want %q", file, key, got, want)
		}
		if got := strings.Contains(buf.String(), "config: ignoring "+key+"="); got != wantLog {
			t.Errorf("%q: logged = %v (%q)", file, got, buf.String())
		}
	}
}

func TestConfigIOWeight(t *testing.T) {
	parseTable(t, "ioweight", func(c Config) string { return c.IOWeight }, [][3]string{
		{"", "10", ""},
		{"ioweight=\n", "10", ""},
		{"ioweight=1\n", "1", ""},
		{"ioweight = 500\n", "500", ""},
		{"ioweight=10000\n", "10000", ""},
		{"ioweight=0\n", "10", "log"},
		{"ioweight=10001\n", "10", "log"},
		{"ioweight=-1\n", "10", "log"},
		{"ioweight=idle\n", "10", "log"},
		{"ioweight=1.5\n", "10", "log"},
		{"ioweight=garbage\n", "10", "log"},
	})
	if c, err := configLoad(cfgPath(t), t.TempDir()); err == nil || c.IOWeight != "10" {
		t.Errorf("no topology: %+v, %v", c, err)
	}
}

func TestConfigSetIOWeight(t *testing.T) {
	path := cfgPath(t)
	writeCfg(t, path, "mode=red\n")
	for _, w := range []string{"", "0", "10001", "-1", "idle", "1.5", "010", " 5"} {
		if err := configSetIOWeight(path, w); err == nil || !strings.Contains(err.Error(), "want an integer from 1 to 10000") {
			t.Errorf("configSetIOWeight(%q) = %v, want error", w, err)
		}
	}
	if got := readCfg(t, path); got != "mode=red\n" {
		t.Errorf("file changed by rejected sets: %q", got)
	}
	for _, w := range []string{"1", "10000", "10"} {
		if err := configSetIOWeight(path, w); err != nil {
			t.Fatal(err)
		}
	}
	if got := readCfg(t, path); got != "mode=red\nioweight=10\n" {
		t.Errorf("config = %q", got)
	}
}

func TestConfigMemoryHigh(t *testing.T) {
	parseTable(t, "memoryhigh", func(c Config) string { return c.MemoryHigh }, [][3]string{
		{"", "75%", ""},
		{"memoryhigh=\n", "75%", ""},
		{"memoryhigh=infinity\n", "infinity", ""},
		{"memoryhigh = 50%\n", "50%", ""},
		{"memoryhigh=1%\n", "1%", ""},
		{"memoryhigh=100%\n", "100%", ""},
		{"memoryhigh=16G\n", "16G", ""},
		{"memoryhigh=512M\n", "512M", ""},
		{"memoryhigh=100K\n", "100K", ""},
		{"memoryhigh=2T\n", "2T", ""},
		{"memoryhigh=1073741824\n", "1073741824", ""},
		{"memoryhigh=0\n", "75%", "log"},
		{"memoryhigh=0%\n", "75%", "log"},
		{"memoryhigh=101%\n", "75%", "log"},
		{"memoryhigh=%\n", "75%", "log"},
		{"memoryhigh=-1\n", "75%", "log"},
		{"memoryhigh=-5G\n", "75%", "log"},
		{"memoryhigh=1.5G\n", "75%", "log"},
		{"memoryhigh=12.5%\n", "75%", "log"},
		{"memoryhigh=16GB\n", "75%", "log"},
		{"memoryhigh=16g\n", "75%", "log"},
		{"memoryhigh=16P\n", "75%", "log"},
		{"memoryhigh=G\n", "75%", "log"},
		{"memoryhigh=010\n", "75%", "log"},
		{"memoryhigh=Infinity\n", "75%", "log"},
		{"memoryhigh=max\n", "75%", "log"},
	})
	if c, err := configLoad(cfgPath(t), t.TempDir()); err == nil || c.MemoryHigh != "75%" {
		t.Errorf("no topology: %+v, %v", c, err)
	}
}

func TestConfigSetMemoryHigh(t *testing.T) {
	path := cfgPath(t)
	writeCfg(t, path, "mode=red\n")
	for _, v := range []string{"", " ", "0", "0%", "101%", "-1", "1.5G", "16GB", "16P", "G", "max", "Infinity"} {
		if err := configSetMemoryHigh(path, v); err == nil || !strings.Contains(err.Error(), "want infinity") {
			t.Errorf("configSetMemoryHigh(%q) = %v, want error", v, err)
		}
	}
	if got := readCfg(t, path); got != "mode=red\n" {
		t.Errorf("file changed by rejected sets: %q", got)
	}
	for _, v := range []string{"infinity", "90%", "16G", "75%"} {
		if err := configSetMemoryHigh(path, v); err != nil {
			t.Fatal(err)
		}
	}
	if got := readCfg(t, path); got != "mode=red\nmemoryhigh=75%\n" {
		t.Errorf("config = %q", got)
	}
}

func TestConfigSplit(t *testing.T) {
	sys := fakeSys(t, smt10())
	for _, tt := range []struct {
		file    string
		want    bool
		wantLog bool
	}{
		{"", true, false},
		{"split=\n", true, false},
		{"split=on\n", true, false},
		{"split=off\n", false, false},
		{"split = off\n", false, false},
		{"split=on\nsplit=off\n", false, false}, // later lines win
		{"split=Off\n", true, true},
		{"split=maybe\n", true, true},
		{"split=false\n", true, true},
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
		if c.Split != tt.want {
			t.Errorf("%q: Split = %v, want %v", tt.file, c.Split, tt.want)
		}
		if got := strings.Contains(buf.String(), "config: ignoring split="); got != tt.wantLog {
			t.Errorf("%q: logged = %v (%q)", tt.file, got, buf.String())
		}
	}
	// Also set when the topology is unreadable.
	if c, err := configLoad(cfgPath(t), t.TempDir()); err == nil || !c.Split {
		t.Errorf("no topology: %+v, %v", c, err)
	}
}

func TestConfigSetSplit(t *testing.T) {
	path := cfgPath(t)
	writeCfg(t, path, "mode=red\n")
	for _, on := range []bool{false, true, false} {
		if err := configSetSplit(path, on); err != nil {
			t.Fatal(err)
		}
	}
	if got := readCfg(t, path); got != "mode=red\nsplit=off\n" {
		t.Errorf("config = %q", got)
	}
	if err := configSetSplit(path, true); err != nil {
		t.Fatal(err)
	}
	if got := readCfg(t, path); got != "mode=red\nsplit=on\n" {
		t.Errorf("config = %q", got)
	}
}
