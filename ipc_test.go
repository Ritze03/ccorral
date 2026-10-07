package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// --- fake backend ---------------------------------------------------------

type fakeBackend struct {
	mu        sync.Mutex
	mode      string
	setErr    error
	reloadErr error
	calls     []string // "mode <m>", "reload", in order
}

func (f *fakeBackend) Status() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mode == "" {
		return "yellow 3-9,13-19"
	}
	return f.mode + " 3-9,13-19"
}

func (f *fakeBackend) SetMode(mode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "mode "+mode)
	if f.setErr == nil {
		f.mode = mode
	}
	return f.setErr
}

func (f *fakeBackend) Reload() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "reload")
	return f.reloadErr
}

func (f *fakeBackend) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// --- helpers --------------------------------------------------------------

// serve starts ServeIPC on a socket in t.TempDir() and waits until it answers.
func serve(t *testing.T, b ipcBackend) (string, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.sock")
	return path, serveAt(t, b, path)
}

func serveAt(t *testing.T, b ipcBackend, path string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- ServeIPC(ctx, b, path) }()
	waitReady(t, path)
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-errc:
			if err != nil {
				t.Errorf("ServeIPC returned %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("ServeIPC did not return after ctx cancel")
		}
	}
	t.Cleanup(stop)
	return stop
}

func waitReady(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("socket %s never became ready", path)
}

func run(t *testing.T, path string, args ...string) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	code := RunIPCClient(path, args, &buf)
	return code, buf.String()
}

// rawRequest speaks the wire protocol directly, bypassing the client's own
// argument validation.
func rawRequest(t *testing.T, path, req string) (status, body string) {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(c); err != nil {
		t.Fatalf("read: %v", err)
	}
	s, rest, _ := strings.Cut(strings.TrimRight(buf.String(), "\n"), "\n")
	return s, rest
}

// --- tests ----------------------------------------------------------------

func TestSocketPath(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got, want := SocketPath(), "/run/user/1000/ccorral.sock"; got != want {
		t.Errorf("SocketPath() = %q, want %q", got, want)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	got := SocketPath()
	if !strings.HasPrefix(got, os.TempDir()) || !strings.HasSuffix(got, ".sock") ||
		!strings.Contains(got, "ccorral-") {
		t.Errorf("SocketPath() fallback = %q, want <tmp>/ccorral-<uid>.sock", got)
	}
}

func TestSocketPathUnderTempRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path := SocketPath()
	serveAt(t, &fakeBackend{}, path)
	if code, out := run(t, path, "status"); code != 0 || out != "yellow 3-9,13-19\n" {
		t.Errorf("exit %d output %q", code, out)
	}
}

func TestStatus(t *testing.T) {
	path, _ := serve(t, &fakeBackend{mode: "red"})
	code, out := run(t, path, "status")
	if code != 0 || out != "red 3-9,13-19\n" {
		t.Errorf("exit %d output %q, want 0 and %q", code, out, "red 3-9,13-19\n")
	}
}

func TestModeRoundTrip(t *testing.T) {
	b := &fakeBackend{}
	path, _ := serve(t, b)
	for _, m := range []string{"green", "yellow", "red"} {
		code, out := run(t, path, "mode", m)
		if code != 0 || out != m+" 3-9,13-19\n" {
			t.Errorf("mode %s: exit %d output %q", m, code, out)
		}
	}
	want := []string{"mode green", "mode yellow", "mode red"}
	if got := b.callLog(); !reflect.DeepEqual(got, want) {
		t.Errorf("backend calls = %v, want %v", got, want)
	}
}

func TestSetModeErrorPassesThroughVerbatim(t *testing.T) {
	const msg = "cannot write drop-in:\npermission denied"
	b := &fakeBackend{setErr: errors.New(msg)}
	path, _ := serve(t, b)
	code, out := run(t, path, "mode", "red")
	if code != 1 || out != msg+"\n" {
		t.Errorf("exit %d output %q, want 1 and %q", code, out, msg+"\n")
	}
}

