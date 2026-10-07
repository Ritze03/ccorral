package main

// settings_test.go never opens a terminal: the whole TUI is a pure render and a
// pure key handler over settingsState, plus one write-and-reload step that
// takes its reload func as a parameter. Everything runs on a fake 10-core SMT
// topology and a temp config dir.

import (
	"bufio"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// settingsFixture is a fresh state on the smt10 topology with default groups
// (yellow 3-9,13-19, red 7-9,17-19), plus the config path and sysfs root.
func settingsFixture(t *testing.T) (s settingsState, path, sys string) {
	t.Helper()
	path, sys = cfgPath(t), fakeSys(t, smt10())
	s, err := settingsLoad(path, sys)
	if err != nil {
		t.Fatal(err)
	}
	return s, path, sys
}

// press runs keys through settingsPress and returns the final state and the
// last act.
func press(s settingsState, keys ...string) (settingsState, settingsAct) {
	var act settingsAct
	for _, k := range keys {
		s, act = settingsPress(s, k)
	}
	return s, act
}

// plain is what the screen looks like with the escape sequences taken out.
func plain(s string) string {
	s = strings.ReplaceAll(s, settingsReverse, "")
	return strings.ReplaceAll(s, settingsReset, "")
}

func TestSettingsLoadAndRender(t *testing.T) {
	s, _, _ := settingsFixture(t)
	if len(s.cores) != 10 || s.interval != intervalDefault {
		t.Fatalf("cores %d, interval %d", len(s.cores), s.interval)
	}
	out := plain(settingsRender(s))
	for _, want := range []string{
		"ccorral settings",
		"0  1  2  3  4  5  6  7  8  9",
		"Yellow   [ ][ ][ ][x][x][x][x][x][x][x]   3-9,13-19",
		"Red      [ ][ ][ ][ ][ ][ ][ ][x][x][x]   7-9,17-19",
		"Interval 5000 ms   (500–10000)",
		"Theme    light (black outline)",
		settingsHint,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render is missing %q:\n%s", want, out)
		}
	}
}

func TestSettingsRenderHighlight(t *testing.T) {
	s, _, _ := settingsFixture(t)
	s.row, s.col = settingsRowRed, 2
	out := settingsRender(s)
	if want := "[ ][ ]" + settingsReverse + "[ ]" + settingsReset + "[ ]"; strings.Count(out, settingsReverse) != 1 ||
		!strings.Contains(out, "Red      "+want) {
		t.Errorf("cursor cell is not the only reversed one:\n%q", out)
	}
	s.row = settingsRowInterval
	if out := settingsRender(s); !strings.Contains(out, settingsReverse+"5000 ms"+settingsReset) {
		t.Errorf("interval not highlighted:\n%q", out)
	}
}

func TestSettingsMove(t *testing.T) {
	s, _, _ := settingsFixture(t)
	s, _ = press(s, "left", "up")
	if s.row != 0 || s.col != 0 {
		t.Errorf("did not stop at the top-left: row %d col %d", s.row, s.col)
	}
	s, _ = press(s, "right", "right", "down")
	if s.row != settingsRowRed || s.col != 2 {
		t.Errorf("row %d col %d", s.row, s.col)
	}
	for i := 0; i < 20; i++ {
		s, _ = press(s, "right")
	}
	if s.col != 9 {
		t.Errorf("col %d, want it stopped at the last core (9)", s.col)
	}
	s, act := press(s, "down", "down", "down", "down")
	if s.row != settingsRowTheme || act != settingsActNone || s.col != 9 {
		t.Errorf("row %d col %d act %d", s.row, s.col, act)
	}
}

