package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
)

// fixedStateTracker is a minimal IssueStateLister/IssueStateRefresher that
// drives reconcileTick's inactive-state fan-out (Part B / derive) and narrow
// refresh (FetchIssueStatesByRefs). It returns scripted, deterministic results
// so reconcileTick branches can be pinned directly.
type fixedStateTracker struct {
	mu sync.Mutex
	// fixedListIssues, when non-nil, is returned verbatim from every
	// ListIssuesByStates call (the production fan-out then applies its own
	// empty-ID/active filters via deriveInactiveIssues). nil means "no inactive
	// issues".
	fixedListIssues []tracker.Issue

	fetchStates map[string]tracker.IssueState
	fetchErr    error
	listErr     error
}

func (f *fixedStateTracker) ListActiveIssues(ctx context.Context) ([]tracker.Issue, error) {
	return f.ListIssuesByStates(ctx, nil)
}

func (f *fixedStateTracker) ListIssuesByStates(_ context.Context, _ []string) ([]tracker.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fixedListIssues == nil {
		return nil, nil
	}
	out := make([]tracker.Issue, len(f.fixedListIssues))
	copy(out, f.fixedListIssues)
	return out, f.listErr
}

func (f *fixedStateTracker) FetchIssueStatesByRefs(_ context.Context, refs []tracker.IssueRef) (map[string]tracker.IssueState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	wanted := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		wanted[ref.ID] = struct{}{}
	}
	out := make(map[string]tracker.IssueState, len(refs))
	for id, st := range f.fetchStates {
		if _, ok := wanted[id]; ok {
			out[id] = st
		}
	}
	return out, f.fetchErr
}

func (f *fixedStateTracker) FetchIssueStatesByIDs(ctx context.Context, ids []string) (map[string]tracker.IssueState, error) {
	return f.FetchIssueStatesByRefs(ctx, tracker.IssueRefsFromIDs(ids))
}

func TestFetchIssueStatesNormalizesMissingRowsToUnknown(t *testing.T) {
	refresher := &fixedStateTracker{fetchStates: map[string]tracker.IssueState{
		"current": {Outcome: tracker.IssueStateOutcomeCurrent, State: "In Progress"},
		"extra":   {Outcome: tracker.IssueStateOutcomeAbsent},
	}}
	states, err := fetchIssueStates(context.Background(), refresher, []tracker.IssueRef{
		{ID: "current", Identifier: "LIN-1"},
		{ID: "missing", Identifier: "LIN-2"},
	})
	if err != nil {
		t.Fatalf("fetchIssueStates() error = %v; want nil", err)
	}
	if got := states["current"].Outcome; got != tracker.IssueStateOutcomeCurrent {
		t.Errorf("states[current].Outcome = %v; want Current", got)
	}
	missing, ok := states["missing"]
	if !ok || missing.Outcome != tracker.IssueStateOutcomeUnknown {
		t.Errorf("states[missing] = %+v present=%v; want explicit Unknown", missing, ok)
	}
	if _, ok := states["extra"]; ok {
		t.Errorf("states[extra] present = true; want false for unrequested adapter row")
	}
}

