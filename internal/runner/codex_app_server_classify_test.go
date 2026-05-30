package runner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// deadlineExceededCtx returns a context whose Err() is context.DeadlineExceeded,
// so the classifier's outer-deadline branch is exercised deterministically.
func deadlineExceededCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	t.Cleanup(cancel)
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("setup: ctx.Err() = %v; want context.DeadlineExceeded", ctx.Err())
	}
	return ctx
}

// readTimeoutRunErr returns the typed read-timeout signal isAppServerReadTimeout
// detects via errors.As — the same value (CodexAppServerRunner).Run propagates
// from readLineOnce.
func readTimeoutRunErr() error {
	return &appServerReadTimeoutError{afterMs: 5000}
}

// The classify tests exercise (CodexAppServerRunner).Run's error-classification
// tail directly. Four of these branches — outer-deadline-with-runErr,
// runErr+waitErr PortExit join, waitErr-only deadline, waitErr-only PortExit —
// were unreachable from the Run-level tests (they need a real subprocess to exit
// with a specific waitErr alongside a chosen runErr/ctx), so extracting
// classifyAppServerOutcome is what makes them testable. The ordering tests lock
// the load-bearing precedence the worker finalize path (internal/worker/
// runtask.go) depends on to pick the run's terminal phase.

func TestClassifyAppServerOutcome_SuccessPreservesResult(t *testing.T) {
	res := Result{Summary: "done", OutputBytes: 7, OutputTail: "tail"}
	got, err := classifyAppServerOutcome(context.Background(), res, nil, nil, 1000, time.Now(), time.Second)
	if err != nil {
		t.Fatalf("classifyAppServerOutcome(runErr=nil, waitErr=nil) err = %v; want nil", err)
	}
	if got.Summary != "done" || got.OutputBytes != 7 || got.OutputTail != "tail" {
		t.Fatalf("classifyAppServerOutcome success res = %+v; want telemetry preserved %+v", got, res)
	}
}

func TestClassifyAppServerOutcome_WaitErrOnlyIsPortExit(t *testing.T) {
	waitErr := errors.New("exit status 2")
	res := Result{OutputBytes: 3}
	got, err := classifyAppServerOutcome(context.Background(), res, nil, waitErr, 1000, time.Now(), time.Second)
	if cat, ok := ErrorCategory(err); !ok || cat != CategoryPortExit {
		t.Fatalf("classifyAppServerOutcome(runErr=nil, waitErr) category = %q ok=%v; want %q", cat, ok, CategoryPortExit)
	}
	if !errors.Is(err, waitErr) {
		t.Fatalf("classifyAppServerOutcome(runErr=nil, waitErr) err = %v; want it to wrap waitErr %v", err, waitErr)
	}
	if got.OutputBytes != 3 {
		t.Fatalf("classifyAppServerOutcome PortExit res = %+v; want telemetry preserved", got)
	}
}

func TestClassifyAppServerOutcome_WaitErrOnlyDeadlineIsTimeout(t *testing.T) {
	waitErr := errors.New("signal: killed")
	_, err := classifyAppServerOutcome(deadlineExceededCtx(t), Result{}, nil, waitErr, 1000, time.Now(), time.Second)
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("classifyAppServerOutcome(runErr=nil, waitErr, deadline) err = %v; want *TimeoutError", err)
	}
	if !errors.Is(te.Cause, waitErr) {
		t.Fatalf("TimeoutError.Cause = %v; want it to wrap waitErr %v", te.Cause, waitErr)
	}
}

func TestClassifyAppServerOutcome_StallReturnedAsIs(t *testing.T) {
	runErr := error(&StallError{Timeout: time.Second, Elapsed: 2 * time.Second})
	_, err := classifyAppServerOutcome(context.Background(), Result{}, runErr, nil, 1000, time.Now(), time.Second)
	// Passthrough happy path; the StallBeatsDeadline ordering test is the
	// non-placebo guard that the stall branch is reached before any wrapping.
	if !IsStall(err) || IsTimeout(err) || IsReadTimeout(err) {
		t.Fatalf("classifyAppServerOutcome(StallError) err = %v; want the StallError returned unwrapped", err)
	}
}

