package main

import (
	"os"
	"path/filepath"
	"reflect"
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
