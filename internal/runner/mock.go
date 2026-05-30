package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// MockRunner is the Symphony-style Day-1 runner used in tests and the
// `mock` agent path. Sleep, when > 0, makes the runner block until either
// it elapses or the context is cancelled — this is how tests drive the
// timeout path without spawning a real subprocess.
type MockRunner struct {
	Sleep               time.Duration
	WriteSourceFiles    bool
	SkipAnalysisPlan    bool
	WriteAiopsWorkflow  bool
	CommitSourceFiles   bool
	CommitOnlyArtifacts bool
	SetBaseToHead       bool
}

func (m MockRunner) Run(ctx context.Context, in RunInput) (Result, error) {
	if err := m.awaitMockSleep(ctx); err != nil {
		return Result{}, err
	}

	dir := filepath.Join(in.Workdir, ".aiops")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}
	completedAt := time.Now().Format(time.RFC3339)

	if err := m.writeAnalysisPlan(dir, in, completedAt); err != nil {
		return Result{}, err
	}
	if err := m.writeMockSourceFile(in.Workdir); err != nil {
		return Result{}, err
	}
	if err := m.commitMockChange(ctx, in.Workdir); err != nil {
		return Result{}, err
	}
	if err := m.setMockWorkspaceBase(ctx, in.Workdir); err != nil {
		return Result{}, err
	}
	if err := m.writeMockWorkflowEdit(dir); err != nil {
		return Result{}, err
	}
	if err := m.writeMockTaskArtifact(dir, in, completedAt); err != nil {
		return Result{}, err
	}
	if err := m.writeMockRunSummary(dir, in, completedAt); err != nil {
		return Result{}, err
	}
	return Result{Summary: "mock completed"}, nil
}

// awaitMockSleep blocks for m.Sleep (when > 0) so tests can drive the timeout
// path. It returns a *TimeoutError when the context deadline elapsed and the
// bare ctx.Err() for any other cancellation (e.g. context.Canceled), matching
// the worker's timeout-vs-cancel retry routing. The timer is always stopped on
// every return path.
func (m MockRunner) awaitMockSleep(ctx context.Context) error {
	if m.Sleep <= 0 {
		return nil
	}
	start := time.Now()
	t := time.NewTimer(m.Sleep)
	defer t.Stop()
	select {
	case <-ctx.Done():
		elapsed := time.Since(start)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &TimeoutError{
				Timeout: deadlineBudget(ctx, start),
				Elapsed: elapsed,
				Cause:   ctx.Err(),
			}
		}
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// writeAnalysisPlan writes .aiops/PLAN.md in analysis-only mode unless the
// mock is configured to skip it.
func (m MockRunner) writeAnalysisPlan(dir string, in RunInput, completedAt string) error {
	if in.Workflow.Config.Policy.Mode != "analysis_only" || m.SkipAnalysisPlan {
		return nil
	}
	plan := fmt.Sprintf(`# Analysis plan (mock)

- Task: %s
- Title: %s
- Actor: %s
- Model: %s
- Completed at: %s

This plan was produced by the mock runner in analysis-only mode. The mock
runner does not edit source files; it writes only analysis artifacts under
.aiops/ so the worker can exercise analysis-only gating without committing,
pushing, opening PRs, or writing tracker comments on the runner's behalf.
`, in.Task.ID, in.Task.Title, in.Task.Actor, in.Task.Model, completedAt)
	if err := os.WriteFile(filepath.Join(dir, "PLAN.md"), []byte(plan), 0o644); err != nil {
		return fmt.Errorf("write PLAN.md: %w", err)
	}
	return nil
}

// writeMockSourceFile writes the mock source-change file when the runner is
// configured to edit source (directly, or to commit a source change rather
// than an analysis-only artifact).
func (m MockRunner) writeMockSourceFile(workdir string) error {
	if m.WriteSourceFiles || (m.CommitSourceFiles && !m.CommitOnlyArtifacts) {
		if err := os.WriteFile(filepath.Join(workdir, "mock-source-change.txt"), []byte("mock source change\n"), 0o644); err != nil {
			return fmt.Errorf("write mock source change: %w", err)
		}
	}
	return nil
}

// commitMockChange adds and commits the mock change when configured. The
// committed path is the source file by default, or .aiops/PLAN.md when the
// runner commits only the analysis artifact.
func (m MockRunner) commitMockChange(ctx context.Context, workdir string) error {
	if !m.CommitSourceFiles {
		return nil
	}
	commitPath := "mock-source-change.txt"
	if m.CommitOnlyArtifacts {
		commitPath = filepath.Join(".aiops", "PLAN.md")
	}
	gitCommands := [][]string{
		{"git", "add", commitPath},
		{"git", "-c", "user.email=mock@example.com", "-c", "user.name=mock", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "commit", "-q", "-m", "mock source commit"},
	}
	for _, args := range gitCommands {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Dir = workdir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w\n%s", strings.Join(args, " "), err, out)
		}
	}
	return nil
}