// TestSettingsToggleSiblings: toggling core 3 in Yellow clears CPU 3 and 13
// together, and toggling it again brings both back in order.
func TestSettingsToggleSiblings(t *testing.T) {
	s, _, _ := settingsFixture(t)
	s.col = 3
	s, act := press(s, "toggle")
	if act != settingsActWrite || !reflect.DeepEqual(s.yellow, []int{4, 5, 6, 7, 8, 9, 14, 15, 16, 17, 18, 19}) {
		t.Fatalf("act %d yellow %v", act, s.yellow)
	}
	if !reflect.DeepEqual(s.red, []int{7, 8, 9, 17, 18, 19}) {
		t.Errorf("red changed: %v", s.red)
	}
	out := plain(settingsRender(s))
	if !strings.Contains(out, "Yellow   [ ][ ][ ][ ][x]") || !strings.Contains(out, "   4-9,14-19") {
		t.Errorf("render:\n%s", out)
	}
	s, act = press(s, "toggle")
	if act != settingsActWrite || !reflect.DeepEqual(s.yellow, []int{3, 4, 5, 6, 7, 8, 9, 13, 14, 15, 16, 17, 18, 19}) {
		t.Errorf("act %d yellow %v", act, s.yellow)
	}
	// A core on the red row pulls both threads in, sorted.
	s.row, s.col = settingsRowRed, 0
	s, _ = press(s, "toggle")
	if !reflect.DeepEqual(s.red, []int{0, 7, 8, 9, 10, 17, 18, 19}) {
		t.Errorf("red %v", s.red)
	}
}

func TestSettingsLastCoreRefused(t *testing.T) {
	s, _, _ := settingsFixture(t)
	s.row = settingsRowRed
	for _, c := range []int{7, 8} {
		s.col = c
		s, _ = press(s, "toggle")
	}
	if !reflect.DeepEqual(s.red, []int{9, 19}) {
		t.Fatalf("red %v", s.red)
	}
	s.col = 9
	s, act := press(s, "toggle")
	if act != settingsActNone || !reflect.DeepEqual(s.red, []int{9, 19}) {
		t.Errorf("act %d red %v: last core was unchecked", act, s.red)
	}
	if !strings.Contains(s.status, "Red needs at least one core") {
		t.Errorf("status %q does not say why", s.status)
	}
	if out := plain(settingsRender(s)); !strings.Contains(out, "Red needs at least one core") {
		t.Errorf("status not on screen:\n%s", out)
	}
	// Any other key clears the message.
	if s, _ = press(s, "up"); s.status != "" {
		t.Errorf("status %q survived a keypress", s.status)
	}
}

func TestSettingsInterval(t *testing.T) {
	s, _, _ := settingsFixture(t)
	s.row = settingsRowInterval

	s, act := press(s, "right")
	if s.interval != 5500 || act != settingsActWrite {
		t.Errorf("right: %d act %d", s.interval, act)
	}
	s, _ = press(s, "left", "left")
	if s.interval != 4500 {
		t.Errorf("left left: %d", s.interval)
	}
	s, _ = press(s, "inc")
	s, _ = press(s, "dec", "dec")
	if s.interval != 4000 {
		t.Errorf("inc dec dec: %d", s.interval)
	}

	// Clamp at both ends; no write when nothing moves.
	for i := 0; i < 30; i++ {
		s, _ = press(s, "right")
	}
	if s.interval != intervalMax {
		t.Errorf("max: %d", s.interval)
	}
	s, act = press(s, "right")
	if act != settingsActNone || s.interval != intervalMax || s.status == "" {
		t.Errorf("past max: %d act %d status %q", s.interval, act, s.status)
	}
	for i := 0; i < 30; i++ {
		s, _ = press(s, "left")
	}
	if s.interval != intervalMin {
		t.Errorf("min: %d", s.interval)
	}
	if _, act = press(s, "dec"); act != settingsActNone {
		t.Errorf("past min: act %d", act)
	}

	// An off-grid value from a hand-edited file still clamps.
	s.interval = 9800
	if s, _ = press(s, "right"); s.interval != intervalMax {
		t.Errorf("9800 right: %d", s.interval)
	}

	// +/- do nothing on a group row.
	s.row, s.interval = settingsRowYellow, 5000
	if s, act = press(s, "inc"); s.interval != 5000 || act != settingsActNone {
		t.Errorf("inc on a group row: %d act %d", s.interval, act)
	}
}