func TestReload(t *testing.T) {
	b := &fakeBackend{}
	path, _ := serve(t, b)
	code, out := run(t, path, "reload")
	if code != 0 || !strings.Contains(out, "reloaded") {
		t.Errorf("reload: exit %d, output %q", code, out)
	}
	if got := b.callLog(); !reflect.DeepEqual(got, []string{"reload"}) {
		t.Errorf("backend calls = %v", got)
	}

	b.mu.Lock()
	b.reloadErr = errors.New("bad config")
	b.mu.Unlock()
	code, out = run(t, path, "reload")
	if code != 1 || out != "bad config\n" {
		t.Errorf("failing reload: exit %d output %q", code, out)
	}
}

func TestUnknownCommandAndBadMode(t *testing.T) {
	b := &fakeBackend{}
	path, _ := serve(t, b)
	for _, c := range []struct{ req, want string }{
		{"frobnicate\n", "unknown command"},
		{"mic toggle\n", "unknown command"},
		{"mode\n", "bad mode"},
		{"mode purple\n", "bad mode"},
		{"mode green extra\n", "bad mode"},
		{"status now\n", "no arguments"},
		{"reload now\n", "no arguments"},
		{"\n", "empty request"},
		{"   \n", "empty request"},
		{"\x00\x01\x02 junk\n", "unknown command"},
	} {
		status, body := rawRequest(t, path, c.req)
		if status != "err" || !strings.Contains(body, c.want) {
			t.Errorf("request %q: status %q body %q, want err containing %q", c.req, status, body, c.want)
		}
	}
	if got := b.callLog(); len(got) != 0 {
		t.Errorf("backend was called for bad requests: %v", got)
	}
	if code, out := run(t, path, "status"); code != 0 {
		t.Errorf("daemon broken after garbage: exit %d, %q", code, out)
	}
}

func TestBadArgsExit2WithoutDialing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepts atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			c.Close()
		}
	}()

	for _, args := range [][]string{
		nil,
		{},
		{"status", "extra"},
		{"mode"},
		{"mode", "purple"},
		{"mode", "red", "now"},
		{"green"},
		{"reload", "now"},
		{"nose"},
		{"--help"},
	} {
		code, out := run(t, path, args...)
		if code != 2 {
			t.Errorf("args %v: exit %d, want 2", args, code)
		}
		if !strings.Contains(out, "ccorral status") {
			t.Errorf("args %v: output %q lacks usage", args, out)
		}
	}
	if n := accepts.Load(); n != 0 {
		t.Errorf("client dialed %d times on bad args, want 0", n)
	}
}

func TestDaemonNotRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.sock")
	code, out := run(t, path, "status")
	if code != 1 || out != ipcNotRunning+"\n" {
		t.Errorf("exit %d output %q, want 1 and %q", code, out, ipcNotRunning+"\n")
	}
}

func TestStaleSocketIsReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	// Bind and close without unlinking: a socket file with nobody behind it.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale socket not on disk: %v", err)
	}

	serveAt(t, &fakeBackend{}, path)
	if code, out := run(t, path, "status"); code != 0 || out != "yellow 3-9,13-19\n" {
		t.Errorf("exit %d output %q after replacing stale socket", code, out)
	}
}

func TestSecondServeIPCOnLiveSocketErrors(t *testing.T) {
	b := &fakeBackend{}
	path, _ := serve(t, b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := ServeIPC(ctx, b, path)
	if err == nil {
		t.Fatal("second ServeIPC returned nil, want an already-running error")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error %q, want it to say already running", err)
	}
	if code, out := run(t, path, "status"); code != 0 {
		t.Errorf("first daemon broken after the failed second: exit %d, %q", code, out)
	}
}

func TestSocketModeIs0600(t *testing.T) {
	path, _ := serve(t, &fakeBackend{})
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode %o, want 600", perm)
	}
}

func TestCtxCancelRemovesSocket(t *testing.T) {
	path, stop := serve(t, &fakeBackend{})
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket missing while serving: %v", err)
	}
	stop()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("socket still present after ctx cancel: stat err = %v", err)
	}
}

func TestConcurrentRequests(t *testing.T) {
	path, _ := serve(t, &fakeBackend{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			args := []string{"status"}
			if i%2 == 1 {
				args = []string{"mode", "red"}
			}
			var buf bytes.Buffer
			if code := RunIPCClient(path, args, &buf); code != 0 {
				t.Errorf("goroutine %d: exit %d, output %q", i, code, buf.String())
			}
		}(i)
	}
	wg.Wait()
}