func TestClassifyAppServerOutcome_ReadTimeoutWrapsWithBudget(t *testing.T) {
	runErr := readTimeoutRunErr()
	_, err := classifyAppServerOutcome(context.Background(), Result{}, runErr, nil, 5000, time.Now(), time.Second)
	var rt *ReadTimeoutError
	if !errors.As(err, &rt) {
		t.Fatalf("classifyAppServerOutcome(read-timeout runErr) err = %v; want *ReadTimeoutError", err)
	}
	if rt.Timeout != 5000*time.Millisecond {
		t.Fatalf("ReadTimeoutError.Timeout = %v; want 5s from readTimeoutMs=5000", rt.Timeout)
	}
	if !errors.Is(rt.Cause, runErr) {
		t.Fatalf("ReadTimeoutError.Cause = %v; want it to wrap runErr %v", rt.Cause, runErr)
	}
}

func TestClassifyAppServerOutcome_DeadlineWithRunErrIsTimeout(t *testing.T) {
	runErr := errors.New("connection reset")
	_, err := classifyAppServerOutcome(deadlineExceededCtx(t), Result{}, runErr, nil, 1000, time.Now(), time.Second)
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("classifyAppServerOutcome(runErr, deadline) err = %v; want *TimeoutError", err)
	}
	if !errors.Is(te.Cause, runErr) {
		t.Fatalf("TimeoutError.Cause = %v; want it to wrap runErr %v", te.Cause, runErr)
	}
}

func TestClassifyAppServerOutcome_CategorizedReturnedAsIs(t *testing.T) {
	runErr := error(NewError(CategoryResponseError, "missing field", nil))
	_, err := classifyAppServerOutcome(context.Background(), Result{}, runErr, nil, 1000, time.Now(), time.Second)
	cat, ok := ErrorCategory(err)
	if !ok || cat != CategoryResponseError || IsTimeout(err) {
		t.Fatalf("classifyAppServerOutcome(categorized runErr) err = %v (category %q ok=%v); want it returned unwrapped", err, cat, ok)
	}
}

func TestClassifyAppServerOutcome_RunErrAndWaitErrJoinAsPortExit(t *testing.T) {
	runErr := errors.New("client loop failed")
	waitErr := errors.New("exit status 1")
	_, err := classifyAppServerOutcome(context.Background(), Result{}, runErr, waitErr, 1000, time.Now(), time.Second)
	if cat, ok := ErrorCategory(err); !ok || cat != CategoryPortExit {
		t.Fatalf("classifyAppServerOutcome(runErr, waitErr) category = %q ok=%v; want %q", cat, ok, CategoryPortExit)
	}
	if !errors.Is(err, runErr) || !errors.Is(err, waitErr) {
		t.Fatalf("classifyAppServerOutcome(runErr, waitErr) err = %v; want it to join both runErr and waitErr", err)
	}
}

func TestClassifyAppServerOutcome_BareRunErrReturnedAsIs(t *testing.T) {
	runErr := errors.New("opaque failure")
	_, err := classifyAppServerOutcome(context.Background(), Result{}, runErr, nil, 1000, time.Now(), time.Second)
	if !errors.Is(err, runErr) {
		t.Fatalf("classifyAppServerOutcome(bare runErr, no waitErr) err = %v; want runErr returned unwrapped %v", err, runErr)
	}
	if _, ok := ErrorCategory(err); ok || IsTimeout(err) || IsReadTimeout(err) {
		t.Fatalf("classifyAppServerOutcome(bare runErr) err = %v; want no category/timeout wrapping", err)
	}
}

// --- precedence/ordering locks ---

func TestClassifyAppServerOutcome_StallBeatsDeadline(t *testing.T) {
	runErr := error(&StallError{Timeout: time.Second})
	_, err := classifyAppServerOutcome(deadlineExceededCtx(t), Result{}, runErr, nil, 1000, time.Now(), time.Second)
	if !IsStall(err) || IsTimeout(err) {
		t.Fatalf("classifyAppServerOutcome(StallError, deadline) err = %v (IsStall=%v IsTimeout=%v); want StallError to win over the deadline branch", err, IsStall(err), IsTimeout(err))
	}
}

func TestClassifyAppServerOutcome_ReadTimeoutBeatsDeadline(t *testing.T) {
	runErr := readTimeoutRunErr()
	_, err := classifyAppServerOutcome(deadlineExceededCtx(t), Result{}, runErr, nil, 5000, time.Now(), time.Second)
	if !IsReadTimeout(err) {
		t.Fatalf("classifyAppServerOutcome(read-timeout runErr, deadline) err = %v; want *ReadTimeoutError to win over the deadline branch", err)
	}
	if IsTimeout(err) {
		t.Fatalf("classifyAppServerOutcome(read-timeout runErr, deadline) err = %v; must not be classified as *TimeoutError", err)
	}
}

