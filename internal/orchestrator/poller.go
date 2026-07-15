package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
	"github.com/xrf9268-hue/aiops-platform/internal/workspace"
)

var errDispatchPreflight = errors.New("dispatch preflight failed")

// ActiveIssueLister is the tracker reader required by the SPEC poll tick.
type ActiveIssueLister interface {
	ListActiveIssues(ctx context.Context) ([]tracker.Issue, error)
}

// IssueStateLister is the tracker reader required for per-tick
// reconciliation. Unlike ListActiveIssues, it can fetch explicit terminal and
// inactive workflow states so a poll tick can cancel already-running work when
// the tracker says the issue left the active set.
type IssueStateLister interface {
	ListIssuesByStates(ctx context.Context, states []string) ([]tracker.Issue, error)
}

// IssueStateRefresher is the optional SPEC §11.2 narrow state-refresh reader.
// When present, reconciliation refreshes in-flight issue states by explicit ID
// instead of relying on a wide active-state listing.
type IssueStateRefresher interface {
	FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]tracker.IssueState, error)
}

type issueStateRefresherByRefs interface {
	FetchIssueStatesByRefs(ctx context.Context, issueRefs []tracker.IssueRef) (map[string]tracker.IssueState, error)
}

type issueStateRefresherWithoutBlockersByRefs interface {
	FetchIssueStatesWithoutBlockersByRefs(ctx context.Context, issueRefs []tracker.IssueRef) (map[string]tracker.IssueState, error)
}

func fetchIssueStates(ctx context.Context, refresher IssueStateRefresher, refs []tracker.IssueRef) (map[string]tracker.IssueState, error) {
	if refresher == nil || len(refs) == 0 {
		return map[string]tracker.IssueState{}, nil
	}
	if refRefresher, ok := refresher.(issueStateRefresherByRefs); ok {
		return refRefresher.FetchIssueStatesByRefs(ctx, refs)
	}
	issueIDs := make([]string, 0, len(refs))
	for _, ref := range refs {
		issueIDs = append(issueIDs, ref.ID)
	}
	return refresher.FetchIssueStatesByIDs(ctx, issueIDs)
}

func fetchIssueStatesWithoutBlockers(ctx context.Context, refresher IssueStateRefresher, refs []tracker.IssueRef) (map[string]tracker.IssueState, error) {
	if refresher == nil || len(refs) == 0 {
		return map[string]tracker.IssueState{}, nil
	}
	if noBlockers, ok := refresher.(issueStateRefresherWithoutBlockersByRefs); ok {
		return noBlockers.FetchIssueStatesWithoutBlockersByRefs(ctx, refs)
	}
	return fetchIssueStates(ctx, refresher, refs)
}

// ReconciliationConfig names the workflow states the poller uses to decide
// whether in-process work is still eligible to run. A running issue absent from
// active states is canceled once it is observed in either terminal states or in
// the known inactive states listed here.
type ReconciliationConfig struct {
	ActiveStates   []string
	TerminalStates []string
	InactiveStates []string

	// RequiredLabels is the SPEC §6.4 opt-in dispatch gate
	// (workflow.TrackerConfig.RequiredLabels): an issue must carry every
	// label here (matched case-insensitively after trimming) to be
	// dispatched or to keep running. Empty disables the gate. Already
	// normalized at config load; issueHasRequiredLabels re-normalizes both
	// sides defensively.
	RequiredLabels []string

	// WorkerExitTimeout bounds how long a poll tick waits after issuing a
	// reconciliation cancel. Zero means the poll tick only requests cancellation;
	// the worker watcher will clean up asynchronously.
	WorkerExitTimeout time.Duration

	// StallTimeoutMs is SPEC §8.5 Part A's `codex.stall_timeout_ms`: the
	// per-issue budget for "no runtime event observed" before the orchestrator
	// cancels the worker so it can be retried (SPEC §16.3
	// reconcile_stalled_runs). The runner has its own self-stall detection;
	// this guards against the case where the runner goroutine itself wedges
	// and never produces a StallError. Zero (or negative) disables detection
	// at the orchestrator layer.
	StallTimeoutMs int
}

