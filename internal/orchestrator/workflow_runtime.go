package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// workflowReloadReadErrorSentinel marks the lastFailedFingerprint when the
// workflow file cannot be read (missing / EISDIR / permission denied). SHA-256
// fingerprints are 64 lowercase hex chars, so the angle-bracketed string
// cannot collide with a real fingerprint.
const workflowReloadReadErrorSentinel = "<workflow-read-error>"

// errWorkflowReloadDeduped is returned by ReloadOnce when the current workflow
// file fingerprint matches a previous failed reload — the caller already
// received an EventWorkflowReloadFailed for this fingerprint, so we skip
// re-emitting and re-running Load/validate. Diagnostic only; the single
// production caller (RunWorkflowReloadLoop) does not branch on the type.
var errWorkflowReloadDeduped = errors.New("workflow reload deduped: identical to last-failed fingerprint")

type WorkflowRuntimeConfig struct {
	Initial              *workflow.Workflow
	Path                 string
	Source               workflow.Source
	ReloadInterval       time.Duration
	Emitter              worker.EventEmitter
	EventTaskID          string
	Validate             func(path string, source workflow.Source, cfg workflow.Config) error
	ReconciliationConfig func(workflow.Config) ReconciliationConfig
}

type WorkflowRuntime struct {
	path                 string
	source               workflow.Source
	reloadInterval       time.Duration
	emitter              worker.EventEmitter
	eventTaskID          string
	validate             func(path string, source workflow.Source, cfg workflow.Config) error
	reconciliationConfig func(workflow.Config) ReconciliationConfig
	current              atomic.Pointer[WorkflowSnapshot]

	// failureMu guards lastFailedFingerprint. ReloadOnce has one production
	// caller (RunWorkflowReloadLoop) that runs sequentially, but the mutex
	// keeps -race tests honest if a future caller (e.g. a refresh endpoint)
	// races with the loop.
	failureMu             sync.Mutex
	lastFailedFingerprint string
}

type WorkflowSnapshot struct {
	Workflow                   *workflow.Workflow
	PollInterval               time.Duration
	MaxConcurrentAgents        int
	MaxConcurrentAgentsByState map[string]int
	MaxContinuationTurns       int
	MaxRetryBackoff            time.Duration
	Reconciliation             ReconciliationConfig
	Fingerprint                string
}

func NewWorkflowRuntime(cfg WorkflowRuntimeConfig) (*WorkflowRuntime, error) {
	if cfg.Initial == nil {
		return nil, errors.New("workflow runtime requires initial workflow")
	}
	path := cfg.Path
	if path == "" {
		path = cfg.Initial.Path
	}
	source := cfg.Source
	if source == "" {
		source = cfg.Initial.Source
	}
	r := &WorkflowRuntime{
		path:                 path,
		source:               source,
		reloadInterval:       cfg.ReloadInterval,
		emitter:              cfg.Emitter,
		eventTaskID:          cfg.EventTaskID,
		validate:             cfg.Validate,
		reconciliationConfig: cfg.ReconciliationConfig,
	}
	r.current.Store(r.snapshotFromWorkflow(cfg.Initial))
	return r, nil
}

func (r *WorkflowRuntime) Current() WorkflowSnapshot {
	if r == nil {
		return WorkflowSnapshot{}
	}
	snap := r.current.Load()
	if snap == nil {
		return WorkflowSnapshot{}
	}
	return *snap
}

func (r *WorkflowRuntime) ReloadInterval() time.Duration {
	if r == nil || r.reloadInterval <= 0 {
		return 0
	}
	return r.reloadInterval
}

func (r *WorkflowRuntime) ReloadOnce(ctx context.Context) error { //nolint:gocognit // baseline (#521)
	if r == nil {
		return errors.New("workflow runtime is nil")
	}
	if r.path == "" || r.source == workflow.SourceDefault {
		return nil
	}
	fingerprint, err := workflowFileFingerprint(r.path)
	if err != nil {
		r.failureMu.Lock()
		suppress := r.lastFailedFingerprint == workflowReloadReadErrorSentinel
		r.lastFailedFingerprint = workflowReloadReadErrorSentinel
		r.failureMu.Unlock()
		if !suppress {
			r.emit(ctx, task.EventWorkflowReloadFailed, fmt.Sprintf("workflow reload failed: %v", err), map[string]any{"path": r.path, "error": err.Error()})
		}
		return err
	}
	if snap := r.Current(); snap.Fingerprint != "" && snap.Fingerprint == fingerprint {
		r.clearLastFailedFingerprint()
		return nil
	}
	r.failureMu.Lock()
	if r.lastFailedFingerprint == fingerprint {
		r.failureMu.Unlock()
		return errWorkflowReloadDeduped
	}
	r.failureMu.Unlock()
	wf, err := workflow.Load(r.path)
	if err == nil && r.validate != nil {
		err = r.validate(wf.Path, wf.Source, wf.Config)
	}
	if err != nil {
		r.failureMu.Lock()
		r.lastFailedFingerprint = fingerprint
		r.failureMu.Unlock()
		r.emit(ctx, task.EventWorkflowReloadFailed, fmt.Sprintf("workflow reload failed: %v", err), map[string]any{"path": r.path, "error": err.Error()})
		return err
	}
	r.clearLastFailedFingerprint()
	r.current.Store(r.snapshotFromWorkflow(wf, fingerprint))
	r.emit(ctx, task.EventWorkflowReloaded, "workflow reloaded", map[string]any{"path": r.path, "poll_interval_ms": wf.Config.Tracker.PollIntervalMs})
	return nil
}

