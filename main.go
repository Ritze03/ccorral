// Command ccorral pins Claude Code sessions, and everything they spawn, to a
// set of CPUs by setting AllowedCPUs on the systemd user slice claude.slice.
// The daemon applies the mode and keeps escaped Claude processes inside the
// slice; the other subcommands are a thin CLI that drives it over a unix socket.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

const usage = `ccorral - pin Claude Code to a CPU set

usage:
  ccorral                       show the mode and cores
  ccorral green|yellow|red      switch mode (green = all CPUs)
  ccorral reload                re-read the config and re-apply it
  ccorral daemon                run the daemon (systemd user unit ccorral.service)
  ccorral install               install the binary and the systemd user unit
  ccorral uninstall             remove them again`

const (
	sweepInterval = 5 * time.Second
	// ipcBindWait covers ServeIPC's worst case for refusing to start: the
	// liveness probe of an existing socket (ipcProbeTimeout) plus slack.
	ipcBindWait      = ipcProbeTimeout + 250*time.Millisecond
	ipcShutdownGrace = 3 * time.Second
)

func main() {
	log.SetFlags(0)

	if len(os.Args) < 2 {
		os.Exit(RunIPCClient(SocketPath(), []string{"status"}, os.Stdout))
	}
	switch os.Args[1] {
	case "green", "yellow", "red":
		os.Exit(RunIPCClient(SocketPath(), []string{"mode", os.Args[1]}, os.Stdout))
	case "reload":
		os.Exit(RunIPCClient(SocketPath(), []string{"reload"}, os.Stdout))
	case "daemon":
		os.Exit(runDaemon())
	case "install":
		os.Exit(installMain())
	case "uninstall":
		os.Exit(uninstallMain())
	default:
		fmt.Println(usage)
		os.Exit(2)
	}
}

// runDaemon serves the IPC socket, applies the saved mode and sweeps until
// SIGINT/SIGTERM. Only a failure to bind the socket is fatal.
func runDaemon() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	b := newSliceBackend()

	// ServeIPC blocks, and a bind failure (a second daemon: "already running")
	// is returned from the same call, so it runs in a goroutine and nothing
	// else starts until it has had time to either fail or be serving.
	var ipcFailed atomic.Bool
	ipcDone := make(chan struct{})
	go func() {
		defer close(ipcDone)
		if err := ServeIPC(ctx, b, SocketPath()); err != nil {
			log.Printf("ccorral: %v", err)
			ipcFailed.Store(true)
			stop()
		}
	}()
	select {
	case <-ipcDone:
		if ipcFailed.Load() {
			return 1
		}
		return 0
	case <-time.After(ipcBindWait):
	}

	if err := b.Start(); err != nil {
		log.Printf("ccorral: applying saved mode: %v", err)
	}

	sweepLoop(ctx)

	// Let an in-flight IPC request finish and the socket get unlinked.
	select {
	case <-ipcDone:
	case <-time.After(ipcShutdownGrace):
		log.Printf("ccorral: IPC did not shut down within %s", ipcShutdownGrace)
	}
	if ipcFailed.Load() {
		return 1
	}
	return 0
}

// sweepLoop sweeps at once, then every sweepInterval until ctx is done.
func sweepLoop(ctx context.Context) {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		if err := sweepOnce(sweepConfig{Mover: sweepDBus{}}); err != nil {
			log.Printf("ccorral: sweep: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