// Poller connects tracker polling to the orchestrator runtime state. It has no
// durable queue dependency: candidates are read from the tracker and claimed by
// the in-process Orchestrator actor.
type Poller struct {
	tracker        ActiveIssueLister
	stateTracker   IssueStateLister
	orchestrator   *Orchestrator
	overflow       []tracker.Issue
	reconcile      ReconciliationConfig
	reconcileKnown bool
	// preflight is the workflow.Config used for SPEC §8.1 step 2
	// dispatch-preflight validation. nil disables the gate (legacy
	// constructors / tests). RuntimePoller sets it on every workflow
	// snapshot reload so `$VAR` resolution drift is detected on the
	// next tick rather than at the next tracker call.
	preflight *workflow.Config
}

// NewPollerWithReconciliation returns a poller that reconciles the
// orchestrator's in-memory running/retry state against tracker state on every
// tick before considering new dispatches. It preserves the SPEC boundary: the
// orchestrator reads tracker state and cancels workers, while tracker writes
// remain agent-side.
func NewPollerWithReconciliation(tracker IssueStateLister, orchestrator *Orchestrator, cfg ReconciliationConfig) *Poller {
	return &Poller{tracker: activeIssueListerFromStates{tracker: tracker, states: cfg.ActiveStates}, stateTracker: tracker, orchestrator: orchestrator, reconcile: cfg, reconcileKnown: true}
}

// PollOnce performs one tracker tick: fetch active issues and ask the
// orchestrator actor to dispatch each candidate. Duplicate candidates are
// ignored by the actor's runtime claim state.
func (p *Poller) PollOnce(ctx context.Context) error { //nolint:gocognit // baseline (#521)
	if p == nil || p.tracker == nil {
		return errors.New("orchestrator poller requires tracker")
	}
	if p.orchestrator == nil {
		return errors.New("orchestrator poller requires orchestrator")
	}
	var pollErr error
	if err := p.orchestrator.ReconcileBudgetExceededRuns(ctx, p.reconcile.WorkerExitTimeout); err != nil {
		if ctx.Err() != nil {
			return err
		}
		pollErr = errors.Join(pollErr, err)
	}
	var reconciliation tickReconciliation
	if p.reconcileKnown {
		var err error
		reconciliation, err = p.reconcileClaimedTick(ctx, nil)
		if err != nil {
			pollErr = errors.Join(pollErr, err)
		}
	}
	if p.preflight != nil {
		if err := validateDispatchPreflight(*p.preflight); err != nil {
			preflightErr := fmt.Errorf("%w: %w", errDispatchPreflight, err)
			return errors.Join(pollErr, preflightErr, p.orchestrator.recordPreflightFailed(ctx, err))
		}
	}
	issues, activeErr := p.tracker.ListActiveIssues(ctx)
	// Multi-tracker clients return (issues, errors.Join(...)) on partial success;
	// keep the successful issues and join activeErr into pollErr below.
	if activeErr != nil {
		pollErr = errors.Join(pollErr, activeErr)
	}
	if p.reconcileKnown && len(issues) > 0 {
		if err := p.refreshListedActiveIssues(ctx, issues, reconciliation.refreshed); err != nil {
			pollErr = errors.Join(pollErr, err)
		}
	}
	if activeErr != nil && len(issues) == 0 {
		return pollErr
	}
	candidates := filterEligibleCandidates(mergeOverflowCandidates(p.overflow, issues), p.reconcile.TerminalStates, p.reconcile.RequiredLabels)
	if len(reconciliation.inactive) > 0 {
		candidates = filterIssuesNotInMap(candidates, reconciliation.inactive)
	}
	sortCandidates(candidates)
	p.overflow = nil
	candidates, revalidateErr := p.revalidateDispatchCandidates(ctx, candidates)
	if revalidateErr != nil {
		pollErr = errors.Join(pollErr, revalidateErr)
	}
	var dispatchErr error
	for _, issue := range candidates {
		if issue.ID == "" {
			continue
		}
		if err := p.orchestrator.RequestDispatchAfterTrackerRecheck(ctx, issue, nil); err != nil {
			switch {
			case errors.Is(err, ErrNotDispatched):
				continue
			case errors.Is(err, ErrCapacityFull):
				p.overflow = append(p.overflow, issue)
			default:
				dispatchErr = errors.Join(dispatchErr, fmt.Errorf("dispatch %s: %w", issue.ID, err))
			}
		}
	}
	return errors.Join(pollErr, dispatchErr)
}

