package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const wantSliceFor10Cores = installSliceMarker + `
[Unit]
Description=Claude Code and everything it spawns (managed by ccorral)

[Slice]
AllowedCPUs=3-9,13-19
`

// instFixture is a temp HOME + config dir + fake sysfs + fake systemctl.
type instFixture struct {
	env    installEnv
	calls  [][]string
	active bool   // what `is-active` reports
	failOn string // first arg of a systemctl call that should fail
}

func newInstFixture(t *testing.T) *instFixture {
	t.Helper()
	f := &instFixture{}
	home := t.TempDir()
	self := filepath.Join(t.TempDir(), "ccorral-build")
	if err := os.WriteFile(self, []byte("BINARY-V2"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.env = installEnv{
		home:      home,
		configDir: filepath.Join(home, ".config"),
		sysRoot:   fakeSys(t, smt10()),
		self:      self,
		run: func(args ...string) (string, error) {
			f.calls = append(f.calls, args)
			if args[0] == "is-active" {
				if f.active {
					return "active", nil
				}
				return "inactive", errors.New("exit status 3")
			}
			if args[0] == f.failOn {
				return "boom", errors.New("exit status 1")
			}
			return "", nil
		},
	}
	return f
}

func (f *instFixture) write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *instFixture) install(t *testing.T, stdin string) (int, string) {
	t.Helper()
	var out strings.Builder
	code := installRun(f.env, strings.NewReader(stdin), &out)
	return code, out.String()
}

func (f *instFixture) uninstall(t *testing.T, stdin string) (int, string) {
	t.Helper()
	var out strings.Builder
	code := uninstallRun(f.env, strings.NewReader(stdin), &out)
	return code, out.String()
}

func (f *instFixture) callStrings() []string {
	var s []string
	for _, c := range f.calls {
		s = append(s, strings.Join(c, " "))
	}
	return s
}

// snapshot maps every file and dir under root to its content ("" for dirs).
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			m[p] = "<dir>"
			return nil
		}
		b, err := os.ReadFile(p)
		m[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestInstallFresh(t *testing.T) {
	f := newInstFixture(t)
	code, out := f.install(t, "y\n")
	if code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	if got := readFile(t, f.env.slicePath()); got != wantSliceFor10Cores {
		t.Errorf("slice:\n%s\nwant:\n%s", got, wantSliceFor10Cores)
	}
	if got := readFile(t, f.env.unitPath()); got != installUnitTemplate {
		t.Errorf("unit:\n%s", got)
	}
	for _, line := range []string{
		"[Unit]\nDescription=ccorral — pin Claude Code to CPUs\n",
		"ExecStart=%h/.local/bin/ccorral daemon\n",
		"Restart=on-failure\nRestartSec=5\n",
		"[Install]\nWantedBy=default.target\n",
	} {
		if !strings.Contains(readFile(t, f.env.unitPath()), line) {
			t.Errorf("unit lacks %q", line)
		}
	}
	if got := readFile(t, f.env.binPath()); got != "BINARY-V2" {
		t.Errorf("binary content %q", got)
	}
	fi, err := os.Stat(f.env.binPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("binary mode %v, want 0755", fi.Mode().Perm())
	}
	// no leftover temp files
	entries, _ := os.ReadDir(f.env.binDir())
	if len(entries) != 1 {
		t.Errorf("bin dir has %d entries, want 1", len(entries))
	}
	want := []string{"is-active --quiet ccorral.service", "daemon-reload", "enable --now ccorral.service"}
	if got := f.callStrings(); !reflect.DeepEqual(got, want) {
		t.Errorf("systemctl calls %q, want %q", got, want)
	}
}

func TestInstallWhenAlreadyActiveRestarts(t *testing.T) {
	f := newInstFixture(t)
	f.active = true
	if code, out := f.install(t, "yes\n"); code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	want := []string{"is-active --quiet ccorral.service", "daemon-reload", "enable --now ccorral.service", "restart ccorral.service"}
	if got := f.callStrings(); !reflect.DeepEqual(got, want) {
		t.Errorf("systemctl calls %q, want %q", got, want)
	}
}

func TestInstallReplacesOldBinary(t *testing.T) {
	f := newInstFixture(t)
	f.write(t, f.env.binPath(), "BINARY-V1")
	if code, out := f.install(t, "y\n"); code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	if got := readFile(t, f.env.binPath()); got != "BINARY-V2" {
		t.Errorf("binary %q, want BINARY-V2", got)
	}
}

func TestInstallSystemctlFailure(t *testing.T) {
	f := newInstFixture(t)
	f.failOn = "enable"
	code, out := f.install(t, "y\n")
	if code != 1 {
		t.Errorf("code %d, want 1", code)
	}
	if !strings.Contains(out, "enable --now ccorral.service failed") || !strings.Contains(out, "boom") {
		t.Errorf("output lacks failure detail:\n%s", out)
	}
}

func TestInstallSliceMarkedDiffersIsRewritten(t *testing.T) {
	f := newInstFixture(t)
	old := installSliceMarker + "\n[Unit]\nDescription=old\n\n[Slice]\nAllowedCPUs=0-1\n"
	f.write(t, f.env.slicePath(), old)
	// only the final confirm is asked: no takeover question for a marked file
	if code, out := f.install(t, "y\n"); code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	if got := readFile(t, f.env.slicePath()); got != wantSliceFor10Cores {
		t.Errorf("slice not rewritten:\n%s", got)
	}
}

func TestInstallSliceMarkedEqualUntouched(t *testing.T) {
	f := newInstFixture(t)
	f.write(t, f.env.slicePath(), wantSliceFor10Cores)
	before, _ := os.Stat(f.env.slicePath())
	code, out := f.install(t, "y\n")
	if code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	after, _ := os.Stat(f.env.slicePath())
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Error("slice file was rewritten although equal")
	}
	if !strings.Contains(out, "unchanged") {
		t.Errorf("summary does not say slice is unchanged:\n%s", out)
	}
}

