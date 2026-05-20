package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"

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

	mu              sync.Mutex
	poller          *Poller
	dispatcher      *RuntimeDispatcher
	lastSnapshotKey string
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
	poller := NewPollerWithReconciliation(multiIssueStateLister{trackers: trackerClients}, p.orchestrator, snap.Reconciliation)
	if snap.Workflow.Config.Tracker.Kind == "linear" && len(snap.Workflow.Config.Services) > 0 {
		poller.routing = &snap.Workflow.Config
	}
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
	for _, stateTracker := range l.trackers {
		refresher, ok := stateTracker.(IssueStateRefresher)
		if !ok {
			continue
		}
		return refresher.FetchIssueStatesByIDs(ctx, issueIDs)
	}
	return map[string]string{}, nil
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
	}
	return dispatcher.Spawn(ctx, issue, attempt)
}

func (d *RuntimeDispatcher) configForSnapshot(snap WorkflowSnapshot) worker.Config {
	cfg := d.baseConfig
	cfg.Workflow = snap.Workflow
	return cfg
}
