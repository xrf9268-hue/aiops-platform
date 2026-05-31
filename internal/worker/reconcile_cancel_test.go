package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// summaryThenCancelRunner writes the agent's RUN_SUMMARY.md (as a successful
// handoff would) and then returns context.Canceled, simulating the agent being
// cut off by an eligibility reconcile-cancel after it finished its work.
type summaryThenCancelRunner struct{ summary string }

func (r summaryThenCancelRunner) Run(_ context.Context, in runner.RunInput) (runner.Result, error) {
	dir := filepath.Join(in.Workdir, ".aiops")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return runner.Result{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "RUN_SUMMARY.md"), []byte(r.summary), 0o644); err != nil {
		return runner.Result{}, err
	}
	return runner.Result{}, context.Canceled
}

// TestRunAgentPreservesRunSummaryOnReconcileCancel pins the #543 headline fix:
// on an eligibility reconcile-cancel the worker must NOT overwrite the agent's
// RUN_SUMMARY.md with a "runner failed" artifact (the artifact-skip wiring, a
// separate path from the event classification).
func TestRunAgentPreservesRunSummaryOnReconcileCancel(t *testing.T) {
	const agentSummary = "# Run summary\n\nagent handoff: opened draft PR, moved issue to In Review.\n"
	oldNew := newRunner
	newRunner = func(string) (runner.Runner, error) { return summaryThenCancelRunner{summary: agentSummary}, nil }
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
	got, err := os.ReadFile(filepath.Join(dir, ".aiops", "RUN_SUMMARY.md"))
	if err != nil {
		t.Fatalf("RUN_SUMMARY.md missing after reconcile cancel: %v", err)
	}
	if !strings.Contains(string(got), "agent handoff") {
		t.Errorf("RUN_SUMMARY.md = %q; want the agent's summary preserved", got)
	}
	if strings.Contains(string(got), "runner failed") {
		t.Errorf("RUN_SUMMARY.md was overwritten with a runner-failed artifact: %q", got)
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
