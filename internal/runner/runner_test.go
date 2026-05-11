package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// shellTestWorkdir creates a temp workdir with a stub .aiops/PROMPT.md
// so the ShellRunner's `< .aiops/PROMPT.md` redirection does not fail
// before the actual command runs (we care about the kill path, not
// the prompt plumbing).
func shellTestWorkdir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".aiops"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aiops", "PROMPT.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestMockRunnerTimeoutReturnsTimeoutError verifies that when the mock
// runner is asked to sleep longer than the parent context's deadline,
// it returns *TimeoutError (not a generic ctx.Err()) so worker retry
// policy can route it to the timeout-specific bucket.
func TestMockRunnerTimeoutReturnsTimeoutError(t *testing.T) {
	t.Parallel()
	r := MockRunner{Sleep: 5 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := r.Run(ctx, RunInput{
		Task:     task.Task{ID: "tsk_test", Model: "mock"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from timed-out runner, got nil")
	}
	if !IsTimeout(err) {
		t.Fatalf("expected TimeoutError, got %T: %v", err, err)
	}
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("errors.As to *TimeoutError failed: %v", err)
	}
	if te.Elapsed <= 0 {
		t.Fatalf("expected non-zero elapsed, got %v", te.Elapsed)
	}
	// We should have returned promptly when ctx fired, well before the
	// 5s sleep would have completed naturally.
	if elapsed >= 2*time.Second {
		t.Fatalf("runner did not honor ctx cancellation; elapsed=%v", elapsed)
	}
}

// TestMockRunnerNoTimeoutWhenSleepShort confirms the happy path: with
// adequate budget the mock runner returns Result without a TimeoutError.
func TestMockRunnerNoTimeoutWhenSleepShort(t *testing.T) {
	t.Parallel()
	r := MockRunner{Sleep: 5 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := r.Run(ctx, RunInput{
		Task:     task.Task{ID: "tsk_ok", Model: "mock"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Summary == "" {
		t.Fatal("expected non-empty Result.Summary on success")
	}
	if IsTimeout(err) {
		t.Fatal("IsTimeout should be false for nil error")
	}
}

// TestShellRunnerKillsRunawayProcess wires the real ShellRunner against
// a `sleep 30` command and asserts that a 50ms timeout actually kills
// the subprocess (i.e. ctx-driven SIGTERM/SIGKILL works end-to-end). The
// guard `time.Since(start) < 5s` would fail loudly if the kill path
// regressed and we waited the full sleep budget.
func TestShellRunnerKillsRunawayProcess(t *testing.T) {
	t.Parallel()
	wf := workflow.Workflow{Config: workflow.Config{
		Claude: workflow.CommandConfig{Command: "sleep 30"},
	}}
	r := ShellRunner{Name: "claude"}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := r.Run(ctx, RunInput{
		Task:     task.Task{ID: "tsk_shell"},
		Workflow: wf,
		Workdir:  shellTestWorkdir(t),
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from killed sh subprocess")
	}
	if !IsTimeout(err) {
		t.Fatalf("expected TimeoutError from shell runner, got %T: %v", err, err)
	}
	// Even with the SIGTERM->SIGKILL grace (5s) the wait must complete
	// well before sleep 30s would have. Allow generous slack for CI.
	if elapsed > 10*time.Second {
		t.Fatalf("shell runner did not kill subprocess promptly; elapsed=%v", elapsed)
	}
}

// TestShellRunnerNonTimeoutErrorNotMisclassified guarantees a runner
// that exits non-zero quickly (no ctx expiry) is *not* tagged as a
// TimeoutError — verify-vs-timeout retry routing depends on this.
func TestShellRunnerNonTimeoutErrorNotMisclassified(t *testing.T) {
	t.Parallel()
	wf := workflow.Workflow{Config: workflow.Config{
		Claude: workflow.CommandConfig{Command: "exit 3"},
	}}
	r := ShellRunner{Name: "claude"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := r.Run(ctx, RunInput{
		Task:     task.Task{ID: "tsk_nonzero"},
		Workflow: wf,
		Workdir:  shellTestWorkdir(t),
	})
	if err == nil {
		t.Fatal("expected non-zero exit error")
	}
	if IsTimeout(err) {
		t.Fatalf("non-zero exit must not be classified as timeout: %v", err)
	}
}

func TestIsTimeoutNilAndOther(t *testing.T) {
	t.Parallel()
	if IsTimeout(nil) {
		t.Fatal("IsTimeout(nil) should be false")
	}
	if IsTimeout(errors.New("boom")) {
		t.Fatal("IsTimeout on plain error should be false")
	}
	te := &TimeoutError{Timeout: time.Second, Elapsed: time.Second, Cause: errors.New("x")}
	if !IsTimeout(te) {
		t.Fatal("IsTimeout on *TimeoutError should be true")
	}
	wrapped := errors.Join(errors.New("ctx"), te)
	if !IsTimeout(wrapped) {
		t.Fatal("IsTimeout should unwrap joined errors")
	}
}
