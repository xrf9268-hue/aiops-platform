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
	"sync"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// WorkerResult is the per-run outcome the Dispatcher delivers when its
// spawned worker exits. Err is nil for SPEC §7.3 normal exit; a non-nil Err is
// an abnormal exit and triggers ScheduleRetry(attempt+1) with §8.4 backoff —
// SPEC §8.4/§16.6 retry unboundedly (a deterministic config/task-build failure
// re-checks tracker eligibility on each backoff-paced retry, matching upstream
// prompt_builder raise → retry_agent_down). Elapsed is folded into
// CodexTotals.SecondsRunning per SPEC §13.3.
type WorkerResult struct {
	Err           error
	InputRequired bool
	// IssueExitState marks a clean SPEC §16.5 self-stop: the runner's per-turn
	// tracker refresh observed the issue outside active states. The structured
	// snapshot lets finalize distinguish terminal self-stop from normal clean
	// continuation.
	IssueExitState *runner.IssueStateSnapshot
	Elapsed        time.Duration
}

// DispatchOptions carries per-run controls derived by the orchestrator for a
// single worker spawn. Zero values keep the workflow-level defaults.
type DispatchOptions struct {
	// CleanTurnBudget is the remaining D34 clean-turn budget for this dispatch.
	// Runner cap exhaustion at this budget is a clean stop, not a failure retry.
	CleanTurnBudget int
}

// Dispatcher is the seam through which the actor spawns a per-issue
// worker goroutine. The returned channel must yield exactly one
// WorkerResult and then close (or close without yielding to signal
// cancellation). The actor watches it to drive normal/abnormal exit
// transitions.
type Dispatcher interface {
	Spawn(ctx context.Context, issue tracker.Issue, attempt *int, opts DispatchOptions) <-chan WorkerResult
}

// Scheduler computes the delay before the next retry request.
type Scheduler interface {
	NextDelay(RetryRequest) time.Duration
}

// Deps bundles construction-time dependencies so adding a new one
// later doesn't ripple through every call site.
type Deps struct {
	Dispatcher Dispatcher
	Scheduler  Scheduler
	// CandidateLister, when set, enables the SPEC §16.6 retry-fire
	// candidate-fetch step. A fired failure-retry timer fetches the
	// active candidate list, confirms the issue is still present,
	// releases the claim when it is not, and surfaces a typed
	// "retry poll failed" reschedule when the fetch itself fails.
	// Production wires this from the same multi-tracker lister the
	// poll loop uses; when nil, retry fires dispatch directly from
	// the cached entry.Issue (legacy behavior, kept for unit tests).
	CandidateLister ActiveIssueLister
	// WorkspaceCleaner, when set, removes the workspace of a run that was
	// cancelled because its issue moved to a terminal tracker state mid-run
	// (SPEC §18.1 active transition). nil leaves cleanup to the startup
	// sweep. Production wires the RuntimeDispatcher, which fires before_remove
	// against the live workflow snapshot's hook config.
	WorkspaceCleaner WorkspaceCleaner
}

// Orchestrator is the SPEC §3.1 / §7.4 "single authority" that owns
// OrchestratorState. All mutations flow through the actor goroutine
// started by Run; reads via Snapshot also serialize through the actor
// so callers always observe a consistent view.
type Orchestrator struct {
	ops chan stateOp

	state      *OrchestratorState
	dispatcher Dispatcher
	retryWake  chan struct{}

	// workspaceCleaner removes the workspace of a terminal-state run after its
	// worker exits (SPEC §18.1 active transition). nil disables the active
	// cleanup; the startup sweep still reclaims the directory on next boot.
	workspaceCleaner WorkspaceCleaner

	// candidateLister supplies the SPEC §16.6 candidate fetch that a
	// fired failure-retry timer consults before re-dispatching. The
	// field is read on every retry fire and written when the runtime
	// poller rebuilds its tracker set, so a mutex guards the swap.
	candidateListerMu sync.Mutex
	candidateLister   ActiveIssueLister

	// retryTerminalResolver lets the SPEC §16.6 retry-fire path tell a
	// terminal issue from a merely-absent one when the active-candidate fetch
	// returns nothing for it. The candidate lister is active-only, so a
	// terminal issue is indistinguishable from a deleted one there; resolving
	// the actual state (the way the reconcile pass does) lets the found==nil
	// branch clean a terminal workspace through the §18.1 seam instead of
	// leaking it (#341). nil leaves the legacy release-only behavior. Guarded
	// by the same swap discipline as candidateLister.
	retryTerminalResolverMu sync.Mutex
	retryTerminalResolver   IssueStateRefresher
	retryTerminalStates     map[string]struct{}

	// scheduler computes retry backoff (SPEC §8.4/§16.6). It is read on every
	// retry fire — including the off-actor terminal-cleanup state-refresh retry
	// (#675) — and swapped on-actor by UpdateRetryScheduler when the runtime
	// poller reloads workflow timing, so a mutex guards the swap (same discipline
	// as candidateLister / retryTerminalResolver). currentScheduler is the only
	// read path.
	schedulerMu sync.Mutex
	scheduler   Scheduler

	// runCtx is captured by Run so followup goroutines can cancel
	// their work when the actor stops. Set once at the top of Run
	// before close(started); reads from outside the actor synchronize
	// via the started channel close.
	runCtx  context.Context
	started chan struct{}

	// workerWG tracks every spawn until its dispatcher result is consumed.
	// WaitForWorkers waits on it so a SIGTERM shutdown drains in-flight
	// agent subprocesses instead of orphaning them when main returns —
	// the explicit Go replacement for the ordered child termination a BEAM
	// supervision tree performs on shutdown (AGENTS.md cross-cutting
	// checklist item 2).
	workerWG sync.WaitGroup
}

