package orchestrator

// actor.go is the SPEC §7.4 "single authority" goroutine that serializes
// every mutation against OrchestratorState. The pure data types and
// transition methods live in state.go; this file owns the goroutine,
// the seams the actor uses to spawn workers and pick retry delays, and
// the small set of public entry points (RequestDispatch, ScheduleRetry,
// Snapshot) that callers from outside the actor use to drive it.
//
// Mutation discipline is supplied by an unbuffered ops channel and one
// reader (Run). Every mutation is a stateOp; apply runs on the actor
// goroutine with exclusive access to the state. Long side-effects
// (timer setup, dispatcher.Spawn, follow-up state mutations) belong in
// the followup func returned by apply, which the actor launches on a
// fresh goroutine after apply returns. apply MUST NOT call submit from
// inside itself — the actor is the same goroutine reading from ops and
// would deadlock against itself.
//
// The Elixir reference uses one GenServer per orchestrator with every
// mutation flowing through handle_call / handle_cast / handle_info
// (orchestrator.ex:6,52,74-217); the Go actor here is the direct analog.
//
// PR 2 of the D21+D6 migration plan shipped this actor, the Dispatcher seam,
// and a scheduler seam. D16 wires that seam to SPEC §8.4 exponential failure
// backoff plus SPEC §16.6's short continuation retry.

import (
	"context"
	"errors"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// WorkerResult is the per-run outcome the Dispatcher delivers when its
// spawned worker exits. Err is nil for SPEC §7.3 normal exit; retryable
// errors are treated as abnormal exits and trigger ScheduleRetry(attempt+1).
// NonRetryable errors fail fast and release the claim so deterministic
// configuration/task-build failures do not spin forever. Elapsed is folded
// into CodexTotals.SecondsRunning per SPEC §13.3.
type WorkerResult struct {
	Err           error
	NonRetryable  bool
	InputRequired bool
	Elapsed       time.Duration
}

// Dispatcher is the seam through which the actor spawns a per-issue
// worker goroutine. The returned channel must yield exactly one
// WorkerResult and then close (or close without yielding to signal
// cancellation). The actor watches it to drive normal/abnormal exit
// transitions.
type Dispatcher interface {
	Spawn(ctx context.Context, issue tracker.Issue, attempt *int) <-chan WorkerResult
}

// Scheduler computes the delay before the next retry request.
type Scheduler interface {
	NextDelay(RetryRequest) time.Duration
}

// RetryKind identifies whether the retry follows a failed worker run or a
// clean continuation turn.
type RetryKind string

const (
	RetryKindFailure      RetryKind = "failure"
	RetryKindContinuation RetryKind = "continuation"
)

// RetryRequest describes the retry being scheduled. Attempt is the 1-based
// failure retry attempt for RetryKindFailure. Continuation retries ignore it
// and always use the short SPEC §16.6 delay.
type RetryRequest struct {
	Kind    RetryKind
	Attempt int
}

// RetryScheduler implements the SPEC retry delays: clean continuation retries
// use one second; failure retries use delay=min(10s*2^(attempt-1), MaxBackoff).
type RetryScheduler struct {
	MaxBackoff time.Duration
}

const continuationRetryDelay = time.Second

// retryFetchTimeout caps the SPEC §16.6 candidate-fetch call a fired
// failure-retry timer makes. Tracker clients enforce per-request
// network timeouts on their own (Linear / Gitea / GitHub all 30s per
// PR #303), but a defensive ceiling here means the orchestrator does
// not depend on every adapter to do so: if a future tracker client
// (or a transport bug) silently drops cancellation, the fetch still
// returns within this bound and the SPEC's "retry poll failed"
// reschedule path takes over instead of leaving the issue stuck in
// Claimed/RetryAttempts forever. The value is comfortably larger
// than any adapter's own deadline so an honest slow tracker is not
// punished. A package-level var (not const) so tests can shrink the
// bound; runtime callers must not mutate it.
var retryFetchTimeout = 45 * time.Second

// NextDelay implements Scheduler.
func (s RetryScheduler) NextDelay(req RetryRequest) time.Duration {
	if req.Kind == RetryKindContinuation {
		return continuationRetryDelay
	}
	if req.Attempt < 1 {
		req.Attempt = 1
	}
	maxBackoff := s.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 5 * time.Minute
	}
	delay := 10 * time.Second
	for i := 1; i < req.Attempt; i++ {
		if delay >= maxBackoff/2 {
			return maxBackoff
		}
		delay *= 2
	}
	if delay > maxBackoff {
		return maxBackoff
	}
	return delay
}

// UnboundedFailureRetries is the maxFailureRetries sentinel that disables
// the failure-retry cap. SPEC §8.4 / §16.6 / §4.1.8 do not budget retry
// attempts, so this value is what an orchestrator constructed without an
// explicit MaxFailureRetries override sees. The check at finalizeRunOp
// skips the cap branch whenever maxFailureRetries is negative; any caller
// that wants the harness-hardening cap (SPEC §15.5) must pass a
// non-negative value through workflow.AgentConfig.MaxRetryAttempts or
// orchestrator.Deps.MaxFailureRetries.
const UnboundedFailureRetries = -1

// Deps bundles construction-time dependencies so adding a new one
// later doesn't ripple through every call site.
type Deps struct {
	Dispatcher Dispatcher
	Scheduler  Scheduler
	// MaxFailureRetries opts into a non-SPEC cap on failure-driven
	// orchestrator retries. nil (the default) and any negative value
	// leave the SPEC §8.4 unbounded behavior in place; a non-negative
	// integer applies the harness-hardening cap.
	MaxFailureRetries *int
	MaxTurns          *int
	// CandidateLister, when set, enables the SPEC §16.6 retry-fire
	// candidate-fetch step. A fired failure-retry timer fetches the
	// active candidate list, confirms the issue is still present,
	// releases the claim when it is not, and surfaces a typed
	// "retry poll failed" reschedule when the fetch itself fails.
	// Production wires this from the same multi-tracker lister the
	// poll loop uses; when nil, retry fires dispatch directly from
	// the cached entry.Issue (legacy behavior, kept for unit tests).
	CandidateLister ActiveIssueLister
}

// Orchestrator is the SPEC §3.1 / §7.4 "single authority" that owns
// OrchestratorState. All mutations flow through the actor goroutine
// started by Run; reads via Snapshot also serialize through the actor
// so callers always observe a consistent view.
type Orchestrator struct {
	ops chan stateOp

	state      *OrchestratorState
	dispatcher Dispatcher
	scheduler  Scheduler
	// maxFailureRetries bounds failure-driven retry entries after retryable
	// worker failures. SPEC §8.4 / §16.6 expect unbounded retries (with the
	// exponential-backoff ceiling), so a negative value here means "no cap —
	// keep retrying until the tracker takes the issue out of active work".
	// Non-negative values opt into the SPEC §15.5 harness-hardening cap and
	// pin the issue under OrchestratorState.Failed once exceeded.
	maxFailureRetries int
	// maxTurns bounds clean continuation dispatches for one-shot runners that
	// cannot enforce agent.max_turns internally. App-server runners still enforce
	// the same setting before they return.
	maxTurns  int
	retryWake chan struct{}

	// candidateLister supplies the SPEC §16.6 candidate fetch that a
	// fired failure-retry timer consults before re-dispatching. The
	// field is read on every retry fire and written when the runtime
	// poller rebuilds its tracker set, so a mutex guards the swap.
	candidateListerMu sync.Mutex
	candidateLister   ActiveIssueLister

	// runCtx is captured by Run so followup goroutines can cancel
	// their work when the actor stops. Set once at the top of Run
	// before close(started); reads from outside the actor synchronize
	// via the started channel close.
	runCtx  context.Context
	started chan struct{}
}