func (r *WorkflowRuntime) clearLastFailedFingerprint() {
	r.failureMu.Lock()
	r.lastFailedFingerprint = ""
	r.failureMu.Unlock()
}

func (r *WorkflowRuntime) emit(ctx context.Context, kind, msg string, payload any) {
	if r.emitter == nil {
		return
	}
	taskID := r.eventTaskID
	if taskID == "" {
		taskID = "workflow-runtime"
	}
	if err := r.emitter.AddEventWithPayload(ctx, taskID, kind, msg, payload); err != nil {
		log.Printf("event=workflow_runtime_emit_failed task_id=%s issue_id=%s kind=%s error=%q", taskID, taskID, kind, err)
	}
}

func (r *WorkflowRuntime) snapshotFromWorkflow(wf *workflow.Workflow, fingerprints ...string) *WorkflowSnapshot {
	if wf == nil {
		return &WorkflowSnapshot{}
	}
	fingerprint := ""
	if len(fingerprints) > 0 {
		fingerprint = fingerprints[0]
	} else if wf.Path != "" && wf.Source != workflow.SourceDefault {
		fingerprint, _ = workflowFileFingerprint(wf.Path)
	}
	reconcile := ReconciliationConfig{
		ActiveStates:      wf.Config.Tracker.ActiveStates,
		TerminalStates:    wf.Config.Tracker.TerminalStates,
		InactiveStates:    wf.Config.Tracker.InactiveStates,
		WorkerExitTimeout: 30 * time.Second,
		StallTimeoutMs:    wf.Config.Codex.StallTimeoutMs,
	}
	if r != nil && r.reconciliationConfig != nil {
		reconcile = r.reconciliationConfig(wf.Config)
	}
	return &WorkflowSnapshot{
		Workflow:                   wf,
		PollInterval:               time.Duration(wf.Config.Tracker.PollIntervalMs) * time.Millisecond,
		MaxConcurrentAgents:        wf.Config.Agent.MaxConcurrentAgents,
		MaxConcurrentAgentsByState: copyStateConcurrencyLimits(wf.Config.Agent.MaxConcurrentAgentsByState),
		MaxContinuationTurns:       wf.Config.Agent.MaxContinuationTurns,
		MaxRetryBackoff:            time.Duration(wf.Config.Agent.MaxRetryBackoffMs) * time.Millisecond,
		Reconciliation:             reconcile,
		Fingerprint:                fingerprint,
	}
}

func copyStateConcurrencyLimits(in map[string]int) map[string]int {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int, len(in))
	for state, limit := range in {
		out[state] = limit
	}
	return out
}

// normalizeStateConcurrencyLimits is an internal-package alias for the
// canonical [workflow.NormalizeStateConcurrencyLimits] helper. Keeping a
// thin local wrapper avoids touching every call site that depended on
// the old local function while the consolidation closes #294: the
// initial-load (loader.go) and the snapshot-build (this file) paths now
// go through one set of semantics.
func normalizeStateConcurrencyLimits(in map[string]int) map[string]int {
	return workflow.NormalizeStateConcurrencyLimits(in)
}

func workflowFileFingerprint(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", &workflow.Error{Category: workflow.CategoryMissingWorkflowFile, Path: path, Message: "read workflow file", Err: err}
		}
		return "", fmt.Errorf("%s: read workflow file: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

type PollLoopRuntimeOptions struct {
	Sleep          func(context.Context, time.Duration) error
	StopAfterPolls int
}

type pollOnce interface {
	PollOnce(context.Context) error
}

func RunPollLoopWithRuntime(ctx context.Context, poller pollOnce, runtime *WorkflowRuntime, opts PollLoopRuntimeOptions) error { //nolint:gocognit // baseline (#521)
	if poller == nil {
		return errors.New("orchestrator poll loop requires poller")
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	polls := 0
	for {
		interval := 30 * time.Second
		if runtime != nil {
			if snap := runtime.Current(); snap.PollInterval > 0 {
				interval = snap.PollInterval
			}
		}
		if err := poller.PollOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("event=tracker_poll_error error=%q", err)
		}
		polls++
		if runtimePoller, ok := poller.(*RuntimePoller); ok {
			if err := sleepOrRetryWake(ctx, sleep, interval, runtimePoller.orchestrator.retryWakeCh()); err != nil {
				return err
			}
		} else if err := sleep(ctx, interval); err != nil {
			return err
		}
		if opts.StopAfterPolls > 0 && polls >= opts.StopAfterPolls {
			return nil
		}
	}
}

func sleepOrRetryWake(ctx context.Context, sleep func(context.Context, time.Duration) error, interval time.Duration, wake <-chan struct{}) error {
	if wake == nil {
		return sleep(ctx, interval)
	}
	sleepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sleepErr := make(chan error, 1)
	go func() {
		defer recoverPanic("orchestrator.workflow_reload_sleep")
		sleepErr <- sleep(sleepCtx, interval)
	}()
	select {
	case err := <-sleepErr:
		return err
	case <-wake:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type WorkflowReloadLoopOptions struct {
	Sleep           func(context.Context, time.Duration) error
	StopAfterChecks int
}

func RunWorkflowReloadLoop(ctx context.Context, runtime *WorkflowRuntime, opts WorkflowReloadLoopOptions) error { //nolint:gocognit // baseline (#521)
	if runtime == nil {
		return errors.New("workflow reload loop requires runtime")
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	interval := runtime.ReloadInterval()
	if interval <= 0 {
		interval = time.Second
	}
	checks := 0
	for {
		if err := runtime.ReloadOnce(ctx); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		checks++
		if err := sleep(ctx, interval); err != nil {
			return err
		}
		if opts.StopAfterChecks > 0 && checks >= opts.StopAfterChecks {
			return nil
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