// setMockWorkspaceBase records aiops.workspaceBase=HEAD in the repo's local
// git config when configured, simulating a runner that pins the workspace
// base after committing.
func (m MockRunner) setMockWorkspaceBase(ctx context.Context, workdir string) error {
	if !m.SetBaseToHead {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "config", "--local", "aiops.workspaceBase", "HEAD")
	cmd.Dir = workdir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git config aiops.workspaceBase: %w\n%s", err, out)
	}
	return nil
}

// writeMockWorkflowEdit writes a tracked .aiops/WORKFLOW.md edit when
// configured, exercising the worker's handling of a workflow-file change.
func (m MockRunner) writeMockWorkflowEdit(dir string) error {
	if !m.WriteAiopsWorkflow {
		return nil
	}
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte("tracked workflow edit\n"), 0o644); err != nil {
		return fmt.Errorf("write WORKFLOW.md: %w", err)
	}
	return nil
}

// writeMockTaskArtifact writes the per-task .aiops/<task>.md file outside
// analysis-only mode.
func (m MockRunner) writeMockTaskArtifact(dir string, in RunInput, completedAt string) error {
	if in.Workflow.Config.Policy.Mode == "analysis_only" {
		return nil
	}
	content := fmt.Sprintf(`# AI Ops Mock Run

Task: %s
Title: %s
Actor: %s
Model: %s
Completed at: %s

This file is generated by the Symphony-style Day 1 mock runner.

Prompt was written to .aiops/PROMPT.md.
`, in.Task.ID, in.Task.Title, in.Task.Actor, in.Task.Model, completedAt)
	if err := os.WriteFile(filepath.Join(dir, in.Task.ID+".md"), []byte(content), 0o644); err != nil {
		return err
	}
	return nil
}

// writeMockRunSummary writes the RUN_SUMMARY.md artifact the worker requires
// before opening a PR. The mock runner produces a minimal but recognisably
// non-stub summary so the worker's CheckSummary gate accepts it.
func (m MockRunner) writeMockRunSummary(dir string, in RunInput, completedAt string) error {
	summary := fmt.Sprintf(`# Run summary (mock)

- Task: %s
- Title: %s
- Actor: %s
- Model: %s
- Completed at: %s

This summary was produced by the mock runner. The mock runner does not edit
source files; it writes only its own .aiops/<task>.md and this RUN_SUMMARY.md
so end-to-end gating logic (worker requires RUN_SUMMARY.md before opening a
PR) can be exercised in tests and local dev.
`, in.Task.ID, in.Task.Title, in.Task.Actor, in.Task.Model, completedAt)
	if err := os.WriteFile(filepath.Join(dir, "RUN_SUMMARY.md"), []byte(summary), 0o644); err != nil {
		return fmt.Errorf("write RUN_SUMMARY.md: %w", err)
	}
	return nil
}