// New constructs an Orchestrator over an existing state value. Callers
// must call Run(ctx) in a separate goroutine before the actor processes
// any submitted op; tests can wait via WaitStarted.
func New(state *OrchestratorState, deps Deps) *Orchestrator {
	// Default to unbounded so the SPEC §8.4 contract — retry forever,
	// bounded only by backoff and tracker state — is what a caller gets
	// without explicit opt-in. A negative pointer normalizes to the same
	// "no cap" sentinel so test/production callers can pass either.
	maxFailureRetries := UnboundedFailureRetries
	if deps.MaxFailureRetries != nil {
		maxFailureRetries = *deps.MaxFailureRetries
		if maxFailureRetries < 0 {
			maxFailureRetries = UnboundedFailureRetries
		}
	}
	maxTurns := 20
	if deps.MaxTurns != nil {
		maxTurns = *deps.MaxTurns
		if maxTurns < 1 {
			maxTurns = 1
		}
	}
	o := &Orchestrator{
		ops:               make(chan stateOp),
		state:             state,
		dispatcher:        deps.Dispatcher,
		scheduler:         deps.Scheduler,
		maxFailureRetries: maxFailureRetries,
		maxTurns:          maxTurns,
		retryWake:         make(chan struct{}, 1),
		candidateLister:   deps.CandidateLister,
		started:           make(chan struct{}),
	}
	if aware, ok := deps.Dispatcher.(interface{ AttachOrchestrator(*Orchestrator) }); ok {
		aware.AttachOrchestrator(o)
	}
	return o
}

// Run is the actor loop. It drains the ops channel until ctx is
// cancelled, applying each op against the state and (if the op
// returns one) launching its followup on a fresh goroutine so the
// followup can do I/O or submit further ops without deadlocking.
//
// Run must be called exactly once per Orchestrator and exits when ctx
// is cancelled. Pending submits at that point return ctx.Err() via
// the caller's context, not the orchestrator's — see submit.
func (o *Orchestrator) Run(ctx context.Context) {
	o.runCtx = ctx
	close(o.started)
	for {
		select {
		case <-ctx.Done():
			return
		case op := <-o.ops:
			followup := o.applyWithRecover(op)
			if followup != nil {
				safeGo("orchestrator.followup", followup)
			}
		}
	}
}

// applyWithRecover invokes op.apply with a panic guard so a malformed
// notification or unexpected nil deep in a state transition cannot
// crash the actor goroutine — the only goroutine driving every
// in-flight run. SPEC §7.4 requires serialized mutation, so on panic
// the actor logs the event and drops the followup; the state may be
// partially mutated but the actor keeps draining subsequent ops
// instead of taking the whole worker down. Operators see the typed
// `event=panic site=orchestrator.op_apply` line plus the runtime stack
// so the failure is diagnosable.
func (o *Orchestrator) applyWithRecover(op stateOp) (followup func()) {
	defer func() {
		if r := recover(); r != nil {
			recoverPanicValue("orchestrator.op_apply", r)
			followup = nil
		}
	}()
	return op.apply(o.state)
}

