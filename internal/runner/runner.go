package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type RunInput struct {
	Task     task.Task
	Workflow workflow.Workflow
	Workdir  string
	// WorkspaceRoot is the runtime root that created Workdir. When set,
	// runner workspace-cwd invariant checks must use this value instead of
	// the workflow default so launch validation matches the worker's actual
	// checkout root.
	WorkspaceRoot string
	Prompt        string

	RuntimeEventSink    func(task.RuntimeEvent)
	PhaseTransitionSink func(from, to task.RunAttemptPhase)
}

type Result struct {
	Summary       string
	RuntimeEvents []task.RuntimeEvent
	OutputBytes   int64  // bytes the runner kept in its capture buffer
	OutputDropped int64  // bytes dropped because the buffer hit its cap
	OutputHead    string // up to CodexEventOutputCap bytes from the start of the captured output
	OutputTail    string // up to CodexEventOutputCap bytes from the end; empty when total <= head cap
}

type Runner interface {
	Run(ctx context.Context, in RunInput) (Result, error)
}

func New(name string) (Runner, error) {
	switch name {
	case "", "mock":
		return MockRunner{}, nil
	case "mock-source-change":
		return MockRunner{WriteSourceFiles: true}, nil
	case "mock-commit-source-change":
		return MockRunner{CommitSourceFiles: true}, nil
	case "mock-commit-analysis-artifact":
		return MockRunner{CommitSourceFiles: true, CommitOnlyArtifacts: true}, nil
	case "mock-commit-source-change-and-reset-base-config":
		return MockRunner{CommitSourceFiles: true, SetBaseToHead: true}, nil
	case "mock-no-plan":
		return MockRunner{SkipAnalysisPlan: true}, nil
	case "mock-aiops-workflow-change":
		return MockRunner{WriteAiopsWorkflow: true}, nil
	case "codex":
		return CodexRunner{}, nil
	case "codex-app-server":
		return CodexAppServerRunner{}, nil
	case "claude":
		return ShellRunner{Name: "claude"}, nil
	default:
		return nil, fmt.Errorf("unknown runner: %s", name)
	}
}

// TimeoutError is returned by Runner implementations when the parent
// context's deadline elapsed before the runner subprocess finished.
// Callers should treat this distinctly from a generic non-zero exit so
// retry policy can differentiate transient hangs from real failures.
type TimeoutError struct {
	// Timeout is the configured per-task budget.
	Timeout time.Duration
	// Elapsed is how long the runner actually ran before being killed.
	Elapsed time.Duration
	// Cause is the wrapped underlying error (typically a context
	// DeadlineExceeded or os/exec error from the killed subprocess).
	Cause error
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("runner timed out after %s (budget %s): %v", e.Elapsed, e.Timeout, e.Cause)
}

func (e *TimeoutError) Unwrap() error { return e.Cause }

// IsTimeout reports whether err is (or wraps) a *TimeoutError.
func IsTimeout(err error) bool {
	var te *TimeoutError
	return errors.As(err, &te)
}

// StallError is returned when a streaming runner remains alive but stops
// emitting events for longer than the configured inactivity budget.
type StallError struct {
	// Timeout is the configured inactivity budget.
	Timeout time.Duration
	// Elapsed is how long the runner was silent since the last event.
	Elapsed time.Duration
	// Cause is the wrapped underlying error, when available.
	Cause error
}

func (e *StallError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("runner stall timeout after %s without events (budget %s): %v", e.Elapsed, e.Timeout, e.Cause)
	}
	return fmt.Sprintf("runner stall timeout after %s without events (budget %s)", e.Elapsed, e.Timeout)
}

func (e *StallError) Unwrap() error { return e.Cause }

// IsStall reports whether err is (or wraps) a *StallError.
func IsStall(err error) bool {
	var se *StallError
	return errors.As(err, &se)
}

// TurnTimeoutError is returned when a single agent turn exceeds its configured
// per-turn budget while the outer run context remains alive.
type TurnTimeoutError struct {
	Timeout time.Duration
	Elapsed time.Duration
	Cause   error
}

func (e *TurnTimeoutError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("runner turn timeout after %s (budget %s): %v", e.Elapsed, e.Timeout, e.Cause)
	}
	return fmt.Sprintf("runner turn timeout after %s (budget %s)", e.Elapsed, e.Timeout)
}

func (e *TurnTimeoutError) Unwrap() error { return e.Cause }

// IsTurnTimeout reports whether err is (or wraps) a *TurnTimeoutError.
func IsTurnTimeout(err error) bool {
	var te *TurnTimeoutError
	return errors.As(err, &te)
}

// ReadTimeoutError is returned when a runner's event stream read exceeds the
// configured per-read transport budget outside a stall-governed turn.
type ReadTimeoutError struct {
	Timeout time.Duration
	Cause   error
}

func (e *ReadTimeoutError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("runner read timeout after %s: %v", e.Timeout, e.Cause)
	}
	return fmt.Sprintf("runner read timeout after %s", e.Timeout)
}

func (e *ReadTimeoutError) Unwrap() error { return e.Cause }

// IsReadTimeout reports whether err is (or wraps) a *ReadTimeoutError.
func IsReadTimeout(err error) bool {
	var te *ReadTimeoutError
	return errors.As(err, &te)
}