// TestReconcileTickPartACancelledContextReturnsFatally pins the load-bearing
// Part A fatal early-return: when the SPEC §16.3 stall reconciliation (Part A)
// errors AND ctx is already cancelled, reconcileTick must surface that error
// (return (nil map, err)) rather than swallowing it or proceeding. The actor
// loop is stopped first so the Part A submit deterministically fails with
// ctx.Err(); the guard's `if ctx.Err() != nil { return nil, err }` must then
// fire. A mutation that swallowed the fatal error (return nil, nil) or that did
// not surface a non-nil error fails this test.
//
// Note on scope: the orthogonal "always-accumulate" mutation (dropping the
// guard so the cancelled-ctx case falls through to Part B) is NOT
// behaviorally observable here — under a cancelled context every downstream
// orchestrator call also fails with the same context error, so the
// (nil map, context error) result is identical with or without the guard.
// The complementary over-eager-fatal direction (treating a LIVE-ctx Part A
// timeout as fatal) is pinned by
// TestPollOnceContinuesReconciliationWhenStalledRunCleanupTimesOut, which
// asserts Part B still reconciles an unrelated issue after a non-fatal Part A
// timeout. Together they fence the fatal-vs-non-fatal split.
func TestReconcileTickPartACancelledContextReturnsFatally(t *testing.T) {
	trackerClient := &fixedStateTracker{
		fixedListIssues: []tracker.Issue{{ID: "inactive-1", Identifier: "LIN-9", State: "Cancelled"}},
	}
	// The actor loop is intentionally never started, so its unbuffered ops
	// channel has no reader. Combined with an already-cancelled context, every
	// submit deterministically resolves via the ctx.Done() branch in submit,
	// making Part A's ReconcileStalledRuns return ctx.Err() with no scheduling
	// race.
	orch := New(NewOrchestratorState(30000, 4), Deps{
		Dispatcher: &cancellationDispatcher{},
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})

	poller := NewPollerWithReconciliation(trackerClient, orch, ReconciliationConfig{
		ActiveStates:      []string{"In Progress"},
		TerminalStates:    []string{"Cancelled"},
		WorkerExitTimeout: time.Millisecond,
		StallTimeoutMs:    50,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := poller.reconcileTick(ctx, []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}})
	if err == nil {
		t.Fatalf("reconcileTick(cancelled-ctx) err = %v; want non-nil Part A fatal error", err)
	}
	if got != nil {
		t.Fatalf("reconcileTick(cancelled-ctx) map = %#v; want nil (fatal early-return)", got)
	}
}

// TestReconcileTickDropsNonActiveNonInactiveRefreshedIssueFromInactiveSet pins
// the `if !p.isConfiguredInactiveState(issue.State) { continue }` guard in the
// derive step: a refreshed running issue that has left the active set but sits
// in a state that is neither terminal nor configured-inactive must NOT be added
// to inactiveByID (reconcileTick's returned map). A mutation that flipped the
// guard would add it; this test fails that.
func TestReconcileTickDropsNonActiveNonInactiveRefreshedIssueFromInactiveSet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fixedStateTracker{
		fetchStates: map[string]tracker.IssueState{"issue-1": {Outcome: tracker.IssueStateOutcomeCurrent, State: "Triage"}},
	}
	dispatcher := &cancellationDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPollerWithReconciliation(trackerClient, orch, ReconciliationConfig{
		ActiveStates:      []string{"In Progress"},
		TerminalStates:    []string{"Cancelled"},
		InactiveStates:    []string{"Backlog"},
		WorkerExitTimeout: time.Second,
	})

	if err := orch.RequestDispatch(ctx, tracker.Issue{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}, nil); err != nil {
		t.Fatalf("request dispatch: %v", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher)

	// "Triage" is neither an active state nor a configured terminal/inactive
	// state. The narrow refresh reports issue-1 there; reconcileTick must drop
	// it from the active set (no longer active) but must NOT add it to the
	// inactive map (it is not a configured inactive/terminal state).
	got, err := poller.reconcileTick(ctx, nil)
	if err != nil {
		t.Fatalf("reconcileTick err = %v; want nil", err)
	}
	if _, present := got["issue-1"]; present {
		t.Fatalf("reconcileTick inactive map[issue-1] present = %v; want false (Triage is not a configured inactive state)", got)
	}
}