func TestClassifyAppServerOutcome_DeadlineBeatsCategorized(t *testing.T) {
	runErr := error(NewError(CategoryResponseError, "missing field", nil))
	_, err := classifyAppServerOutcome(deadlineExceededCtx(t), Result{}, runErr, nil, 1000, time.Now(), time.Second)
	// The deadline branch precedes the ErrorCategory branch: a categorized
	// runErr that fires under an exceeded deadline is wrapped as *TimeoutError,
	// not returned bare. If the order flipped, err would be the bare categorized
	// error (not a TimeoutError).
	if !IsTimeout(err) {
		t.Fatalf("classifyAppServerOutcome(categorized runErr, deadline) err = %v; want the deadline branch to wrap it as *TimeoutError", err)
	}
}

func TestClassifyAppServerOutcome_CategorizedBeatsPortExit(t *testing.T) {
	runErr := error(NewError(CategoryResponseError, "missing field", nil))
	waitErr := errors.New("exit status 1")
	_, err := classifyAppServerOutcome(context.Background(), Result{}, runErr, waitErr, 1000, time.Now(), time.Second)
	// The ErrorCategory branch precedes the PortExit-on-waitErr branch: a
	// categorized runErr is returned as-is even when the process also exited
	// non-zero. If the order flipped, err would carry CategoryPortExit.
	if cat, ok := ErrorCategory(err); !ok || cat != CategoryResponseError {
		t.Fatalf("classifyAppServerOutcome(categorized runErr, waitErr) category = %q ok=%v; want %q (categorized wins over PortExit)", cat, ok, CategoryResponseError)
	}
}

// TestClassifyAppServerOutcome_TurnTimeoutUnderDeadlineIsTimeout pins the #507
// item-3 wontfix: when the outer run deadline has fired, a coinciding
// *TurnTimeoutError is reported as a *TimeoutError (the outer-deadline branch
// precedes the ErrorCategory branch that would otherwise return the categorized
// turn timeout as-is). This is a deliberate cosmetic precedence — the worker
// routes both to PhaseTimedOut (TestRunRunnerWithTimeoutEmitsTerminalErrorPhases)
// — so the lock guards against an accidental future flip, not a bug.
func TestClassifyAppServerOutcome_TurnTimeoutUnderDeadlineIsTimeout(t *testing.T) {
	turnTimeout := &TurnTimeoutError{Timeout: 30 * time.Millisecond, Elapsed: 60 * time.Millisecond, Cause: context.DeadlineExceeded}
	_, err := classifyAppServerOutcome(deadlineExceededCtx(t), Result{}, error(turnTimeout), nil, 1000, time.Now(), time.Second)
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("classifyAppServerOutcome(*TurnTimeoutError, deadline) err = %T %[1]v; want outer *TimeoutError", err)
	}
	if !errors.Is(te.Cause, error(turnTimeout)) {
		t.Fatalf("TimeoutError.Cause = %v; want it to wrap the *TurnTimeoutError %v", te.Cause, turnTimeout)
	}
}

// TestIsAppServerReadTimeout_TypedNotSubstring proves the read-timeout signal is
// detected by type (errors.As), not by the old strings.Contains match: the typed
// error is detected through a %w wrap, while a lookalike string error carrying
// the same words is not.
func TestIsAppServerReadTimeout_TypedNotSubstring(t *testing.T) {
	typed := error(&appServerReadTimeoutError{afterMs: 5000})
	if !isAppServerReadTimeout(typed) {
		t.Fatalf("isAppServerReadTimeout(%v) = false; want true for the typed read-timeout error", typed)
	}
	wrapped := fmt.Errorf("codex app-server initialize: %w", typed)
	if !isAppServerReadTimeout(wrapped) {
		t.Fatalf("isAppServerReadTimeout(%v) = false; want true through a %%w wrap", wrapped)
	}
	lookalike := errors.New("codex app-server read timeout after 5s")
	if isAppServerReadTimeout(lookalike) {
		t.Fatalf("isAppServerReadTimeout(%v) = true; want false — detection must be typed, not substring", lookalike)
	}
}
