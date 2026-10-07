package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	tsVers  = "/fake/versions/"
	tsOut   = "/user.slice/user-1000.slice/user@1000.service/tmux-spawn-1.scope"
	tsIn    = "/user.slice/user-1000.slice/user@1000.service/claude.slice/run-u1.scope"
	tsClaud = tsVers + "2.0.0/claude"
)

type sweepFake struct {
	calls [][]uint32
	fail  map[uint32]bool // a call whose first pid is here fails
	// failPid makes every call that includes the pid fail (a pid that died).
	failPid uint32
}

func (f *sweepFake) StartScope(name, desc string, pids []uint32) error {
	if !strings.HasPrefix(name, fmt.Sprintf("ccorral-%d-", pids[0])) || !strings.HasSuffix(name, ".scope") {
		return fmt.Errorf("bad scope name %q", name)
	}
	f.calls = append(f.calls, pids)
	for _, p := range pids {
		if p == f.failPid {
			return errors.New("no such process")
		}
	}
	if f.fail[pids[0]] {
		return errors.New("no such process")
	}
	return nil
}

type tsProc struct {
	pid, ppid int
	exe, cg   string
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func tsTree(t *testing.T, ps ...tsProc) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range ps {
		d := filepath.Join(root, fmt.Sprint(p.pid))
		must(t, os.Mkdir(d, 0o755))
		if p.exe != "" { // "": exe unreadable
			must(t, os.Symlink(p.exe, filepath.Join(d, "exe")))
		}
		// nasty comm: a space and a ")" inside
		must(t, os.WriteFile(filepath.Join(d, "stat"),
			[]byte(fmt.Sprintf("%d (a) b) S %d 1 1 0 -1 4194304 1 2 3\n", p.pid, p.ppid)), 0o644))
		must(t, os.WriteFile(filepath.Join(d, "cgroup"), []byte("0::"+p.cg+"\n"), 0o644))
	}
	// noise: a non-numeric entry and a process whose exe is unreadable
	must(t, os.Mkdir(filepath.Join(root, "self"), 0o755))
	must(t, os.Mkdir(filepath.Join(root, "2"), 0o755))
	return root
}

func tsRun(t *testing.T, f *sweepFake, ps ...tsProc) {
	t.Helper()
	must(t, sweepOnce(sweepConfig{ProcRoot: tsTree(t, ps...), Prefix: tsVers, Mover: f}))
}

func tsWant(t *testing.T, f *sweepFake, want ...[]uint32) {
	t.Helper()
	if len(f.calls) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
}

func TestSweepEscapedRootAndDescendants(t *testing.T) {
	f := &sweepFake{}
	tsRun(t, f,
		tsProc{100, 1, tsClaud + " (deleted)", tsOut}, // root, binary replaced
		tsProc{101, 100, "/usr/bin/zsh", tsOut},
		tsProc{102, 101, "/usr/bin/node", tsOut},
		tsProc{103, 100, "/usr/bin/git", tsIn}, // already inside: not moved
	)
	tsWant(t, f, []uint32{100, 101, 102})
}

// Root already inside the slice (wrapper or earlier move): only the stray child
// that started its own scope gets a new scope.
func TestSweepStragglerGetsNewScope(t *testing.T) {
	f := &sweepFake{}
	tsRun(t, f,
		tsProc{100, 1, tsClaud, tsIn},
		tsProc{101, 100, "/usr/bin/zsh", tsIn},
		tsProc{102, 101, "/usr/bin/sleep", tsOut},
	)
	tsWant(t, f, []uint32{102})
}

func TestSweepUnreadableExeKeepsDescendants(t *testing.T) {
	f := &sweepFake{}
	tsRun(t, f,
		tsProc{100, 1, tsClaud, tsOut},
		tsProc{101, 100, "", tsOut}, // setuid helper: exe unreadable
		tsProc{102, 101, "/usr/bin/node", tsOut},
		tsProc{300, 1, "", tsOut}, // never a root
	)
	tsWant(t, f, []uint32{100, 101, 102})
}

// A pid that died after the scan fails the batch: one scope per pid, the dead one ignored.
func TestSweepBatchFailsPerPidScopes(t *testing.T) {
	f := &sweepFake{failPid: 102}
	tsRun(t, f,
		tsProc{100, 1, tsClaud, tsOut},
		tsProc{101, 100, "/usr/bin/zsh", tsOut},
		tsProc{102, 101, "/usr/bin/gone", tsOut},
		tsProc{103, 101, "/usr/bin/node", tsOut},
	)
	tsWant(t, f, []uint32{100, 101, 102, 103}, []uint32{100}, []uint32{101}, []uint32{102}, []uint32{103})
}