type activeIssueListerFromStates struct {
	tracker IssueStateLister
	states  []string
}

func (l activeIssueListerFromStates) ListActiveIssues(ctx context.Context) ([]tracker.Issue, error) {
	return l.tracker.ListIssuesByStates(ctx, l.states)
}

type tickReconciliation struct {
	inactive  map[string]tracker.Issue
	refreshed map[string]tracker.Issue
}

type claimedNarrowRefresh struct {
	activeIssuesByID    map[string]tracker.Issue
	activeStateKeys     map[string]struct{}
	refreshedIssuesByID map[string]tracker.Issue
}

type claimedInactiveResult struct {
	issues       map[string]tracker.Issue
	deriveErr    error
	reconcileErr error
}

func (p *Poller) reconcileTick(ctx context.Context, activeIssues []tracker.Issue) (map[string]tracker.Issue, error) {
	result, err := p.reconcileClaimedTick(ctx, activeIssues)
	return result.inactive, err
}

func (p *Poller) reconcileClaimedTick(ctx context.Context, activeIssues []tracker.Issue) (tickReconciliation, error) {
	var result tickReconciliation
	if p.stateTracker == nil {
		return result, errors.New("orchestrator poller reconciliation requires state tracker")
	}
	// D37 budget reconciliation already ran before the tracker-dependent active
	// listing, so SPEC §16.3 Part A can treat remaining quiet stale runs as
	// ordinary stalls. A WorkerExitTimeout on a worker that ignores cancellation
	// surfaces as context.DeadlineExceeded; surface that as a non-fatal poll
	// error so one stuck run cannot block Part B from reconciling unrelated
	// inactive/terminal issues in the same tick.
	fetchErr := p.orchestrator.ReconcileStalledRuns(ctx, p.reconcile.StallTimeoutMs, p.reconcile.WorkerExitTimeout)
	if fetchErr != nil && ctx.Err() != nil {
		return result, fetchErr
	}
	issueRefs := p.orchestrator.RunningRetryingAndBlockedIssueRefs(ctx)
	if len(issueRefs) == 0 {
		return result, fetchErr
	}
	narrow, narrowErr := p.refreshClaimedNarrowPhase(ctx, activeIssues, issueRefs)
	fetchErr = errors.Join(fetchErr, narrowErr)
	result.refreshed = narrow.refreshedIssuesByID
	if err := p.applyClaimedNarrowPhase(ctx, narrow); err != nil {
		return result, errors.Join(fetchErr, err)
	}
	inactive := p.reconcileClaimedInactivePhase(ctx, activeIssues, narrow)
	result.inactive = inactive.issues
	fetchErr = errors.Join(fetchErr, inactive.deriveErr)
	return result, errors.Join(inactive.reconcileErr, fetchErr)
}

func (p *Poller) refreshClaimedNarrowPhase(ctx context.Context, activeIssues []tracker.Issue, issueRefs []tracker.IssueRef) (claimedNarrowRefresh, error) {
	refresh := claimedNarrowRefresh{
		activeIssuesByID: issueMap(activeIssues),
		activeStateKeys:  normalizedStates(p.reconcile.ActiveStates),
	}
	var err error
	refresh.refreshedIssuesByID, err = p.refreshRunningIssueStates(ctx, refresh.activeIssuesByID, issueRefs)
	return refresh, err
}

func (p *Poller) applyClaimedNarrowPhase(ctx context.Context, refresh claimedNarrowRefresh) error {
	if err := p.orchestrator.PatchActiveClaimedTrackerIssueStates(ctx, refresh.refreshedIssuesByID, refresh.activeStateKeys); err != nil {
		return err
	}
	mergeRefreshedActiveStates(refresh.activeIssuesByID, refresh.refreshedIssuesByID, refresh.activeStateKeys)
	// The active listing may be partial, so absence is "no information." Only
	// refresh stored metadata for explicit active observations here.
	if len(refresh.activeIssuesByID) == 0 {
		return nil
	}
	return p.orchestrator.RefreshActiveTrackerIssues(ctx, refresh.activeIssuesByID, refresh.activeStateKeys)
}