// --- regression tests (from ts6tray) --------------------------------------

// A client that never sends a newline must be rejected without the daemon
// buffering what it sent.
func TestOverlongRequestLineIsCappedAndRejected(t *testing.T) {
	b := &fakeBackend{}
	path, _ := serve(t, b)

	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))

	werr := make(chan error, 1)
	go func() {
		_, err := c.Write(bytes.Repeat([]byte("A"), 1<<20))
		werr <- err
	}()

	replied := make(chan string, 1)
	go func() {
		buf := new(bytes.Buffer)
		buf.ReadFrom(c)
		s, _, _ := strings.Cut(strings.TrimRight(buf.String(), "\n"), "\n")
		replied <- s
	}()

	select {
	case status := <-replied:
		if status != "err" {
			t.Errorf("first reply line %q, want \"err\"", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon never answered an unterminated request line")
	}
	<-werr

	if got := b.callLog(); len(got) != 0 {
		t.Errorf("backend was called for an unterminated request: %v", got)
	}
	if code, out := run(t, path, "status"); code != 0 {
		t.Errorf("daemon broken after overlong request: exit %d, %q", code, out)
	}
}

// A request line just under the cap is still served; one byte over is not.
func TestRequestLineAtCapBoundary(t *testing.T) {
	path, _ := serve(t, &fakeBackend{})

	req := "status" + strings.Repeat(" ", ipcMaxRequest-7) + "\n"
	if len(req) != ipcMaxRequest {
		t.Fatalf("test built a %d-byte request, want %d", len(req), ipcMaxRequest)
	}
	if status, body := rawRequest(t, path, req); status != "ok" {
		t.Errorf("exactly-at-cap request: status %q body %q, want ok", status, body)
	}
	if status, _ := rawRequest(t, path, "status"+strings.Repeat(" ", ipcMaxRequest-6)+"\n"); status != "err" {
		t.Errorf("over-cap request: status %q, want err", status)
	}
}

// A daemon that accepts and then closes without replying must not surface as
// `unexpected reply from daemon: ""`.
func TestDaemonClosesWithoutReplying(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var drain atomic.Bool
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if drain.Load() {
				bufio.NewReader(io.LimitReader(c, 4096)).ReadString('\n')
			}
			c.Close()
		}
	}()

	for _, d := range []bool{true, false} {
		drain.Store(d)
		code, out := run(t, path, "status")
		if code != 1 {
			t.Errorf("drain=%v: exit %d, want 1; output %q", d, code, out)
		}
		if strings.Contains(out, "unexpected reply") {
			t.Errorf("drain=%v: output %q reports an empty reply as unexpected", d, out)
		}
		if d && out != ipcClosedEarly+"\n" {
			t.Errorf("drain=%v: output %q, want %q", d, out, ipcClosedEarly+"\n")
		}
		if !d && out != ipcClosedEarly+"\n" && out != ipcNotRunning+"\n" {
			t.Errorf("drain=%v: output %q, want the closed-early or not-running message", d, out)
		}
	}
}

// The socket must never exist with looser-than-0600 permissions: it is created
// under a 0177 umask, and the caller's umask is restored.
func TestSocketIsCreated0600UnderLooseUmask(t *testing.T) {
	old := syscall.Umask(0o000)
	defer syscall.Umask(old)

	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := ipcListen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode %o under umask 000, want 600", perm)
	}
	if got := syscall.Umask(0o000); got != 0o000 {
		t.Errorf("umask after ipcListen is %o, want the 000 it was called with", got)
	}
}

// Shutdown must unlink only the socket this daemon bound: a successor that
// rebinds the same path afterwards keeps its own socket.
func TestShutdownDoesNotUnlinkSuccessorSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	stop := serveAt(t, &fakeBackend{}, path)
	stop()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("first daemon left %s behind: stat err = %v", path, err)
	}

	serveAt(t, &fakeBackend{}, path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("successor socket missing: %v", err)
	}
	if code, out := run(t, path, "status"); code != 0 {
		t.Errorf("successor daemon not answering: exit %d, %q", code, out)
	}
}
