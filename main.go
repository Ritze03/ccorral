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
	"syscall"
	"time"
)

const usage = `ccorral - pin Claude Code to a CPU set

usage:
  ccorral [status]              show the mode and cores
  ccorral green|yellow|red      switch mode (green = all CPUs)
  ccorral reload                re-read the config and re-apply it
  ccorral settings              terminal UI: cores per mode, sweep interval
  ccorral daemon                run the daemon (systemd user unit ccorral.service)
  ccorral install               install the binary and the systemd user unit
  ccorral uninstall             remove them again`

const ipcShutdownGrace = 3 * time.Second

func main() {
	log.SetFlags(0)

	cmd := "status"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "-h", "--help", "help":
		fmt.Println(usage)
	case "status", "reload", "green", "yellow", "red":
		if len(os.Args) > 2 {
			fmt.Println(usage)
			os.Exit(2)
		}
		args := []string{cmd}
		if cmd != "status" && cmd != "reload" {
			args = []string{"mode", cmd}
		}
		os.Exit(RunIPCClient(SocketPath(), args, os.Stdout))
	case "settings":
		if len(os.Args) > 2 {
			fmt.Println(usage)
			os.Exit(2)
		}
		os.Exit(RunSettings(os.Stdin, os.Stdout))
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
// SIGINT/SIGTERM. Only a failure to bind the socket is fatal; the tray icon is
// best-effort.
func runDaemon() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	b := newSliceBackend()

	// The listen is the single-instance guard: fail before touching anything.
	ln, err := ipcListen(SocketPath())
	if err != nil {
		log.Printf("ccorral: %v", err)
		return 1
	}
	ipcDone := make(chan struct{})
	var ipcErr error // written before ipcDone closes
	go func() {
		defer close(ipcDone)
		if ipcErr = ServeIPC(ctx, b, ln); ipcErr != nil {
			log.Printf("ccorral: %v", ipcErr)
			stop()
		}
	}()

	// The tray is optional: it only logs on failure and never stops the daemon.
	go RunTray(ctx, b)

	// First sweep before Start: systemctl may be slow, and escaped sessions
	// should not wait for it.
	sweepAndLog()
	if err := b.Start(); err != nil {
		log.Printf("ccorral: applying saved mode: %v", err)
	}
	sweepLoop(ctx, b.Interval, b.IntervalChanged(), sweepAndLog)

	// Let an in-flight IPC request finish and the socket get unlinked.
	select {
	case <-ipcDone:
		if ipcErr != nil {
			return 1
		}
	case <-time.After(ipcShutdownGrace):
		log.Printf("ccorral: IPC did not shut down within %s", ipcShutdownGrace)
	}
	return 0
}

func sweepAndLog() {
	if err := sweepOnce(sweepConfig{Mover: sweepDBus{}}); err != nil {
		log.Printf("ccorral: sweep: %v", err)
	}
}

// sweepLoop calls sweep every interval() until ctx is done. A nudge (the
// interval changed) restarts the wait with the new value, without a sweep.
func sweepLoop(ctx context.Context, interval func() time.Duration, nudge <-chan struct{}, sweep func()) {
	t := time.NewTimer(interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		case <-nudge:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
		}
		t.Reset(interval())
	}
}
