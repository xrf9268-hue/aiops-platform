package worker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

// TestClassifyRunnerError is the decision table that isolates the runner error
// taxonomy from RunRunnerWithTimeout's timeout/event orchestration (#886). Each
// row pins the terminal phase, the ordered event kinds, the exact discriminating
// payload fields, and the surfaced error for one error class; deleting or
// flipping any one branch in classifyRunnerError fails a row here.
//
// wantFields and wantAbsent are asserted on EVERY emitted event in the row, so
// the stall path's two events (stalled, runner_timeout) are both checked — that
// pins their shared budget payload as well as the ordering.
func TestClassifyRunnerError(t *testing.T) {
	t.Parallel()

	const (
		model   = "codex-app-server"
		elapsed = 70 * time.Millisecond
		budget  = time.Second
	)
	res := runner.Result{OutputBytes: 12, OutputHead: "head", OutputTail: "tail"}

	stall := &runner.StallError{Timeout: 30 * time.Millisecond, Elapsed: 60 * time.Millisecond}
	turn := &runner.TurnTimeoutError{Timeout: 30 * time.Millisecond, Elapsed: 60 * time.Millisecond}
	read := &runner.ReadTimeoutError{Timeout: 30 * time.Millisecond}
	outer := &runner.TimeoutError{Timeout: 30 * time.Millisecond, Elapsed: 60 * time.Millisecond}
	wrappedOuter := fmt.Errorf("agent layer: %w", outer)
	plain := errors.New("agent crashed")

	reconcileCtx, cancelReconcile := context.WithCancelCause(context.Background())
	cancelReconcile(ErrReconcileCancel) // the orchestrator's eligibility stop

	cases := []struct {
		name              string
		ctx               context.Context
		runErr            error
		runCtxErr         error
		wantPhase         task.RunAttemptPhase
		wantKinds         []string
		wantFields        map[string]any // exact payload values asserted on every event
		wantAbsent        []string       // payload keys that must NOT be present on any event
		wantMessage       string         // exact event message (constant-message classes); "" skips
		wantErrIs         error          // errors.Is target the surfaced error must satisfy
		wantTimeout       bool           // surfaced error must satisfy runner.IsTimeout
		wantWrapsDeadline bool           // surfaced error must be a synthesized *TimeoutError wrapping the deadline
	}{
		{
			name: "stall", ctx: context.Background(), runErr: stall,
			wantPhase:  task.PhaseStalled,
			wantKinds:  []string{task.EventStalled, task.EventRunnerTimeout},
			wantFields: map[string]any{"timeout_ms": int64(30), "elapsed_ms": int64(60)},
			wantAbsent: []string{"ok", "reason", "duration_ms"},
			wantErrIs:  stall,
		},
		{
			name: "turn_timeout", ctx: context.Background(), runErr: turn,
			wantPhase:  task.PhaseTimedOut,
			wantKinds:  []string{task.EventRunnerTimeout},
			wantFields: map[string]any{"timeout_ms": int64(30), "elapsed_ms": int64(60)},
			wantAbsent: []string{"ok", "reason", "duration_ms"},
			wantErrIs:  turn,
		},
		{
			// read_timeout has no Elapsed field of its own; elapsed_ms must be the
			// OUTER wall-clock elapsed, not a budget-derived value.
			name: "read_timeout", ctx: context.Background(), runErr: read,
			wantPhase:  task.PhaseTimedOut,
			wantKinds:  []string{task.EventRunnerTimeout},
			wantFields: map[string]any{"timeout_ms": int64(30), "elapsed_ms": int64(70)},
			wantAbsent: []string{"ok", "reason", "duration_ms"},
			wantErrIs:  read,
		},
		{
			name: "outer_timeout", ctx: context.Background(), runErr: outer,
			wantPhase:   task.PhaseTimedOut,
			wantKinds:   []string{task.EventRunnerTimeout},
			wantFields:  map[string]any{"timeout_ms": int64(30), "elapsed_ms": int64(60)},
			wantAbsent:  []string{"ok", "reason", "duration_ms"},
			wantErrIs:   outer,
			wantTimeout: true,
		},
		{
			// A *TimeoutError reaches the classifier wrapped by an outer layer:
			// the SURFACED error must stay the wrapper (errors.Is target = the
			// wrapper, not the inner te), while the payload uses the inner te's
			// budget/elapsed. Catches a regression that surfaces the unwrapped te.
			name: "wrapped_outer_timeout", ctx: context.Background(), runErr: wrappedOuter,
			wantPhase:   task.PhaseTimedOut,
			wantKinds:   []string{task.EventRunnerTimeout},
			wantFields:  map[string]any{"timeout_ms": int64(30), "elapsed_ms": int64(60)},
			wantAbsent:  []string{"ok", "reason", "duration_ms"},
			wantErrIs:   wrappedOuter,
			wantTimeout: true,
		},
		{
			// Bare deadline + the run context's own deadline → synthesize a
			// *TimeoutError{Timeout: budget, Elapsed: elapsed, Cause: deadline}.
			name: "bare_deadline_normalized", ctx: context.Background(),
			runErr: context.DeadlineExceeded, runCtxErr: context.DeadlineExceeded,
			wantPhase:         task.PhaseTimedOut,
			wantKinds:         []string{task.EventRunnerTimeout},
			wantFields:        map[string]any{"timeout_ms": int64(1000), "elapsed_ms": int64(70)},
			wantAbsent:        []string{"ok", "reason", "duration_ms"},
			wantErrIs:         context.DeadlineExceeded,
			wantTimeout:       true,
			wantWrapsDeadline: true,
		},
		{
			name: "reconcile_cancel", ctx: reconcileCtx, runErr: context.Canceled,
			wantPhase:   task.PhaseCanceledByReconciliation,
			wantKinds:   []string{task.EventRunnerStopped},
			wantFields:  map[string]any{"duration_ms": int64(70), "ok": true, "reason": "reconcile_ineligible"},
			wantAbsent:  []string{"timeout_ms", "elapsed_ms", "error"},
			wantMessage: "runner stopped: reconcile ineligible",
			wantErrIs:   context.Canceled,
		},
		{
			name: "plain_error", ctx: context.Background(), runErr: plain,
			wantPhase:   task.PhaseFailed,
			wantKinds:   []string{task.EventRunnerEnd},
			wantFields:  map[string]any{"duration_ms": int64(70), "ok": false, "error": plain.Error()},
			wantAbsent:  []string{"timeout_ms", "elapsed_ms", "reason"},
			wantMessage: "runner failed",
			wantErrIs:   plain,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyRunnerError(tc.ctx, tc.runErr, tc.runCtxErr, res, model, elapsed, budget)

			if got.phase != tc.wantPhase {
				t.Errorf("classifyRunnerError(%s) phase = %q; want %q", tc.name, got.phase, tc.wantPhase)
			}

			var gotKinds []string
			for _, e := range got.events {
				gotKinds = append(gotKinds, e.kind)
				assertEventPayload(t, tc.name, e, model, res, tc.wantFields, tc.wantAbsent, tc.wantMessage)
			}
			if !slices.Equal(gotKinds, tc.wantKinds) {
				t.Errorf("classifyRunnerError(%s) event kinds = %v; want %v", tc.name, gotKinds, tc.wantKinds)
			}

			if tc.wantErrIs != nil && !errors.Is(got.err, tc.wantErrIs) {
				t.Errorf("classifyRunnerError(%s) err = %v; want errors.Is target %v", tc.name, got.err, tc.wantErrIs)
			}
			if tc.wantTimeout && !runner.IsTimeout(got.err) {
				t.Errorf("classifyRunnerError(%s) err = %v; want runner.IsTimeout", tc.name, got.err)
			}
			if tc.wantWrapsDeadline {
				var te *runner.TimeoutError
				if !errors.As(got.err, &te) {
					t.Errorf("classifyRunnerError(%s) surfaced %T; want a synthesized *runner.TimeoutError", tc.name, got.err)
				} else if !errors.Is(te.Cause, context.DeadlineExceeded) {
					t.Errorf("classifyRunnerError(%s) TimeoutError.Cause = %v; want it to wrap the bare deadline", tc.name, te.Cause)
				}
			}
		})
	}
}

