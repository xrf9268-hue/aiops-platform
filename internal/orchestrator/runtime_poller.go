package orchestrator

import (
	"context"
	"errors"
	"sync"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
)

type RuntimePoller struct {
	tracker      IssueStateLister
	orchestrator *Orchestrator
	runtime      *WorkflowRuntime
	config       worker.Config
	emitter      worker.EventEmitter

	mu              sync.Mutex
	poller          *Poller
	dispatcher      *RuntimeDispatcher
	lastSnapshotKey string
}

func NewRuntimePoller(tracker IssueStateLister, orchestrator *Orchestrator, runtime *WorkflowRuntime, cfg worker.Config, emitter worker.EventEmitter) (*RuntimePoller, error) {
	if tracker == nil {
		return nil, errors.New("runtime poller requires tracker")
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
	rp := &RuntimePoller{tracker: tracker, orchestrator: orchestrator, runtime: runtime, config: cfg, emitter: emitter, dispatcher: dispatcher}
	rp.poller = NewPollerWithReconciliation(tracker, orchestrator, runtime.Current().Reconciliation)
	return rp, nil
}

func (p *RuntimePoller) PollOnce(ctx context.Context) error {
	if p == nil {
		return errors.New("runtime poller is nil")
	}
	p.mu.Lock()
	p.poller = p.pollerForSnapshot(p.runtime.Current())
	poller := p.poller
	p.mu.Unlock()
	return poller.PollOnce(ctx)
}

func (p *RuntimePoller) pollerForSnapshot(snap WorkflowSnapshot) *Poller {
	key := snapshotWorkflowKey(snap)
	if p.poller != nil && p.lastSnapshotKey == key {
		return p.poller
	}
	p.lastSnapshotKey = key
	return NewPollerWithReconciliation(p.tracker, p.orchestrator, snap.Reconciliation)
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
