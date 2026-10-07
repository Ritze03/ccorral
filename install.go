package main

// install.go implements `ccorral install` and `ccorral uninstall`. Install asks
// every question first and writes nothing until the user has confirmed a
// summary of exactly what will change; only then does it copy the binary, write
// the systemd user units and talk to systemctl.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	installService = "ccorral.service"

	// installSliceMarker is the first line of a claude.slice that ccorral
	// owns: install rewrites such a file freely, and never touches one
	// without it unless the user agrees.
	installSliceMarker = "# Managed by ccorral — rewritten by 'ccorral install'"
)

const installUnitTemplate = `[Unit]
Description=ccorral — pin Claude Code to CPUs

[Service]
ExecStart=%h/.local/bin/ccorral daemon
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`

// installSliceTemplate is the managed claude.slice; %s is the cpulist.
const installSliceTemplate = installSliceMarker + `
[Unit]
Description=Claude Code and everything it spawns (managed by ccorral)

[Slice]
AllowedCPUs=%s
`

// installRunner runs `systemctl --user <args...>` and returns its combined
// output. Tests substitute a fake so systemctl is never invoked.
type installRunner func(args ...string) (string, error)

func installSystemctl(args ...string) (string, error) {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// installEnv is everything the install/uninstall core touches, so tests can
// point all of it into temp dirs.
type installEnv struct {
	home      string // user home; ~/.local/bin lives under it
	configDir string // user config dir; systemd/user lives under it
	sysRoot   string // "/sys" in production
	self      string // path of the binary to copy (install only)
	run       installRunner
}

func (e installEnv) binDir() string    { return filepath.Join(e.home, ".local", "bin") }
func (e installEnv) binPath() string   { return filepath.Join(e.binDir(), "ccorral") }
func (e installEnv) oldScript() string { return filepath.Join(e.binDir(), "claude-cpus") }
func (e installEnv) unitDir() string   { return filepath.Join(e.configDir, "systemd", "user") }
func (e installEnv) unitPath() string  { return filepath.Join(e.unitDir(), installService) }
func (e installEnv) slicePath() string { return filepath.Join(e.unitDir(), "claude.slice") }

// installDefaultEnv builds the production environment.
func installDefaultEnv(needSelf bool, out io.Writer) (installEnv, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(out, "cannot determine home directory: %v\n", err)
		return installEnv{}, false
	}
	env := installEnv{home: home, configDir: os.Getenv("XDG_CONFIG_HOME"), sysRoot: "/sys", run: installSystemctl}
	if env.configDir == "" {
		env.configDir = filepath.Join(home, ".config")
	}
	if needSelf {
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintf(out, "cannot determine own executable: %v\n", err)
			return installEnv{}, false
		}
		if resolved, rerr := filepath.EvalSymlinks(self); rerr == nil {
			self = resolved
		}
		env.self = self
	}
	return env, true
}

// installMain is `ccorral install`. It returns the process exit code.
func installMain() int {
	env, ok := installDefaultEnv(true, os.Stdout)
	if !ok {
		return 1
	}
	return installRun(env, os.Stdin, os.Stdout)
}

// uninstallMain is `ccorral uninstall`. It returns the process exit code.
func uninstallMain() int {
	env, ok := installDefaultEnv(false, os.Stdout)
	if !ok {
		return 1
	}
	return uninstallRun(env, os.Stdin, os.Stdout)
}

// installSliceAction is what install decided to do with claude.slice.
type installSliceAction int

const (
	sliceWrite   installSliceAction = iota // missing: write the template
	sliceRewrite                           // ours and outdated: rewrite
	sliceKeep                              // ours and current: leave
	sliceTake                              // not ours, user agreed: replace
	sliceSkip                              // not ours, user declined: leave
)