func (p *Poller) reconcileClaimedInactivePhase(ctx context.Context, activeIssues []tracker.Issue, refresh claimedNarrowRefresh) claimedInactiveResult {
	activeEvidenceByID := issueMap(activeIssues)
	for id, issue := range refresh.refreshedIssuesByID {
		if isActiveTrackerState(issue.State, refresh.activeStateKeys) {
			activeEvidenceByID[id] = issue
		}
	}
	inactiveByID, deriveErr := p.deriveInactiveIssues(ctx, activeEvidenceByID, refresh.refreshedIssuesByID, refresh.activeStateKeys)
	reconcileErr := p.orchestrator.ReconcileInactiveTrackerIssuesAndWait(ctx, inactiveByID, normalizedStates(p.reconcile.TerminalStates), p.reconcile.WorkerExitTimeout)
	return claimedInactiveResult{issues: inactiveByID, deriveErr: deriveErr, reconcileErr: reconcileErr}
}

func (p *Poller) refreshListedActiveIssues(ctx context.Context, issues []tracker.Issue, refreshed map[string]tracker.Issue) error {
	issuesByID := issueMap(issues)
	activeStateKeys := normalizedStates(p.reconcile.ActiveStates)
	mergeRefreshedActiveStates(issuesByID, refreshed, activeStateKeys)
	return p.orchestrator.RefreshActiveTrackerIssues(ctx, issuesByID, activeStateKeys)
}

// mergeRefreshedActiveStates folds the narrow per-issue state refresh back into
// the active set: issues still in an active state keep their stored entry with
// the refreshed state copied in (value-copy update, not pointer mutation),
// while issues that left the active set are dropped from activeIssuesByID. It
// mutates activeIssuesByID in place and treats refreshedIssuesByID as read-only.
func mergeRefreshedActiveStates(activeIssuesByID, refreshedIssuesByID map[string]tracker.Issue, activeStateKeys map[string]struct{}) {
	for id, issue := range refreshedIssuesByID {
		if isActiveTrackerState(issue.State, activeStateKeys) {
			if existing, ok := activeIssuesByID[id]; ok {
				activeIssuesByID[id] = patchTrackerIssueState(existing, issue)
			}
		} else {
			delete(activeIssuesByID, id)
		}
	}
}

// deriveInactiveIssues builds the SPEC §16.3 inactive/terminal set for the
// inactive reconcile pass: refreshed running issues that left the active set
// for a configured inactive/terminal state, plus explicit terminal/inactive
// state-group listings (skipping empty IDs and issues still considered active).
// The returned error joins any per-group listing errors in loop order; it is
// non-fatal (accumulated into fetchErr by the caller). activeIssuesByID is read
// only here; refreshedIssuesByID is read-only throughout.
func (p *Poller) deriveInactiveIssues(ctx context.Context, activeIssuesByID, refreshedIssuesByID map[string]tracker.Issue, activeStateKeys map[string]struct{}) (map[string]tracker.Issue, error) {
	activeByID := issueMapIDSet(activeIssuesByID)
	inactiveByID := make(map[string]tracker.Issue)
	for id, issue := range refreshedIssuesByID {
		if !p.refreshedIssueIsInactive(issue, activeStateKeys) {
			continue
		}
		delete(activeByID, id)
		inactiveByID[id] = issue
	}
	groupErr := p.appendInactiveFromStateGroups(ctx, inactiveByID, activeByID)
	return inactiveByID, groupErr
}

// refreshedIssueIsInactive reports whether a narrow-refreshed in-flight issue
// should be reconciled as inactive. Two SPEC paths converge here: an issue that
// left the active set for a configured inactive/terminal state (SPEC §16.3), and
// an issue still in an active state but no longer carrying every required label
// (SPEC §6.4 "continue" gate). Sourcing the label check from the refreshed set —
// which the narrow refresh queries by claimed ref — covers running/blocked/retry
// issues even when they sit beyond the active-listing page, and only acts on
// rows the tracker actually returned, preserving the no-information-on-absence
// invariant. The label clause is a no-op when required_labels is empty.
func (p *Poller) refreshedIssueIsInactive(issue tracker.Issue, activeStateKeys map[string]struct{}) bool {
	if isActiveTrackerState(issue.State, activeStateKeys) {
		return !issueHasRequiredLabels(issue, p.reconcile.RequiredLabels)
	}
	return p.isConfiguredInactiveState(issue.State)
}

