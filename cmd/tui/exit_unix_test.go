//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
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

	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("subprocess returned %v, want non-zero termination", err)
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		if ws.Signal() != syscall.SIGINT {
			t.Errorf("terminated by signal %v, want SIGINT", ws.Signal())
		}
		return // killed by the re-raised SIGINT — correct path
	}
	if want := 128 + int(syscall.SIGINT); ee.ExitCode() != want {
		t.Errorf("exit code = %d, want %d (128+SIGINT) or death by SIGINT", ee.ExitCode(), want)
	}
}
