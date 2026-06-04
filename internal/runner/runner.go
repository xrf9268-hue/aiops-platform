package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// CleanTurnBudget optionally caps completed turns for this run as a clean
	// budget stop. It never expands workflow.Config.Agent.MaxTurns; callers use
	// it to spend an issue-level budget without mutating the workflow snapshot.
	CleanTurnBudget int

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
// codex app-server runner. It runs its own per-session turn loop bounded by
// agent.max_turns (SPEC §5.3.5), optionally tightened per run by
// RunInput.CleanTurnBudget. The orchestrator uses that clean budget to enforce
// D34 / agent.max_continuation_turns before a session can overspend the issue
// budget.
const NameCodexAppServer = "codex-app-server"

func New(name string) (Runner, error) {
	switch name {
	case "", "mock":
		return MockRunner{}, nil
	case NameCodexAppServer:
		return CodexAppServerRunner{}, nil
	case "claude":
		return ShellRunner{Name: "claude"}, nil
	default:
		return nil, fmt.Errorf("unknown runner: %s", name)
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

// SandboxStartupError marks a run that failed because the coding agent's
// sandbox could not start — most commonly codex's bwrap user namespace being
// denied by the host (e.g. "bwrap: setting up uid map: Permission denied").
// It is re-tagged from the raw runner failure so Detail carries a fixed,
// output-free description: the raw captured subprocess text (which may hold
// secrets) never reaches an error string. The worker treats it as a generic
// retryable failure that rides the SPEC §8.4 backoff like any other runner
// error (the dedicated external-blocker cooldown was removed in #572). The
// preventive layer is internal/doctor's codex sandbox probe (#542), which
// catches the host misconfiguration before dispatch.
type SandboxStartupError struct {
	Detail string
}

func (e *SandboxStartupError) Error() string {
	if e == nil || strings.TrimSpace(e.Detail) == "" {
		return "agent sandbox failed to start"
	}
	return "agent sandbox failed to start: " + e.Detail
}

func IsSandboxStartup(err error) bool {
	var se *SandboxStartupError
	return errors.As(err, &se)
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
	// LastEvent is the last runtime event the runner emitted before silence.
	LastEvent string
	// Cause is the wrapped underlying error, when available.
	Cause error
}

func (e *StallError) Error() string {
	last := ""
	if strings.TrimSpace(e.LastEvent) != "" {
		last = fmt.Sprintf(" after last event %q", e.LastEvent)
	}
	if e.Cause != nil {
		return fmt.Sprintf("runner stall timeout after %s without events%s (budget %s): %v", e.Elapsed, last, e.Timeout, e.Cause)
	}
	return fmt.Sprintf("runner stall timeout after %s without events%s (budget %s)", e.Elapsed, last, e.Timeout)
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
