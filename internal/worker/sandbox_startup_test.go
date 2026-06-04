package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

type captureInputRunner struct {
	input runner.RunInput
}

func (r *captureInputRunner) Run(_ context.Context, in runner.RunInput) (runner.Result, error) {
	r.input = in
	return runner.Result{Summary: "ok"}, nil
}

type resultRunner struct {
	result runner.Result
}

func (r resultRunner) Run(context.Context, runner.RunInput) (runner.Result, error) {
	return r.result, nil
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

func TestRunAgentPassesCleanTurnBudgetToRunner(t *testing.T) {
	ev := &fakeEmitter{}
	capture := &captureInputRunner{}
	oldNew := newRunner
	newRunner = func(string) (runner.Runner, error) { return capture, nil }
	t.Cleanup(func() { newRunner = oldNew })
	dir := t.TempDir()
	rs := &runState{
		ctx: context.Background(), ev: ev, t: task.Task{ID: "tsk", Model: "codex-app-server"}, cfg: Config{CleanTurnBudget: 4},
		wf: &workflow.Workflow{Config: workflow.Config{
			Agent: workflow.AgentConfig{MaxTurns: 20},
		}},
		wcfg: workflow.Config{
			Agent: workflow.AgentConfig{MaxTurns: 20},
		},
		workdir: dir, workspaceRoot: filepath.Dir(dir),
	}

	if rtErr := rs.runAgent(); rtErr != nil {
		t.Fatalf("runAgent: %v", rtErr.Err)
	}
	if got := capture.input.CleanTurnBudget; got != 4 {
		t.Fatalf("RunInput.CleanTurnBudget = %d; want 4", got)
	}
}

func TestRunAgentCapturesIssueExitStateResult(t *testing.T) {
	ev := &fakeEmitter{}
	oldNew := newRunner
	newRunner = func(string) (runner.Runner, error) {
		return resultRunner{result: runner.Result{Summary: "ok", IssueExitState: &runner.IssueStateSnapshot{
			Found:  true,
			State:  "In Review",
			Active: false,
		}}}, nil
	}
	t.Cleanup(func() { newRunner = oldNew })
	dir := t.TempDir()
	rs := &runState{
		ctx: context.Background(), ev: ev, t: task.Task{ID: "tsk", Model: "codex-app-server"}, cfg: Config{},
		wf: &workflow.Workflow{Config: workflow.Config{
			Agent: workflow.AgentConfig{MaxTurns: 20},
		}},
		wcfg: workflow.Config{
			Agent: workflow.AgentConfig{MaxTurns: 20},
		},
		workdir: dir, workspaceRoot: filepath.Dir(dir),
	}

	if rtErr := rs.runAgent(); rtErr != nil {
		t.Fatalf("runAgent: %v", rtErr.Err)
	}
	if rs.res.IssueExitState == nil || rs.res.IssueExitState.State != "In Review" {
		t.Fatalf("runAgent result IssueExitState = %+v, want In Review snapshot", rs.res.IssueExitState)
	}
}

// TestRunAgentSandboxStartupFailureRidesGenericFailurePath pins the #572
// behavior: the dedicated external-blocker cooldown is gone, so a codex
// sandbox-startup denial is now a generic retryable runner failure (it rides the
// SPEC §8.4 backoff like any other error). The failure reason survives via the
// SPEC §13.1/§13.2 structured `runner_end "runner failed"` event whose `error`
// payload is the output-free SandboxStartupError detail, so raw bwrap subprocess
// output (which may hold secrets) never leaks — and no .aiops/FAILURE.md
// post-mortem artifact is written (removed in #575).
func TestRunAgentSandboxStartupFailureRidesGenericFailurePath(t *testing.T) {
	ev := &fakeEmitter{}
	rs := sandboxStartupRunState(t, ev, &runner.SandboxStartupError{Detail: "host denied the codex bwrap user namespace"})

	rtErr := rs.runAgent()
	if rtErr == nil {
		t.Fatal("runAgent() = nil; want a RunTaskError for the sandbox-startup failure")
	}
	if !runner.IsSandboxStartup(rtErr.Err) {
		t.Fatalf("runAgent() Err = %T %[1]v; want the *SandboxStartupError classification preserved", rtErr.Err)
	}

	// The failure reason is carried by the structured runner_end event, and its
	// `error` payload is the fixed output-free detail — no raw bwrap output
	// reaches the event.
	ends := ev.byKind(task.EventRunnerEnd)
	var failed *recordedEvent
	for i := range ends {
		if ends[i].Message == "runner failed" {
			failed = &ends[i]
			break
		}
	}
	if failed == nil {
		t.Fatalf("no runner_end %q event among %d runner_end events; want the failure recorded structurally", "runner failed", len(ends))
	}
	payload, ok := failed.Payload.(map[string]any)
	if !ok {
		t.Fatalf("runner_end payload = %T; want map[string]any", failed.Payload)
	}
	gotErr, _ := payload["error"].(string)
	if !strings.Contains(gotErr, "host denied the codex bwrap user namespace") {
		t.Fatalf("runner_end error payload = %q; want the output-free sandbox-startup detail", gotErr)
	}

	// The .aiops/FAILURE.md post-mortem artifact must no longer be written (#575):
	// the structured event above is the sole source of truth for the reason.
	if _, err := os.Stat(filepath.Join(rs.workdir, ".aiops", "FAILURE.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("FAILURE.md stat = %v; want not-exist (the post-mortem artifact was removed in #575)", err)
	}
}