const handWrittenSlice = "[Unit]\nDescription=mine\n\n[Slice]\nAllowedCPUs=6-9,16-19\n"

func TestInstallSliceUnmarkedDeclined(t *testing.T) {
	f := newInstFixture(t)
	f.write(t, f.env.slicePath(), handWrittenSlice)
	code, out := f.install(t, "n\ny\n")
	if code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	if got := readFile(t, f.env.slicePath()); got != handWrittenSlice {
		t.Errorf("slice changed after 'no':\n%s", got)
	}
	// both files were shown
	if !strings.Contains(out, "AllowedCPUs=6-9,16-19") || !strings.Contains(out, "AllowedCPUs=3-9,13-19") {
		t.Errorf("current file and template not both shown:\n%s", out)
	}
	// install continued
	if !exists(f.env.unitPath()) || !exists(f.env.binPath()) {
		t.Error("install did not continue after declining the slice")
	}
	if got := f.callStrings(); len(got) != 3 || got[1] != "daemon-reload" {
		t.Errorf("calls %q", got)
	}
}

func TestInstallSliceUnmarkedAccepted(t *testing.T) {
	f := newInstFixture(t)
	f.write(t, f.env.slicePath(), handWrittenSlice)
	if code, out := f.install(t, "y\ny\n"); code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	if got := readFile(t, f.env.slicePath()); got != wantSliceFor10Cores {
		t.Errorf("slice not replaced:\n%s", got)
	}
}

func TestInstallSliceUnmarkedEmptyAnswerIsNo(t *testing.T) {
	f := newInstFixture(t)
	f.write(t, f.env.slicePath(), handWrittenSlice)
	if code, out := f.install(t, "\ny\n"); code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	if got := readFile(t, f.env.slicePath()); got != handWrittenSlice {
		t.Errorf("slice changed on default answer:\n%s", got)
	}
}

// Nothing may be written, and no systemctl run, before the final "yes".
func TestInstallAbortWritesNothing(t *testing.T) {
	for _, stdin := range []string{
		"",          // EOF at the confirm
		"\n",        // default answer
		"n\n",       // explicit no
		"maybe\n",   // anything but y/yes
		"yess\n",    // near miss
		"n\ny\nn\n", // unmarked: decline slice, remove script, then refuse confirm
	} {
		f := newInstFixture(t)
		f.write(t, f.env.slicePath(), handWrittenSlice)
		f.write(t, f.env.oldScript(), "#!/bin/sh\n")
		before := snapshot(t, f.env.home)
		// stdin "n\ny\nn\n" answers: takeover=n, remove claude-cpus=y, confirm=n
		code, out := f.install(t, stdin)
		if code != 1 {
			t.Errorf("stdin %q: code %d, want 1\n%s", stdin, code, out)
		}
		if after := snapshot(t, f.env.home); !reflect.DeepEqual(before, after) {
			t.Errorf("stdin %q: filesystem changed", stdin)
		}
		if len(f.calls) != 0 {
			t.Errorf("stdin %q: systemctl ran: %q", stdin, f.callStrings())
		}
		if !strings.Contains(out, "nothing was written") {
			t.Errorf("stdin %q: no abort message:\n%s", stdin, out)
		}
	}
}

