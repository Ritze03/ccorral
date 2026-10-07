package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestSweepLoop(t *testing.T) {
	var ms atomic.Int64
	ms.Store(20)
	var n atomic.Int32
	nudge := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sweepLoop(ctx, func() time.Duration { return time.Duration(ms.Load()) * time.Millisecond },
			nudge, func() { n.Add(1) })
		close(done)
	}()

	time.Sleep(300 * time.Millisecond) // ~15 sweeps at 20 ms
	if got := n.Load(); got < 5 || got > 16 {
		t.Errorf("sweeps in 300 ms at 20 ms = %d, want ~15", got)
	}

	// The interval change applies at once, not after the current wait.
	ms.Store(200)
	nudge <- struct{}{}
	before := n.Load()
	time.Sleep(150 * time.Millisecond)
	if got := n.Load() - before; got > 1 {
		t.Errorf("%d sweeps within 150 ms after switching to 200 ms", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sweepLoop did not stop on ctx cancel")
	}
	stopped := n.Load()
	time.Sleep(250 * time.Millisecond)
	if n.Load() != stopped {
		t.Error("sweep ran after ctx cancel")
	}
}
