package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
	"github.com/xrf9268-hue/aiops-platform/internal/workspace"
)

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
	FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error)
}

// ReconciliationConfig names the workflow states the poller uses to decide
// whether in-process work is still eligible to run. A running issue absent from
// active states is canceled once it is observed in either terminal states or in
// the known inactive states listed here.
type ReconciliationConfig struct {
	ActiveStates   []string
	TerminalStates []string
	InactiveStates []string

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
	routing        *workflow.Config
	// preflight is the workflow.Config used for SPEC §8.1 step 2
	// dispatch-preflight validation. nil disables the gate (legacy
	// constructors / tests). RuntimePoller sets it on every workflow
	// snapshot reload so `$VAR` resolution drift is detected on the
	// next tick rather than at the next tracker call.
	preflight *workflow.Config
}

// NewPoller returns a SPEC-aligned tracker poller backed by orchestrator-owned
// runtime state instead of the legacy Postgres task queue.
//
// Callers that do not supply a ReconciliationConfig still get the SPEC §5.3.1
// default terminal_states so the Todo blocker rule continues to honor
// "Done"/"Canceled"/etc. without the hardcoded overlay #232 removed.
// Operators who genuinely want to disable the blocker rule must construct via
// NewPollerWithReconciliation with an explicit empty terminal_states slice.
func NewPoller(tracker ActiveIssueLister, orchestrator *Orchestrator) *Poller {
	return &Poller{
		tracker:      tracker,
		orchestrator: orchestrator,
		reconcile:    ReconciliationConfig{TerminalStates: workflow.DefaultConfig().Tracker.TerminalStates},
	}
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
func (p *Poller) PollOnce(ctx context.Context) error {
	if p == nil || p.tracker == nil {
		return errors.New("orchestrator poller requires tracker")
	}
	if p.orchestrator == nil {
		return errors.New("orchestrator poller requires orchestrator")
	}
	if p.preflight != nil {
		if err := validateDispatchPreflight(*p.preflight); err != nil {
			if recErr := p.orchestrator.recordPreflightFailed(ctx, err); recErr != nil {
				return errors.Join(fmt.Errorf("dispatch preflight failed: %w", err), recErr)
			}
			return fmt.Errorf("dispatch preflight failed: %w", err)
		}
	}
	issues, activeErr := p.tracker.ListActiveIssues(ctx)
	// Multi-tracker clients return (issues, errors.Join(...)) on partial success;
	// keep the successful issues and join activeErr into pollErr below.
	if activeErr != nil && len(issues) == 0 {
		return activeErr
	}
	var pollErr error
	if activeErr != nil {
		pollErr = errors.Join(pollErr, activeErr)
	}
	routedIssues := issues
	if p.routing != nil {
		var routeErr error
		routedIssues, routeErr = selectRoutedCandidates(issues, *p.routing)
		if routeErr != nil {
			pollErr = errors.Join(pollErr, routeErr)
		}
	}
	var reconciledInactive map[string]tracker.Issue
	if p.reconcileKnown && activeErr == nil {
		var err error
		reconciledInactive, err = p.reconcileTick(ctx, routedIssues)
		if err != nil {
			pollErr = errors.Join(pollErr, err)
		}
	}
	candidates := filterEligibleCandidates(mergeOverflowCandidates(p.overflow, routedIssues), p.reconcile.TerminalStates)
	if len(reconciledInactive) > 0 {
		candidates = filterIssuesNotInMap(candidates, reconciledInactive)
	}
	sortCandidates(candidates)
	p.overflow = nil
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

func (p *Poller) reconcileTick(ctx context.Context, activeIssues []tracker.Issue) (map[string]tracker.Issue, error) {
	if p.stateTracker == nil {
		return nil, errors.New("orchestrator poller reconciliation requires state tracker")
	}
	var fetchErr error
	// SPEC §16.3 reconcile_running_issues order: Part A (stall reconciliation)
	// first so any wedged worker is cancelled before Part B's tracker-state
	// refresh would otherwise leave it claimed indefinitely. A WorkerExitTimeout
	// on a worker that ignores cancellation surfaces as context.DeadlineExceeded;
	// surface that as a non-fatal poll error so one stuck run cannot block Part B
	// from reconciling unrelated inactive/terminal issues in the same tick.
	if err := p.orchestrator.ReconcileStalledRuns(ctx, p.reconcile.StallTimeoutMs, p.reconcile.WorkerExitTimeout); err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		fetchErr = errors.Join(fetchErr, err)
	}
	activeIssuesByID := issueMap(activeIssues)
	activeStateKeys := normalizedStates(p.reconcile.ActiveStates)
	refreshedIssuesByID, err := p.refreshRunningIssueStates(ctx, activeIssuesByID)
	if err != nil {
		fetchErr = errors.Join(fetchErr, err)
	}
	for id, issue := range refreshedIssuesByID {
		if isActiveTrackerState(issue.State, activeStateKeys) {
			if existing, ok := activeIssuesByID[id]; ok {
				existing.State = issue.State
				activeIssuesByID[id] = existing
			}
		} else {
			delete(activeIssuesByID, id)
		}
	}
	if p.routing != nil {
		// Routing-aware listings are complete for their scope, so absence from
		// the active set is real evidence of inactivity and may cancel runs.
		//
		// NOTE(#331): a routed running issue that moves to a terminal state is
		// cancelled here (it drops out of the active set) before the inactive
		// pass below — which carries the terminal-state set — can flag it for
		// SPEC §18.1 active workspace cleanup. So in routing mode the terminal
		// workspace is reclaimed by the next startup sweep rather than mid-run.
		// The before_remove hook still fires on the active transition for the
		// non-routing (default) path; wiring it through the routing cancel is
		// tracked as #340.
		if err := p.orchestrator.ReconcileTrackerIssuesAndWait(ctx, activeIssuesByID, activeStateKeys, p.reconcile.WorkerExitTimeout); err != nil {
			return nil, err
		}
	} else {
		// Without routing the active listing may be partial; still refresh
		// stored issue metadata for runs we DO see so per-state capacity gates
		// observe the latest tracker state without treating absence as inactive.
		if err := p.orchestrator.RefreshActiveTrackerIssues(ctx, activeIssuesByID, activeStateKeys); err != nil {
			return nil, err
		}
	}
	activeByID := issueMapIDSet(activeIssuesByID)
	inactiveByID := make(map[string]tracker.Issue)
	for id, issue := range refreshedIssuesByID {
		if isActiveTrackerState(issue.State, activeStateKeys) {
			continue
		}
		if !p.isConfiguredInactiveState(issue.State) {
			continue
		}
		delete(activeByID, id)
		inactiveByID[id] = issue
	}
	for _, states := range p.reconcileInactiveStateGroups() {
		issues, err := p.stateTracker.ListIssuesByStates(ctx, states)
		if err != nil {
			fetchErr = errors.Join(fetchErr, err)
			continue
		}
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
	reconcileErr := p.orchestrator.ReconcileInactiveTrackerIssuesAndWait(ctx, inactiveByID, normalizedStates(p.reconcile.TerminalStates), p.reconcile.WorkerExitTimeout)
	return inactiveByID, errors.Join(reconcileErr, fetchErr)
}

func (p *Poller) refreshRunningIssueStates(ctx context.Context, activeIssuesByID map[string]tracker.Issue) (map[string]tracker.Issue, error) {
	refresher, ok := p.stateTracker.(IssueStateRefresher)
	if !ok {
		return nil, nil
	}
	issueIDs := p.orchestrator.RunningRetryingAndBlockedIssueIDs(ctx)
	if len(issueIDs) == 0 {
		return nil, nil
	}
	statesByID, err := refresher.FetchIssueStatesByIDs(ctx, issueIDs)
	refreshed := make(map[string]tracker.Issue, len(statesByID))
	for id, state := range statesByID {
		if strings.TrimSpace(id) == "" || strings.TrimSpace(state) == "" {
			continue
		}
		issue, ok := activeIssuesByID[id]
		if !ok {
			issue = tracker.Issue{ID: id}
		}
		issue.ID = id
		issue.State = state
		refreshed[id] = issue
	}
	return refreshed, err
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

func issueIDSet(issues []tracker.Issue) map[string]struct{} {
	out := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if issue.ID != "" {
			out[issue.ID] = struct{}{}
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

func filterIssuesNotInMap(issues []tracker.Issue, excluded map[string]tracker.Issue) []tracker.Issue {
	if len(excluded) == 0 {
		return issues
	}
	out := make([]tracker.Issue, 0, len(issues))
	for _, issue := range issues {
		if _, ok := excluded[issue.ID]; ok {
			continue
		}
		out = append(out, issue)
	}
	return out
}

func filterEligibleCandidates(issues []tracker.Issue, terminalStates []string) []tracker.Issue {
	// Honor exactly what the caller supplied per SPEC §5.3.1. Callers that
	// want the SPEC 5-state default get it from workflow.DefaultConfig at
	// construction time (NewPoller seeds it; workflow.Load supplies it for
	// omitted YAML). An explicit empty slice from
	// NewPollerWithReconciliation disables the blocker rule entirely, which
	// is the operator's call.
	terminal := normalizedStates(terminalStates)
	out := make([]tracker.Issue, 0, len(issues))
	for _, issue := range issues {
		if !issueHasRequiredCandidateFields(issue) {
			continue
		}
		if todoIssueBlockedByOpenDependency(issue, terminal) {
			continue
		}
		out = append(out, issue)
	}
	return out
}

func issueHasRequiredCandidateFields(issue tracker.Issue) bool {
	return strings.TrimSpace(issue.ID) != "" &&
		strings.TrimSpace(issue.Identifier) != "" &&
		strings.TrimSpace(issue.Title) != "" &&
		strings.TrimSpace(issue.State) != ""
}

func todoIssueBlockedByOpenDependency(issue tracker.Issue, terminalStates map[string]struct{}) bool {
	if !strings.EqualFold(strings.TrimSpace(issue.State), "Todo") {
		return false
	}
	for _, blocker := range issue.BlockedBy {
		state := strings.ToLower(strings.TrimSpace(blocker.State))
		if state == "" {
			return true
		}
		if _, terminal := terminalStates[state]; !terminal {
			return true
		}
	}
	return false
}

func sortCandidates(issues []tracker.Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		left, right := issues[i], issues[j]
		leftPriority := linearPrioritySortKey(left.Priority)
		rightPriority := linearPrioritySortKey(right.Priority)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		if compareCreatedAt(left.CreatedAt, right.CreatedAt) < 0 {
			return true
		}
		if compareCreatedAt(left.CreatedAt, right.CreatedAt) > 0 {
			return false
		}
		return left.Identifier < right.Identifier
	})
}

func compareCreatedAt(left, right time.Time) int {
	switch {
	case left.IsZero() && right.IsZero():
		return 0
	case left.IsZero():
		return 1
	case right.IsZero():
		return -1
	case left.Before(right):
		return -1
	case left.After(right):
		return 1
	default:
		return 0
	}
}

func linearPrioritySortKey(priority int) int {
	if priority == 0 {
		return 1 << 30
	}
	return priority
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

func mergeOverflowCandidates(overflow, fresh []tracker.Issue) []tracker.Issue {
	if len(overflow) == 0 {
		return fresh
	}
	candidates := make([]tracker.Issue, 0, len(overflow)+len(fresh))
	freshByID := make(map[string]tracker.Issue, len(fresh))
	seen := make(map[string]struct{}, len(overflow)+len(fresh))
	for _, issue := range fresh {
		if issue.ID == "" {
			continue
		}
		freshByID[issue.ID] = issue
	}
	for _, issue := range overflow {
		freshIssue, ok := freshByID[issue.ID]
		if !ok {
			continue
		}
		seen[issue.ID] = struct{}{}
		candidates = append(candidates, freshIssue)
	}
	for _, issue := range fresh {
		if _, ok := seen[issue.ID]; ok {
			continue
		}
		candidates = append(candidates, issue)
	}
	return candidates
}

func selectRoutedCandidates(issues []tracker.Issue, cfg workflow.Config) ([]tracker.Issue, error) {
	if len(cfg.Services) == 0 {
		return issues, nil
	}
	out := make([]tracker.Issue, 0, len(issues))
	var routeErr error
	for _, issue := range issues {
		matches := matchingServices(issue, cfg)
		switch len(matches) {
		case 0:
			if strings.TrimSpace(cfg.Repo.CloneURL) != "" && issueInRootTrackerProject(issue, cfg) {
				out = append(out, issue)
				continue
			}
			log.Printf("event=tracker_route_skipped issue_id=%s issue_identifier=%s reason=%q", issue.ID, issue.Identifier, "no configured service matched")
		case 1:
			issue.ServiceName = matches[0].Name
			out = append(out, issue)
		default:
			names := make([]string, 0, len(matches))
			for _, service := range matches {
				names = append(names, service.Name)
			}
			routeErr = errors.Join(routeErr, fmt.Errorf("ambiguous route for issue %s: matched services %s", issue.ID, strings.Join(names, ", ")))
		}
	}
	return out, routeErr
}

func issueInRootTrackerProject(issue tracker.Issue, cfg workflow.Config) bool {
	rootProject := strings.TrimSpace(cfg.Tracker.ProjectSlug)
	if rootProject == "" {
		return false
	}
	return strings.EqualFold(rootProject, strings.TrimSpace(issue.ProjectSlug))
}

func matchingServices(issue tracker.Issue, cfg workflow.Config) []workflow.ServiceConfig {
	matches := make([]workflow.ServiceConfig, 0, len(cfg.Services))
	for _, service := range cfg.Services {
		if serviceMatchesIssue(service, cfg.Tracker, issue) {
			matches = append(matches, service)
		}
	}
	return matches
}

func serviceMatchesIssue(service workflow.ServiceConfig, defaults workflow.TrackerConfig, issue tracker.Issue) bool {
	route := service.Tracker
	if !hasExplicitServiceRoute(route) {
		return false
	}
	projectSlug := strings.TrimSpace(route.ProjectSlug)
	if projectSlug == "" {
		projectSlug = strings.TrimSpace(defaults.ProjectSlug)
	}
	if projectSlug != "" && !strings.EqualFold(projectSlug, strings.TrimSpace(issue.ProjectSlug)) {
		return false
	}
	if route.TeamKey != "" && !strings.EqualFold(strings.TrimSpace(route.TeamKey), strings.TrimSpace(issue.TeamKey)) {
		return false
	}
	issueLabels := make(map[string]struct{}, len(issue.Labels))
	for _, label := range issue.Labels {
		if label = strings.ToLower(strings.TrimSpace(label)); label != "" {
			issueLabels[label] = struct{}{}
		}
	}
	for _, label := range route.Labels {
		if _, ok := issueLabels[strings.ToLower(strings.TrimSpace(label))]; !ok {
			return false
		}
	}
	for key, want := range route.CustomFields {
		got, ok := issue.CustomFields[key]
		if !ok || strings.TrimSpace(got) != strings.TrimSpace(want) {
			return false
		}
	}
	return true
}

func hasExplicitServiceRoute(route workflow.ServiceTrackerRouteConfig) bool {
	return strings.TrimSpace(route.ProjectSlug) != "" ||
		strings.TrimSpace(route.TeamKey) != "" ||
		len(route.Labels) > 0 ||
		len(route.CustomFields) > 0
}

// TaskBuilder converts a tracker candidate into the task shape consumed by the
// existing worker runner. This is an adapter between the SPEC poller/runtime
// path and the legacy task execution API; it is intentionally in-memory only.
type TaskBuilder func(issue tracker.Issue) (task.Task, error)

type BuiltTask struct {
	Task            task.Task
	RecordedQueueID string
}

// RecordedTaskBuilder converts a tracker candidate into a worker task and can
// return the queue row ID recorded by transitional compatibility paths. When a
// queue assigns the row ID, it may differ from the worker task's stable
// tracker-derived ID.
type RecordedTaskBuilder func(issue tracker.Issue) (BuiltTask, error)

// TaskCompleter is the optional queue compatibility hook implemented by
// queue.Store. The SPEC-aligned runtime path does not require durable rows, but
// tests and transitional tools that record a task row need it marked terminal
// after the in-memory worker exits successfully.
type TaskCompleter interface {
	Complete(ctx context.Context, id string) error
}

// WorkerTaskDispatcher runs the existing worker task executor for issues
// accepted by the orchestrator actor. It replaces the old Postgres claim loop:
// the orchestrator owns scheduling/claim state, while worker.RunTask
// continues to prepare workspaces and run the configured agent.
type WorkerTaskDispatcher struct {
	BuildTask         TaskBuilder
	BuildRecordedTask RecordedTaskBuilder
	Config            worker.Config
	Emitter           worker.EventEmitter
	WorkspacePrepared func(context.Context, tracker.Issue, task.Task, string)
}

// Spawn implements Dispatcher.
func (d WorkerTaskDispatcher) Spawn(ctx context.Context, issue tracker.Issue, attempt *int) <-chan WorkerResult {
	var copiedAttempt *int
	if attempt != nil {
		attemptValue := *attempt
		copiedAttempt = &attemptValue
	}
	out := make(chan WorkerResult, 1)
	go func() {
		defer close(out)
		defer recoverPanic("orchestrator.worker_task_dispatcher")
		start := time.Now()
		tk, recordedTaskID, err := d.buildTaskWithAttempt(issue, copiedAttempt)
		if err != nil {
			out <- WorkerResult{Err: err, NonRetryable: true, Elapsed: time.Since(start)}
			return
		}
		if d.WorkspacePrepared != nil {
			d.WorkspacePrepared(ctx, issue, tk, workspacePathForTask(d.Config, tk))
		}
		if rterr := worker.RunTask(ctx, d.Emitter, tk, d.Config); rterr != nil {
			out <- WorkerResult{Err: rterr.Err, NonRetryable: rterr.NonRetryable, InputRequired: runner.IsInputRequired(rterr.Err), Elapsed: time.Since(start)}
			return
		}
		if err := completeRecordedTask(ctx, d.Emitter, recordedTaskID, tk.ID); err != nil {
			out <- WorkerResult{Err: err, Elapsed: time.Since(start)}
			return
		}
		out <- WorkerResult{Elapsed: time.Since(start)}
	}()
	return out
}

// workspacePathForTask resolves where the worker will materialize tk's
// worktree. The dispatcher constructors (RuntimeDispatcher.Spawn,
// cmd/worker, e2e harness, every poller_test fixture) all set
// cfg.Workflow non-nil before reaching this path; the previous defensive
// nil branch was load-bearing only when LoadConfigFromEnv stamped a
// literal `/tmp/aiops-workspaces` onto cfg.WorkspaceRoot. Post-#319 that
// literal is gone, so a nil Workflow would yield an empty root and a
// useless workspace path — surfacing that as a nil-deref panic at the
// call site beats silently writing to "" further downstream.
func workspacePathForTask(cfg worker.Config, tk task.Task) string {
	return workspace.New(worker.EffectiveWorkspaceRoot(cfg, cfg.Workflow.Config)).PathFor(tk)
}

func (d WorkerTaskDispatcher) buildTask(issue tracker.Issue) (task.Task, string, error) {
	if d.BuildRecordedTask != nil {
		built, err := d.BuildRecordedTask(issue)
		return built.Task, built.RecordedQueueID, err
	}
	if d.BuildTask == nil {
		return task.Task{}, "", errors.New("worker task dispatcher requires task builder")
	}
	tk, err := d.BuildTask(issue)
	return tk, tk.ID, err
}

func (d WorkerTaskDispatcher) buildTaskWithAttempt(issue tracker.Issue, attempt *int) (task.Task, string, error) {
	tk, recordedTaskID, err := d.buildTask(issue)
	if err != nil {
		return task.Task{}, "", err
	}
	if attempt != nil {
		tk.Attempts = *attempt + 1
	}
	return tk, recordedTaskID, nil
}

// TaskFromIssue builds the in-memory task handed to worker execution for a
// tracker candidate. Dedupe/claiming lives in OrchestratorState, not in this
// task ID: the ID is only a stable per-run/workspace identifier.
func completeRecordedTask(ctx context.Context, ev worker.EventEmitter, recordedTaskID, fallbackTaskID string) error {
	if completer, ok := ev.(TaskCompleter); ok {
		if recordedTaskID != "" {
			return completer.Complete(ctx, recordedTaskID)
		}
		return completer.Complete(ctx, fallbackTaskID)
	}
	return nil
}

func TaskFromIssue(issue tracker.Issue, cfg workflow.Config) (task.Task, error) {
	if issue.ServiceName != "" {
		serviceCfg, ok := serviceConfigByName(cfg, issue.ServiceName)
		if !ok {
			return task.Task{}, fmt.Errorf("service %q not found in WORKFLOW.md", issue.ServiceName)
		}
		cfg.Repo = serviceCfg.Repo
	}
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
	if issue.ServiceName != "" {
		sourceEventID += "|service|" + issue.ServiceName
	} else if issue.Identifier != "" {
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

func serviceConfigByName(cfg workflow.Config, name string) (workflow.ServiceConfig, bool) {
	for _, service := range cfg.Services {
		if service.Name == name {
			return service, true
		}
	}
	return workflow.ServiceConfig{}, false
}

// RunPollLoop repeatedly polls the tracker until ctx is canceled.
func RunPollLoop(ctx context.Context, poller *Poller, interval time.Duration) error {
	if poller == nil {
		return errors.New("orchestrator poll loop requires poller")
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	intervalTimer := time.NewTimer(interval)
	if !intervalTimer.Stop() {
		<-intervalTimer.C
	}
	defer intervalTimer.Stop()
	for {
		if err := poller.PollOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("event=tracker_poll_error error=%q", err)
		}
		intervalTimer.Reset(interval)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poller.orchestrator.retryWakeCh():
			if !intervalTimer.Stop() {
				<-intervalTimer.C
			}
		case <-intervalTimer.C:
		}
	}
}