// WaitStarted blocks until Run has begun executing or ctx is cancelled.
// Tests use it to avoid races between setup goroutines and the actor's
// initialization; production callers usually construct + Run together.
func (o *Orchestrator) WaitStarted(ctx context.Context) error {
	select {
	case <-o.started:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *Orchestrator) retryWakeCh() <-chan struct{} {
	if o == nil {
		return nil
	}
	return o.retryWake
}

func (o *Orchestrator) wakeRetryPollLoop() {
	_ = o.queuePollWake()
}

// SetCandidateLister installs (or swaps) the lister the actor consults on a
// fired failure-retry timer. Safe to call before or after Run.
func (o *Orchestrator) SetCandidateLister(l ActiveIssueLister) {
	if o == nil {
		return
	}
	o.candidateListerMu.Lock()
	o.candidateLister = l
	o.candidateListerMu.Unlock()
}

func (o *Orchestrator) currentCandidateLister() ActiveIssueLister {
	if o == nil {
		return nil
	}
	o.candidateListerMu.Lock()
	defer o.candidateListerMu.Unlock()
	return o.candidateLister
}

func (o *Orchestrator) queuePollWake() bool {
	if o == nil || o.retryWake == nil {
		return false
	}
	select {
	case o.retryWake <- struct{}{}:
		return false
	default:
		return true
	}
}

// RefreshRequestResult is the SPEC §13.7.2 /api/v1/refresh response shape.
type RefreshRequestResult struct {
	Queued      bool      `json:"queued"`
	Coalesced   bool      `json:"coalesced"`
	RequestedAt time.Time `json:"requested_at"`
	Operations  []string  `json:"operations"`
}

// RequestRefresh asks the poll loop to run one immediate poll/reconcile cycle.
// The wake channel has one slot, so repeated requests before the loop consumes
// the signal are coalesced into a single extra cycle.
func (o *Orchestrator) RequestRefresh(ctx context.Context) (RefreshRequestResult, error) {
	if err := ctx.Err(); err != nil {
		return RefreshRequestResult{}, err
	}
	return RefreshRequestResult{
		Queued:      true,
		Coalesced:   o.queuePollWake(),
		RequestedAt: time.Now().UTC(),
		Operations:  []string{"poll", "reconcile"},
	}, nil
}

// submit delivers op to the actor. It blocks until the actor reads the
// op or ctx is cancelled. Callers outside the actor (tests, timer
// callbacks, follow-up closures) use this helper. The actor itself
// MUST NOT call submit from inside an apply method — the unbuffered
// ops channel has exactly one reader (the actor goroutine), and a
// re-entrant send would deadlock against it.
func (o *Orchestrator) submit(ctx context.Context, op stateOp) error {
	select {
	case o.ops <- op:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stateOp is one mutation against OrchestratorState. apply runs on the
// actor goroutine; the optional followup runs on a fresh goroutine
// after apply returns. apply MUST NOT block on the ops channel.
type stateOp interface {
	apply(*OrchestratorState) (followup func())
}

// opFunc adapts a function literal to stateOp so simple ops don't need
// a struct just to carry their behavior.
type opFunc func(*OrchestratorState) func()

func (f opFunc) apply(st *OrchestratorState) func() { return f(st) }

// Snapshot returns a SPEC §13.3-shaped view of the orchestrator state.
// The snapshot is taken on the actor goroutine so it observes a
// consistent state between mutations. Returns ctx.Err() if ctx is
// cancelled before the actor produces the view.
func (o *Orchestrator) Snapshot(ctx context.Context) (StateView, error) {
	reply := make(chan StateView, 1)
	op := opFunc(func(st *OrchestratorState) func() {
		reply <- st.Snapshot()
		return nil
	})
	if err := o.submit(ctx, op); err != nil {
		return StateView{}, err
	}
	select {
	case v := <-reply:
		return v, nil
	case <-ctx.Done():
		return StateView{}, ctx.Err()
	}
}

// RecordWorkspace stores the deterministic workspace path for a running issue
// so blocked-session status and later reconciliation cleanup can refer to the
// actual on-disk checkout.
func (o *Orchestrator) RecordWorkspace(ctx context.Context, issueID string, workspace Workspace) error {
	if o == nil || strings.TrimSpace(issueID) == "" || strings.TrimSpace(workspace.Path) == "" {
		return nil
	}
	done := make(chan struct{})
	op := opFunc(func(st *OrchestratorState) func() {
		if run := st.Running[IssueID(issueID)]; run != nil {
			run.Workspace = workspace
		}
		close(done)
		return nil
	})
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// UpdateMaxConcurrentAgents applies a reloaded workflow capacity limit through
// the actor so dispatch and retry capacity checks observe the new value without
// restarting the process.
func (o *Orchestrator) UpdateMaxConcurrentAgents(ctx context.Context, maxConcurrentAgents int) error {
	if maxConcurrentAgents <= 0 {
		return nil
	}
	done := make(chan struct{}, 1)
	op := opFunc(func(st *OrchestratorState) func() {
		st.MaxConcurrentAgents = maxConcurrentAgents
		done <- struct{}{}
		return nil
	})
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// UpdateMaxConcurrentAgentsByState applies reloaded per-state capacity limits
// through the actor so dispatch and retry capacity checks observe them without
// restarting the process.
func (o *Orchestrator) UpdateMaxConcurrentAgentsByState(ctx context.Context, limits map[string]int) error {
	done := make(chan struct{}, 1)
	op := opFunc(func(st *OrchestratorState) func() {
		st.MaxConcurrentAgentsByState = normalizeStateConcurrencyLimits(limits)
		done <- struct{}{}
		return nil
	})
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// UpdatePollIntervalMs applies reloaded workflow poll cadence metadata through
// the actor so /api/v1/state reflects the runtime cadence after workflow reload.
func (o *Orchestrator) UpdatePollIntervalMs(ctx context.Context, pollIntervalMs int64) error {
	if pollIntervalMs <= 0 {
		return nil
	}
	done := make(chan struct{}, 1)
	op := opFunc(func(st *OrchestratorState) func() {
		st.PollIntervalMs = pollIntervalMs
		done <- struct{}{}
		return nil
	})
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// UpdateRetryScheduler applies reloaded retry timing through the actor so
// subsequently scheduled retries observe workflow changes without a process
// restart.
func (o *Orchestrator) UpdateRetryScheduler(ctx context.Context, scheduler Scheduler) error {
	if scheduler == nil {
		return nil
	}
	done := make(chan struct{}, 1)
	op := opFunc(func(*OrchestratorState) func() {
		o.scheduler = scheduler
		done <- struct{}{}
		return nil
	})
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// UpdateMaxFailureRetries applies the reloaded failure retry budget through the
// actor. The budget counts scheduled failure retry entries after the first run;
// any negative value (including the workflow-layer UnboundedRetryBudget sentinel
// that a workflow with no `agent.max_retry_attempts` produces) disables the cap
// and restores SPEC §8.4 unbounded retries. Zero disables failure retries
// outright as a deliberate opt-in. Clean continuations are bounded separately
// by agent.max_turns.
func (o *Orchestrator) UpdateMaxFailureRetries(ctx context.Context, maxFailureRetries int) error {
	if maxFailureRetries < 0 {
		maxFailureRetries = UnboundedFailureRetries
	}
	done := make(chan struct{}, 1)
	op := opFunc(func(*OrchestratorState) func() {
		o.maxFailureRetries = maxFailureRetries
		done <- struct{}{}
		return nil
	})
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// UpdateMaxTurns applies the reloaded clean-continuation budget through the
// actor. Values below one are clamped to one so a normal first run can finish
// but will not schedule any continuation.
func (o *Orchestrator) UpdateMaxTurns(ctx context.Context, maxTurns int) error {
	if maxTurns < 1 {
		maxTurns = 1
	}
	done := make(chan struct{}, 1)
	op := opFunc(func(*OrchestratorState) func() {
		o.maxTurns = maxTurns
		done <- struct{}{}
		return nil
	})
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ErrNotDispatched is returned by RequestDispatch when the issue was
// already claimed (running, retry-queued, or otherwise reserved) and
// dispatch was therefore deduped. It is not an error condition — SPEC
// §7.4's duplicate-dispatch guard is doing its job — but callers
// distinguish "rejected" from "succeeded" by inspecting it.
var ErrNotDispatched = errors.New("orchestrator: issue already claimed")

// ErrCapacityFull is returned by RequestDispatch when the issue is eligible and
// unclaimed, but the orchestrator is already running the configured maximum
// number of agents. Callers should keep the issue eligible for a future poll
// tick rather than treating it as a duplicate dispatch.
var ErrCapacityFull = errors.New("orchestrator: max_concurrent_agents reached")

// ReconcileTrackerIssues cancels or releases in-process work that is no longer
// tracker-eligible. It is the per-tick half of SPEC §2.1/#78: each tracker poll
// revalidates active runs against the latest tracker state and cancels workers
// whose issues moved out of active states.
func (o *Orchestrator) ReconcileTrackerIssues(ctx context.Context, issuesByID map[string]tracker.Issue, activeStates map[string]struct{}) error {
	return o.ReconcileTrackerIssuesAndWait(ctx, issuesByID, activeStates, 0)
}

// ReconcileStalledRuns implements SPEC §8.5 Part A / §16.3
// reconcile_stalled_runs: for each running issue compute elapsed time
// since the last observed runtime event (RunningEntry.LastCodexAt,
// falling back to StartedAt before any event has been seen) and, if it
// exceeds stallTimeoutMs, cancel the worker so the finalize path
// schedules a retry. stallTimeoutMs <= 0 skips detection entirely (SPEC
// §6.4 default).
//
// The Codex app-server runner has its own self-stall detection; this
// orchestrator-side path closes the gap when the runner goroutine itself
// wedges or when a non-Codex runner (mock, codex exec) produces no
// StallError. Without this an issue with `LastCodexAt` long in the past
// would stay claimed forever.
//
// wait is the per-tick budget for waiting on cancelled worker
// goroutines to exit (mirrors ReconcileTrackerIssuesAndWait). Use 0 for
// fire-and-forget cancel.
func (o *Orchestrator) ReconcileStalledRuns(ctx context.Context, stallTimeoutMs int, wait time.Duration) error {
	if stallTimeoutMs <= 0 {
		return nil
	}
	reply := make(chan []*RunningEntry, 1)
	if err := o.submit(ctx, &reconcileStalledRunsOp{
		timeout: time.Duration(stallTimeoutMs) * time.Millisecond,
		now:     time.Now(),
		result:  reply,
	}); err != nil {
		return err
	}
	var canceled []*RunningEntry
	select {
	case canceled = <-reply:
	case <-ctx.Done():
		return ctx.Err()
	}
	return waitForReconciledWorkers(ctx, canceled, wait)
}

// RefreshActiveTrackerIssues updates stored issue metadata for in-process runs
// and queued retries whose issues are still observed in the active set, without
// canceling any work. Use this when the active listing may be partial (so
// absence from issuesByID must not imply inactivity) but per-state capacity
// gates still need the latest tracker state.
func (o *Orchestrator) RefreshActiveTrackerIssues(ctx context.Context, issuesByID map[string]tracker.Issue, activeStates map[string]struct{}) error {
	done := make(chan struct{}, 1)
	if err := o.submit(ctx, &refreshActiveTrackerIssuesOp{issuesByID: issuesByID, activeStates: activeStates, done: done}); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReconcileTrackerIssuesAndWait performs the same reconciliation as
// ReconcileTrackerIssues, then optionally waits for canceled workers to exit.
// This lets poll ticks provide prompt cancellation semantics without making the
// actor itself block on worker goroutines.
func (o *Orchestrator) ReconcileTrackerIssuesAndWait(ctx context.Context, issuesByID map[string]tracker.Issue, activeStates map[string]struct{}, wait time.Duration) error {
	reply := make(chan []*RunningEntry, 1)
	if err := o.submit(ctx, &reconcileTrackerIssuesOp{issuesByID: issuesByID, activeStates: activeStates, result: reply}); err != nil {
		return err
	}
	var canceled []*RunningEntry
	select {
	case canceled = <-reply:
	case <-ctx.Done():
		return ctx.Err()
	}
	return waitForReconciledWorkers(ctx, canceled, wait)
}

func waitForReconciledWorkers(ctx context.Context, canceled []*RunningEntry, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for _, entry := range canceled {
		if entry.Done == nil {
			continue
		}
		select {
		case <-entry.Done:
		case <-waitCtx.Done():
			return waitCtx.Err()
		}
	}
	return nil
}

// RequestDispatch is the public entry to dispatch issue if no other
// claim exists. It returns nil on accepted dispatch (a worker is being
// spawned) and ErrNotDispatched if the actor saw an existing claim.
//
// Dispatch decisions are serialized through the actor: concurrent calls
// for the same issue produce at most one Running entry, even when many
// goroutines race on the same id.
func (o *Orchestrator) RequestDispatch(ctx context.Context, issue tracker.Issue, attempt *int) error {
	reply := make(chan error, 1)
	op := &dispatchOp{o: o, issue: issue, attempt: attempt, result: reply}
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *Orchestrator) RequestDispatchAfterTrackerRecheck(ctx context.Context, issue tracker.Issue, attempt *int) error {
	reply := make(chan error, 1)
	op := &dispatchOp{o: o, issue: issue, attempt: attempt, result: reply, trackerRechecked: true}
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *Orchestrator) RunningRetryingAndBlockedIssueIDs(ctx context.Context) []string {
	reply := make(chan []string, 1)
	if err := o.submit(ctx, &runningRetryingAndBlockedIssueIDsOp{result: reply}); err != nil {
		return nil
	}
	select {
	case ids := <-reply:
		return ids
	case <-ctx.Done():
		return nil
	}
}

func (o *Orchestrator) RunningAndRetryingIssueIDs(ctx context.Context) []string {
	return o.RunningRetryingAndBlockedIssueIDs(ctx)
}

// ReconcileInactiveTrackerIssuesAndWait cancels only issues explicitly observed
// in a terminal or configured inactive tracker state. Missing issues are
// treated as unknown instead of inactive because tracker adapters may return
// partial state listings under pagination caps.
func (o *Orchestrator) ReconcileInactiveTrackerIssuesAndWait(ctx context.Context, issuesByID map[string]tracker.Issue, workerExitTimeout time.Duration) error {
	reply := make(chan []*RunningEntry, 1)
	op := &reconcileInactiveTrackerIssuesOp{issuesByID: issuesByID, result: reply}
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	var canceled []*RunningEntry
	select {
	case canceled = <-reply:
	case <-ctx.Done():
		return ctx.Err()
	}
	return waitForReconciledWorkers(ctx, canceled, workerExitTimeout)
}

// ScheduleRetry enters the SPEC §7.1 retry-queued substate for issue.
// The orchestrator picks a delay via Scheduler.NextDelay(attempt),
// stores a RetryEntry under RetryAttempts, holds the Claimed slot so
// concurrent ticks cannot dispatch the issue, and starts a timer that
// re-dispatches through the actor when it fires.
//
// The 1-based attempt counter is the attempt number this retry will
// run as (i.e. the prior run was attempt-1, or 0 for first-run).
func (o *Orchestrator) ScheduleRetry(ctx context.Context, issue tracker.Issue, identifier string, attempt int, runErr string) error {
	return o.scheduleRetry(ctx, issue, identifier, RetryRequest{Kind: RetryKindFailure, Attempt: attempt}, attempt, runErr)
}

func (o *Orchestrator) scheduleContinuationRetry(ctx context.Context, issue tracker.Issue, identifier string, attempt int) error {
	return o.scheduleRetry(ctx, issue, identifier, RetryRequest{Kind: RetryKindContinuation, Attempt: attempt}, attempt, "")
}

func (o *Orchestrator) scheduleRetry(ctx context.Context, issue tracker.Issue, identifier string, req RetryRequest, attempt int, runErr string) error {
	op := &scheduleRetryOp{
		o:          o,
		issue:      issue,
		identifier: identifier,
		attempt:    attempt,
		runErr:     runErr,
		kind:       req.Kind,
		req:        req,
	}
	return o.submit(ctx, op)
}

// refreshActiveTrackerIssuesOp refreshes RunningEntry.Issue and the matching
// ClaimedIssues snapshot for every in-process issue observed in the active set,
// without canceling anything. It is safe to call when the active listing may be
// partial, because absence from issuesByID is treated as "no information," not
// "now inactive."
type refreshActiveTrackerIssuesOp struct {
	issuesByID   map[string]tracker.Issue
	activeStates map[string]struct{}
	done         chan<- struct{}
}

func (r *refreshActiveTrackerIssuesOp) apply(st *OrchestratorState) func() {
	for id, run := range st.Running {
		issue, ok := r.issuesByID[string(id)]
		if !ok || !isActiveTrackerState(issue.State, r.activeStates) || !sameServiceRoute(run.Issue, issue) {
			continue
		}
		run.Issue = issue
		st.ClaimedIssues[id] = issue
	}
	for id, retry := range st.RetryAttempts {
		issue, ok := r.issuesByID[string(id)]
		if !ok || !isActiveTrackerState(issue.State, r.activeStates) || !sameServiceRoute(retry.Issue, issue) {
			continue
		}
		retry.Issue = issue
		st.ClaimedIssues[id] = issue
	}
	for id, blocked := range st.Blocked {
		issue, ok := r.issuesByID[string(id)]
		if !ok || !isActiveTrackerState(issue.State, r.activeStates) || !sameServiceRoute(blocked.Issue, issue) {
			continue
		}
		blocked.Issue = issue
		st.ClaimedIssues[id] = issue
	}
	done := r.done
	return func() {
		if done != nil {
			close(done)
		}
	}
}

type runningRetryingAndBlockedIssueIDsOp struct {
	result chan<- []string
}

func (r *runningRetryingAndBlockedIssueIDsOp) apply(st *OrchestratorState) func() {
	seen := map[string]struct{}{}
	issueIDs := make([]string, 0, len(st.Running)+len(st.RetryAttempts)+len(st.Blocked))
	add := func(id IssueID) {
		s := strings.TrimSpace(string(id))
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		issueIDs = append(issueIDs, s)
	}
	for id := range st.Running {
		add(id)
	}
	for id := range st.RetryAttempts {
		add(id)
	}
	for id := range st.Blocked {
		add(id)
	}
	sort.Strings(issueIDs)
	result := r.result
	return func() {
		if result != nil {
			result <- issueIDs
		}
	}
}

// reconcileStalledRunsOp is the actor-side handler for SPEC §8.5 Part A.
// It scans st.Running for entries whose LastCodexAt (or StartedAt when no
// runtime event has been observed yet) is older than the configured
// stall budget and cancels the worker. The entry stays Claimed: the
// finalize op observes the canceled ctx as a worker failure and schedules
// a normal failure retry (no ReconcileCancel — operator cancellation and
// stall recovery have different semantics).
type reconcileStalledRunsOp struct {
	timeout time.Duration
	now     time.Time
	result  chan<- []*RunningEntry
}

func (r *reconcileStalledRunsOp) apply(st *OrchestratorState) func() {
	var canceled []*RunningEntry
	for id, run := range st.Running {
		ref := run.LastCodexAt
		if ref.IsZero() {
			ref = run.StartedAt
		}
		if ref.IsZero() {
			// Fresh dispatch without a StartedAt is exceedingly rare (a
			// test fixture). Skip rather than treat the Unix epoch as a
			// stall reference, which would cancel every such entry.
			continue
		}
		if r.now.Sub(ref) <= r.timeout {
			continue
		}
		canceled = append(canceled, run)
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventFailed, IssueID: id, Identifier: run.Identifier, Message: "stalled"})
	}
	return func() {
		for _, entry := range canceled {
			if entry.CancelWorker != nil {
				entry.CancelWorker()
			}
		}
		if r.result != nil {
			r.result <- canceled
		}
	}
}

type reconcileTrackerIssuesOp struct {
	issuesByID   map[string]tracker.Issue
	activeStates map[string]struct{}
	result       chan<- []*RunningEntry
}

func (r *reconcileTrackerIssuesOp) apply(st *OrchestratorState) func() {
	var cancelEntries []*RunningEntry
	var cleanupEntries []workspaceCleanup
	for id, run := range st.Running {
		issue, ok := r.issuesByID[string(id)]
		if ok && isActiveTrackerState(issue.State, r.activeStates) && sameServiceRoute(run.Issue, issue) {
			// Refresh stored issue metadata so per-state capacity gates
			// (RunningCountByState, StateCapacityFull) see the latest tracker
			// state. Without this, an issue that moved between active states
			// keeps counting toward its dispatch-time bucket and a later poll
			// can exceed max_concurrent_agents_by_state for the new state.
			run.Issue = issue
			st.ClaimedIssues[id] = issue
			continue
		}
		st.ReleaseClaim(id)
		run.ReconcileCancel = true
		cancelEntries = append(cancelEntries, run)
	}
	for id, retry := range st.RetryAttempts {
		issue, ok := r.issuesByID[string(id)]
		if ok && isActiveTrackerState(issue.State, r.activeStates) && sameServiceRoute(retry.Issue, issue) {
			retry.Issue = issue
			st.ClaimedIssues[id] = issue
			continue
		}
		st.ReleaseClaim(id)
	}
	for id, blocked := range st.Blocked {
		issue, ok := r.issuesByID[string(id)]
		if ok && isActiveTrackerState(issue.State, r.activeStates) && sameServiceRoute(blocked.Issue, issue) {
			blocked.Issue = issue
			st.ClaimedIssues[id] = issue
			continue
		}
		cleanupEntries = appendBlockedWorkspaceCleanup(cleanupEntries, id, blocked)
		st.ReleaseClaim(id)
	}
	return reconcileCancelFollowup(cancelEntries, cleanupEntries, r.result)
}

func sameServiceRoute(previous, current tracker.Issue) bool {
	return strings.TrimSpace(previous.ServiceName) == strings.TrimSpace(current.ServiceName)
}

type reconcileInactiveTrackerIssuesOp struct {
	issuesByID map[string]tracker.Issue
	result     chan<- []*RunningEntry
}

func (r *reconcileInactiveTrackerIssuesOp) apply(st *OrchestratorState) func() {
	var cancelEntries []*RunningEntry
	var cleanupEntries []workspaceCleanup
	for id, run := range st.Running {
		if _, ok := r.issuesByID[string(id)]; !ok {
			continue
		}
		st.ReleaseClaim(id)
		run.ReconcileCancel = true
		cancelEntries = append(cancelEntries, run)
	}
	for id := range st.RetryAttempts {
		if _, ok := r.issuesByID[string(id)]; !ok {
			continue
		}
		st.ReleaseClaim(id)
	}
	for id := range st.Blocked {
		if _, ok := r.issuesByID[string(id)]; !ok {
			continue
		}
		cleanupEntries = appendBlockedWorkspaceCleanup(cleanupEntries, id, st.Blocked[id])
		st.ReleaseClaim(id)
	}
	return reconcileCancelFollowup(cancelEntries, cleanupEntries, r.result)
}

type workspaceCleanup struct {
	issueID    IssueID
	identifier string
	path       string
}

func appendBlockedWorkspaceCleanup(cleanups []workspaceCleanup, id IssueID, blocked *BlockedEntry) []workspaceCleanup {
	if blocked == nil {
		return cleanups
	}
	path := strings.TrimSpace(blocked.Workspace.Path)
	if path == "" {
		return cleanups
	}
	return append(cleanups, workspaceCleanup{issueID: id, identifier: blocked.Identifier, path: path})
}

func reconcileCancelFollowup(cancelEntries []*RunningEntry, cleanupEntries []workspaceCleanup, result chan<- []*RunningEntry) func() {
	return func() {
		for _, entry := range cancelEntries {
			if entry.CancelWorker != nil {
				entry.CancelWorker()
			}
		}
		for _, cleanup := range cleanupEntries {
			if err := os.RemoveAll(cleanup.path); err != nil {
				if cleanup.identifier != "" {
					log.Printf("event=blocked_workspace_remove_failed issue_id=%s issue_identifier=%s workspace=%q error=%q", cleanup.issueID, cleanup.identifier, cleanup.path, err)
				} else {
					log.Printf("event=blocked_workspace_remove_failed issue_id=%s workspace=%q error=%q", cleanup.issueID, cleanup.path, err)
				}
			}
		}
		if result != nil {
			result <- cancelEntries
		}
	}
}

func isActiveTrackerState(state string, activeStates map[string]struct{}) bool {
	if len(activeStates) == 0 {
		return false
	}
	_, ok := activeStates[strings.ToLower(strings.TrimSpace(state))]
	return ok
}

// dispatchOp is the actor-side half of RequestDispatch: it checks
// IsClaimed and either reserves the slot (followup spawns + records
// Running) or signals dispatch denied. The two-step design keeps the
// dispatch decision atomic against concurrent claims while keeping I/O
// off the actor goroutine.
type dispatchOp struct {
	o                *Orchestrator
	issue            tracker.Issue
	attempt          *int
	result           chan<- error
	trackerRechecked bool
}

func (d *dispatchOp) apply(st *OrchestratorState) func() {
	id := IssueID(d.issue.ID)
	st.ReleaseFailedIfIssueChanged(d.issue)
	attempt := d.attempt
	continuationAttempt := 0
	var consumedContinuation *RetryEntry
	if d.trackerRechecked {
		if entry, ok := st.RetryAttempts[id]; ok {
			if entry.Kind != RetryKindContinuation {
				d.result <- ErrNotDispatched
				return nil
			}
			if !entry.IsDue(time.Now()) {
				d.result <- ErrNotDispatched
				return nil
			}
			// Tracker-rechecked dispatch only consumes continuation retries.
			// Failure retries stay claimed until retryFireOp carries their
			// scheduled attempt into a retry dispatch.
			continuationAttempt = entry.Attempt
			consumedContinuation = entry
		} else if st.IsClaimed(id) {
			d.result <- ErrNotDispatched
			return nil
		}
	} else if st.IsClaimed(id) {
		d.result <- ErrNotDispatched
		return nil
	}
	if st.RunningCount() >= st.MaxConcurrentAgents {
		d.result <- ErrCapacityFull
		return nil
	}
	capacityExcluded := IssueID("")
	if consumedContinuation != nil {
		capacityExcluded = id
	}
	if st.StateCapacityFullExcluding(d.issue.State, capacityExcluded) {
		d.result <- ErrCapacityFull
		return nil
	}
	if consumedContinuation != nil {
		if consumedContinuation.Timer != nil {
			consumedContinuation.Timer.Stop()
		}
		delete(st.RetryAttempts, id)
		delete(st.Claimed, id)
	}
	st.RecordEvent(RuntimeEvent{
		Kind:       RuntimeEventCandidate,
		IssueID:    id,
		Identifier: d.issue.Identifier,
		Message:    "candidate fetched from tracker",
	})
	// Reserve the slot synchronously so a concurrent dispatchOp aborts
	// on its IsClaimed check. The followup records Running once the
	// worker is spawned.
	st.Claimed[id] = struct{}{}
	st.ClaimedIssues[id] = d.issue
	o := d.o
	issue := d.issue
	result := d.result
	return func() {
		o.spawn(id, issue, attempt, continuationAttempt)
		result <- nil
	}
}

// scheduleRetryOp is the actor-side half of ScheduleRetry: it stores
// the RetryEntry through OrchestratorState.ScheduleRetry (which stops
// any prior timer for the same id) and starts a new timer whose
// callback submits a retryFireOp.
type scheduleRetryOp struct {
	o          *Orchestrator
	issue      tracker.Issue
	identifier string
	attempt    int
	runErr     string
	kind       RetryKind
	req        RetryRequest
}

func (s *scheduleRetryOp) apply(st *OrchestratorState) func() {
	id := IssueID(s.issue.ID)
	o := s.o
	issue := s.issue
	attempt := s.attempt
	kind := s.kind
	delay := o.scheduler.NextDelay(s.req)
	// time.AfterFunc schedules immediately and is cheap (no goroutine
	// until fire), so we can safely create the timer on the actor
	// without blocking. ScheduleRetry needs the Timer set on the entry
	// before storing so a stale prior timer is stopped atomically.
	entry := &RetryEntry{
		Issue:      s.issue,
		IssueID:    id,
		Identifier: s.identifier,
		Attempt:    attempt,
		DueAt:      time.Now().Add(delay),
		Error:      s.runErr,
		Kind:       s.kind,
	}
	entry.Timer = time.AfterFunc(delay, func() {
		defer recoverPanic("orchestrator.retry_timer")
		_ = o.submit(o.runCtx, &retryFireOp{
			o:       o,
			id:      id,
			issue:   issue,
			attempt: attempt,
			kind:    kind,
		})
	})
	st.ScheduleRetry(entry)
	return nil
}

// retryFireOp is the actor-side handler for a fired retry timer. The
// SPEC §16.6 retry path is "if the entry is still queued, re-dispatch;
// otherwise drop the fire." Two timers may race here in pathological
// cases (a ScheduleRetry replace where the prior timer's Stop missed
// because the callback was already queued); the attempt/kind equality
// guard makes the stale fire a no-op.
type retryFireOp struct {
	o       *Orchestrator
	id      IssueID
	issue   tracker.Issue
	attempt int
	kind    RetryKind
}

func (r *retryFireOp) apply(st *OrchestratorState) func() {
	entry, ok := st.RetryAttempts[r.id]
	if !ok {
		// Already consumed by reconciliation (ReleaseClaim) or by an
		// earlier fire of the same retry. Either is correct.
		return nil
	}
	if entry.Attempt != r.attempt || entry.Kind != r.kind {
		// A newer ScheduleRetry replaced this entry; the older timer
		// fired late. Drop the stale fire — the newer entry will
		// re-dispatch on its own timer.
		return nil
	}
	if entry.Kind == RetryKindContinuation {
		// Continuation retries are only a short wake-up signal after a clean
		// worker exit. They must not spawn from the cached issue snapshot or
		// carry failure retry accounting: a poll has to observe the issue still
		// active and call RequestDispatchAfterTrackerRecheck, which consumes
		// this entry before spawning the next normal turn. Wake the poll loop
		// now so the one-second continuation delay is honored instead of
		// waiting for the next regular tracker poll interval.
		entry.Timer = nil
		o := r.o
		return func() { o.wakeRetryPollLoop() }
	}
	// SPEC §16.6 on_retry_timer: the fired failure-retry timer must (1)
	// fetch active candidates, (2) re-schedule with "retry poll failed"
	// on fetch error, (3) release the claim if the issue is absent, and
	// only then (4/5) dispatch from the refreshed tracker state. When a
	// CandidateLister is wired (production via RuntimePoller) we defer
	// the I/O to a followup; without one we fall back to direct dispatch
	// from the cached entry.Issue so existing unit tests keep working.
	o := r.o
	if lister := o.currentCandidateLister(); lister != nil {
		entry.Timer = nil
		id := r.id
		attempt := r.attempt
		kind := r.kind
		return func() {
			// Per-fetch timeout. The followup runs on a fresh goroutine
			// outside the actor, and o.runCtx has no deadline of its own.
			// A tracker client that ignores ctx cancellation would otherwise
			// pin this goroutine indefinitely — entry.Timer is already
			// cleared and no retryFireOp would be resubmitted, leaving the
			// issue stuck in Claimed/RetryAttempts forever. Surfacing the
			// timeout as a "retry poll failed" reschedule keeps the SPEC
			// §16.6 backoff window the only source of forward progress.
			fetchCtx, cancel := context.WithTimeout(o.runCtx, retryFetchTimeout)
			defer cancel()
			issues, fetchErr := lister.ListActiveIssues(fetchCtx)
			found := findIssueByID(issues, id)
			if fetchErr != nil && found == nil {
				// Either the whole fetch failed (including timeout), or a
				// multi-tracker partial failure happened on the tracker that
				// owns this issue. We can't tell "absent" from "tracker down"
				// — treat as fetch failure per SPEC §16.6 and reschedule
				// with the typed error.
				_ = o.submit(o.runCtx, &retryPollFailedOp{
					o:        o,
					id:       id,
					attempt:  attempt,
					fetchErr: fetchErr,
				})
				return
			}
			_ = o.submit(o.runCtx, &retryFireAfterFetchOp{
				o:       o,
				id:      id,
				attempt: attempt,
				kind:    kind,
				found:   found,
			})
		}
	}
	return retryFireDispatchTail(st, entry, r.id, r.attempt, r.kind, o)
}

// retryFireDispatchTail runs the post-fetch tail of a failure-retry fire:
// honor global + per-state capacity gates, then either spawn or reschedule
// via the configured backoff. Shared between the legacy direct-dispatch
// path (no CandidateLister) and the SPEC §16.6 post-fetch path.
func retryFireDispatchTail(st *OrchestratorState, entry *RetryEntry, id IssueID, attempt int, kind RetryKind, o *Orchestrator) func() {
	// Use entry.Issue rather than any timer-captured snapshot: reconciliation
	// (and the SPEC §16.6 candidate fetch) may have refreshed the tracker
	// state, and both the per-state capacity gate and the spawned worker
	// must see the live state.
	issue := entry.Issue
	identifier := entry.Identifier
	if st.RunningCount() >= st.MaxConcurrentAgents {
		// Retry timers must obey the same capacity gate as fresh dispatch.
		// Mirror upstream handle_active_retry (orchestrator.ex:1142-1161):
		// reschedule through the configured backoff with attempt+1 and a
		// typed "no available orchestrator slots" error instead of arming a
		// short 100ms re-fire timer.
		return capacityDeferRetry(st, id, issue, identifier, attempt, o)
	}
	if st.StateCapacityFull(issue.State) {
		// Retry timers must also obey per-state capacity gates. Same
		// upstream-aligned reschedule shape as the global-cap branch.
		return capacityDeferRetry(st, id, issue, identifier, attempt, o)
	}
	// Consume the retry entry but keep Claimed: the re-dispatch
	// immediately re-adds Running, and dropping Claimed in between
	// would let a concurrent tick race in.
	delete(st.RetryAttempts, id)
	return func() {
		a := attempt
		o.spawn(id, issue, &a, 0)
	}
}

// capacityDeferRetry mirrors upstream handle_active_retry's no-slots
// branch (elixir/lib/symphony_elixir/orchestrator.ex:1142-1161): when a
// fired failure-retry observes a full global or per-state capacity gate,
// reschedule the retry through the configured backoff (SPEC §8.4), bump
// the attempt counter, and stamp the entry with the upstream-canonical
// "no available orchestrator slots" error so operators can observe
// sustained capacity pressure in the runtime event stream. The prior
// 100ms re-fire loop bypassed the backoff formula, left the attempt
// counter frozen across thousands of re-fires, and produced no runtime
// event for the cap-pressure case.
func capacityDeferRetry(st *OrchestratorState, id IssueID, issue tracker.Issue, identifier string, attempt int, o *Orchestrator) func() {
	if o.runCtx.Err() != nil {
		// Mirror retryPollFailedOp's shutdown guard (actor.go above):
		// the followup's ScheduleRetry would fail submit anyway, so
		// recording a cap-pressure event during shutdown would only
		// leak a misleading line into shutdown logs.
		return nil
	}
	const runErr = "no available orchestrator slots"
	nextAttempt := attempt + 1
	st.RecordEvent(RuntimeEvent{
		Kind:       RuntimeEventFailed,
		IssueID:    id,
		Identifier: identifier,
		Message:    runErr,
	})
	return func() {
		_ = o.ScheduleRetry(o.runCtx, issue, identifier, nextAttempt, runErr)
	}
}

func findIssueByID(issues []tracker.Issue, id IssueID) *tracker.Issue {
	for i := range issues {
		if IssueID(issues[i].ID) == id {
			out := issues[i]
			return &out
		}
	}
	return nil
}

// retryPollFailedOp implements SPEC §16.6 step 1 alt: when a fired
// failure-retry timer's candidate fetch fails, reschedule the same
// retry (attempt+1) with a typed "retry poll failed" error so the
// next backoff window can try the fetch again.
type retryPollFailedOp struct {
	o        *Orchestrator
	id       IssueID
	attempt  int
	fetchErr error
}

func (r *retryPollFailedOp) apply(st *OrchestratorState) func() {
	entry, ok := st.RetryAttempts[r.id]
	if !ok {
		// Reconciliation released the claim between fetch and apply.
		return nil
	}
	if entry.Attempt != r.attempt || entry.Kind != RetryKindFailure {
		// Replaced by a newer ScheduleRetry; the newer entry owns the
		// re-dispatch.
		return nil
	}
	o := r.o
	if o.runCtx.Err() != nil {
		// Orchestrator is shutting down between the fetch and this apply.
		// The followup's ScheduleRetry would fail anyway; recording the
		// event and then dropping the followup would only leak a
		// "retry poll failed" line into shutdown logs while the entry
		// goes nowhere. Drop silently and let process exit reclaim the
		// entry along with everything else.
		return nil
	}
	issue := entry.Issue
	identifier := entry.Identifier
	nextAttempt := r.attempt + 1
	runErr := "retry poll failed"
	if r.fetchErr != nil {
		runErr = "retry poll failed: " + r.fetchErr.Error()
	}
	st.RecordEvent(RuntimeEvent{
		Kind:       RuntimeEventFailed,
		IssueID:    r.id,
		Identifier: identifier,
		Message:    runErr,
	})
	return func() {
		_ = o.ScheduleRetry(o.runCtx, issue, identifier, nextAttempt, runErr)
	}
}

// retryFireAfterFetchOp implements SPEC §16.6 steps 3-5 after the
// candidate fetch completes. found == nil means the issue is absent
// from the active candidate set (step 3 / step 5: release the claim);
// otherwise refresh entry.Issue with the live tracker state and proceed
// to capacity check + dispatch.
type retryFireAfterFetchOp struct {
	o       *Orchestrator
	id      IssueID
	attempt int
	kind    RetryKind
	found   *tracker.Issue
}

func (r *retryFireAfterFetchOp) apply(st *OrchestratorState) func() {
	entry, ok := st.RetryAttempts[r.id]
	if !ok {
		return nil
	}
	if entry.Attempt != r.attempt || entry.Kind != r.kind {
		return nil
	}
	if r.found == nil {
		// SPEC §16.6 steps 3 + 5: issue no longer in the active candidate
		// set (either absent or moved to a non-active state). Drop the
		// retry and release the claim.
		identifier := entry.Identifier
		st.ReleaseClaim(r.id)
		st.RecordEvent(RuntimeEvent{
			Kind:       RuntimeEventFailed,
			IssueID:    r.id,
			Identifier: identifier,
			Message:    "retry released: issue absent from active candidates",
		})
		return nil
	}
	// SPEC §16.6 step 4: refresh entry.Issue from the live tracker state
	// before proceeding to capacity check + dispatch. The per-state cap
	// must see the latest state, not the dispatch-time snapshot. Upstream
	// handle_active_retry (orchestrator.ex:1142-1161) uses issue.identifier
	// from the refreshed issue on every subsequent reschedule, so a
	// Linear identifier rename between schedule and fire would otherwise
	// leave runtime events stamped with the stale identifier.
	entry.Issue = *r.found
	st.ClaimedIssues[r.id] = *r.found
	if id := strings.TrimSpace(r.found.Identifier); id != "" {
		entry.Identifier = id
	}
	return retryFireDispatchTail(st, entry, r.id, r.attempt, r.kind, r.o)
}

// finalizeRunOp is the actor-side handler for a worker exit. Result.Err
// drives the SPEC §7.3 normal/abnormal exit branch; the followup
// schedules a retry on abnormal exit.
type finalizeRunOp struct {
	o          *Orchestrator
	id         IssueID
	issue      tracker.Issue
	identifier string
	attempt    *int
	result     WorkerResult
	started    time.Time
	entry      *RunningEntry
	done       chan struct{}
}

func (f *finalizeRunOp) apply(st *OrchestratorState) func() {
	elapsed := f.result.Elapsed
	if elapsed == 0 {
		elapsed = time.Since(f.started)
	}
	if f.entry.InputRequired || f.result.InputRequired || runner.IsInputRequired(f.result.Err) {
		runErr := "input required"
		if f.result.Err != nil {
			runErr = f.result.Err.Error()
		}
		if !st.BlockRun(f.id, f.entry, f.entry.InputRequiredAt, runErr, elapsed) {
			close(f.done)
			return nil
		}
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventInputBlocked, IssueID: f.id, Identifier: f.identifier, Message: runErr})
		close(f.done)
		return nil
	}
	if f.result.Err == nil {
		nextContinuationAttempt := f.entry.ContinuationAttempt + 1
		if nextContinuationAttempt >= f.o.maxTurns {
			if !st.FinishRunNonRetryableFailed(f.id, f.entry, elapsed) {
				close(f.done)
				return nil
			}
			msg := "clean continuation budget exhausted after " + strconv.Itoa(f.o.maxTurns) + " turns while tracker issue remained active"
			st.RecordEvent(RuntimeEvent{Kind: RuntimeEventFailed, IssueID: f.id, Identifier: f.identifier, Message: msg})
			close(f.done)
			return nil
		}
		if !st.FinishRunSucceeded(f.id, f.entry, elapsed) {
			close(f.done)
			return nil
		}
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventCompleted, IssueID: f.id, Identifier: f.identifier, Message: "worker exited cleanly"})
		// Use f.entry.Issue, not f.issue: reconciliation may have refreshed
		// the tracker state mid-run, and per-state capacity gates must see
		// the live state, not the dispatch-time snapshot.
		issue := f.entry.Issue
		st.Claimed[f.id] = struct{}{}
		st.ClaimedIssues[f.id] = issue
		close(f.done)
		// A clean continuation is a new normal turn. Keep its retry entry
		// 1-based for the continuation budget, but do not carry it into future
		// failure backoff; otherwise many successful turns inflate the next
		// transient failure straight to the max backoff.
		nextAttempt := nextContinuationAttempt
		o := f.o
		identifier := f.identifier
		return func() {
			_ = o.scheduleContinuationRetry(o.runCtx, issue, identifier, nextAttempt)
		}
	}
	if f.result.NonRetryable {
		if !st.FinishRunNonRetryableFailed(f.id, f.entry, elapsed) {
			close(f.done)
			return nil
		}
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventFailed, IssueID: f.id, Identifier: f.identifier, Message: f.result.Err.Error()})
		close(f.done)
		return nil
	}
	if f.entry.ReconcileCancel {
		if !st.FinishRunReconciledCancelled(f.id, f.entry, elapsed) {
			close(f.done)
			return nil
		}
		close(f.done)
		return nil
	}
	// Schedule a retry with attempt+1. Per SPEC §4.1.5 the first run's
	// RetryAttempt is nil; the first retry is attempt 1, the second 2,
	// and so on. SPEC §8.4 / §16.6 expect the orchestrator to keep
	// scheduling retries indefinitely (with the exponential-backoff
	// ceiling) — only an explicit harness-hardening opt-in (SPEC §15.5)
	// caps the count. A negative maxFailureRetries leaves the SPEC
	// behavior in place; non-negative opts into the cap.
	nextAttempt := 1
	if f.attempt != nil {
		nextAttempt = *f.attempt + 1
	}
	runErr := f.result.Err.Error()
	if f.o.maxFailureRetries >= 0 && nextAttempt > f.o.maxFailureRetries {
		if !st.FinishRunNonRetryableFailed(f.id, f.entry, elapsed) {
			close(f.done)
			return nil
		}
		msg := "failure retry budget exhausted after " + strconv.Itoa(f.o.maxFailureRetries) + " retries: " + runErr
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventFailed, IssueID: f.id, Identifier: f.identifier, Message: msg})
		close(f.done)
		return nil
	}
	if !st.FinishRunFailed(f.id, f.entry, elapsed) {
		close(f.done)
		return nil
	}
	st.RecordEvent(RuntimeEvent{Kind: RuntimeEventFailed, IssueID: f.id, Identifier: f.identifier, Message: runErr})
	// Hold the Claimed slot across the gap between this apply (which
	// returns control to the actor's select loop) and the
	// scheduleRetryOp that the followup enqueues. Without this re-set,
	// any RequestDispatch op already queued behind finalizeRunOp would
	// observe IsClaimed=false (Running gone, Claimed gone, RetryAttempts
	// not yet set) and dispatch the issue immediately — bypassing
	// backoff and racing a phantom retry timer against a live worker.
	// scheduleRetryOp's call to OrchestratorState.ScheduleRetry re-sets
	// Claimed idempotently, so this is safe.
	// Use f.entry.Issue, not f.issue: reconciliation may have refreshed
	// the tracker state mid-run, and the retry must carry the live state.
	issue := f.entry.Issue
	st.Claimed[f.id] = struct{}{}
	st.ClaimedIssues[f.id] = issue
	close(f.done)
	o := f.o
	identifier := f.identifier
	return func() {
		_ = o.ScheduleRetry(o.runCtx, issue, identifier, nextAttempt, runErr)
	}
}

// spawn asks the dispatcher for a worker, records the Running entry
// through the actor, and starts the watcher goroutine that submits
// finalizeRunOp on worker exit. The caller must already hold the
// Claimed slot for id (set by dispatchOp.apply or persisted across
// retryFireOp.apply); spawn does not check IsClaimed.
//
// spawn is invoked from a followup goroutine, never from inside an
// apply method, so its calls into o.submit are safe.
func (o *Orchestrator) spawn(id IssueID, issue tracker.Issue, attempt *int, continuationAttempt int) {
	runCtx, cancel := context.WithCancel(o.runCtx)
	startedAt := time.Now()
	workerDone := make(chan struct{})
	entry := &RunningEntry{
		Issue:               issue,
		Identifier:          issue.Identifier,
		StartedAt:           startedAt,
		RetryAttempt:        attempt,
		ContinuationAttempt: continuationAttempt,
		CancelWorker:        cancel,
		Done:                workerDone,
	}
	registered := make(chan struct{})
	if err := o.submit(o.runCtx, opFunc(func(st *OrchestratorState) func() {
		st.Running[id] = entry
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventRunning, IssueID: id, Identifier: issue.Identifier, Message: "dispatched to agent", At: startedAt})
		close(registered)
		return nil
	})); err != nil {
		cancel()
		close(workerDone)
		return
	}
	select {
	case <-registered:
	case <-o.runCtx.Done():
		cancel()
		close(workerDone)
		return
	}
	resultCh := o.dispatcher.Spawn(runCtx, issue, attempt)
	go func() {
		defer recoverPanic("orchestrator.spawn_result_fanout")
		var res WorkerResult
		select {
		case r, ok := <-resultCh:
			cancel()
			if ok {
				res = r
			} else {
				// Dispatcher closed without yielding a result: treat
				// as a cancellation, which becomes an abnormal exit
				// and triggers a retry per SPEC §7.3.
				res = WorkerResult{Err: context.Canceled, Elapsed: time.Since(startedAt)}
			}
		case <-o.runCtx.Done():
			cancel()
			close(workerDone)
			return
		}
		// workerDone is closed in exactly one path: either by
		// finalizeRunOp.apply once the actor accepts this submit, or by this
		// goroutine when submit fails because o.runCtx was canceled before
		// the actor accepted the op (typical SIGTERM shutdown race).
		// Dropping the submit error here would leak the close and stall every
		// consumer waiting on entry.Done — including reconcile termination and
		// the graceful-shutdown drain path.
		if err := o.submit(o.runCtx, &finalizeRunOp{
			o:          o,
			id:         id,
			issue:      issue,
			identifier: issue.Identifier,
			attempt:    attempt,
			result:     res,
			started:    startedAt,
			entry:      entry,
			done:       workerDone,
		}); err != nil {
			close(workerDone)
		}
	}()
}