// TestReconcileTickTreatsActiveIssueMissingRequiredLabelAsInactive pins the
// SPEC §6.4 label-removal release path: an issue still in an active state but
// missing a configured required label must be added to inactiveByID so the
// reconcile cancels its running worker / releases its retry+blocked claim. The
// narrow state refresh carries no labels, so this relies on the active listing
// (passed to reconcileTick) surfacing the reduced label set.
func TestReconcileTickTreatsActiveIssueMissingRequiredLabelAsInactive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Narrow refresh reports the running issue is STILL in an active state but
	// WITHOUT the required label (label removed mid-run). The §6.4 gate now reads
	// labels from the refresh, so only label removal (not a state change) makes it
	// ineligible here.
	trackerClient := &fixedStateTracker{fetchStates: map[string]tracker.IssueState{"issue-1": {Outcome: tracker.IssueStateOutcomeCurrent, State: "In Progress", Labels: []string{"backend"}}}}
	dispatcher := &cancellationDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPollerWithReconciliation(trackerClient, orch, ReconciliationConfig{
		ActiveStates:      []string{"In Progress"},
		TerminalStates:    []string{"Cancelled"},
		RequiredLabels:    []string{"aiops-ready"},
		WorkerExitTimeout: time.Second,
	})

	if err := orch.RequestDispatch(ctx, tracker.Issue{ID: "issue-1", Identifier: "LIN-1", State: "In Progress", Labels: []string{"aiops-ready"}}, nil); err != nil {
		t.Fatalf("request dispatch: %v", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher)

	// The narrow refresh (above) shows issue-1 still In Progress but missing the
	// required label, so reconcileTick must classify it inactive and cancel the
	// running worker. The active-listing arg still carries the label here; the
	// dedicated out-of-page test proves the refresh path independently.
	got, err := poller.reconcileTick(ctx, []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress", Labels: []string{"backend"}}})
	if err != nil {
		t.Fatalf("reconcileTick err = %v; want nil", err)
	}
	if _, present := got["issue-1"]; !present {
		t.Fatalf("reconcileTick inactive map[issue-1] present = false; want true (active issue missing required label must be reconciled inactive), map=%#v", got)
	}
}

// TestReconcileTickKeepsActiveIssueRetainingRequiredLabel is the negative
// counterpart: an active issue that still carries the required label must NOT be
// treated as inactive.
func TestReconcileTickKeepsActiveIssueRetainingRequiredLabel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Narrow refresh shows issue-1 still In Progress and STILL carrying the
	// required label, so it must NOT be reconciled inactive.
	trackerClient := &fixedStateTracker{fetchStates: map[string]tracker.IssueState{"issue-1": {Outcome: tracker.IssueStateOutcomeCurrent, State: "In Progress", Labels: []string{"aiops-ready", "backend"}}}}
	dispatcher := &cancellationDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPollerWithReconciliation(trackerClient, orch, ReconciliationConfig{
		ActiveStates:      []string{"In Progress"},
		TerminalStates:    []string{"Cancelled"},
		RequiredLabels:    []string{"aiops-ready"},
		WorkerExitTimeout: time.Second,
	})
	if err := orch.RequestDispatch(ctx, tracker.Issue{ID: "issue-1", Identifier: "LIN-1", State: "In Progress", Labels: []string{"aiops-ready"}}, nil); err != nil {
		t.Fatalf("request dispatch: %v", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher)

	got, err := poller.reconcileTick(ctx, []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress", Labels: []string{"aiops-ready", "backend"}}})
	if err != nil {
		t.Fatalf("reconcileTick err = %v; want nil", err)
	}
	if _, present := got["issue-1"]; present {
		t.Fatalf("reconcileTick inactive map[issue-1] present = true; want false (active issue still has required label), map=%#v", got)
	}
}

// newRunningLabelReconcilePoller dispatches a single running issue-1 (carrying
// the required label at dispatch time) and returns a poller whose reconcile
// config gates on requiredLabels and whose narrow refresh reports refresh. It is
// shared by the SPEC §6.4 label-removal reconcile tests below. The returned ctx
// is cancelled via t.Cleanup.
func newRunningLabelReconcilePoller(t *testing.T, refresh map[string]tracker.IssueState, requiredLabels []string) (*Poller, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	trackerClient := &fixedStateTracker{fetchStates: refresh}
	dispatcher := &cancellationDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}
	poller := NewPollerWithReconciliation(trackerClient, orch, ReconciliationConfig{
		ActiveStates:      []string{"In Progress"},
		TerminalStates:    []string{"Cancelled"},
		RequiredLabels:    requiredLabels,
		WorkerExitTimeout: time.Second,
	})
	if err := orch.RequestDispatch(ctx, tracker.Issue{ID: "issue-1", Identifier: "LIN-1", State: "In Progress", Labels: []string{"aiops-ready"}}, nil); err != nil {
		t.Fatalf("request dispatch: %v", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher)
	return poller, ctx
}

// TestReconcileTickCancelsOutOfPageRunningIssueMissingRequiredLabel is the P2-b
// proof: a running issue absent from the active listing (beyond its page) whose
// narrow refresh shows it still active but missing a required label must be
// reconciled inactive. The label evidence comes only from the refresh (the
// active-listing arg is empty here), which the old active-listing-only sweep
// could not observe.
func TestReconcileTickCancelsOutOfPageRunningIssueMissingRequiredLabel(t *testing.T) {
	poller, ctx := newRunningLabelReconcilePoller(t,
		map[string]tracker.IssueState{"issue-1": {Outcome: tracker.IssueStateOutcomeCurrent, State: "In Progress", Labels: []string{"backend"}}},
		[]string{"aiops-ready"})

	// Active listing is EMPTY (issue-1 is out of page); only the refresh carries it.
	got, err := poller.reconcileTick(ctx, nil)
	if err != nil {
		t.Fatalf("reconcileTick err = %v; want nil", err)
	}
	if _, present := got["issue-1"]; !present {
		t.Fatalf("reconcileTick inactive map[issue-1] present = false; want true (out-of-page running issue missing required label must be reconciled inactive via refresh), map=%#v", got)
	}
}

// TestReconcileTickKeepsOutOfPageRunningIssueRetainingRequiredLabel pins that
// the refresh's labels (not the active listing's) are authoritative for an
// out-of-page in-flight issue: issue-1 is absent from the active listing, and
// only the narrow refresh reports it — still active and STILL carrying the
// required label — so it must NOT be reconciled inactive. Without carrying the
// refreshed labels onto the refreshed issue, it would be seen as label-less and
// wrongly cancelled (false cancel).
func TestReconcileTickKeepsOutOfPageRunningIssueRetainingRequiredLabel(t *testing.T) {
	poller, ctx := newRunningLabelReconcilePoller(t,
		map[string]tracker.IssueState{"issue-1": {Outcome: tracker.IssueStateOutcomeCurrent, State: "In Progress", Labels: []string{"aiops-ready", "backend"}}},
		[]string{"aiops-ready"})

	got, err := poller.reconcileTick(ctx, nil) // issue-1 out of page; only refresh has it
	if err != nil {
		t.Fatalf("reconcileTick err = %v; want nil", err)
	}
	if _, present := got["issue-1"]; present {
		t.Fatalf("reconcileTick inactive map[issue-1] present = true; want false (out-of-page running issue still carries the required label per the refresh), map=%#v", got)
	}
}

// TestReconcileTickKeepsRunningIssueAbsentFromRefresh pins the
// no-information-on-absence invariant: when the narrow refresh returns NO row
// for a running issue (transient/partial fetch) and the active listing is empty,
// reconcileTick must NOT treat it as label-ineligible and must NOT cancel it.
func TestReconcileTickKeepsRunningIssueAbsentFromRefresh(t *testing.T) {
	poller, ctx := newRunningLabelReconcilePoller(t,
		map[string]tracker.IssueState{}, // refresh returns no row for issue-1
		[]string{"aiops-ready"})

	got, err := poller.reconcileTick(ctx, nil)
	if err != nil {
		t.Fatalf("reconcileTick err = %v; want nil", err)
	}
	if _, present := got["issue-1"]; present {
		t.Fatalf("reconcileTick inactive map[issue-1] present = true; want false (a refresh that returns no row is no-information, not label removal), map=%#v", got)
	}
}

func TestPollerReconcileClaimedTickAppliesAuthoritativeRefreshOutcomesDespiteError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		runningAbsent       = IssueID("running-absent")
		runningAbsentListed = IssueID("running-absent-listed")
		runningActive       = IssueID("running-active")
		runningTerminal     = IssueID("running-terminal")
		blockedAbsent       = IssueID("blocked-absent")
		blockedUnknown      = IssueID("blocked-unknown")
		continuation        = IssueID("continuation-absent")
		failure             = IssueID("failure-absent")
		quota               = IssueID("quota-absent")
		missing             = IssueID("continuation-missing")
	)
	issue := func(id IssueID) tracker.Issue {
		return tracker.Issue{ID: string(id), Identifier: strings.ToUpper(string(id)), State: "In Progress"}
	}
	closedDone := make(chan struct{})
	close(closedDone)
	cancelCauses := make(map[IssueID]error)
	st := NewOrchestratorState(30000, 16)
	seedRun := func(id IssueID) *RunningEntry {
		run := &RunningEntry{
			Issue: issue(id), Identifier: issue(id).Identifier, Done: closedDone,
			CancelWorker: func(err error) { cancelCauses[id] = err },
		}
		st.BeginDispatch(id, run)
		return run
	}
	absentRun := seedRun(runningAbsent)
	absentRun.ReconcileCleanupWorkspace = true
	absentRun.AgentCurrentIssueHandoff = true
	absentRun.AgentCurrentIssueTerminalHandoff = true
	absentRun.AgentCurrentIssueTerminalHandoffState = "Done"
	seedRun(runningAbsentListed)
	seedRun(runningActive)
	seedRun(runningTerminal)
	for _, id := range []IssueID{blockedAbsent, blockedUnknown} {
		st.Blocked[id] = &BlockedEntry{Issue: issue(id), Identifier: issue(id).Identifier}
		st.Claimed[id] = struct{}{}
		st.ClaimedIssues[id] = issue(id)
	}
	for _, retry := range []struct {
		id   IssueID
		kind RetryKind
	}{
		{id: continuation, kind: RetryKindContinuation},
		{id: failure, kind: RetryKindFailure},
		{id: quota, kind: RetryKindQuotaBackoff},
		{id: missing, kind: RetryKindContinuation},
	} {
		st.ScheduleRetry(&RetryEntry{Issue: issue(retry.id), IssueID: retry.id, Identifier: issue(retry.id).Identifier, Kind: retry.kind})
	}

	refreshErr := errors.New("one refresh chunk failed")
	trackerClient := &fixedStateTracker{
		fixedListIssues: []tracker.Issue{
			{ID: string(runningAbsent), Identifier: issue(runningAbsent).Identifier, State: "Done"},
			{ID: string(runningTerminal), Identifier: issue(runningTerminal).Identifier, State: "Done"},
		},
		fetchStates: map[string]tracker.IssueState{
			string(runningAbsent):       {Outcome: tracker.IssueStateOutcomeAbsent},
			string(runningAbsentListed): {Outcome: tracker.IssueStateOutcomeAbsent},
			string(runningActive):       {Outcome: tracker.IssueStateOutcomeCurrent, State: "Rework", Labels: []string{"fresh"}},
			string(runningTerminal):     {Outcome: tracker.IssueStateOutcomeCurrent, State: "Done"},
			string(blockedAbsent):       {Outcome: tracker.IssueStateOutcomeAbsent},
			string(blockedUnknown):      {Outcome: tracker.IssueStateOutcomeUnknown},
			string(continuation):        {Outcome: tracker.IssueStateOutcomeAbsent},
			string(failure):             {Outcome: tracker.IssueStateOutcomeAbsent},
			string(quota):               {Outcome: tracker.IssueStateOutcomeAbsent},
		},
		fetchErr: refreshErr,
	}
	cleaner := &recordingWorkspaceCleaner{}
	o := New(st, Deps{Dispatcher: &cancellationDispatcher{}, Scheduler: RetryScheduler{MaxBackoff: time.Hour}, WorkspaceCleaner: cleaner})
	safeGo("test.refresh_outcome_matrix_actor", func() { o.Run(ctx) })
	if err := o.WaitStarted(ctx); err != nil {
		t.Fatalf("WaitStarted() error = %v; want nil", err)
	}
	poller := NewPollerWithReconciliation(trackerClient, o, ReconciliationConfig{
		ActiveStates: []string{"In Progress", "Rework"}, TerminalStates: []string{"Done"}, WorkerExitTimeout: time.Second,
	})

	result, err := poller.reconcileClaimedTick(ctx, []tracker.Issue{issue(runningAbsentListed)})
	if !errors.Is(err, refreshErr) {
		t.Fatalf("reconcileClaimedTick error = %v; want mixed refresh error %v", err, refreshErr)
	}
	for _, id := range []IssueID{runningActive, runningTerminal} {
		if _, ok := result.refreshed[string(id)]; !ok {
			t.Errorf("refreshed[%s] missing; want authoritative Current row applied", id)
		}
	}
	if _, ok := result.refreshed[string(runningAbsent)]; ok {
		t.Errorf("refreshed[%s] present = true; want false for Absent", runningAbsent)
	}
	if _, ok := result.inactive[string(runningAbsent)]; ok {
		t.Errorf("inactive[%s] present from stale listing = true; want false because explicit Absent wins", runningAbsent)
	}
	if _, ok := result.inactive[string(runningTerminal)]; !ok {
		t.Errorf("inactive[%s] missing; want Current terminal row reconciled", runningTerminal)
	}

	o.WithStateForTest(func(got *OrchestratorState) {
		active := got.Running[runningActive]
		if active == nil || active.Issue.State != "Rework" || active.ReconcileCancel {
			t.Errorf("active run = %+v; want refreshed Rework run retained", active)
		}
		absent := got.Running[runningAbsent]
		if absent == nil || !absent.ReconcileCancel || absent.ReconcileCleanupWorkspace || absent.AgentCurrentIssueHandoff || absent.AgentCurrentIssueTerminalHandoff {
			t.Errorf("absent run flags = %+v; want cancel without cleanup/handoff", absent)
		}
		terminal := got.Running[runningTerminal]
		if terminal == nil || !terminal.ReconcileCancel || !terminal.ReconcileCleanupWorkspace {
			t.Errorf("terminal run flags = %+v; want terminal cancel with cleanup", terminal)
		}
		if _, ok := got.Blocked[blockedAbsent]; ok {
			t.Errorf("Blocked[%s] retained; want released on explicit Absent", blockedAbsent)
		}
		if _, ok := got.Blocked[blockedUnknown]; !ok {
			t.Errorf("Blocked[%s] missing; want retained on Unknown", blockedUnknown)
		}
		if _, ok := got.RetryAttempts[continuation]; ok {
			t.Errorf("RetryAttempts[%s] retained; want continuation released on Absent", continuation)
		}
		for _, id := range []IssueID{failure, quota, missing} {
			if got.RetryAttempts[id] == nil {
				t.Errorf("RetryAttempts[%s] = nil; want retained for timer-owned or unknown outcome", id)
			}
		}
		if _, ok := got.LookupOperatorTerminalStop(runningAbsent); ok {
			t.Errorf("operator terminal stop[%s] present = true; want false because absence is not terminal", runningAbsent)
		}
		if _, ok := got.ClaimedIssues[runningAbsentListed]; ok {
			t.Errorf("ClaimedIssues[%s] present after stale active listing = true; want false because explicit Absent wins", runningAbsentListed)
		}
		if _, ok := got.LookupOperatorTerminalStop(runningTerminal); !ok {
			t.Errorf("operator terminal stop[%s] present = false; want true for Current terminal", runningTerminal)
		}
	})
	for _, id := range []IssueID{runningAbsent, runningAbsentListed, runningTerminal} {
		if !errors.Is(cancelCauses[id], worker.ErrReconcileCancel) {
			t.Errorf("cancel cause[%s] = %v; want %v", id, cancelCauses[id], worker.ErrReconcileCancel)
		}
	}
	if _, canceled := cancelCauses[runningActive]; canceled {
		t.Errorf("active run %s canceled = true; want false", runningActive)
	}
	if calls := cleaner.snapshot(); len(calls) != 0 {
		t.Errorf("workspace cleanups = %+v; want none before running workers finalize and for Absent", calls)
	}
}

