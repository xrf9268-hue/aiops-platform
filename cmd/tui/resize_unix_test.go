//go:build unix

package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// lockedBuffer is a goroutine-safe io.Writer so the test can read frames while
// run() writes them from its own goroutine (keeps -race quiet).
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A SIGWINCH must reflow the dashboard by redrawing the last snapshot — without
// issuing another fetch — so a live terminal resize is reflected immediately.
func TestRun_RedrawsOnSIGWINCH(t *testing.T) {
	lb := &lockedBuffer{}
	scr := newScreen(lb, true /* isTTY */, false /* raw */)

	var fetches int32
	fetch := func(context.Context) *stateResponse {
		atomic.AddInt32(&fetches, 1)
		return &stateResponse{MaxConcurrentAgents: 3}
	}
	fetchState := func(ctx context.Context) (*stateResponse, error) { return fetch(ctx), ctx.Err() }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// A long interval keeps the ticker silent: the only frames are the
		// initial poll and the SIGWINCH-driven redraw.
		run(ctx, scr, fetchState, time.Hour, "http://example")
		close(done)
	}()

	frames := func() int { return strings.Count(lb.String(), "AIOPS STATUS") }
	waitUntil(t, "initial frame", func() bool { return frames() >= 1 })
	before := frames()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatalf("raise SIGWINCH: %v", err)
	}
	waitUntil(t, "redraw after SIGWINCH", func() bool { return frames() > before })

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancellation")
	}

	if got := atomic.LoadInt32(&fetches); got != 1 {
		t.Errorf("fetch called %d times, want 1 (SIGWINCH redraw must not refetch)", got)
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