// appendInactiveFromStateGroups fetches the explicit terminal/inactive
// state-group listings and adds each freshly-listed issue to inactiveByID,
// skipping empty IDs and issues still considered active. It mutates
// inactiveByID in place and reads activeByID only; per-group listing errors are
// joined in loop order and returned non-fatally.
func (p *Poller) appendInactiveFromStateGroups(ctx context.Context, inactiveByID map[string]tracker.Issue, activeByID map[string]struct{}) error {
	var groupErr error
	for _, states := range p.reconcileInactiveStateGroups() {
		issues, err := p.stateTracker.ListIssuesByStates(ctx, states)
		if err != nil {
			groupErr = errors.Join(groupErr, err)
			continue
		}
		addInactiveListedIssues(inactiveByID, activeByID, issues)
	}
	return groupErr
}

// addInactiveListedIssues adds each freshly-listed issue to inactiveByID,
// skipping empty IDs and issues still considered active. It mutates inactiveByID
// in place and reads activeByID only.
func addInactiveListedIssues(inactiveByID map[string]tracker.Issue, activeByID map[string]struct{}, issues []tracker.Issue) {
	for _, issue := range issues {
		if issue.ID == "" {
			continue
		}
		if _, active := activeByID[issue.ID]; active {
			continue
		}
		inactiveByID[issue.ID] = issue
	}
}

func (p *Poller) refreshRunningIssueStates(ctx context.Context, activeIssuesByID map[string]tracker.Issue, issueRefs []tracker.IssueRef) (map[string]tracker.Issue, error) {
	refresher, ok := p.stateTracker.(IssueStateRefresher)
	if !ok {
		return nil, nil
	}
	statesByID, err := fetchIssueStates(ctx, refresher, issueRefs)
	refsByID := make(map[string]tracker.IssueRef, len(issueRefs))
	for _, ref := range issueRefs {
		refsByID[ref.ID] = ref
	}
	refreshed := make(map[string]tracker.Issue, len(statesByID))
	for id, st := range statesByID {
		if strings.TrimSpace(id) == "" || strings.TrimSpace(st.State) == "" {
			continue
		}
		issue, ok := activeIssuesByID[id]
		if !ok {
			issue = tracker.Issue{ID: id, Identifier: refsByID[id].Identifier}
		}
		issue.ID = id
		issue.State = st.State
		// Carry refreshed labels so deriveInactiveIssues can observe SPEC §6.4
		// label removal on in-flight issues that sit beyond the active-listing
		// page (the narrow refresh queries them by claimed ref, the listing may
		// not). Only rows the tracker actually returned with a non-empty state
		// reach here, so absence stays "no information" (never a mass-cancel).
		issue.Labels = st.Labels
		issue.BlockedBy = st.BlockedBy
		refreshed[id] = issue
	}
	if err == nil {
		err = p.releaseVanishedContinuations(ctx, issueRefs, statesByID)
	}
	return refreshed, err
}

// releaseVanishedContinuations releases queued continuation entries whose
// issue a CLEAN narrow refresh was asked about but did not return with a
// usable state (deleted, or a Gitea issue whose aiops/* state labels were
// stripped). Reconcile's cancel paths deliberately treat absence as
// no-information, so without this sweep such a continuation wedges in
// RetryAttempts/Claimed forever — the poll loop never lists the issue again
// and nothing else releases it (#740 review). Gated on err == nil: with a
// failed fetch a missing row is indistinguishable from tracker downtime.
// Kind filtering (continuations only, release-only) happens actor-side in
// ReleaseVanishedContinuations.
func (p *Poller) releaseVanishedContinuations(ctx context.Context, queried []tracker.IssueRef, statesByID map[string]tracker.IssueState) error {
	vanished := make([]tracker.IssueRef, 0, len(queried))
	for _, ref := range queried {
		if st, ok := statesByID[ref.ID]; !ok || strings.TrimSpace(st.State) == "" {
			vanished = append(vanished, ref)
		}
	}
	if len(vanished) == 0 {
		return nil
	}
	return p.orchestrator.ReleaseVanishedContinuations(ctx, vanished)
}