// Abort on a fresh machine (no existing files at all) leaves HOME empty.
func TestInstallAbortFreshWritesNothing(t *testing.T) {
	f := newInstFixture(t)
	before := snapshot(t, f.env.home)
	if code, _ := f.install(t, "n\n"); code != 1 {
		t.Errorf("code %d, want 1", code)
	}
	if after := snapshot(t, f.env.home); !reflect.DeepEqual(before, after) {
		t.Errorf("filesystem changed: %v", after)
	}
	if len(f.calls) != 0 {
		t.Errorf("systemctl ran: %q", f.callStrings())
	}
}

func TestInstallOldScript(t *testing.T) {
	// asked, answered no -> kept
	f := newInstFixture(t)
	f.write(t, f.env.oldScript(), "#!/bin/sh\n")
	code, out := f.install(t, "n\ny\n")
	if code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	if !strings.Contains(out, "claude-cpus") {
		t.Errorf("claude-cpus never mentioned:\n%s", out)
	}
	if !exists(f.env.oldScript()) {
		t.Error("claude-cpus removed after 'no'")
	}

	// answered yes -> removed
	f = newInstFixture(t)
	f.write(t, f.env.oldScript(), "#!/bin/sh\n")
	if code, out := f.install(t, "y\ny\n"); code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	if exists(f.env.oldScript()) {
		t.Error("claude-cpus still present after 'yes'")
	}

	// not present -> not asked: one answer is enough
	f = newInstFixture(t)
	if code, out := f.install(t, "y\n"); code != 0 || strings.Contains(out, "old script") {
		t.Errorf("code %d, asked about absent script:\n%s", code, out)
	}
}

func TestInstallSummaryListsActions(t *testing.T) {
	f := newInstFixture(t)
	f.write(t, f.env.oldScript(), "x")
	_, out := f.install(t, "y\nn\n") // remove script, then refuse
	for _, want := range []string{
		"copy " + f.env.self + " to " + f.env.binPath(),
		"write " + f.env.unitPath(),
		"write " + f.env.slicePath() + " (AllowedCPUs=3-9,13-19)",
		"remove " + f.env.oldScript(),
		"daemon-reload",
		"enable --now ccorral.service",
		"Proceed? [y/N]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
}

func TestUninstall(t *testing.T) {
	f := newInstFixture(t)
	if code, out := f.install(t, "y\n"); code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	cfg := filepath.Join(f.env.configDir, "ccorral", "config")
	f.write(t, cfg, "mode=yellow\n")
	f.write(t, f.env.oldScript(), "keep me")
	sliceBefore := readFile(t, f.env.slicePath())
	f.calls = nil

	code, out := f.uninstall(t, "y\n")
	if code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	want := []string{"disable --now ccorral.service", "daemon-reload"}
	if got := f.callStrings(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls %q, want %q", got, want)
	}
	if exists(f.env.unitPath()) {
		t.Error("unit file not removed")
	}
	if exists(f.env.binPath()) {
		t.Error("binary not removed")
	}
	if got := readFile(t, f.env.slicePath()); got != sliceBefore {
		t.Error("slice changed")
	}
	if got := readFile(t, cfg); got != "mode=yellow\n" {
		t.Error("config changed")
	}
	if !exists(f.env.oldScript()) {
		t.Error("claude-cpus touched by uninstall")
	}
	if !strings.Contains(out, "Left in place: "+f.env.slicePath()) {
		t.Errorf("output does not say the slice is left:\n%s", out)
	}
	if !strings.Contains(out, "systemctl --user revert claude.slice") {
		t.Errorf("output does not mention revert:\n%s", out)
	}
}

func TestUninstallAbort(t *testing.T) {
	for _, stdin := range []string{"", "\n", "n\n"} {
		f := newInstFixture(t)
		f.write(t, f.env.binPath(), "BIN")
		f.write(t, f.env.unitPath(), "unit")
		before := snapshot(t, f.env.home)
		if code, _ := f.uninstall(t, stdin); code != 1 {
			t.Errorf("stdin %q: code %d, want 1", stdin, code)
		}
		if after := snapshot(t, f.env.home); !reflect.DeepEqual(before, after) {
			t.Errorf("stdin %q: filesystem changed", stdin)
		}
		if len(f.calls) != 0 {
			t.Errorf("stdin %q: systemctl ran: %q", stdin, f.callStrings())
		}
	}
}

// Uninstalling when nothing is installed must not blow up on missing files.
func TestUninstallNothingInstalled(t *testing.T) {
	f := newInstFixture(t)
	if code, out := f.uninstall(t, "y\n"); code != 0 {
		t.Errorf("code %d\n%s", code, out)
	}
}