// assertEventPayload pins one emitted event's message and payload: the shared
// model + output telemetry, every exact field in wantFields, the absence of
// every key in wantAbsent, and the exact message when wantMessage is set.
func assertEventPayload(t *testing.T, name string, e terminalRunnerEvent, model string, res runner.Result, wantFields map[string]any, wantAbsent []string, wantMessage string) {
	t.Helper()
	if e.message == "" {
		t.Errorf("classifyRunnerError(%s) emitted empty message for kind %q", name, e.kind)
	}
	if wantMessage != "" && e.message != wantMessage {
		t.Errorf("classifyRunnerError(%s) %s message = %q; want %q", name, e.kind, e.message, wantMessage)
	}
	if e.payload["model"] != model {
		t.Errorf("classifyRunnerError(%s) %s payload[model] = %v; want %q", name, e.kind, e.payload["model"], model)
	}
	// addOutputFields must thread the runner telemetry into every terminal payload.
	if e.payload["output_bytes"] != res.OutputBytes {
		t.Errorf("classifyRunnerError(%s) %s payload[output_bytes] = %v; want %v", name, e.kind, e.payload["output_bytes"], res.OutputBytes)
	}
	if e.payload["output_head"] != res.OutputHead {
		t.Errorf("classifyRunnerError(%s) %s payload[output_head] = %v; want %q", name, e.kind, e.payload["output_head"], res.OutputHead)
	}
	for k, want := range wantFields {
		if got := e.payload[k]; got != want {
			t.Errorf("classifyRunnerError(%s) %s payload[%s] = %#v; want %#v", name, e.kind, k, got, want)
		}
	}
	for _, k := range wantAbsent {
		if _, present := e.payload[k]; present {
			t.Errorf("classifyRunnerError(%s) %s payload has unexpected %s = %#v", name, e.kind, k, e.payload[k])
		}
	}
}
