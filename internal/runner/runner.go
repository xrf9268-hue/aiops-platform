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
	Prompt   string
}

type Result struct {
	Summary string
}

type Runner interface {
	Run(ctx context.Context, in RunInput) (Result, error)
}

func New(name string) (Runner, error) {
	switch name {
	case "", "mock":
		return MockRunner{}, nil
	case "codex":
		return ShellRunner{Name: "codex"}, nil
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
