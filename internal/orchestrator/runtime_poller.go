package orchestrator

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type RuntimePoller struct {
	trackerFactory func(workflow.Config) (IssueStateLister, error)
	tracker        IssueStateLister
	trackers       []IssueStateLister
	orchestrator   *Orchestrator
	runtime        *WorkflowRuntime
	config         worker.Config
	emitter        worker.EventEmitter

	mu               sync.Mutex
	poller           *Poller
	dispatcher       *RuntimeDispatcher
	lastSnapshotKey  string
	currentRefresher IssueStateRefresher
}

func NewRuntimePoller(tracker IssueStateLister, orchestrator *Orchestrator, runtime *WorkflowRuntime, cfg worker.Config, emitter worker.EventEmitter) (*RuntimePoller, error) {
	return NewRuntimePollerWithTrackerFactory(func(workflow.Config) (IssueStateLister, error) {
		return tracker, nil
	}, orchestrator, runtime, cfg, emitter)
}

func NewRuntimePollerWithTrackerFactory(trackerFactory func(workflow.Config) (IssueStateLister, error), orchestrator *Orchestrator, runtime *WorkflowRuntime, cfg worker.Config, emitter worker.EventEmitter) (*RuntimePoller, error) {
	if trackerFactory == nil {
		return nil, errors.New("runtime poller requires tracker factory")
	}
	if orchestrator == nil {
		return nil, errors.New("runtime poller requires orchestrator")
	}
	if runtime == nil {
		return nil, errors.New("runtime poller requires workflow runtime")
	}
	dispatcher, err := NewRuntimeDispatcher(runtime, cfg, emitter)
	if err != nil {
		return nil, err
	}
	dispatcher.AttachOrchestrator(orchestrator)
	rp := &RuntimePoller{trackerFactory: trackerFactory, orchestrator: orchestrator, runtime: runtime, config: cfg, emitter: emitter, dispatcher: dispatcher}
	initialPoller, err := rp.pollerForSnapshot(runtime.Current())
	if err != nil {
		return nil, err
	}
	rp.poller = initialPoller
	return rp, nil
}

// AttachDispatcher rewires the dispatcher the RuntimePoller updates when
// it builds a new tracker fan-in. Callers that construct their own
// *RuntimeDispatcher externally (and pass it to orchestrator.Deps.Dispatcher
// so the actor's Spawn uses it) must call this — otherwise the SPEC §16.5
// refresher would land on the poller's internal dispatcher, which the
// actor never sees. Passing nil is a no-op so tests that do not exercise
// the refresher path can keep their existing construction.
//
// The poller copies its most recent tracker refresher onto the freshly
// attached dispatcher so callers don't have to wait for the next workflow
// snapshot change before the refresher hook activates.
func (p *RuntimePoller) AttachDispatcher(d *RuntimeDispatcher) {
	if p == nil || d == nil {
		return
	}
	p.mu.Lock()
	p.dispatcher = d
	carry := p.currentRefresher
	p.mu.Unlock()
	if carry != nil {
		d.SetIssueStateRefresher(carry)
	}
}

func (p *RuntimePoller) PollOnce(ctx context.Context) error {
	if p == nil {
		return errors.New("runtime poller is nil")
	}
	p.mu.Lock()
	snap := p.runtime.Current()
	poller, err := p.pollerForSnapshot(snap)
	if err == nil {
		p.poller = poller
	}
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if err := p.orchestrator.UpdateMaxConcurrentAgents(ctx, snap.MaxConcurrentAgents); err != nil {
		return err
	}
	if err := p.orchestrator.UpdateMaxConcurrentAgentsByState(ctx, snap.MaxConcurrentAgentsByState); err != nil {
		return err
	}
	if err := p.orchestrator.UpdatePollIntervalMs(ctx, snap.PollInterval.Milliseconds()); err != nil {
		return err
	}
	if err := p.orchestrator.UpdateRetryScheduler(ctx, RetryScheduler{MaxBackoff: snap.MaxRetryBackoff}); err != nil {
		return err
	}
	if err := p.orchestrator.UpdateMaxFailureRetries(ctx, snap.MaxRetryAttempts); err != nil {
		return err
	}
	if err := p.orchestrator.UpdateMaxTurns(ctx, snap.MaxTurns); err != nil {
		return err
	}
	if err := p.orchestrator.UpdateRunnerEnforcesMaxTurns(ctx, runner.EnforcesMaxTurnsInternally(snap.Workflow.Config.Agent.Default)); err != nil {
		return err
	}
	return poller.PollOnce(ctx)
}