// TestReconcileTickEmptyRequiredLabelsKeepsLabellessRunningIssue pins the
// gate-off default: with no required_labels, a running issue carrying no matching
// label must NOT be reconciled inactive on label grounds.
func TestReconcileTickEmptyRequiredLabelsKeepsLabellessRunningIssue(t *testing.T) {
	poller, ctx := newRunningLabelReconcilePoller(t,
		map[string]tracker.IssueState{"issue-1": {Outcome: tracker.IssueStateOutcomeCurrent, State: "In Progress", Labels: []string{"backend"}}},
		nil) // gate off

	got, err := poller.reconcileTick(ctx, nil)
	if err != nil {
		t.Fatalf("reconcileTick err = %v; want nil", err)
	}
	if _, present := got["issue-1"]; present {
		t.Fatalf("reconcileTick inactive map[issue-1] present = true; want false (empty required_labels disables the label gate), map=%#v", got)
	}
}

// TestReconcileTickSkipsEmptyIDIssueFromStateGroupFanOut pins the
// `if issue.ID == "" { continue }` skip inside the inactive state-group
// fan-out: an inactive-state listing that returns an entry with an empty ID
// must not be added to inactiveByID under the "" key. A mutation removing the
// skip would insert a "" key; this test fails that.
func TestReconcileTickSkipsEmptyIDIssueFromStateGroupFanOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fixedStateTracker{
		fixedListIssues: []tracker.Issue{
			{ID: "", Identifier: "LIN-EMPTY", State: "Cancelled"},
			{ID: "inactive-1", Identifier: "LIN-2", State: "Cancelled"},
		},
	}
	dispatcher := &cancellationDispatcher{}
	st := NewOrchestratorState(30000, 1)
	st.Blocked[IssueID("claimed-1")] = &BlockedEntry{Identifier: "LIN-1", Issue: tracker.Issue{ID: "claimed-1", Identifier: "LIN-1", State: "In Progress"}}
	orch := New(st, Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPollerWithReconciliation(trackerClient, orch, ReconciliationConfig{
		ActiveStates:      []string{"In Progress"},
		TerminalStates:    []string{"Cancelled"},
		WorkerExitTimeout: time.Second,
	})

	got, err := poller.reconcileTick(ctx, nil)
	if err != nil {
		t.Fatalf("reconcileTick err = %v; want nil", err)
	}
	if _, present := got[""]; present {
		t.Fatalf("reconcileTick inactive map[\"\"] present = %v; want false (empty-ID entry must be skipped)", got)
	}
	if _, present := got["inactive-1"]; !present {
		t.Fatalf("reconcileTick inactive map[inactive-1] present = false; want true (valid fan-out entry kept), map=%#v", got)
	}
}

// TestReconcileTickRequiresStateTracker pins the nil stateTracker guard: a
// poller without a state tracker must fail reconciliation with the sentinel
// error rather than dereferencing a nil tracker. A mutation removing the guard
// would panic or behave differently.
func TestReconcileTickRequiresStateTracker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: &cancellationDispatcher{},
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := &Poller{orchestrator: orch, reconcile: ReconciliationConfig{ActiveStates: []string{"In Progress"}}}
	got, err := poller.reconcileTick(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "requires state tracker") {
		t.Fatalf("reconcileTick(nil stateTracker) err = %v; want \"requires state tracker\" error", err)
	}
	if got != nil {
		t.Fatalf("reconcileTick(nil stateTracker) map = %#v; want nil", got)
	}
}
