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

// TestResetStaleArtifactsClearsWorkerArtifacts pins the retired-artifact
// hygiene: stale untracked worker artifacts left on a reused workspace must not
// survive into the next run. PrepareGitWorkspace preserves untracked files on
// reuse, so a leftover FAILURE.md / CHANGED_FILES.txt (#575), RUN_SUMMARY.md
// (#561), VERIFICATION.txt (#560), or BLOCKED.json (#572) written by an older
// worker version would otherwise linger and could be committed by an agent that
// runs `git add -A`.
func TestResetStaleArtifactsClearsWorkerArtifacts(t *testing.T) {
	dir := t.TempDir()
	aiops := filepath.Join(dir, ".aiops")
	if err := os.MkdirAll(aiops, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := []string{"FAILURE.md", "CHANGED_FILES.txt", "RUN_SUMMARY.md", "VERIFICATION.txt", "BLOCKED.json"}
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

// TestResetStaleArtifactsClearsAnalysisPlan pins the Codex review fix on #574:
// in analysis-only mode .aiops/PLAN.md is the agent's handoff artifact, so a
// stale copy from a previous attempt on a reused workspace must be cleared
// before the run — otherwise it can be read as this run's assessment. Removing
// the post-turn analysis-only gate must not take this hygiene reset with it;
// this test fails if the reset is dropped again.
func TestResetStaleArtifactsClearsAnalysisPlan(t *testing.T) {
	dir := t.TempDir()
	aiops := filepath.Join(dir, ".aiops")
	if err := os.MkdirAll(aiops, 0o755); err != nil {
		t.Fatal(err)
	}
	plan := filepath.Join(aiops, "PLAN.md")
	if err := os.WriteFile(plan, []byte("stale assessment from a previous attempt\n"), 0o644); err != nil {
		t.Fatalf("seed stale PLAN.md: %v", err)
	}
	rs := &runState{workdir: dir, wcfg: workflow.Config{Policy: workflow.PolicyConfig{Mode: "analysis_only"}}}
	if rtErr := rs.resetStaleArtifacts(); rtErr != nil {
		t.Fatalf("resetStaleArtifacts() = %v; want nil", rtErr.Err)
	}
	if _, err := os.Stat(plan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("PLAN.md stat error = %v; want not-exist (a stale analysis-only handoff artifact must not survive a rerun)", err)
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