func (p *Poller) reconcileInactiveStateGroups() [][]string {
	groups := make([][]string, 0, 2)
	if states := nonEmptyStateList(p.reconcile.TerminalStates); len(states) > 0 {
		groups = append(groups, states)
	}
	if states := nonEmptyStateList(p.reconcile.InactiveStates); len(states) > 0 {
		groups = append(groups, states)
	}
	return groups
}

func (p *Poller) isConfiguredInactiveState(state string) bool {
	configured := normalizedStates(append(append([]string(nil), p.reconcile.TerminalStates...), p.reconcile.InactiveStates...))
	_, ok := configured[strings.ToLower(strings.TrimSpace(state))]
	return ok
}

func nonEmptyStateList(states []string) []string {
	out := make([]string, 0, len(states))
	for _, state := range states {
		if strings.TrimSpace(state) != "" {
			out = append(out, state)
		}
	}
	return out
}

func issueMapIDSet(issues map[string]tracker.Issue) map[string]struct{} {
	out := make(map[string]struct{}, len(issues))
	for id := range issues {
		if id != "" {
			out[id] = struct{}{}
		}
	}
	return out
}

func issueMap(issues []tracker.Issue) map[string]tracker.Issue {
	out := make(map[string]tracker.Issue, len(issues))
	for _, issue := range issues {
		if issue.ID != "" {
			out[issue.ID] = issue
		}
	}
	return out
}

func normalizedStates(states []string) map[string]struct{} {
	out := make(map[string]struct{}, len(states))
	for _, state := range states {
		state = strings.ToLower(strings.TrimSpace(state))
		if state != "" {
			out[state] = struct{}{}
		}
	}
	return out
}

// TaskBuilder converts a tracker candidate into the task shape consumed by the
// existing worker runner.
type TaskBuilder func(issue tracker.Issue) (task.Task, error)

// WorkerTaskDispatcher runs the existing worker task executor for issues
// accepted by the orchestrator actor. It replaces the old Postgres claim loop:
// the orchestrator owns scheduling/claim state, while worker.RunTask
// continues to prepare workspaces and run the configured agent.
type WorkerTaskDispatcher struct {
	BuildTask         TaskBuilder
	Config            worker.Config
	Emitter           worker.EventEmitter
	WorkspacePrepared func(context.Context, tracker.Issue, task.Task, string)
}

// Spawn implements Dispatcher.
func (d WorkerTaskDispatcher) Spawn(ctx context.Context, issue tracker.Issue, attempt *int, opts DispatchOptions) <-chan WorkerResult {
	var copiedAttempt *int
	if attempt != nil {
		attemptValue := *attempt
		copiedAttempt = &attemptValue
	}
	cfg := configWithDispatchOptions(d.Config, opts)
	out := make(chan WorkerResult, 1)
	go func() {
		defer close(out)
		defer recoverPanic("orchestrator.worker_task_dispatcher")
		start := time.Now()
		tk, err := d.buildTaskWithAttempt(issue, copiedAttempt)
		if err != nil {
			out <- WorkerResult{Err: err, Elapsed: time.Since(start)}
			return
		}
		if d.WorkspacePrepared != nil {
			d.WorkspacePrepared(ctx, issue, tk, workspacePathForTask(cfg, tk))
		}
		runResult, rterr := worker.RunTaskWithResult(ctx, d.Emitter, tk, cfg)
		if rterr != nil {
			out <- WorkerResult{
				Err:           rterr.Err,
				InputRequired: runner.IsInputRequired(rterr.Err),
				Elapsed:       time.Since(start),
			}
			return
		}
		out <- WorkerResult{Elapsed: time.Since(start), IssueExitState: runResult.IssueExitState}
	}()
	return out
}

func configWithDispatchOptions(cfg worker.Config, opts DispatchOptions) worker.Config {
	cfg.CleanTurnBudget = opts.CleanTurnBudget
	return cfg
}

