package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// errRunner returns a fixed error so a runState test can drive runAgent's
// failure classification without a subprocess.
type errRunner struct{ err error }

func (r errRunner) Run(_ context.Context, _ runner.RunInput) (runner.Result, error) {
	return runner.Result{}, r.err
}

func sandboxStartupRunState(t *testing.T, ev *fakeEmitter, runErr error) *runState {
	t.Helper()
	oldNew := newRunner
	newRunner = func(string) (runner.Runner, error) { return errRunner{err: runErr}, nil }
	t.Cleanup(func() { newRunner = oldNew })
	dir := t.TempDir()
	return &runState{
		ctx: context.Background(), ev: ev, t: task.Task{ID: "tsk", Model: "x"}, cfg: Config{},
		wf: &workflow.Workflow{}, wcfg: workflow.Config{},
		workdir: dir, workspaceRoot: filepath.Dir(dir),
	}
}

// TestRunAgentParksSandboxStartupFailureOnCooldown pins the #550 fix: a recurring
// codex sandbox-startup failure is routed to the external-blocker cooldown path
// (parked + re-dispatched after a backoff) instead of the hot failure-retry loop.
func TestRunAgentParksSandboxStartupFailureOnCooldown(t *testing.T) {
	ev := &fakeEmitter{}
	rs := sandboxStartupRunState(t, ev, &runner.SandboxStartupError{Detail: "host denied the codex bwrap user namespace"})

	rtErr := rs.runAgent()
	if rtErr == nil {
		t.Fatal("runAgent() = nil; want a RunTaskError for the sandbox-startup failure")
	}
	if !rtErr.ExternalBlocked {
		t.Fatalf("runAgent() ExternalBlocked = false; want true so the orchestrator parks it on a cooldown")
	}
	if !runner.IsSandboxStartup(rtErr.Err) {
		t.Fatalf("runAgent() Err = %T %[1]v; want a SandboxStartupError", rtErr.Err)
	}
	wantRetry := int(sandboxStartupRetryAfter / time.Second)
	if got := rtErr.Blocker.RetryAfterSeconds; got != wantRetry {
		t.Fatalf("Blocker.RetryAfterSeconds = %d; want %d (the sandbox cooldown)", got, wantRetry)
	}
	if got := (&ExternalBlockerError{Artifact: rtErr.Blocker}).RetryAfter(); got != sandboxStartupRetryAfter {
		t.Fatalf("RetryAfter() = %v; want %v", got, sandboxStartupRetryAfter)
	}

	// Observability: a dedicated event fires, distinct from a normal external
	// blocker so an operator can tell a host sandbox denial apart.
	if got := len(ev.byKind(task.EventSandboxStartupBlocked)); got != 1 {
		t.Fatalf("sandbox_startup_blocked events = %d; want 1; events=%#v", got, ev.events)
	}
	if got := len(ev.byKind(task.EventExternalBlocker)); got != 0 {
		t.Fatalf("external_blocker events = %d; want 0 (sandbox uses its own event); events=%#v", got, ev.events)
	}

	// A parked run is not a failure: no RUN_SUMMARY artifact is written.
	if _, err := os.Stat(filepath.Join(rs.workdir, ".aiops", "RUN_SUMMARY.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("RUN_SUMMARY.md stat error = %v; want not-exist (parked, not failed)", err)
	}
}

// TestRunAgentGenericFailureSkipsSandboxCooldown guards the negative: the routing
// must hinge on the typed error, so an ordinary turn failure keeps the standard
// (non-blocked) failure path and emits no sandbox event.
func TestRunAgentGenericFailureSkipsSandboxCooldown(t *testing.T) {
	ev := &fakeEmitter{}
	rs := sandboxStartupRunState(t, ev, runner.NewError(runner.CategoryTurnFailed, "turn/failed: tests failed", nil))

	rtErr := rs.runAgent()
	if rtErr == nil {
		t.Fatal("runAgent() = nil; want a RunTaskError for the turn failure")
	}
	if rtErr.ExternalBlocked {
		t.Fatalf("runAgent() ExternalBlocked = true; want false for a generic turn failure")
	}
	if got := len(ev.byKind(task.EventSandboxStartupBlocked)); got != 0 {
		t.Fatalf("sandbox_startup_blocked events = %d; want 0 for a generic failure", got)
	}
}
