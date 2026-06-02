package worker

import (
	"context"
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

// TestRunAgentSandboxStartupFailureRidesGenericFailurePath pins the #572
// behavior: the dedicated external-blocker cooldown is gone, so a codex
// sandbox-startup denial is now a generic retryable runner failure (it rides the
// SPEC §8.4 backoff like any other error). The run is NOT parked: a FAILURE.md
// post-mortem is written and its text is the output-free SandboxStartupError
// detail, so raw bwrap subprocess output (which may hold secrets) never leaks.
func TestRunAgentSandboxStartupFailureRidesGenericFailurePath(t *testing.T) {
	ev := &fakeEmitter{}
	rs := sandboxStartupRunState(t, ev, &runner.SandboxStartupError{Detail: "host denied the codex bwrap user namespace"})

	rtErr := rs.runAgent()
	if rtErr == nil {
		t.Fatal("runAgent() = nil; want a RunTaskError for the sandbox-startup failure")
	}
	if rtErr.NonRetryable {
		t.Fatalf("runAgent() NonRetryable = true; want false so it rides the §8.4 backoff like any failure")
	}
	if !runner.IsSandboxStartup(rtErr.Err) {
		t.Fatalf("runAgent() Err = %T %[1]v; want the *SandboxStartupError classification preserved", rtErr.Err)
	}

	// A generic failure writes the FAILURE.md post-mortem, and its content is the
	// fixed output-free detail — no raw bwrap output reaches the artifact.
	body, err := os.ReadFile(filepath.Join(rs.workdir, ".aiops", "FAILURE.md"))
	if err != nil {
		t.Fatalf("read FAILURE.md = %v; want the failure post-mortem written", err)
	}
	if !strings.Contains(string(body), "host denied the codex bwrap user namespace") {
		t.Fatalf("FAILURE.md = %q; want the output-free sandbox-startup detail", string(body))
	}
}
