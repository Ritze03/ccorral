// sweep.go: the 5 s sweep (D8, D11, D13). It finds every Claude Code process
// by its executable, collects the process tree below it, and moves whatever
// is not yet inside claude.slice into it through the systemd user manager on
// the session bus. Only the Claude tree is ever moved: a non-Claude process
// that merely shares an old scope stays where it is. Processes that
// reparented away (double-forked daemons) are no longer descendants and are
// accepted losses (D13).
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	sweepSlice       = "claude.slice"
	sweepDeleted     = " (deleted)" // suffix of /proc/<pid>/exe after the binary was replaced
	sweepCallTimeout = 2 * time.Second
)

// sweepMover is the part of systemd's user manager the sweep needs.
type sweepMover interface {
	// StartScope creates a transient scope inside claude.slice holding pids.
	StartScope(name, desc string, pids []uint32) error
	// Attach moves pids into the existing unit.
	Attach(unit string, pids []uint32) error
}

// sweepConfig is everything sweepOnce needs; the zero ProcRoot means /proc.
type sweepConfig struct {
	ProcRoot string     // default "/proc"
	Prefix   string     // exe prefix of a Claude binary, see sweepDefaultPrefix
	Mover    sweepMover // required
}

// sweepDefaultPrefix is where Claude Code keeps its versioned binaries.
func sweepDefaultPrefix() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local/share/claude/versions") + "/"
}

type sweepProc struct {
	ppid   int
	exe    string
	cgroup string
}

// sweepScan reads every process under root whose stat is readable. An
// unreadable exe (setuid or non-dumpable processes) leaves exe empty, which
// never matches as a root but keeps the process in its parent's tree.
func sweepScan(root string) (map[int]*sweepProc, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	procs := map[int]*sweepProc{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(root, e.Name())
		exe, _ := os.Readlink(filepath.Join(dir, "exe"))
		stat, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			continue
		}
		// comm (field 2) may hold spaces and ")": parse after the last one.
		s := string(stat)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:]) // state ppid ...
		if len(f) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		cg, _ := os.ReadFile(filepath.Join(dir, "cgroup"))
		procs[pid] = &sweepProc{ppid: ppid, exe: exe, cgroup: sweepCgroupV2(string(cg))}
	}
	return procs, nil
}

// sweepCgroupV2 returns the path of the "0::<path>" line, or "".
func sweepCgroupV2(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if p, ok := strings.CutPrefix(l, "0::"); ok {
			return p
		}
	}
	return ""
}

// sweepUnit is the unit directly below claude.slice in cgroup, even when the
// process sits in a sub-cgroup of it.
func sweepUnit(cgroup string) string {
	_, rest, _ := strings.Cut(cgroup+"/", "/"+sweepSlice+"/")
	unit, _, _ := strings.Cut(rest, "/")
	return unit
}

// sweepAttach attaches pids to unit. The batch fails as a whole when one PID
// died since the scan, so it then goes PID by PID and only counts failures.
func sweepAttach(m sweepMover, unit string, pids []uint32) error {
	if m.Attach(unit, pids) == nil {
		return nil
	}
	failed := 0
	for _, p := range pids {
		if m.Attach(unit, []uint32{p}) != nil {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d pids not attached to %s", failed, len(pids), unit)
	}
	return nil
}

func sweepInside(cgroup string) bool {
	return strings.Contains(cgroup+"/", "/"+sweepSlice+"/")
}

// sweepOnce moves every Claude process tree that is not yet inside
// claude.slice. A failed move (typically a PID that died after the scan) is
// logged and skipped; the next sweep catches what is left. Only a failure to
// read the proc tree is returned.
func sweepOnce(cfg sweepConfig) error {
	root := cfg.ProcRoot
	if root == "" {
		root = "/proc"
	}
	prefix := cfg.Prefix
	if prefix == "" { // never match everything
		prefix = sweepDefaultPrefix()
	}
	procs, err := sweepScan(root)
	if err != nil {
		return fmt.Errorf("sweep: %w", err)
	}
	children := map[int][]int{}
	var roots []int
	for pid, p := range procs {
		children[p.ppid] = append(children[p.ppid], pid)
		if strings.HasPrefix(strings.TrimSuffix(p.exe, sweepDeleted), prefix) {
			roots = append(roots, pid)
		}
	}
	sort.Ints(roots)

	seen := map[int]bool{} // each PID is handled once, even in nested Claude trees
	for _, r := range roots {
		if seen[r] {
			continue
		}
		var outside []uint32
		stack := []int{r}
		for len(stack) > 0 {
			pid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[pid] {
				continue
			}
			seen[pid] = true
			if !sweepInside(procs[pid].cgroup) {
				outside = append(outside, uint32(pid))
			}
			stack = append(stack, children[pid]...)
		}
		if len(outside) == 0 {
			continue
		}
		sort.Slice(outside, func(i, j int) bool { return outside[i] < outside[j] })

		if cg := procs[r].cgroup; sweepInside(cg) {
			// Root is in the slice already (wrapper or earlier sweep): stragglers join its unit.
			err = sweepAttach(cfg.Mover, sweepUnit(cg), outside)
		} else {
			name := fmt.Sprintf("ccorral-%d-%d.scope", r, time.Now().UnixNano())
			desc := fmt.Sprintf("ccorral: claude %d", r)
			err = cfg.Mover.StartScope(name, desc, outside)
			if err != nil && len(outside) > 1 {
				// A child may have exited since the scan and the batch is
				// all-or-nothing: start with the root alone, then attach the
				// rest (stragglers are left to the next sweep).
				name = fmt.Sprintf("ccorral-%d-%d.scope", r, time.Now().UnixNano())
				if err = cfg.Mover.StartScope(name, desc, []uint32{uint32(r)}); err == nil {
					var rest []uint32
					for _, p := range outside {
						if p != uint32(r) {
							rest = append(rest, p)
						}
					}
					err = sweepAttach(cfg.Mover, name, rest)
				}
			}
		}
		if err != nil {
			log.Printf("sweep: claude %d: moving %d pids: %v", r, len(outside), err)
		}
	}
	return nil
}

// sweepDBus is the real mover. It dials the session bus per call: moves are
// rare, and a connection that outlives a bus restart would need handling.
type sweepDBus struct{}

func (sweepDBus) call(method string, args ...any) error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("session bus: %w", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), sweepCallTimeout)
	defer cancel()
	obj := conn.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1")
	return obj.CallWithContext(ctx, "org.freedesktop.systemd1.Manager."+method, 0, args...).Err
}

type sweepProp struct {
	Name  string
	Value dbus.Variant
}

type sweepAux struct {
	Name  string
	Props []sweepProp
}

func (d sweepDBus) StartScope(name, desc string, pids []uint32) error {
	props := []sweepProp{
		{"Description", dbus.MakeVariant(desc)},
		{"Slice", dbus.MakeVariant(sweepSlice)},
		{"PIDs", dbus.MakeVariant(pids)},
	}
	return d.call("StartTransientUnit", name, "fail", props, []sweepAux{})
}

func (d sweepDBus) Attach(unit string, pids []uint32) error {
	return d.call("AttachProcessesToUnit", unit, "", pids)
}
