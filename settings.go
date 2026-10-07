package main

// settings.go is `ccorral settings`: a small full-screen terminal UI over the
// config file (config.go) for the two core groups and the sweep interval. Mode
// switching is not here; that is the tray and `ccorral green|yellow|red`.
//
// It is a separate, short-lived process on purpose: every change is written
// through immediately and the running daemon is told to re-read the file.
//
// Everything that decides *what is on screen* is a pure function of state —
// settingsRender and settingsPress — so the whole UI is testable without a
// terminal. Only RunSettings touches the tty.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// ANSI: alternate screen in and out, cursor hidden and shown, home + erase.
const (
	settingsAltEnter   = "\x1b[?1049h"
	settingsAltLeave   = "\x1b[?1049l"
	settingsHideCursor = "\x1b[?25l"
	settingsShowCursor = "\x1b[?25h"
	settingsClear      = "\x1b[H\x1b[2J"
	settingsReverse    = "\x1b[7m"
	settingsReset      = "\x1b[0m"
)

const (
	settingsHint         = "←↑↓→ move · space toggle · +/- interval · d default · q quit"
	settingsNoDaemon     = "saved — daemon not running, applies on next start"
	settingsReloadedOK   = "saved, daemon reloaded"
	settingsNotATTY      = "ccorral settings needs a terminal; run it in one, or edit ~/.config/ccorral/config"
	settingsIntervalStep = 500
)

// The rows, top to bottom. The first two double as indexes into
// settingsGroupNames, which are the config group names.
const (
	settingsRowYellow = iota
	settingsRowRed
	settingsRowInterval
	settingsRows
)

var (
	settingsGroupNames = [...]string{modeYellow, modeRed}
	settingsGroupLabel = [...]string{"Yellow", "Red"}
)

// settingsState is the whole UI. cores is the physical topology; row and col
// are the cursor (col only matters on the two group rows).
type settingsState struct {
	cores       [][]int
	yellow, red []int
	interval    int
	row, col    int
	status      string
}

// settingsLoad reads the topology and the current config into a fresh state.
func settingsLoad(path, sysRoot string) (settingsState, error) {
	c, err := configLoad(path, sysRoot)
	if err != nil {
		return settingsState{}, err
	}
	cores, err := cpuCores(sysRoot)
	if err != nil {
		return settingsState{}, err
	}
	return settingsState{cores: cores, yellow: c.Yellow, red: c.Red, interval: c.IntervalMs}, nil
}

// group is the CPU list of the group row r.
func (s settingsState) group(r int) []int {
	if r == settingsRowRed {
		return s.red
	}
	return s.yellow
}

// settingsHas says whether any thread of core is in list.
func settingsHas(list, core []int) bool {
	return slices.ContainsFunc(core, func(id int) bool { return slices.Contains(list, id) })
}

// --- pure rendering --------------------------------------------------------

// settingsRender draws the whole screen. The selected cell is reverse video;
// nothing else is coloured, so it reads the same on every theme.
func settingsRender(s settingsState) string {
	var b strings.Builder
	// cur wraps text in reverse video when it is the cursor's cell.
	cur := func(on bool, text string) string {
		if on {
			return settingsReverse + text + settingsReset
		}
		return text
	}

	b.WriteString("ccorral settings\n\n")

	b.WriteString(strings.Repeat(" ", 11))
	for i := range s.cores {
		fmt.Fprintf(&b, "%2d ", i)
	}
	b.WriteString("\n")

	for r, name := range settingsGroupLabel {
		list := s.group(r)
		fmt.Fprintf(&b, "  %-9s", name)
		for i, core := range s.cores {
			box := "[ ]"
			if settingsHas(list, core) {
				box = "[x]"
			}
			b.WriteString(cur(s.row == r && s.col == i, box))
		}
		b.WriteString("   " + cpuFormat(list) + "\n")
	}

	fmt.Fprintf(&b, "  %-9s", "Interval")
	b.WriteString(cur(s.row == settingsRowInterval, fmt.Sprintf("%d ms", s.interval)))
	fmt.Fprintf(&b, "   (%d–%d)\n", intervalMin, intervalMax)

	b.WriteString("\n  " + settingsHint + "\n")
	b.WriteString("  " + s.status + "\n")
	return b.String()
}

