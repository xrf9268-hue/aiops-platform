package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

type ShellRunner struct {
	Name string
}

// killGrace is how long the runner waits between SIGTERM and SIGKILL when
// the parent context is cancelled or its deadline elapses. We prefer a
// graceful shutdown so codex/claude can flush output, then fall back to
// SIGKILL to guarantee the worker does not block forever.
const killGrace = 5 * time.Second

func (r ShellRunner) Run(ctx context.Context, in RunInput) (Result, error) {
	command := ""
	switch r.Name {
	case "codex":
		command = in.Workflow.Config.Codex.Command
	case "claude":
		command = in.Workflow.Config.Claude.Command
	}
	if command == "" {
		return Result{}, fmt.Errorf("empty command for runner %s", r.Name)
	}

	start := time.Now()
	cmd := exec.CommandContext(ctx, "sh", "-lc", command+" < .aiops/PROMPT.md")
	cmd.Dir = in.Workdir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Wire platform-specific group-kill semantics. On Unix we put the
	// shell in its own process group so SIGTERM/SIGKILL reach
	// grandchildren too; on Windows this is a no-op and exec defaults
	// apply.
	configurePlatformKill(cmd)
	cmd.WaitDelay = killGrace

	err := cmd.Run()
	elapsed := time.Since(start)
	if err != nil {
		// Distinguish ctx-driven termination (timeout/cancel) from a
		// regular non-zero exit. A killed subprocess often surfaces as
		// `signal: terminated` or `signal: killed`; the authoritative
		// signal is ctx.Err().
		if cerr := ctx.Err(); errors.Is(cerr, context.DeadlineExceeded) {
			return Result{}, &TimeoutError{
				Timeout: deadlineBudget(ctx, start),
				Elapsed: elapsed,
				Cause:   err,
			}
		}
		return Result{}, err
	}
	return Result{Summary: r.Name + " completed"}, nil
}

// deadlineBudget reports the originally-configured timeout window. When
// the parent context has a deadline we report the gap between start and
// that deadline; otherwise we fall back to the elapsed runtime so the
// emitted event still carries a useful number.
func deadlineBudget(ctx context.Context, start time.Time) time.Duration {
	if d, ok := ctx.Deadline(); ok {
		if budget := d.Sub(start); budget > 0 {
			return budget
		}
	}
	return time.Since(start)
}
