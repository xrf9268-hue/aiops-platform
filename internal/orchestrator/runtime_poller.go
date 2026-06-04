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
	if err := p.orchestrator.UpdateMaxContinuationTurns(ctx, snap.MaxContinuationTurns); err != nil {
		return err
	}
	if err := p.orchestrator.UpdatePollIntervalMs(ctx, snap.PollInterval.Milliseconds()); err != nil {
		return err
	}
	if err := p.orchestrator.UpdateRetryScheduler(ctx, RetryScheduler{MaxBackoff: snap.MaxRetryBackoff}); err != nil {
		return err
	}
	return poller.PollOnce(ctx)
}

func (p *RuntimePoller) pollerForSnapshot(snap WorkflowSnapshot) (*Poller, error) {
	key := snapshotWorkflowKey(snap)
	if p.poller != nil && p.lastSnapshotKey == key {
		return p.poller, nil
	}
	trackerClient, err := p.trackerClientForSnapshot(snap)
	if err != nil {
		return nil, err
	}
	if trackerClient == nil {
		return nil, errors.New("runtime poller tracker factory returned nil tracker")
	}
	p.lastSnapshotKey = key
	// Concrete tracker clients (Linear/Gitea/GitHub) implement IssueStateRefresher;
	// a bare lister (e.g. a test fake) does not, in which case the §11.2/§16.5
	// refresh hooks no-op on a nil refresher.
	refresher, _ := trackerClient.(IssueStateRefresher)
	p.currentRefresher = refresher
	if p.dispatcher != nil {
		p.dispatcher.SetIssueStateRefresher(refresher)
	}
	poller := NewPollerWithReconciliation(trackerClient, p.orchestrator, snap.Reconciliation)
	preflightCfg := snap.Workflow.Config
	poller.preflight = &preflightCfg
	// Feed the orchestrator the same lister the poll loop uses so a
	// fired failure-retry timer can run the SPEC §16.6 candidate fetch
	// against the same active-state vocabulary. The lister must mirror
	// every filter the poll loop applies between ListActiveIssues and
	// dispatch: filterEligibleCandidates' required-field and
	// Todo-blocked-by-non-terminal checks. Skipping them means a retry
	// can dispatch an issue the poll loop would refuse.
	var retryLister ActiveIssueLister = activeIssueListerFromStates{
		tracker: trackerClient,
		states:  snap.Reconciliation.ActiveStates,
	}
	retryLister = eligibleActiveIssueLister{inner: retryLister, terminalStates: snap.Reconciliation.TerminalStates}
	p.orchestrator.SetCandidateLister(retryLister)
	// The candidate lister above is active-only, so a fired failure-retry whose
	// issue moved to a terminal state sees found==nil — indistinguishable there
	// from a deleted issue. Give the retry-fire path the same state reader and
	// terminal set the reconcile pass uses so its found==nil branch can clean a
	// terminal workspace through the §18.1 seam instead of leaking it (#341).
	p.orchestrator.SetRetryTerminalStateResolver(refresher, snap.Reconciliation.TerminalStates)
	return poller, nil
}

func (p *RuntimePoller) trackerClientForSnapshot(snap WorkflowSnapshot) (IssueStateLister, error) {
	cfg := workflow.Config{}
	if snap.Workflow != nil {
		cfg = snap.Workflow.Config
	}
	trackerClient, err := p.trackerFactory(cfg)
	if err != nil {
		return nil, err
	}
	if trackerClient == nil {
		return nil, errors.New("runtime poller tracker factory returned nil tracker")
	}
	return trackerClient, nil
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

func (d *RuntimeDispatcher) Spawn(ctx context.Context, issue tracker.Issue, attempt *int, opts DispatchOptions) <-chan WorkerResult {
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
	return dispatcher.Spawn(ctx, issue, attempt, opts)
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
	cfg.OperatorTerminalStopLookup = func(t task.Task, _ workflow.Config) runner.OperatorTerminalStopLookup {
		issueID := strings.TrimSpace(t.ID)
		if issueID == "" {
			return nil
		}
		return func(ctx context.Context) (runner.IssueStateSnapshot, bool) {
			return d.operatorTerminalStopSnapshot(ctx, issueID)
		}
	}
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
			terminalStates := normalizedStateSet(wcfg.Tracker.TerminalStates)
			issueRef := tracker.IssueRef{ID: issueID, Identifier: taskIssueIdentifier(t)}
			return func(ctx context.Context) (runner.IssueStateSnapshot, error) {
				if stopped, ok := d.operatorTerminalStopSnapshot(ctx, issueID); ok {
					return stopped, nil
				}
				statesByID, err := fetchIssueStates(ctx, refresher, []tracker.IssueRef{issueRef})
				if err != nil {
					return runner.IssueStateSnapshot{}, err
				}
				state, ok := statesByID[issueID]
				if !ok || strings.TrimSpace(state) == "" {
					// SPEC §16.5 "issue = refreshed_issue[0] or
					// issue": no row means we treat the issue as
					// still in its prior (active) state rather
					// than aborting on a benign absence.
					return runner.IssueStateSnapshot{Found: false, Active: true}, nil
				}
				normalized := strings.ToLower(strings.TrimSpace(state))
				_, active := activeStates[normalized]
				_, terminal := terminalStates[normalized]
				return runner.IssueStateSnapshot{Found: true, State: state, Active: active, Terminal: terminal}, nil
			}
		}
	}
	return cfg
}

func (d *RuntimeDispatcher) operatorTerminalStopSnapshot(ctx context.Context, issueID string) (runner.IssueStateSnapshot, bool) {
	if d == nil || d.orchestrator == nil {
		return runner.IssueStateSnapshot{}, false
	}
	stop, ok, err := d.orchestrator.LookupOperatorTerminalStop(ctx, IssueID(issueID))
	if err != nil || !ok {
		return runner.IssueStateSnapshot{}, false
	}
	return runner.IssueStateSnapshot{
		Found:                true,
		State:                stop.State,
		Active:               false,
		Terminal:             true,
		OperatorTerminalStop: true,
	}, true
}

func taskIssueIdentifier(t task.Task) string {
	if identifier, ok := t.IssueRender["identifier"].(string); ok && strings.TrimSpace(identifier) != "" {
		return strings.TrimSpace(identifier)
	}
	return strings.TrimSpace(t.SourceEventID)
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