// --- pure input ------------------------------------------------------------

// settingsAct is what the caller has to do after a keypress.
type settingsAct int

const (
	settingsActNone  settingsAct = iota
	settingsActWrite             // save the row under the cursor
	settingsActReset             // the row under the cursor went back to its default: save that
	settingsActQuit
)

// settingsPress applies one key to the state. key is one of the names
// settingsReadKey produces: up, down, left, right, inc, dec, toggle, default,
// quit, none.
func settingsPress(s settingsState, key string) (settingsState, settingsAct) {
	s.status = ""
	switch key {
	case "quit":
		return s, settingsActQuit
	case "up":
		if s.row > 0 {
			s.row--
		}
	case "down":
		if s.row < settingsRows-1 {
			s.row++
		}
	case "left", "right":
		step := 1
		if key == "left" {
			step = -1
		}
		if s.row == settingsRowInterval {
			return settingsStep(s, step)
		}
		if c := s.col + step; c >= 0 && c < len(s.cores) {
			s.col = c
		}
	case "inc", "dec":
		if s.row == settingsRowInterval {
			step := 1
			if key == "dec" {
				step = -1
			}
			return settingsStep(s, step)
		}
	case "toggle":
		if s.row != settingsRowInterval {
			return settingsToggle(s)
		}
	case "default":
		yellow, red := cpuDefaults(s.cores)
		switch s.row {
		case settingsRowYellow:
			s.yellow = yellow
		case settingsRowRed:
			s.red = red
		default:
			s.interval = intervalDefault
		}
		return s, settingsActReset
	}
	return s, settingsActNone
}

// settingsStep moves the interval one step, clamped to the allowed range.
func settingsStep(s settingsState, dir int) (settingsState, settingsAct) {
	ms := min(max(s.interval+dir*settingsIntervalStep, intervalMin), intervalMax)
	if ms == s.interval {
		s.status = fmt.Sprintf("interval is already at its limit (%d–%d ms)", intervalMin, intervalMax)
		return s, settingsActNone
	}
	s.interval = ms
	return s, settingsActWrite
}

// settingsToggle flips the core under the cursor in its group, siblings and
// all. Unchecking the last core is refused: a group can't be empty.
func settingsToggle(s settingsState) (settingsState, settingsAct) {
	core := s.cores[s.col]
	list := s.group(s.row)
	var out []int // list without the core's threads
	for _, id := range list {
		if !slices.Contains(core, id) {
			out = append(out, id)
		}
	}
	if settingsHas(list, core) {
		if len(out) == 0 {
			s.status = fmt.Sprintf("%s needs at least one core, can't uncheck the last one", settingsGroupLabel[s.row])
			return s, settingsActNone
		}
	} else {
		out = append(out, core...)
		sort.Ints(out)
	}
	if s.row == settingsRowRed {
		s.red = out
	} else {
		s.yellow = out
	}
	return s, settingsActWrite
}

// --- write and reload ------------------------------------------------------

// settingsSave persists the row the last act touched and asks the daemon to
// re-read the config, putting the outcome in the status line. reload is a
// parameter so tests can hand it a fake instead of a socket. If the write
// fails the state is re-read from disk, so the screen never shows a value that
// was not saved.
func settingsSave(path, sysRoot string, s settingsState, act settingsAct, reload func() error) settingsState {
	var err error
	switch {
	case s.row == settingsRowInterval:
		err = configSetInterval(path, s.interval)
	case act == settingsActReset:
		err = configResetGroup(path, settingsGroupNames[s.row])
	default:
		err = configSetGroup(path, sysRoot, settingsGroupNames[s.row], cpuFormat(s.group(s.row)))
	}
	if err != nil {
		msg := "could not save: " + err.Error()
		if n, lerr := settingsLoad(path, sysRoot); lerr == nil {
			n.row, n.col = s.row, s.col
			s = n
		}
		s.status = msg
		return s
	}
	if err := reload(); err != nil {
		if m := err.Error(); m == ipcNotRunning || m == ipcNotInstalled {
			s.status = settingsNoDaemon
		} else {
			s.status = "saved, but daemon: " + m
		}
		return s
	}
	s.status = settingsReloadedOK
	return s
}

