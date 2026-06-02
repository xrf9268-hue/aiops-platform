package worker

import (
	"context"
	"errors"
)

// ErrReconcileCancel is the cancellation cause the orchestrator attaches when
// per-tick eligibility reconciliation stops a run whose tracker issue left the
// active set — e.g. the agent's own PR handoff moved the issue to In Review.
// The worker detects it via context.Cause so it classifies the stop as a
// superseded run (PhaseCanceledByReconciliation, runner_stopped) rather than a
// runner failure for a run that actually handed off (#543).
//
// It is deliberately distinct from a stall cancel or a shutdown cancel, which
// the orchestrator triggers without this cause and which keep their existing
// failure/cancel classification.
var ErrReconcileCancel = errors.New("run stopped: tracker issue left the active set")

// isReconcileCancel reports whether a failed run was stopped by an eligibility
// reconcile. The authoritative signal is the run context's cancellation CAUSE
// (ErrReconcileCancel), not the runner's error value: the codex app-server
// returns context.Canceled, but the claude ShellRunner surfaces a killed
// subprocess as a bare exec error ("signal: terminated"), so keying on
// context.Canceled would miss it. A genuine local timeout (DeadlineExceeded) is
// excluded — it is classified by the timeout branch — as is a nil error.
func isReconcileCancel(ctx context.Context, err error) bool {
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return errors.Is(context.Cause(ctx), ErrReconcileCancel)
}
