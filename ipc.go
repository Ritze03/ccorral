package main

// ipc.go is the Unix-socket link between the ccorral daemon and its CLI.
// Ported from ts6tray's ipc.go; only the commands and backend differ.
//
// Wire format: the client sends one request line of space-separated words
// ("status\n", "mode yellow\n", "reload\n"); the server answers with text
// lines, the first "ok" or "err" and the rest a human-readable body, then
// closes. One request per connection. The socket doubles as the single-instance
// guard: a live socket means a daemon is already running.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ipcBackend is what the IPC server needs from the daemon. Tests use a fake.
type ipcBackend interface {
	// Status returns the current mode and cores, e.g. "yellow 3-9,13-19".
	Status() string
	// SetMode applies green|yellow|red and saves the config. The server has
	// already validated mode.
	SetMode(mode string) error
	// Reload re-reads the config and re-applies it.
	Reload() error
}

const (
	// ipcTimeout bounds one request/response exchange.
	ipcTimeout = 5 * time.Second
	// ipcProbeTimeout bounds the "is a daemon already there?" dial.
	ipcProbeTimeout = 500 * time.Millisecond
	// ipcMaxRequest caps one request line; the socket is a trust boundary
	// even at 0600, so nothing unbounded is read.
	ipcMaxRequest = 4096
)

const ipcUsage = "usage:\n  ccorral status\n  ccorral green|yellow|red\n  ccorral reload"

// ipcNotRunning is what the CLI prints when nothing answers the socket.
const ipcNotRunning = "ccorral daemon not running — start it with `systemctl --user start ccorral`"

// ipcClosedEarly is what the CLI prints when the daemon accepted the
// connection but closed it without a reply (it was stopped mid-request).
const ipcClosedEarly = "ccorral daemon closed the connection without replying (was it stopped?)"

// SocketPath is where the daemon listens: $XDG_RUNTIME_DIR/ccorral.sock, or
// <tmp>/ccorral-<uid>.sock when XDG_RUNTIME_DIR is unset.
func SocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "ccorral.sock")
	}
	return filepath.Join(os.TempDir(), "ccorral-"+strconv.Itoa(os.Getuid())+".sock")
}

// --- server ---------------------------------------------------------------

// ServeIPC listens on path and serves requests until ctx is done, then returns
// nil (the listener's Close unlinks the socket). It returns an error if another
// daemon is already listening.
func ServeIPC(ctx context.Context, b ipcBackend, path string) error {
	ln, err := ipcListen(path)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	done := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		// Close unlinks the socket: *net.UnixListener does that itself, so
		// ServeIPC must never os.Remove(path) — by the time it returns, the
		// path may already belong to a successor daemon.
		ln.Close()
		close(closed)
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			close(done)
			<-closed // the unlink is part of shutdown, so wait for it
			wg.Wait()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ipcHandle(b, conn)
		}()
	}
}

// ipcListen binds path, clearing a stale socket but refusing to steal a live
// one.
func ipcListen(path string) (net.Listener, error) {
	if _, err := os.Stat(path); err == nil {
		if c, derr := net.DialTimeout("unix", path, ipcProbeTimeout); derr == nil {
			c.Close()
			return nil, fmt.Errorf("ccorral already running on %s", path)
		}
		// Nobody home: the socket file outlived its daemon.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("removing stale socket %s: %w", path, err)
		}
	}
	// Create the socket 0600 in the first place: the chmod below closes the
	// window only after the fact, which matters on the world-writable /tmp
	// fallback path.
	// ponytail: umask is process-wide, but ipcListen runs once at startup
	// before any other goroutine creates files, so a save/restore is enough.
	oldMask := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(oldMask)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		os.Remove(path)
		return nil, err
	}
	return ln, nil
}