func (p *RuntimePoller) pollerForSnapshot(snap WorkflowSnapshot) (*Poller, error) {
	key := snapshotWorkflowKey(snap)
	if p.poller != nil && p.lastSnapshotKey == key {
		return p.poller, nil
	}
	trackerClients, err := p.trackerClientsForSnapshot(snap)
	if err != nil {
		return nil, err
	}
	if len(trackerClients) == 0 {
		return nil, errors.New("runtime poller tracker factory returned no trackers")
	}
	p.tracker = trackerClients[0]
	p.trackers = trackerClients
	p.lastSnapshotKey = key
	multiLister := multiIssueStateLister{trackers: trackerClients}
	p.currentRefresher = multiLister
	if p.dispatcher != nil {
		p.dispatcher.SetIssueStateRefresher(multiLister)
	}
	poller := NewPollerWithReconciliation(multiLister, p.orchestrator, snap.Reconciliation)
	if snap.Workflow.Config.Tracker.Kind == "linear" && len(snap.Workflow.Config.Services) > 0 {
		poller.routing = &snap.Workflow.Config
	}
	preflightCfg := snap.Workflow.Config
	poller.preflight = &preflightCfg
	// Feed the orchestrator the same lister the poll loop uses so a
	// fired failure-retry timer can run the SPEC §16.6 candidate fetch
	// against the same active-state vocabulary. The lister must mirror
	// every filter the poll loop applies between ListActiveIssues and
	// dispatch (poller.go:152): service routing (selectRoutedCandidates)
	// when configured, plus filterEligibleCandidates' required-field
	// and Todo-blocked-by-non-terminal checks. Mirror order matches the
	// poll loop: active → routed → eligible. Skipping any of these
	// means a retry can dispatch an issue the poll loop would refuse.
	var retryLister ActiveIssueLister = activeIssueListerFromStates{
		tracker: multiLister,
		states:  snap.Reconciliation.ActiveStates,
	}
	if poller.routing != nil {
		retryLister = routedActiveIssueLister{inner: retryLister, cfg: *poller.routing}
	}
	retryLister = eligibleActiveIssueLister{inner: retryLister, terminalStates: snap.Reconciliation.TerminalStates}
	p.orchestrator.SetCandidateLister(retryLister)
	// The candidate lister above is active-only, so a fired failure-retry whose
	// issue moved to a terminal state sees found==nil — indistinguishable there
	// from a deleted issue. Give the retry-fire path the same state reader and
	// terminal set the reconcile pass uses so its found==nil branch can clean a
	// terminal workspace through the §18.1 seam instead of leaking it (#341).
	p.orchestrator.SetRetryTerminalStateResolver(multiLister, snap.Reconciliation.TerminalStates)
	return poller, nil
}

func (p *RuntimePoller) trackerClientsForSnapshot(snap WorkflowSnapshot) ([]IssueStateLister, error) {
	if snap.Workflow == nil {
		trackerClient, err := p.trackerFactory(workflow.Config{})
		if err != nil {
			return nil, err
		}
		if trackerClient == nil {
			return nil, errors.New("runtime poller tracker factory returned nil tracker")
		}
		return []IssueStateLister{trackerClient}, nil
	}
	cfg := snap.Workflow.Config
	projectConfigs := trackerProjectConfigs(cfg)
	clients := make([]IssueStateLister, 0, len(projectConfigs))
	for _, projectCfg := range projectConfigs {
		trackerClient, err := p.trackerFactory(projectCfg)
		if err != nil {
			return nil, err
		}
		if trackerClient == nil {
			return nil, errors.New("runtime poller tracker factory returned nil tracker")
		}
		clients = append(clients, trackerClient)
	}
	return clients, nil
}