func TestSettingsDefault(t *testing.T) {
	s, _, _ := settingsFixture(t)
	s.yellow, s.red, s.interval = []int{0, 10}, []int{1, 11}, 1500

	for _, tc := range []struct {
		row  int
		want func(settingsState) bool
	}{
		{settingsRowYellow, func(s settingsState) bool {
			return reflect.DeepEqual(s.yellow, []int{3, 4, 5, 6, 7, 8, 9, 13, 14, 15, 16, 17, 18, 19})
		}},
		{settingsRowRed, func(s settingsState) bool { return reflect.DeepEqual(s.red, []int{7, 8, 9, 17, 18, 19}) }},
		{settingsRowInterval, func(s settingsState) bool { return s.interval == intervalDefault }},
	} {
		s.row = tc.row
		var act settingsAct
		s, act = press(s, "default")
		if act != settingsActReset || !tc.want(s) {
			t.Errorf("row %d: act %d state %+v", tc.row, act, s)
		}
	}
}

func TestSettingsQuit(t *testing.T) {
	s, _, _ := settingsFixture(t)
	if _, act := press(s, "quit"); act != settingsActQuit {
		t.Errorf("act %d", act)
	}
}

// TestSettingsSave: each act writes the right config key and calls reload.
func TestSettingsSave(t *testing.T) {
	s, path, sys := settingsFixture(t)
	calls := 0
	reload := func() error { calls++; return nil }
	save := func(keys ...string) {
		t.Helper()
		var act settingsAct
		s, act = press(s, keys...)
		s = settingsSave(path, sys, s, act, reload)
	}

	s.col = 3
	save("toggle")
	if got := readCfg(t, path); !strings.Contains(got, "yellow=4-9,14-19") {
		t.Errorf("config after toggle:\n%s", got)
	}
	if calls != 1 || s.status != settingsReloadedOK {
		t.Errorf("calls %d status %q", calls, s.status)
	}

	save("down", "toggle") // red, core 3 on: 3,13 join
	if got := readCfg(t, path); !strings.Contains(got, "red=3,7-9,13,17-19") {
		t.Errorf("config after red toggle:\n%s", got)
	}

	save("down", "right") // interval 5500
	if got := readCfg(t, path); !strings.Contains(got, "interval=5500") {
		t.Errorf("config after interval:\n%s", got)
	}

	save("default") // interval back to 5000
	if got := readCfg(t, path); !strings.Contains(got, "interval=5000") {
		t.Errorf("config after interval reset:\n%s", got)
	}

	save("up", "default") // red row reset: key dropped
	got := readCfg(t, path)
	if strings.Contains(got, "red=") || !strings.Contains(got, "yellow=4-9,14-19") {
		t.Errorf("config after red reset:\n%s", got)
	}
	if calls != 5 {
		t.Errorf("reload calls %d, want 5", calls)
	}

	// What a fresh process sees is what the screen showed.
	n, err := settingsLoad(path, sys)
	if err != nil || !reflect.DeepEqual(n.yellow, s.yellow) || !reflect.DeepEqual(n.red, s.red) || n.interval != s.interval {
		t.Errorf("reload from disk: %+v err %v, screen %+v", n, err, s)
	}
}

func TestSettingsSaveReloadOutcomes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{errors.New(ipcNotRunning), settingsNoDaemon},
		{errors.New(ipcNotInstalled), settingsNoDaemon},
		{errors.New("boom"), "saved, but daemon: boom"},
	} {
		s, path, sys := settingsFixture(t)
		s.row = settingsRowInterval
		s, act := press(s, "right")
		s = settingsSave(path, sys, s, act, func() error { return tc.err })
		if s.status != tc.want {
			t.Errorf("reload err %q: status %q, want %q", tc.err, s.status, tc.want)
		}
		if got := readCfg(t, path); !strings.Contains(got, "interval=5500") {
			t.Errorf("not saved when the daemon is unreachable:\n%s", got)
		}
	}
}