// ipcHandle serves exactly one request on conn and closes it.
func ipcHandle(b ipcBackend, conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(ipcTimeout))

	// The cap is the reader, not just its buffer: bufio.ReadString keeps
	// growing its own string past the buffer size, so only a LimitReader
	// actually bounds what a client can make the daemon hold.
	r := bufio.NewReaderSize(io.LimitReader(conn, ipcMaxRequest), ipcMaxRequest)
	line, err := r.ReadString('\n')
	if err != nil {
		// No newline within the cap, or the peer vanished mid-line. Either
		// way the request is malformed and nothing reaches the backend.
		ipcReply(conn, false, "malformed request: no newline within "+strconv.Itoa(ipcMaxRequest)+" bytes")
		// Half-close so the peer sees the reply and EOF at once, then swallow
		// the rest of its line: closing with data still unread would reset the
		// connection and take the reply with it. The tail is discarded, never
		// buffered, and bounded by the deadline set above.
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		io.Copy(io.Discard, conn)
		return
	}
	ok, body := ipcDispatch(b, strings.Fields(line))
	ipcReply(conn, ok, body)
}

// ipcReply writes one "ok"/"err" status line plus an optional body.
func ipcReply(conn net.Conn, ok bool, body string) {
	status := "err"
	if ok {
		status = "ok"
	}
	fmt.Fprintf(conn, "%s\n", status)
	if body != "" {
		fmt.Fprintf(conn, "%s\n", body)
	}
}

func ipcValidMode(m string) bool {
	return m == "green" || m == "yellow" || m == "red"
}

// ipcDispatch validates the request words and runs the command. The socket is
// a trust boundary, so nothing here trusts the client's spelling.
func ipcDispatch(b ipcBackend, words []string) (ok bool, body string) {
	if len(words) == 0 {
		return false, "empty request\n" + ipcUsage
	}
	switch words[0] {
	case "status":
		if len(words) != 1 {
			return false, "status takes no arguments"
		}
		return true, b.Status()
	case "reload":
		if len(words) != 1 {
			return false, "reload takes no arguments"
		}
		if err := b.Reload(); err != nil {
			return false, err.Error()
		}
		return true, "reloaded"
	case "mode":
		if len(words) != 2 || !ipcValidMode(words[1]) {
			return false, "bad mode: want green|yellow|red\n" + ipcUsage
		}
		if err := b.SetMode(words[1]); err != nil {
			return false, err.Error()
		}
		return true, b.Status()
	}
	return false, "unknown command " + strconv.Quote(words[0]) + "\n" + ipcUsage
}

// --- client ---------------------------------------------------------------

// RunIPCClient sends one command (e.g. {"status"}, {"mode","red"}, {"reload"})
// to the daemon and prints the response body to out. It returns the process
// exit code: 0 on ok, 1 on err or no daemon, 2 on bad arguments (rejected
// without dialing).
func RunIPCClient(path string, args []string, out io.Writer) int {
	if !ipcValidArgs(args) {
		fmt.Fprintln(out, ipcUsage)
		return 2
	}

	conn, err := net.DialTimeout("unix", path, ipcTimeout)
	if err != nil {
		fmt.Fprintln(out, ipcNotRunning)
		return 1
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(ipcTimeout))

	if _, err := io.WriteString(conn, strings.Join(args, " ")+"\n"); err != nil {
		fmt.Fprintln(out, ipcNotRunning)
		return 1
	}
	resp, _ := io.ReadAll(conn)
	if len(resp) == 0 {
		// Nothing came back at all: the daemon accepted and then went away.
		fmt.Fprintln(out, ipcClosedEarly)
		return 1
	}
	status, body, _ := strings.Cut(strings.TrimRight(string(resp), "\n"), "\n")
	if body != "" {
		fmt.Fprintln(out, body)
	}
	if status == "ok" {
		return 0
	}
	if status != "err" {
		fmt.Fprintln(out, "unexpected reply from daemon: "+strconv.Quote(status))
	}
	return 1
}

// ipcValidArgs mirrors the server's validation so a typo never reaches the
// daemon.
func ipcValidArgs(args []string) bool {
	switch len(args) {
	case 1:
		return args[0] == "status" || args[0] == "reload"
	case 2:
		return args[0] == "mode" && ipcValidMode(args[1])
	}
	return false
}