// TrackerProjectConfigs returns one workflow config per Linear project that a
// poll/reconcile pass must query for a service-routed workflow.
func TrackerProjectConfigs(cfg workflow.Config) []workflow.Config {
	return trackerProjectConfigs(cfg)
}

func trackerProjectConfigs(cfg workflow.Config) []workflow.Config {
	if cfg.Tracker.Kind != "linear" {
		return []workflow.Config{cfg}
	}
	seen := map[string]struct{}{}
	add := func(project string, out *[]workflow.Config) {
		project = strings.TrimSpace(project)
		if project == "" {
			return
		}
		key := strings.ToLower(project)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		projectCfg := cfg
		projectCfg.Tracker.ProjectSlug = project
		projectCfg.Services = nil
		*out = append(*out, projectCfg)
	}
	out := make([]workflow.Config, 0, len(cfg.Services)+1)
	add(cfg.Tracker.ProjectSlug, &out)
	for _, service := range cfg.Services {
		add(service.Tracker.ProjectSlug, &out)
	}
	if len(out) == 0 {
		out = append(out, cfg)
	}
	return out
}

type multiIssueStateLister struct {
	trackers []IssueStateLister
}

func (l multiIssueStateLister) ListIssuesByStates(ctx context.Context, states []string) ([]tracker.Issue, error) {
	var issues []tracker.Issue
	var errOut error
	for _, stateTracker := range l.trackers {
		got, err := stateTracker.ListIssuesByStates(ctx, states)
		if err != nil {
			errOut = errors.Join(errOut, err)
			continue
		}
		issues = append(issues, got...)
	}
	return issues, errOut
}

func (l multiIssueStateLister) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error) {
	return l.FetchIssueStatesByRefs(ctx, tracker.IssueRefsFromIDs(issueIDs))
}

func (l multiIssueStateLister) FetchIssueStatesByRefs(ctx context.Context, issueRefs []tracker.IssueRef) (map[string]string, error) { //nolint:gocognit // baseline (#521)
	out := make(map[string]string, len(issueRefs))
	remaining := append([]tracker.IssueRef(nil), issueRefs...)
	var errOut error
	for _, stateTracker := range l.trackers {
		refresher, ok := stateTracker.(IssueStateRefresher)
		if !ok {
			continue
		}
		got, err := fetchIssueStates(ctx, refresher, remaining)
		if err != nil {
			errOut = errors.Join(errOut, err)
		}
		if len(got) == 0 {
			continue
		}
		next := remaining[:0]
		for _, ref := range remaining {
			if state, ok := got[ref.ID]; ok {
				out[ref.ID] = state
				continue
			}
			next = append(next, ref)
		}
		remaining = next
		if len(remaining) == 0 {
			return out, nil
		}
	}
	return out, errOut
}

// routedActiveIssueLister wraps an ActiveIssueLister with the same
// service-routing filter the poll loop applies in (*Poller).runOnce
// (see poller.go selectRoutedCandidates). SPEC §16.6's on_retry_timer
// only knows about candidate fetch; service routing is an
// aiops-platform extension layered on top, and the retry-fire path
// must mirror the poll loop's filter so a queued retry whose issue
// has since routed to another service cannot bypass the gate. Routing
// errors are propagated so a fetch that resolves to an ambiguous
// route is treated as a fetch failure (reschedule via "retry poll
// failed"), not as a silent absence (release claim).
type routedActiveIssueLister struct {
	inner ActiveIssueLister
	cfg   workflow.Config
}

func (l routedActiveIssueLister) ListActiveIssues(ctx context.Context) ([]tracker.Issue, error) {
	issues, fetchErr := l.inner.ListActiveIssues(ctx)
	routed, routeErr := selectRoutedCandidates(issues, l.cfg)
	if fetchErr != nil || routeErr != nil {
		return routed, errors.Join(fetchErr, routeErr)
	}
	return routed, nil
}