// TestSettingsSaveFailure: when the write fails, the screen goes back to what
// is on disk and the daemon is left alone.
func TestSettingsSaveFailure(t *testing.T) {
	s, path, sys := settingsFixture(t)
	// A config path whose parent is a regular file can't be created.
	bad := path + "/sub/config"
	writeCfg(t, path, "interval=2000\n") // a regular file where a directory is needed
	s.row = settingsRowInterval
	s, act := press(s, "right")
	called := false
	s = settingsSave(bad, sys, s, act, func() error { called = true; return nil })
	if !strings.HasPrefix(s.status, "could not save: ") {
		t.Errorf("status %q", s.status)
	}
	if called {
		t.Error("reload was sent after a failed save")
	}
	if s.interval != intervalDefault || s.row != settingsRowInterval {
		t.Errorf("interval %d row %d: unsaved value still on screen", s.interval, s.row)
	}
}

func TestSettingsReadKey(t *testing.T) {
	for in, want := range map[string]string{
		"q": "quit", "\x03": "quit", "\x04": "quit", "\x1b": "quit",
		"j": "down", "k": "up", "h": "left", "l": "right",
		"\x1b[A": "up", "\x1b[B": "down", "\x1b[C": "right", "\x1b[D": "left",
		"\x1bOA": "up",
		" ":      "toggle", "\r": "toggle",
		"+": "inc", "=": "inc", "-": "dec",
		"d": "default", "x": "none", "\x1b[Z": "none",
	} {
		got, err := settingsReadKey(bufio.NewReader(strings.NewReader(in)))
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := settingsReadKey(bufio.NewReader(strings.NewReader(""))); err == nil {
		t.Error("EOF should be an error")
	}
}

func TestRunSettingsNotATerminal(t *testing.T) {
	var out strings.Builder
	if code := RunSettings(strings.NewReader(""), &out); code != 1 || !strings.Contains(out.String(), "needs a terminal") {
		t.Errorf("code %d out %q", code, out.String())
	}
}

func TestSettingsTheme(t *testing.T) {
	s, path, sys := settingsFixture(t)
	s.row = settingsRowTheme
	if s.theme != themeLight {
		t.Fatalf("default theme %q", s.theme)
	}
	for _, k := range []string{"right", "toggle", "left"} { // dark, light, dark
		var act settingsAct
		s, act = press(s, k)
		if act != settingsActWrite {
			t.Errorf("%s: act %d", k, act)
		}
	}
	if s.theme != themeDark || !strings.Contains(plain(settingsRender(s)), "Theme    dark (white outline)") {
		t.Errorf("theme %q:\n%s", s.theme, plain(settingsRender(s)))
	}
	// inc/dec are for the interval only.
	if n, act := press(s, "inc"); n.theme != themeDark || act != settingsActNone {
		t.Errorf("inc: theme %q act %d", n.theme, act)
	}
	// Saved immediately, reload sent, and a fresh process sees it.
	calls := 0
	reload := func() error { calls++; return nil }
	s, act := press(s, "toggle", "toggle") // light, dark
	s = settingsSave(path, sys, s, act, reload)
	if got := readCfg(t, path); got != "theme=dark\n" || calls != 1 || s.status != settingsReloadedOK {
		t.Errorf("config %q calls %d status %q", got, calls, s.status)
	}
	if n, err := settingsLoad(path, sys); err != nil || n.theme != themeDark {
		t.Errorf("reload from disk: %+v err %v", n, err)
	}
	// d resets to light and saves it.
	s, act = press(s, "default")
	if s.theme != themeLight || act != settingsActReset {
		t.Fatalf("default: theme %q act %d", s.theme, act)
	}
	settingsSave(path, sys, s, act, reload)
	if got := readCfg(t, path); got != "theme=light\n" || calls != 2 {
		t.Errorf("config %q calls %d", got, calls)
	}
	// The cursor can't go past the theme row.
	if s, _ = press(s, "down", "down"); s.row != settingsRowTheme {
		t.Errorf("row %d", s.row)
	}
}
