package orchestrator

import (
	"context"
	"errors"
	"sync"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type RuntimePoller struct {
	trackerFactory func(workflow.Config) (IssueStateLister, error)
	tracker        IssueStateLister
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
	poller, err := p.pollerForSnapshot(p.runtime.Current())
	if err == nil {
		p.poller = poller
	}
	p.mu.Unlock()
	if err != nil {
		return err
	}
	return poller.PollOnce(ctx)
}

func (p *RuntimePoller) pollerForSnapshot(snap WorkflowSnapshot) (*Poller, error) {
	key := snapshotWorkflowKey(snap)
	if p.poller != nil && p.lastSnapshotKey == key {
		return p.poller, nil
	}
	trackerClient, err := p.trackerFactory(snap.Workflow.Config)
	if err != nil {
		return nil, err
	}
	if trackerClient == nil {
		return nil, errors.New("runtime poller tracker factory returned nil tracker")
	}
	p.tracker = trackerClient
	p.lastSnapshotKey = key
	return NewPollerWithReconciliation(trackerClient, p.orchestrator, snap.Reconciliation), nil
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
	runtime    *WorkflowRuntime
	baseConfig worker.Config
	emitter    worker.EventEmitter
}

func NewRuntimeDispatcher(runtime *WorkflowRuntime, cfg worker.Config, emitter worker.EventEmitter) (*RuntimeDispatcher, error) {
	if runtime == nil {
		return nil, errors.New("runtime dispatcher requires workflow runtime")
	}
	return &RuntimeDispatcher{runtime: runtime, baseConfig: cfg, emitter: emitter}, nil
}

func (d *RuntimeDispatcher) Spawn(ctx context.Context, issue tracker.Issue, attempt *int) <-chan WorkerResult {
	snap := d.runtime.Current()
	dispatcher := WorkerTaskDispatcher{
		BuildTask: func(issue tracker.Issue) (task.Task, error) {
			return TaskFromIssue(issue, snap.Workflow.Config)
		},
		Config:  d.configForSnapshot(snap),
		Emitter: d.emitter,
	}
	return dispatcher.Spawn(ctx, issue, attempt)
}

func (d *RuntimeDispatcher) configForSnapshot(snap WorkflowSnapshot) worker.Config {
	cfg := d.baseConfig
	cfg.Workflow = snap.Workflow
	return cfg
}