// eligibleActiveIssueLister wraps an ActiveIssueLister with the same
// eligibility filter the poll loop applies between routing and dispatch
// (filterEligibleCandidates in poller.go). The filter drops issues
// missing required SPEC §4.1.1 fields (ID / Identifier / Title / State)
// and Todo-state issues whose BlockedBy carries a non-terminal blocker.
// Without this wrap a queued failure-retry whose Todo issue gained a
// fresh non-terminal blocker between schedule and fire would still be
// dispatched, while the poll loop would refuse the same issue on the
// next tick. The fetch error is propagated unchanged because an empty
// post-filter result is meaningful (genuine absence) but only if the
// fetch itself succeeded.
type eligibleActiveIssueLister struct {
	inner          ActiveIssueLister
	terminalStates []string
}

func (l eligibleActiveIssueLister) ListActiveIssues(ctx context.Context) ([]tracker.Issue, error) {
	issues, fetchErr := l.inner.ListActiveIssues(ctx)
	return filterEligibleCandidates(issues, l.terminalStates), fetchErr
}

func snapshotWorkflowKey(snap WorkflowSnapshot) string {
	if snap.Fingerprint != "" {
		return snap.Fingerprint
	}
	if snap.Workflow == nil {
		return ""
	}
	return snap.Workflow.Path
}

type RuntimeDispatcher struct {
	runtime      *WorkflowRuntime
	baseConfig   worker.Config
	emitter      worker.EventEmitter
	orchestrator *Orchestrator

	mu        sync.Mutex
	refresher IssueStateRefresher
}

// SetIssueStateRefresher updates the tracker reader the dispatcher uses to
// build SPEC §16.5 per-turn refresh closures handed to worker.RunTask. The
// RuntimePoller calls it after each workflow-snapshot reload so the closure
// always points at the current tracker fan-in (multiIssueStateLister).
func (d *RuntimeDispatcher) SetIssueStateRefresher(refresher IssueStateRefresher) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refresher = refresher
}

func (d *RuntimeDispatcher) currentRefresher() IssueStateRefresher {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.refresher
}

func NewRuntimeDispatcher(runtime *WorkflowRuntime, cfg worker.Config, emitter worker.EventEmitter) (*RuntimeDispatcher, error) {
	if runtime == nil {
		return nil, errors.New("runtime dispatcher requires workflow runtime")
	}
	return &RuntimeDispatcher{runtime: runtime, baseConfig: cfg, emitter: emitter}, nil
}

func (d *RuntimeDispatcher) AttachOrchestrator(orchestrator *Orchestrator) {
	if d != nil {
		d.orchestrator = orchestrator
	}
}

func (d *RuntimeDispatcher) Spawn(ctx context.Context, issue tracker.Issue, attempt *int) <-chan WorkerResult {
	snap := d.runtime.Current()
	dispatcher := WorkerTaskDispatcher{
		BuildTask: func(issue tracker.Issue) (task.Task, error) {
			return TaskFromIssue(issue, snap.Workflow.Config)
		},
		Config: d.configForSnapshot(snap),
		Emitter: runtimeEventForwardingEmitter{
			EventEmitter: d.emitter,
			Orchestrator: d.orchestrator,
			IssueID:      issue.ID,
		},
		WorkspacePrepared: func(ctx context.Context, issue tracker.Issue, _ task.Task, path string) {
			if d.orchestrator != nil {
				// Capture the root this path was created under so SPEC §18.1
				// cleanup removes it against the same root even if
				// workspace.root is hot-reloaded before terminal reconciliation.
				_ = d.orchestrator.RecordWorkspace(ctx, issue.ID, Workspace{
					Path: path,
					Root: worker.EffectiveWorkspaceRoot(d.baseConfig, snap.Workflow.Config),
				})
			}
		},
	}
	return dispatcher.Spawn(ctx, issue, attempt)
}

