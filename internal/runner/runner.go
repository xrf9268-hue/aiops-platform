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

	// RefreshIssueState, when non-nil, implements SPEC §16.5's per-turn
	// tracker refresh: the runner calls it after each agent turn finishes
	// and exits cleanly when the issue is no longer in the workflow's
	// active states. Without it the runner falls back to the agent-driven
	// `continue` notification flag and operator-cancel only becomes
	// visible at the next orchestrator poll tick. Nil keeps the legacy
	// behavior for callers (tests, mock runners) that have no tracker.
	RefreshIssueState IssueStateRefresher
}

// IssueStateRefresher is the SPEC §16.5 per-turn tracker hook. The runner
// invokes it after `awaitTurnCompletion` succeeds; the returned bool reports
// whether the linked issue is still in an active workflow state. A non-nil
// error short-circuits the turn loop with the wrapped error (SPEC: "if
// refreshed_issue failed: fail"). Defined as a function value rather than an
// interface so callers can adapt any tracker client (or test fake) without
// pulling internal/tracker into the runner package.
type IssueStateRefresher func(ctx context.Context) (active bool, err error)

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

// InputRequiredError marks a runner exit caused by the coding agent requesting
// operator input that this non-interactive harness cannot provide.
type InputRequiredError struct {
	Method string
}

func (e *InputRequiredError) Error() string {
	if e == nil || e.Method == "" {
		return "input required"
	}
	return "input required: " + e.Method
}

func IsInputRequired(err error) bool {
	var input *InputRequiredError
	return errors.As(err, &input)
}

// NameCodexAppServer is the agent.default value selecting the SPEC §10.1
// codex app-server runner. Shared by New (which constructs the runner) and
// EnforcesMaxTurnsInternally (which the orchestrator consults to gate the
// continuation-spawn cap, see issue #216) so the two lists cannot drift.
const NameCodexAppServer = "codex-app-server"

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
	case NameCodexAppServer:
		return CodexAppServerRunner{}, nil
	case "claude":
		return ShellRunner{Name: "claude"}, nil
	default:
		return nil, fmt.Errorf("unknown runner: %s", name)
	}
}

// EnforcesMaxTurnsInternally reports whether the runner selected by name runs
// its own per-session turn loop bounded by agent.max_turns. SPEC §5.3.5 scopes
// max_turns to "within one worker session"; SPEC §7.1 leaves continuation
// worker spawns unbounded. The orchestrator uses this to decide whether to
// apply its own continuation-spawn cap: when the runner already enforces
// max_turns inside the session, the orchestrator must not reuse the same
// value as a cross-worker budget (see issue #216). Only the codex app-server
// runner runs an in-session turn loop today; the one-shot codex exec runner
// and the shell-based claude runner exit after a single turn, so the
// orchestrator-side cap remains their only spawn safety net.
func EnforcesMaxTurnsInternally(name string) bool {
	switch name {
	case NameCodexAppServer:
		return true
	default:
		return false
	}
}

// QuotaBackoffError marks a recoverable coding-agent quota/rate-limit
// condition. It is distinct from ordinary turn failures so orchestrators can
// hold the issue in retry/backoff without consuming the workflow's normal
// failure retry budget.
type QuotaBackoffError struct {
	Message    string
	RetryAfter time.Duration
}

func (e *QuotaBackoffError) Error() string {
	if e == nil {
		return ""
	}
	msg := e.Message
	if msg == "" {
		msg = "coding-agent quota exceeded"
	}
	if e.RetryAfter > 0 {
		return fmt.Sprintf("quota backoff: %s; retry after %s", msg, e.RetryAfter)
	}
	return "quota backoff: " + msg
}

func IsQuotaBackoff(err error) bool {
	var quota *QuotaBackoffError
	return errors.As(err, &quota)
}

type RunnerErrorCategory string

const (
	CategoryCodexNotFound       RunnerErrorCategory = "codex_not_found"
	CategoryInvalidWorkspaceCWD RunnerErrorCategory = "invalid_workspace_cwd"
	CategoryResponseTimeout     RunnerErrorCategory = "response_timeout"
	CategoryTurnTimeout         RunnerErrorCategory = "turn_timeout"
	CategoryPortExit            RunnerErrorCategory = "port_exit"
	CategoryResponseError       RunnerErrorCategory = "response_error"
	CategoryTurnFailed          RunnerErrorCategory = "turn_failed"
	CategoryTurnCancelled       RunnerErrorCategory = "turn_cancelled"
	CategoryTurnInputRequired   RunnerErrorCategory = "turn_input_required"
)

var (
	ErrCodexNotFound       = &Error{Category: CategoryCodexNotFound}
	ErrInvalidWorkspaceCWD = &Error{Category: CategoryInvalidWorkspaceCWD}
	ErrResponseTimeout     = &Error{Category: CategoryResponseTimeout}
	ErrTurnTimeout         = &Error{Category: CategoryTurnTimeout}
	ErrPortExit            = &Error{Category: CategoryPortExit}
	ErrResponseError       = &Error{Category: CategoryResponseError}
	ErrTurnFailed          = &Error{Category: CategoryTurnFailed}
	ErrTurnCancelled       = &Error{Category: CategoryTurnCancelled}
	ErrTurnInputRequired   = &Error{Category: CategoryTurnInputRequired}
)

type Error struct {
	Category RunnerErrorCategory
	Message  string
	Err      error
}

func NewError(category RunnerErrorCategory, message string, err error) *Error {
	return &Error{Category: category, Message: message, Err: err}
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	msg := e.Message
	if msg == "" {
		msg = string(e.Category)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *Error) Is(target error) bool {
	return runnerCategoryMatches(e.category(), target)
}

func (e *Error) category() RunnerErrorCategory {
	if e == nil {
		return ""
	}
	return e.Category
}

type runnerCategorized interface {
	category() RunnerErrorCategory
}

func ErrorCategory(err error) (RunnerErrorCategory, bool) {
	var categorizedErr runnerCategorized
	if errors.As(err, &categorizedErr) {
		category := categorizedErr.category()
		return category, category != ""
	}
	return "", false
}

func runnerCategoryMatches(category RunnerErrorCategory, target error) bool {
	if category == "" || target == nil {
		return false
	}
	var targetCategory runnerCategorized
	return errors.As(target, &targetCategory) && targetCategory.category() == category
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

func (e *TurnTimeoutError) Is(target error) bool {
	return runnerCategoryMatches(e.category(), target)
}

func (e *TurnTimeoutError) category() RunnerErrorCategory {
	return CategoryTurnTimeout
}

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

func (e *ReadTimeoutError) Is(target error) bool {
	return runnerCategoryMatches(e.category(), target)
}

func (e *ReadTimeoutError) category() RunnerErrorCategory {
	return CategoryResponseTimeout
}

// IsReadTimeout reports whether err is (or wraps) a *ReadTimeoutError.
func IsReadTimeout(err error) bool {
	var te *ReadTimeoutError
	return errors.As(err, &te)
}