// New constructs an Orchestrator over an existing state value. Callers
// must call Run(ctx) in a separate goroutine before the actor processes
// any submitted op; tests can wait via WaitStarted.
func New(state *OrchestratorState, deps Deps) *Orchestrator {
	o := &Orchestrator{
		ops:              make(chan stateOp),
		state:            state,
		dispatcher:       deps.Dispatcher,
		scheduler:        deps.Scheduler,
		retryWake:        make(chan struct{}, 1),
		candidateLister:  deps.CandidateLister,
		workspaceCleaner: deps.WorkspaceCleaner,
		started:          make(chan struct{}),
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

// SetRetryTerminalStateResolver installs (or swaps) the reader the §16.6
// retry-fire found==nil branch uses to tell a terminal issue from a
// merely-absent one (#341), along with the lowercased terminal-state set used
// to classify the resolved state. Production wires the same multi-tracker
// state reader and terminal_states the reconcile pass uses. nil/empty leaves
// the legacy release-only behavior (no workspace cleanup on retry-fire). Safe
// to call before or after Run.
func (o *Orchestrator) SetRetryTerminalStateResolver(refresher IssueStateRefresher, terminalStates []string) {
	if o == nil {
		return
	}
	o.retryTerminalResolverMu.Lock()
	o.retryTerminalResolver = refresher
	o.retryTerminalStates = normalizedStates(terminalStates)
	o.retryTerminalResolverMu.Unlock()
}
func (o *Orchestrator) currentRetryTerminalResolver() (IssueStateRefresher, map[string]struct{}) {
	if o == nil {
		return nil, nil
	}
	o.retryTerminalResolverMu.Lock()
	defer o.retryTerminalResolverMu.Unlock()
	return o.retryTerminalResolver, o.retryTerminalStates
}

// currentScheduler returns the live retry scheduler under schedulerMu so the
// off-actor terminal-cleanup retry can read it without racing an on-actor
// UpdateRetryScheduler swap (#675).
func (o *Orchestrator) currentScheduler() Scheduler {
	o.schedulerMu.Lock()
	defer o.schedulerMu.Unlock()
	return o.scheduler
}

func (o *Orchestrator) LookupOperatorTerminalStop(ctx context.Context, id IssueID) (OperatorTerminalStopEntry, bool, error) {
	reply := make(chan operatorTerminalStopLookupResult, 1)
	if err := o.submit(ctx, &lookupOperatorTerminalStopOp{id: id, result: reply}); err != nil {
		return OperatorTerminalStopEntry{}, false, err
	}
	select {
	case res := <-reply:
		return res.entry, res.ok, nil
	case <-ctx.Done():
		return OperatorTerminalStopEntry{}, false, ctx.Err()
	}
}

type operatorTerminalStopLookupResult struct {
	entry OperatorTerminalStopEntry
	ok    bool
}

type lookupOperatorTerminalStopOp struct {
	id     IssueID
	result chan<- operatorTerminalStopLookupResult
}

func (op *lookupOperatorTerminalStopOp) apply(st *OrchestratorState) func() {
	entry, ok := st.LookupOperatorTerminalStop(op.id)
	op.result <- operatorTerminalStopLookupResult{entry: entry, ok: ok}
	return nil
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