// CleanupReconciledWorkspace implements WorkspaceCleaner: it removes the
// workspace of a run cancelled because its issue moved to a terminal state
// mid-run (SPEC §18.1 active transition). It reads the live workflow snapshot
// so the before_remove hook, workspace root, and hook env passthrough match
// what a dispatch on this same tick would have used (honoring hot reloads),
// then delegates to worker.RemoveIssueWorkspace — the same routine the
// startup sweep uses — so both paths fire before_remove and emit the same
// reconcile_workspace remove event.
func (d *RuntimeDispatcher) CleanupReconciledWorkspace(ctx context.Context, w ReconciledWorkspace) {
	if d == nil {
		return
	}
	cfg := d.runtime.Current().Workflow.Config
	hooks := cfg.WorkspaceHooks()
	// Prefer the root captured when the path was created; fall back to the live
	// snapshot root only when it was not recorded (older entry). Using a
	// hot-reloaded root here would make SafeRemove reject the path as escaping
	// root and silently skip cleanup (Codex P2).
	root := strings.TrimSpace(w.Root)
	if root == "" {
		root = worker.EffectiveWorkspaceRoot(d.baseConfig, cfg)
	}
	if _, err := worker.RemoveIssueWorkspace(ctx, d.emitter, worker.RemoveWorkspaceRequest{
		WorkspaceRoot:      root,
		TaskID:             "reconcile-active",
		Path:               w.Path,
		IssueID:            string(w.IssueID),
		Identifier:         w.Identifier,
		State:              w.State,
		Reason:             w.Reason,
		BeforeRemoveHook:   hooks.BeforeRemove,
		HookTimeoutMillis:  hooks.TimeoutMs,
		HookEnvPassthrough: hooks.EnvPassthrough,
	}); err != nil {
		log.Printf("event=reconcile_active_workspace_remove_failed issue_id=%s issue_identifier=%s reason=%s workspace=%q error=%q", w.IssueID, w.Identifier, w.Reason, w.Path, err)
	}
}

func (d *RuntimeDispatcher) configForSnapshot(snap WorkflowSnapshot) worker.Config { //nolint:gocognit // baseline (#521)
	cfg := d.baseConfig
	cfg.Workflow = snap.Workflow
	refresher := d.currentRefresher()
	if refresher != nil {
		cfg.IssueStateRefresher = func(t task.Task, wcfg workflow.Config) runner.IssueStateRefresher {
			issueID := strings.TrimSpace(t.ID)
			if issueID == "" {
				return nil
			}
			activeStates := normalizedStateSet(wcfg.Tracker.ActiveStates)
			if len(activeStates) == 0 {
				return nil
			}
			issueRef := tracker.IssueRef{ID: issueID, Identifier: taskIssueIdentifier(t)}
			return func(ctx context.Context) (bool, error) {
				statesByID, err := fetchIssueStates(ctx, refresher, []tracker.IssueRef{issueRef})
				if err != nil {
					return false, err
				}
				state, ok := statesByID[issueID]
				if !ok || strings.TrimSpace(state) == "" {
					// SPEC §16.5 "issue = refreshed_issue[0] or
					// issue": no row means we treat the issue as
					// still in its prior (active) state rather
					// than aborting on a benign absence.
					return true, nil
				}
				_, active := activeStates[strings.ToLower(strings.TrimSpace(state))]
				return active, nil
			}
		}
	}
	return cfg
}

func taskIssueIdentifier(t task.Task) string {
	if identifier, ok := t.IssueRender["identifier"].(string); ok && strings.TrimSpace(identifier) != "" {
		return strings.TrimSpace(identifier)
	}
	sourceEventID := strings.TrimSpace(t.SourceEventID)
	if strings.Contains(sourceEventID, "|service|") {
		return ""
	}
	return sourceEventID
}

func normalizedStateSet(states []string) map[string]struct{} {
	out := make(map[string]struct{}, len(states))
	for _, state := range states {
		trimmed := strings.ToLower(strings.TrimSpace(state))
		if trimmed == "" {
			continue
		}
		out[trimmed] = struct{}{}
	}
	return out
}