// settingsReload sends one `reload` request to the daemon, reusing the CLI's
// wire code so there is only one implementation of it.
func settingsReload() error {
	var buf strings.Builder
	if code := RunIPCClient(SocketPath(), []string{"reload"}, &buf); code != 0 {
		return errors.New(strings.TrimSpace(buf.String()))
	}
	return nil
}

// --- the terminal ----------------------------------------------------------

// settingsReadKey turns the next keypress into a name. Arrow keys arrive as a
// three-byte escape sequence; a lone Esc (nothing buffered behind it) quits.
func settingsReadKey(r *bufio.Reader) (string, error) {
	b, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	switch b {
	case 'q', 'Q', 0x03, 0x04: // q, Ctrl-C, Ctrl-D
		return "quit", nil
	case 'j':
		return "down", nil
	case 'k':
		return "up", nil
	case 'h':
		return "left", nil
	case 'l':
		return "right", nil
	case '+', '=':
		return "inc", nil
	case '-', '_':
		return "dec", nil
	case 'd', 'D':
		return "default", nil
	case ' ', '\r', '\n':
		return "toggle", nil
	case 0x1b:
		if r.Buffered() < 2 {
			return "quit", nil
		}
		intro, _ := r.ReadByte()
		final, _ := r.ReadByte()
		if intro != '[' && intro != 'O' {
			return "none", nil
		}
		switch final {
		case 'A':
			return "up", nil
		case 'B':
			return "down", nil
		case 'C':
			return "right", nil
		case 'D':
			return "left", nil
		}
	}
	return "none", nil
}

// RunSettings is the `ccorral settings` subcommand. It returns the process exit
// code: 0 on a clean quit, 1 when stdin is not a terminal or the CPU topology
// can't be read.
func RunSettings(in io.Reader, out io.Writer) int {
	f, ok := in.(*os.File)
	if !ok {
		fmt.Fprintln(out, settingsNotATTY)
		return 1
	}
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		fmt.Fprintln(out, settingsNotATTY)
		return 1
	}

	path := configPath()
	s, err := settingsLoad(path, "/sys")
	if err != nil {
		fmt.Fprintln(out, "ccorral settings: "+err.Error())
		return 1
	}

	// One restore, however we leave: normal return, panic, or a signal. Under
	// raw mode ISIG is off, so Ctrl-C arrives as a byte and is handled as a
	// key; the signal handler is for the kill that comes from elsewhere.
	var once sync.Once
	restore := func() {
		unix.IoctlSetTermios(fd, unix.TCSETS, old)
		fmt.Fprint(out, settingsShowCursor+settingsAltLeave)
	}
	defer once.Do(restore)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		if _, ok := <-sigs; !ok {
			return
		}
		once.Do(restore)
		os.Exit(1)
	}()

	raw := *old
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG
	raw.Iflag &^= unix.IXON | unix.ICRNL
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		fmt.Fprintln(out, "ccorral settings: cannot set raw mode: "+err.Error())
		return 1
	}
	fmt.Fprint(out, settingsAltEnter+settingsHideCursor)

	r := bufio.NewReader(f)
	for {
		fmt.Fprint(out, settingsClear+settingsRender(s))
		key, err := settingsReadKey(r)
		if err != nil {
			return 0 // stdin closed: leave as if quit
		}
		var act settingsAct
		s, act = settingsPress(s, key)
		switch act {
		case settingsActQuit:
			return 0
		case settingsActWrite, settingsActReset:
			s = settingsSave(path, "/sys", s, act, settingsReload)
		}
	}
}