func installRun(env installEnv, in io.Reader, out io.Writer) int {
	r := bufio.NewReader(in)
	abort := func() int {
		fmt.Fprintln(out, "Aborted; nothing was written.")
		return 1
	}

	cores, err := cpuCores(env.sysRoot)
	if err != nil {
		fmt.Fprintf(out, "cannot read CPU topology: %v\n", err)
		return 1
	}
	yellow, _ := cpuDefaults(cores)
	cpulist := cpuFormat(yellow)
	slice := fmt.Sprintf(installSliceTemplate, cpulist)

	fmt.Fprintf(out, "Install ccorral to %s.\n\n", env.binPath())

	// --- questions (nothing is written until all are answered) ---
	action := sliceWrite
	cur, rerr := os.ReadFile(env.slicePath())
	switch {
	case rerr != nil && !os.IsNotExist(rerr):
		fmt.Fprintf(out, "cannot read %s: %v\n", env.slicePath(), rerr)
		return 1
	case rerr != nil:
		action = sliceWrite
	case installFirstLine(string(cur)) != installSliceMarker:
		fmt.Fprintf(out, "%s exists and is not managed by ccorral.\n\nCurrent file:\n%s\nccorral would replace it with:\n%s\n",
			env.slicePath(), installIndent(string(cur)), installIndent(slice))
		yes, ok := installAsk(r, out, "Let ccorral take over claude.slice? [y/N] ")
		if !ok {
			return abort()
		}
		action = sliceSkip
		if yes {
			action = sliceTake
		}
		fmt.Fprintln(out)
	case string(cur) == slice:
		action = sliceKeep
	default:
		action = sliceRewrite
	}

	removeOld := false
	if _, err := os.Lstat(env.oldScript()); err == nil {
		yes, ok := installAsk(r, out, fmt.Sprintf("Remove the old script %s (replaced by ccorral)? [y/N] ", env.oldScript()))
		if !ok {
			return abort()
		}
		removeOld = yes
		fmt.Fprintln(out)
	}

	// --- summary + confirm ---
	sameBin := false
	if ti, terr := os.Stat(env.binPath()); terr == nil {
		if si, serr := os.Stat(env.self); serr == nil && os.SameFile(ti, si) {
			sameBin = true
		}
	}
	if sameBin {
		fmt.Fprintf(out, "Note: you are running the installed copy (%s), so there is no new binary to install —\n"+
			"this only rewrites the unit and restarts the service. To install a new build, run it from the build:\n"+
			"  ./ccorral install\n\n", env.binPath())
	}
	fmt.Fprintln(out, "This will:")
	fmt.Fprintf(out, "  - stop %s if it is running\n", installService)
	if sameBin {
		fmt.Fprintf(out, "  - reinstall from the installed copy (same version): %s is left as is\n", env.binPath())
	} else {
		fmt.Fprintf(out, "  - copy %s to %s\n", env.self, env.binPath())
	}
	fmt.Fprintf(out, "  - write %s\n", env.unitPath())
	switch action {
	case sliceWrite:
		fmt.Fprintf(out, "  - write %s (AllowedCPUs=%s)\n", env.slicePath(), cpulist)
	case sliceRewrite:
		fmt.Fprintf(out, "  - rewrite %s (AllowedCPUs=%s)\n", env.slicePath(), cpulist)
	case sliceTake:
		fmt.Fprintf(out, "  - replace %s (AllowedCPUs=%s)\n", env.slicePath(), cpulist)
	case sliceKeep:
		fmt.Fprintf(out, "  - leave %s unchanged (already up to date)\n", env.slicePath())
	case sliceSkip:
		fmt.Fprintf(out, "  - leave %s untouched (not managed by ccorral)\n", env.slicePath())
	}
	if removeOld {
		fmt.Fprintf(out, "  - remove %s\n", env.oldScript())
	}
	fmt.Fprintln(out, "  - run: systemctl --user daemon-reload")
	fmt.Fprintf(out, "  - run: systemctl --user enable %s\n", installService)
	fmt.Fprintf(out, "  - run: systemctl --user start %s\n\n", installService)
	if yes, ok := installAsk(r, out, "Proceed? [y/N] "); !ok || !yes {
		return abort()
	}
	fmt.Fprintln(out)

	// --- act ---
	fail := func(what string, err error) int {
		fmt.Fprintf(out, "%s: %v\n", what, err)
		return 1
	}
	_, activeErr := env.run("is-active", "--quiet", installService)
	if activeErr == nil {
		if msg, err := env.run("stop", installService); err != nil {
			fmt.Fprintf(out, "systemctl --user stop %s failed: %v\n", installService, err)
			if msg != "" {
				fmt.Fprintln(out, msg)
			}
			return 1
		}
		fmt.Fprintf(out, "Ran systemctl --user stop %s\n", installService)
	}

	if sameBin {
		fmt.Fprintf(out, "Kept %s (same file as the running binary)\n", env.binPath())
	} else {
		if err := installCopy(env.self, env.binPath()); err != nil {
			return fail("install failed", err)
		}
		fmt.Fprintf(out, "Wrote %s\n", env.binPath())
	}
	if err := installWriteFile(env.unitPath(), []byte(installUnitTemplate)); err != nil {
		return fail("writing unit failed", err)
	}
	fmt.Fprintf(out, "Wrote %s\n", env.unitPath())
	switch action {
	case sliceWrite, sliceRewrite, sliceTake:
		if err := installWriteFile(env.slicePath(), []byte(slice)); err != nil {
			return fail("writing slice failed", err)
		}
		fmt.Fprintf(out, "Wrote %s\n", env.slicePath())
	}
	if removeOld {
		if err := os.Remove(env.oldScript()); err != nil && !os.IsNotExist(err) {
			return fail("removing old script failed", err)
		}
		fmt.Fprintf(out, "Removed %s\n", env.oldScript())
	}

	steps := [][]string{{"daemon-reload"}, {"enable", installService}, {"start", installService}}
	for _, s := range steps {
		if msg, err := env.run(s...); err != nil {
			fmt.Fprintf(out, "systemctl --user %s failed: %v\n", strings.Join(s, " "), err)
			if msg != "" {
				fmt.Fprintln(out, msg)
			}
			return 1
		}
		fmt.Fprintf(out, "Ran systemctl --user %s\n", strings.Join(s, " "))
	}

	if !installOnPath(env.binDir()) {
		fmt.Fprintf(out, "\nNote: %s is not on your $PATH. Add it, e.g.:\n  export PATH=\"$HOME/.local/bin:$PATH\"\n", env.binDir())
	}
	fmt.Fprintf(out, "\nccorral is running as %s.\n", installService)
	return 0
}