// workspacePathForTask resolves where the worker will materialize tk's
// worktree. Runtime dispatcher, cmd/worker, and e2e harness constructors all
// set cfg.Workflow before reaching this path.
func workspacePathForTask(cfg worker.Config, tk task.Task) string {
	return workspace.New(worker.EffectiveWorkspaceRoot(cfg, cfg.Workflow.Config)).PathFor(tk)
}

func (d WorkerTaskDispatcher) buildTask(issue tracker.Issue) (task.Task, error) {
	if d.BuildTask == nil {
		return task.Task{}, errors.New("worker task dispatcher requires task builder")
	}
	tk, err := d.BuildTask(issue)
	return tk, err
}

func (d WorkerTaskDispatcher) buildTaskWithAttempt(issue tracker.Issue, attempt *int) (task.Task, error) {
	tk, err := d.buildTask(issue)
	if err != nil {
		return task.Task{}, err
	}
	if attempt != nil {
		tk.Attempts = *attempt + 1
	}
	return tk, nil
}

// TaskFromIssue builds the in-memory task handed to worker execution for a
// tracker candidate. Dedupe/claiming lives in OrchestratorState, not in this
// task ID: the ID is only a stable per-run/workspace identifier.
func TaskFromIssue(issue tracker.Issue, cfg workflow.Config) (task.Task, error) {
	if cfg.Repo.CloneURL == "" {
		return task.Task{}, fmt.Errorf("repo.clone_url missing in WORKFLOW.md")
	}
	sourceType := cfg.Tracker.Kind + "_issue"
	if cfg.Tracker.Kind == "" {
		sourceType = "tracker_issue"
	}
	sourceEventID := issue.ID
	if sourceEventID == "" {
		return task.Task{}, fmt.Errorf("tracker issue id is required")
	}
	if issue.Identifier != "" {
		sourceEventID = issue.Identifier
	}
	return task.Task{
		ID:            string(IssueID(issue.ID)),
		SourceType:    sourceType,
		SourceEventID: sourceEventID,
		RepoOwner:     cfg.Repo.Owner,
		RepoName:      cfg.Repo.Name,
		CloneURL:      cfg.Repo.CloneURL,
		BaseBranch:    cfg.Repo.DefaultBranch,
		WorkBranch:    "ai/" + issue.ID,
		Title:         fmt.Sprintf("%s %s", issue.Identifier, issue.Title),
		Description:   issue.Description + "\n\nTracker: " + issue.URL,
		Actor:         cfg.Tracker.Kind,
		Model:         cfg.Agent.Default,
		Priority:      50,
		IssueRender:   IssueRenderVars(issue),
	}, nil
}

// IssueRenderVars returns the SPEC §4.1.1 normalized issue snapshot the
// prompt template's `issue` variable expects. Field set matches SPEC §12.1
// "includes all normalized issue fields, including labels and blockers"; per
// SPEC §5.4 strict template rendering, every field documented in §4.1.1
// must be present (or empty) so a workflow that references it does not crash
// with template_render_error. Labels and blocked_by are always slices (never
// nil) so `{% for ... %}` over an empty issue does not surface a strict-mode
// error. The actor field is an aiops-platform extension carried alongside
// the SPEC fields and read from the tracker kind by the caller.
func IssueRenderVars(issue tracker.Issue) map[string]any {
	labels := append([]string(nil), issue.Labels...)
	if labels == nil {
		labels = []string{}
	}
	blockedBy := make([]map[string]any, 0, len(issue.BlockedBy))
	for _, b := range issue.BlockedBy {
		blockedBy = append(blockedBy, map[string]any{
			"id":         b.ID,
			"identifier": b.Identifier,
			"state":      b.State,
		})
	}
	return map[string]any{
		"id":          issue.ID,
		"identifier":  issue.Identifier,
		"title":       issue.Title,
		"description": issue.Description,
		"priority":    issue.Priority,
		"state":       issue.State,
		"branch_name": issue.BranchName,
		"url":         issue.URL,
		"labels":      labels,
		"blocked_by":  blockedBy,
		"created_at":  issue.CreatedAt,
		"updated_at":  issue.UpdatedAt,
	}
}
