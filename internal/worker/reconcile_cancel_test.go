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

// cancelRunner returns context.Canceled, simulating the agent being cut off by
// an eligibility reconcile-cancel after it finished its work (e.g. it already
// opened a draft PR and moved its issue to In Review).
type cancelRunner struct{}

func (cancelRunner) Run(_ context.Context, _ runner.RunInput) (runner.Result, error) {
	return runner.Result{}, context.Canceled
}

// TestRunAgentWritesNoFailureArtifactOnReconcileCancel pins the #543 fix: an
// eligibility reconcile-cancel is a supervised stop (the agent already handed
// off), not a runner failure, so the worker must NOT write a .aiops/FAILURE.md
// post-mortem for it. (Before #561 this asserted the agent's RUN_SUMMARY.md was
// preserved; that gate/artifact was removed, so the invariant is now expressed
// as "no FAILURE.md for a superseded run".)
func TestRunAgentWritesNoFailureArtifactOnReconcileCancel(t *testing.T) {
	oldNew := newRunner
	newRunner = func(string) (runner.Runner, error) { return cancelRunner{}, nil }
	t.Cleanup(func() { newRunner = oldNew })

	dir := t.TempDir()
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrReconcileCancel)
	rs := &runState{
		ctx: ctx, ev: &fakeEmitter{}, t: task.Task{ID: "tsk", Model: "x"}, cfg: Config{},
		wf: &workflow.Workflow{}, wcfg: workflow.Config{},
		workdir: dir, workspaceRoot: filepath.Dir(dir),
	}

	rtErr := rs.runAgent()
	if rtErr == nil || !errors.Is(rtErr.Err, context.Canceled) {
		t.Fatalf("runAgent() = %v; want a RunTaskError wrapping context.Canceled", rtErr)
	}
	if _, err := os.Stat(filepath.Join(dir, ".aiops", "FAILURE.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("FAILURE.md stat error = %v; want not-exist (a reconcile cancel is a supervised stop, not a failure)", err)
	}
}

// TestResetStaleArtifactsClearsWorkerArtifacts pins the #561 review fixes: stale
// untracked worker artifacts left on a reused workspace must not survive into
// the next run. PrepareGitWorkspace preserves untracked files on reuse, so
// without the reset (a) a stale FAILURE.md would be swept into CHANGED_FILES.txt
// and could be committed by an `git add -A` agent, and (b) a RUN_SUMMARY.md
// (#561) or VERIFICATION.txt (#560) left by an older worker version — no longer
// in AllowedHandoffArtifactPaths — would trip the analysis-only diff check.
func TestResetStaleArtifactsClearsWorkerArtifacts(t *testing.T) {
	dir := t.TempDir()
	aiops := filepath.Join(dir, ".aiops")
	if err := os.MkdirAll(aiops, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := []string{"FAILURE.md", "RUN_SUMMARY.md", "VERIFICATION.txt"}
	for _, name := range stale {
		if err := os.WriteFile(filepath.Join(aiops, name), []byte("left over from a previous attempt\n"), 0o644); err != nil {
			t.Fatalf("seed stale %s: %v", name, err)
		}
	}
	rs := &runState{workdir: dir, wcfg: workflow.Config{}}
	if rtErr := rs.resetStaleArtifacts(); rtErr != nil {
		t.Fatalf("resetStaleArtifacts() = %v; want nil", rtErr.Err)
	}
	for _, name := range stale {
		if _, err := os.Stat(filepath.Join(aiops, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s stat error = %v; want not-exist (a stale worker artifact must not survive a rerun)", name, err)
		}
	}
}

func TestIsReconcileCancel(t *testing.T) {
	reconcile := func() context.Context {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(ErrReconcileCancel)
		return ctx
	}
	genericCause := func() context.Context {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(nil) // shutdown-style cancel: no reconcile cause
		return ctx
	}
	plainCancel := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"reconcile cancel, codex context.Canceled", reconcile(), context.Canceled, true},
		{"reconcile cancel, claude shell killed error", reconcile(), errors.New("signal: terminated"), true},
		{"generic (shutdown) cancel", genericCause(), context.Canceled, false},
		{"plain cancel, no cause", plainCancel(), context.Canceled, false},
		{"deadline excluded even under reconcile ctx", reconcile(), context.DeadlineExceeded, false},
		{"nil error", reconcile(), nil, false},
		{"live ctx, canceled err but ctx not reconcile-canceled", context.Background(), context.Canceled, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isReconcileCancel(tt.ctx, tt.err); got != tt.want {
				t.Errorf("isReconcileCancel(%s) = %v; want %v", tt.name, got, tt.want)
			}
		})
	}
}

// TestRunRunnerWithTimeoutReconcileCancelRecordsStopped pins the #543 fix: an
// eligibility reconcile-cancel (ctx canceled with ErrReconcileCancel, runner
// returns context.Canceled) is recorded as a supervised stop (runner_stopped,
// ok=true, reason=reconcile_ineligible, phase CanceledByReconciliation), NOT as
// a runner failure (runner_end "runner failed").
func TestRunRunnerWithTimeoutReconcileCancelRecordsStopped(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrReconcileCancel) // the orchestrator's eligibility stop
	em := &fakeEmitter{}

	_, err := RunRunnerWithTimeout(ctx, em, runner.MockRunner{Sleep: time.Second},
		runner.RunInput{Task: task.Task{ID: "tsk", Model: "mock"}, Workflow: workflow.Workflow{}, Workdir: t.TempDir()},
		time.Minute, "test")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunRunnerWithTimeout() err = %v; want context.Canceled", err)
	}

	stopped := em.byKind(task.EventRunnerStopped)
	if len(stopped) != 1 {
		t.Fatalf("runner_stopped events = %d; want 1", len(stopped))
	}
	payload, ok := stopped[0].Payload.(map[string]any)
	if !ok || payload["ok"] != true || payload["reason"] != "reconcile_ineligible" {
		t.Errorf("runner_stopped payload = %#v; want ok=true reason=reconcile_ineligible", stopped[0].Payload)
	}
	for _, e := range em.byKind(task.EventRunnerEnd) {
		if e.Message == "runner failed" {
			t.Errorf("emitted runner_end %q for a reconcile cancel; want runner_stopped only", e.Message)
		}
	}
	sawPhase := false
	for _, e := range em.byKind(task.EventRunPhaseTransition) {
		if e.Message == string(task.PhaseCanceledByReconciliation) {
			sawPhase = true
		}
	}
	if !sawPhase {
		t.Errorf("no phase transition to %s among %d transitions", task.PhaseCanceledByReconciliation, len(em.byKind(task.EventRunPhaseTransition)))
	}
}