func uninstallRun(env installEnv, in io.Reader, out io.Writer) int {
	r := bufio.NewReader(in)
	fmt.Fprintln(out, "Uninstall ccorral. This will:")
	fmt.Fprintf(out, "  - run: systemctl --user disable --now %s\n", installService)
	fmt.Fprintf(out, "  - remove %s\n", env.unitPath())
	fmt.Fprintln(out, "  - run: systemctl --user daemon-reload")
	fmt.Fprintf(out, "  - remove %s\n", env.binPath())
	fmt.Fprintf(out, "It leaves %s and the ccorral config in place (the claude() shell wrapper depends on the slice).\n\n", env.slicePath())
	if yes, ok := installAsk(r, out, "Proceed? [y/N] "); !ok || !yes {
		fmt.Fprintln(out, "Aborted; nothing was changed.")
		return 1
	}
	fmt.Fprintln(out)

	code := 0
	warn := func(what string, err error) {
		fmt.Fprintf(out, "%s: %v\n", what, err)
		code = 1
	}
	if msg, err := env.run("disable", "--now", installService); err != nil {
		warn("systemctl disable failed", fmt.Errorf("%v %s", err, msg))
	} else {
		fmt.Fprintf(out, "Ran systemctl --user disable --now %s\n", installService)
	}
	if err := os.Remove(env.unitPath()); err != nil && !os.IsNotExist(err) {
		warn("removing unit failed", err)
	} else {
		fmt.Fprintf(out, "Removed %s\n", env.unitPath())
	}
	if msg, err := env.run("daemon-reload"); err != nil {
		warn("systemctl daemon-reload failed", fmt.Errorf("%v %s", err, msg))
	} else {
		fmt.Fprintln(out, "Ran systemctl --user daemon-reload")
	}
	if err := os.Remove(env.binPath()); err != nil && !os.IsNotExist(err) {
		warn("removing binary failed", err)
	} else {
		fmt.Fprintf(out, "Removed %s\n", env.binPath())
	}
	fmt.Fprintf(out, "\nLeft in place: %s and the ccorral config.\n", env.slicePath())
	fmt.Fprintln(out, "claude.slice keeps its current CPU limit until reboot; run 'systemctl --user revert claude.slice' to drop it now.")
	return code
}

// installAsk prints prompt and reads one answer. yes is true only for y/yes
// (any case); anything else, including an empty line, is a no. ok is false on
// EOF with nothing typed.
func installAsk(r *bufio.Reader, out io.Writer, prompt string) (yes, ok bool) {
	fmt.Fprint(out, prompt)
	line, err := r.ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	if err != nil && ans == "" {
		fmt.Fprintln(out)
		return false, false
	}
	return ans == "y" || ans == "yes", true
}

func installFirstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimRight(line, "\r")
}

// installIndent indents every line of s by four spaces for display.
func installIndent(s string) string {
	s = strings.TrimRight(s, "\n")
	return "    " + strings.ReplaceAll(s, "\n", "\n    ") + "\n"
}

// installCopy copies src to target atomically with mode 0755.
func installCopy(src, target string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return installAtomic(target, in, 0o755)
}

// installWriteFile writes data to path atomically with mode 0644.
func installWriteFile(path string, data []byte) error {
	return installAtomic(path, bytes.NewReader(data), 0o644)
}

// installAtomic writes r to a temp file beside target and renames it into place.
func installAtomic(target string, r io.Reader, mode os.FileMode) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ccorral-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

// installOnPath reports whether dir is listed in $PATH.
func installOnPath(dir string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == "" {
			continue
		}
		if p == dir {
			return true
		}
		if abs, err := filepath.Abs(p); err == nil && abs == dir {
			return true
		}
	}
	return false
}
