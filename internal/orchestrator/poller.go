package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
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
}

// NewPoller returns a SPEC-aligned tracker poller backed by orchestrator-owned
// runtime state instead of the legacy Postgres task queue.
func NewPoller(tracker ActiveIssueLister, orchestrator *Orchestrator) *Poller {
	return &Poller{tracker: tracker, orchestrator: orchestrator}
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
	issues, err := p.tracker.ListActiveIssues(ctx)
	if err != nil {
		return err
	}
	var pollErr error
	routedIssues := issues
	if p.routing != nil {
		routedIssues, err = selectRoutedCandidates(issues, *p.routing)
		if err != nil {
			return err
		}
	}
	if p.reconcileKnown {
		if err := p.reconcileTick(ctx, routedIssues); err != nil {
			pollErr = errors.Join(pollErr, err)
		}
	}
	candidates := filterEligibleCandidates(mergeOverflowCandidates(p.overflow, routedIssues), p.reconcile.TerminalStates)
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

func (p *Poller) reconcileTick(ctx context.Context, activeIssues []tracker.Issue) error {
	if p.stateTracker == nil {
		return errors.New("orchestrator poller reconciliation requires state tracker")
	}
	if p.routing != nil {
		if err := p.orchestrator.ReconcileTrackerIssuesAndWait(ctx, issueMap(activeIssues), normalizedStates(p.reconcile.ActiveStates), p.reconcile.WorkerExitTimeout); err != nil {
			return err
		}
	}
	activeByID := issueIDSet(activeIssues)
	inactiveByID := make(map[string]tracker.Issue)
	var fetchErr error
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
	reconcileErr := p.orchestrator.ReconcileInactiveTrackerIssuesAndWait(ctx, inactiveByID, p.reconcile.WorkerExitTimeout)
	return errors.Join(reconcileErr, fetchErr)
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

func issueMap(issues []tracker.Issue) map[string]tracker.Issue {
	out := make(map[string]tracker.Issue, len(issues))
	for _, issue := range issues {
		if issue.ID != "" {
			out[issue.ID] = issue
		}
	}
	return out
}

func filterEligibleCandidates(issues []tracker.Issue, terminalStates []string) []tracker.Issue {
	terminal := normalizedStates(terminalStates)
	for state := range normalizedStates([]string{"Done", "Canceled", "Cancelled", "Closed", "Duplicate"}) {
		terminal[state] = struct{}{}
	}
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

func compareCreatedAt(left, right string) int {
	leftTime, leftErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(left))
	rightTime, rightErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(right))
	if leftErr == nil && rightErr == nil {
		switch {
		case leftTime.Before(rightTime):
			return -1
		case leftTime.After(rightTime):
			return 1
		default:
			return 0
		}
	}
	if leftErr == nil {
		return -1
	}
	if rightErr == nil {
		return 1
	}
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left != right {
		if left < right {
			return -1
		}
		return 1
	}
	return 0
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
			log.Printf("tracker route skipped issue %s: no configured service matched", issue.ID)
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
}

// Spawn implements Dispatcher.
func (d WorkerTaskDispatcher) Spawn(ctx context.Context, issue tracker.Issue, _ *int) <-chan WorkerResult {
	out := make(chan WorkerResult, 1)
	go func() {
		defer close(out)
		start := time.Now()
		tk, recordedTaskID, err := d.buildTask(issue)
		if err != nil {
			out <- WorkerResult{Err: err, NonRetryable: true, Elapsed: time.Since(start)}
			return
		}
		if rterr := worker.RunTask(ctx, d.Emitter, tk, d.Config); rterr != nil {
			out <- WorkerResult{Err: rterr.Err, Elapsed: time.Since(start)}
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
	}, nil
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
			log.Printf("tracker poll error: %v", err)
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
