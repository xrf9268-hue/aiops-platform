//go:build unix

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// raiseAfterSignal must terminate the process via the re-raised signal so the
// shell sees the conventional 128+signum status. It calls os.Exit / kills the
// process, so it is exercised in a subprocess: the child re-invokes this test
// with TUI_RERAISE_TEST=1, runs raiseAfterSignal(SIGINT), and is expected to die
// by SIGINT (or, via the fallback, exit 128+SIGINT) — never returning normally.
func TestRaiseAfterSignal_ExitsBySignal(t *testing.T) {
	if os.Getenv("TUI_RERAISE_TEST") == "1" {
		raiseAfterSignal(syscall.SIGINT)
		os.Exit(99) // unreachable if raiseAfterSignal terminates the process
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRaiseAfterSignal_ExitsBySignal$")
	cmd.Env = append(os.Environ(), "TUI_RERAISE_TEST=1")
	err := cmd.Run()

	assertExitBySIGINT(t, err)
}

// TestMain_SignalExitsBySignal exercises the full main() wiring end to end:
// NotifyContext cancellation → run returns → the sigCh snoop → raiseAfterSignal.
// It builds and runs the real binary (non-TTY, pointed at a closed port) and
// sends it SIGINT, asserting it terminates by signal / exit 130.
func TestMain_SignalExitsBySignal(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary; skipped in -short")
	}

	bin := filepath.Join(t.TempDir(), "tui")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build tui: %v", err)
	}

	// Closed port → fetches fail fast; io.Discard stdout → non-TTY mode.
	cmd := exec.Command(bin, "--url", "http://127.0.0.1:0", "--interval", "1s")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tui: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	time.Sleep(300 * time.Millisecond) // let it install handlers and enter the loop
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal tui: %v", err)
	}

	select {
	case err := <-done:
		assertExitBySIGINT(t, err)
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("process did not exit within 5s of SIGINT")
	}
}

// assertExitBySIGINT requires that err reflects termination by SIGINT or, via
// the os.Exit fallback, exit code 128+SIGINT.
func assertExitBySIGINT(t *testing.T, err error) {
	t.Helper()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("process returned %v, want non-zero termination", err)
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		if ws.Signal() != syscall.SIGINT {
			t.Errorf("terminated by signal %v, want SIGINT", ws.Signal())
		}
		return
	}
	if want := 128 + int(syscall.SIGINT); ee.ExitCode() != want {
		t.Errorf("exit code = %d, want %d (128+SIGINT) or death by SIGINT", ee.ExitCode(), want)
	}
}
