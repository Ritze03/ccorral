package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	runUsage     = "usage: ccorral -- CMD [ARGS...]   (or: ccorral run [--] CMD [ARGS...])"
	outsideUsage = "usage: ccorral outside [--] CMD [ARGS...]"
)

// runArgv builds the argv to exec. Inside claude.slice the command runs as is,
// children inherit the cgroup. Outside, systemd-run --scope starts it in the
// slice and execs it itself, so stdin, exit code and signals pass through.
// systemd-run expands ${VAR} and turns $$ into $ in the command's arguments, so
// every $ is doubled to hand them over unchanged.
func runArgv(inside bool, slice, systemdRun, unit, cmd string, args []string) []string {
	if inside {
		return append([]string{cmd}, args...)
	}
	argv := []string{systemdRun, "--user", "--scope", "--slice=" + slice, "--unit=" + unit, "--collect", "-q", "--", runEscape(cmd)}
	for _, a := range args {
		argv = append(argv, runEscape(a))
	}
	return argv
}

func runEscape(s string) string { return strings.ReplaceAll(s, "$", "$$") }

// runMain replaces this process with args[0] running inside claude.slice. It
// only returns on failure: the exit code (2 usage, 127 not found, 126 not
// executable, 1 other).
func runMain(args []string) int { return runIn(sweepSlice, runUsage, args) }

// outsideMain is runMain for the desktop side: args[0] runs in app.slice.
func outsideMain(args []string) int { return runIn(outsideSlice, outsideUsage, args) }

// runIn runs args[0] in slice. Only claude.slice has the "already inside"
// shortcut; app.slice is always entered through systemd-run.
func runIn(slice, usage string, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	cmdPath, err := exec.LookPath(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "ccorral: %s: %s\n", args[0], runErrText(err))
		if runNotFound(err) {
			return 127
		}
		return 126
	}
	b, _ := os.ReadFile("/proc/self/cgroup")
	inside := slice == sweepSlice && sweepInside(sweepCgroupV2(string(b)))
	bin, sr := cmdPath, ""
	if !inside {
		if sr, err = exec.LookPath("systemd-run"); err != nil {
			fmt.Fprintln(os.Stderr, "ccorral: systemd-run: not found")
			return 1
		}
		bin = sr
	}
	err = syscall.Exec(bin, runArgv(inside, slice, sr, runUnit(), args[0], args[1:]), os.Environ())
	fmt.Fprintf(os.Stderr, "ccorral: %s: %v\n", args[0], err)
	return 1
}

// runUnit is an explicit scope name. systemd-run's default (run-p<pid>-i<n>) is
// the same after an exec, so it would collide when ccorral itself was started
// by `systemd-run --scope`.
func runUnit() string {
	return fmt.Sprintf("ccorral-run-%d-%d", os.Getpid(), time.Now().UnixNano())
}

func runNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist)
}

func runErrText(err error) string {
	if runNotFound(err) {
		return "not found"
	}
	return err.Error()
}