func TestSweepLogsMove(t *testing.T) {
	var buf strings.Builder
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	tsRun(t, &sweepFake{},
		tsProc{100, 1, tsClaud, tsOut},
		tsProc{101, 100, "/usr/bin/bash", tsOut},
	)
	if got := buf.String(); !strings.Contains(got, "sweep: claude 100: moved 2 pids to ccorral-100-") {
		t.Errorf("log = %q", got)
	}
}

func TestSweepLeavesNonClaudeAlone(t *testing.T) {
	f := &sweepFake{}
	tsRun(t, f,
		tsProc{50, 1, "/usr/bin/tmux", tsOut},
		tsProc{51, 50, "/usr/bin/zsh", tsOut},       // non-claude parent of claude
		tsProc{52, 51, "/usr/bin/nvim", tsOut},      // sibling in the same scope
		tsProc{100, 51, tsClaud, tsOut},             // claude
		tsProc{101, 100, "/usr/bin/bash", tsOut},    // its child
		tsProc{200, 1, "/fake/other/claude", tsOut}, // wrong prefix
		tsProc{201, 1, "/fake/versions", tsOut},     // prefix without the slash
	)
	tsWant(t, f, []uint32{100, 101})
}

func TestSweepNested(t *testing.T) {
	f := &sweepFake{}
	tsRun(t, f,
		tsProc{100, 1, tsClaud, tsOut},
		tsProc{101, 100, "/usr/bin/bash", tsOut},
		tsProc{102, 101, tsClaud, tsOut}, // claude inside claude
		tsProc{103, 102, "/usr/bin/bash", tsOut},
	)
	tsWant(t, f, []uint32{100, 101, 102, 103})
}

func TestSweepNothingToDo(t *testing.T) {
	f := &sweepFake{}
	tsRun(t, f,
		tsProc{100, 1, tsClaud, tsIn},
		tsProc{101, 100, "/usr/bin/bash", tsIn},
		tsProc{50, 1, "/usr/bin/nvim", tsOut},
	)
	tsWant(t, f)
	tsRun(t, f) // empty tree
	tsWant(t, f)
}

func TestSweepMoverErrorContinues(t *testing.T) {
	f := &sweepFake{fail: map[uint32]bool{100: true}}
	tsRun(t, f,
		tsProc{100, 1, tsClaud, tsOut},
		tsProc{300, 1, tsClaud, tsOut},
		tsProc{301, 300, "/usr/bin/bash", tsOut},
	)
	tsWant(t, f,
		[]uint32{100},
		[]uint32{100}, // the per-pid retry
		[]uint32{300, 301},
	)
}

func TestSweepMissingProcRoot(t *testing.T) {
	err := sweepOnce(sweepConfig{ProcRoot: filepath.Join(t.TempDir(), "nope"), Prefix: tsVers, Mover: &sweepFake{}})
	if err == nil {
		t.Fatal("want error")
	}
}

// TestSweepLive moves a real process through the session bus. It never
// touches real Claude sessions: the prefix is a temp dir. Opt in with
// CCORRAL_LIVE=1.
func TestSweepLive(t *testing.T) {
	if os.Getenv("CCORRAL_LIVE") == "" {
		t.Skip("set CCORRAL_LIVE=1")
	}
	dir := filepath.Join(t.TempDir(), "versions") + "/"
	must(t, os.MkdirAll(dir+"x", 0o755))
	sleep, err := os.ReadFile("/usr/bin/sleep")
	must(t, err)
	bin := dir + "x/claude"
	must(t, os.WriteFile(bin, sleep, 0o755))
	cmd := exec.Command("systemd-run", "--user", "--scope", "--slice=app.slice", bin, "300")
	must(t, cmd.Start())
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	// systemd-run execs the target, so cmd.Process.Pid becomes the sleep.
	pid := cmd.Process.Pid
	for i := 0; i < 50; i++ {
		if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil && exe == bin {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	must(t, sweepOnce(sweepConfig{Prefix: dir, Mover: sweepDBus{}}))
	// StartTransientUnit returns once the job is queued, so the move is async.
	var cg []byte
	for i := 0; i < 50 && !strings.Contains(string(cg), "/claude.slice/ccorral-"); i++ {
		time.Sleep(100 * time.Millisecond)
		cg, err = os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
		must(t, err)
	}
	st, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	for _, l := range strings.Split(string(st), "\n") {
		if strings.HasPrefix(l, "Cpus_allowed_list") {
			t.Log(l)
		}
	}
	t.Logf("cgroup: %s", cg)
	if !strings.Contains(string(cg), "/claude.slice/ccorral-") {
		t.Fatalf("not moved: %s", cg)
	}
}
