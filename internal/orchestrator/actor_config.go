package orchestrator

// actor_config.go holds the Orchestrator entry points that submit
// configuration hot-reload and read-only snapshot/query ops onto the actor
// goroutine. See actor.go for the actor's mutation discipline.

import (
	"context"
	"sort"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// Snapshot returns a SPEC §13.3-shaped view of the orchestrator state.
// The snapshot is taken on the actor goroutine so it observes a
// consistent state between mutations. Returns ctx.Err() if ctx is
// cancelled before the actor produces the view.
func (o *Orchestrator) Snapshot(ctx context.Context) (StateView, error) {
	reply := make(chan StateView, 1)
	op := opFunc(func(st *OrchestratorState) func() {
		reply <- st.Snapshot()
		return nil
	})
	if err := o.submit(ctx, op); err != nil {
		return StateView{}, err
	}
	select {
	case v := <-reply:
		return v, nil
	case <-ctx.Done():
		return StateView{}, ctx.Err()
	}
}

// RecordWorkspace stores the deterministic workspace path for a running issue
// so blocked-session status and later reconciliation cleanup can refer to the
// actual on-disk checkout.
func (o *Orchestrator) RecordWorkspace(ctx context.Context, issueID string, workspace Workspace) error {
	if o == nil || strings.TrimSpace(issueID) == "" || strings.TrimSpace(workspace.Path) == "" {
		return nil
	}
	done := make(chan struct{})
	op := opFunc(func(st *OrchestratorState) func() {
		if run := st.Running[IssueID(issueID)]; run != nil {
			run.Workspace = workspace
		}
		close(done)
		return nil
	})
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// UpdateMaxConcurrentAgents applies a reloaded workflow capacity limit through
// the actor so dispatch and retry capacity checks observe the new value without
// restarting the process.
func (o *Orchestrator) UpdateMaxConcurrentAgents(ctx context.Context, maxConcurrentAgents int) error {
	if maxConcurrentAgents <= 0 {
		return nil
	}
	done := make(chan struct{}, 1)
	op := opFunc(func(st *OrchestratorState) func() {
		st.MaxConcurrentAgents = maxConcurrentAgents
		done <- struct{}{}
		return nil
	})
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// UpdateMaxConcurrentAgentsByState applies reloaded per-state capacity limits
// through the actor so dispatch and retry capacity checks observe them without
// restarting the process.
func (o *Orchestrator) UpdateMaxConcurrentAgentsByState(ctx context.Context, limits map[string]int) error {
	done := make(chan struct{}, 1)
	op := opFunc(func(st *OrchestratorState) func() {
		st.MaxConcurrentAgentsByState = normalizeStateConcurrencyLimits(limits)
		done <- struct{}{}
		return nil
	})
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// UpdatePollIntervalMs applies reloaded workflow poll cadence metadata through
// the actor so /api/v1/state reflects the runtime cadence after workflow reload.
func (o *Orchestrator) UpdatePollIntervalMs(ctx context.Context, pollIntervalMs int64) error {
	if pollIntervalMs <= 0 {
		return nil
	}
	done := make(chan struct{}, 1)
	op := opFunc(func(st *OrchestratorState) func() {
		st.PollIntervalMs = pollIntervalMs
		done <- struct{}{}
		return nil
	})
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// UpdateRetryScheduler applies reloaded retry timing through the actor so
// subsequently scheduled retries observe workflow changes without a process
// restart.
func (o *Orchestrator) UpdateRetryScheduler(ctx context.Context, scheduler Scheduler) error {
	if scheduler == nil {
		return nil
	}
	done := make(chan struct{}, 1)
	op := opFunc(func(*OrchestratorState) func() {
		o.scheduler = scheduler
		done <- struct{}{}
		return nil
	})
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// UpdateMaxFailureRetries applies the reloaded failure retry budget through the
// actor. The budget counts scheduled failure retry entries after the first run;
// any negative value (including the workflow-layer UnboundedRetryBudget sentinel
// that a workflow with no `agent.max_retry_attempts` produces) disables the cap
// and restores SPEC §8.4 unbounded retries. Zero disables failure retries
// outright as a deliberate opt-in. Clean continuations are bounded separately
// by agent.max_turns.
func (o *Orchestrator) UpdateMaxFailureRetries(ctx context.Context, maxFailureRetries int) error {
	if maxFailureRetries < 0 {
		maxFailureRetries = UnboundedFailureRetries
	}
	done := make(chan struct{}, 1)
	op := opFunc(func(*OrchestratorState) func() {
		o.maxFailureRetries = maxFailureRetries
		done <- struct{}{}
		return nil
	})
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *Orchestrator) RunningRetryingAndBlockedIssueIDs(ctx context.Context) []string {
	reply := make(chan []string, 1)
	if err := o.submit(ctx, &runningRetryingAndBlockedIssueIDsOp{result: reply}); err != nil {
		return nil
	}
	select {
	case ids := <-reply:
		return ids
	case <-ctx.Done():
		return nil
	}
}
func (o *Orchestrator) RunningRetryingAndBlockedIssueRefs(ctx context.Context) []tracker.IssueRef {
	view, err := o.Snapshot(ctx)
	if err != nil {
		return nil
	}
	refs := make([]tracker.IssueRef, 0, len(view.Running)+len(view.Retrying)+len(view.Blocked))
	seen := map[string]struct{}{}
	add := func(issueID IssueID, identifier string) {
		id := strings.TrimSpace(string(issueID))
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		refs = append(refs, tracker.IssueRef{ID: id, Identifier: strings.TrimSpace(identifier)})
	}
	for _, run := range view.Running {
		add(run.IssueID, run.Identifier)
	}
	for _, retry := range view.Retrying {
		add(retry.IssueID, retry.Identifier)
	}
	for _, blocked := range view.Blocked {
		add(blocked.IssueID, blocked.Identifier)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	return refs
}
func (o *Orchestrator) RunningAndRetryingIssueIDs(ctx context.Context) []string {
	return o.RunningRetryingAndBlockedIssueIDs(ctx)
}

type runningRetryingAndBlockedIssueIDsOp struct {
	result chan<- []string
}

func (r *runningRetryingAndBlockedIssueIDsOp) apply(st *OrchestratorState) func() {
	seen := map[string]struct{}{}
	issueIDs := make([]string, 0, len(st.Running)+len(st.RetryAttempts)+len(st.Blocked))
	add := func(id IssueID) {
		s := strings.TrimSpace(string(id))
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		issueIDs = append(issueIDs, s)
	}
	for id := range st.Running {
		add(id)
	}
	for id := range st.RetryAttempts {
		add(id)
	}
	for id := range st.Blocked {
		add(id)
	}
	sort.Strings(issueIDs)
	result := r.result
	return func() {
		if result != nil {
			result <- issueIDs
		}
	}
}
