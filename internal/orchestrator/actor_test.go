package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// fakeDispatcher records every Spawn call and lets the test control
// when each spawned worker "exits" by closing/sending on the per-call
// result channel. Concurrent calls are safe: spawnCount and the
// results slice are guarded by a mutex.
type fakeDispatcher struct {
	mu           sync.Mutex
	spawnCount   int
	results      []chan WorkerResult
	contexts     []context.Context
	issues       []tracker.Issue
	attempts     []*int
	cleanBudgets []int

	// onSpawn, if set, runs synchronously inside Spawn so tests can
	// inject scheduling pressure or block until they want the worker
	// to "start running".
	onSpawn func(ctx context.Context, issue tracker.Issue, attempt *int)
}

type recordingWorkerEmitter struct {
	kinds []string
}

func (r *recordingWorkerEmitter) AddEvent(ctx context.Context, taskID, typ, msg string) error {
	return r.AddEventWithPayload(ctx, taskID, typ, msg, nil)
}

func (r *recordingWorkerEmitter) AddEventWithPayload(_ context.Context, _, typ, _ string, _ any) error {
	r.kinds = append(r.kinds, typ)
	return nil
}

type earlyRuntimeEventDispatcher struct {
	orchestrator *Orchestrator
}

func (d *earlyRuntimeEventDispatcher) AttachOrchestrator(o *Orchestrator) {
	d.orchestrator = o
}

func (d *earlyRuntimeEventDispatcher) Spawn(ctx context.Context, issue tracker.Issue, _ *int, _ DispatchOptions) <-chan WorkerResult {
	_ = d.orchestrator.RecordRuntimeEvent(ctx, issue.ID, task.RuntimeEvent{
		Event:   task.EventTurnCompleted,
		Payload: map[string]any{"usage": map[string]any{"total_tokens": 5}},
	})
	out := make(chan WorkerResult)
	return out
}

type sequenceScheduler struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (s *sequenceScheduler) NextDelay(RetryRequest) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextDelayLocked()
}

func (s *sequenceScheduler) nextDelayLocked() time.Duration {
	if len(s.delays) == 0 {
		return time.Hour
	}
	d := s.delays[0]
	s.delays = s.delays[1:]
	return d
}

type recordingScheduler struct {
	delay time.Duration

	mu       sync.Mutex
	requests []RetryRequest
}

func (s *recordingScheduler) NextDelay(req RetryRequest) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	if req.DelayOverride > 0 {
		return req.DelayOverride
	}
	if s.delay > 0 {
		return s.delay
	}
	return time.Hour
}

func (s *recordingScheduler) lastRequest() (RetryRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return RetryRequest{}, false
	}
	return s.requests[len(s.requests)-1], true
}

func (s *recordingScheduler) snapshotRequests() []RetryRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RetryRequest(nil), s.requests...)
}

func (f *fakeDispatcher) Spawn(ctx context.Context, issue tracker.Issue, attempt *int, opts DispatchOptions) <-chan WorkerResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onSpawn != nil {
		f.onSpawn(ctx, issue, attempt)
	}
	ch := make(chan WorkerResult, 1)
	f.results = append(f.results, ch)
	f.contexts = append(f.contexts, ctx)
	f.issues = append(f.issues, issue)
	f.attempts = append(f.attempts, attempt)
	f.cleanBudgets = append(f.cleanBudgets, opts.CleanTurnBudget)
	f.spawnCount++
	return ch
}

func (f *fakeDispatcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawnCount
}

func (f *fakeDispatcher) issueAt(i int) tracker.Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issues[i]
}

func (f *fakeDispatcher) attemptValueAt(i int) *int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attempts[i] == nil {
		return nil
	}
	attempt := *f.attempts[i]
	return &attempt
}

func (f *fakeDispatcher) cleanTurnBudgetAt(i int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cleanBudgets[i]
}

func (f *fakeDispatcher) context() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.contexts[0]
}

// finishAt completes the i-th spawned worker with the given result.
// Tests use this to drive the worker-exit path through the actor.
func (f *fakeDispatcher) finishAt(i int, res WorkerResult) {
	f.mu.Lock()
	ch := f.results[i]
	f.mu.Unlock()
	ch <- res
	close(ch)
}

func startActor(t *testing.T, deps Deps) (*Orchestrator, context.CancelFunc) {
	t.Helper()
	st := NewOrchestratorState(15000, 100)
	o := New(st, deps)
	ctx, cancel := context.WithCancel(context.Background())
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}
	return o, cancel
}

func TestRequestRefreshQueuesOnePollWakeAndCoalescesRepeatedRequests(t *testing.T) {
	o, cancel := startActor(t, Deps{Dispatcher: &fakeDispatcher{}, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	first, err := o.RequestRefresh(context.Background())
	if err != nil {
		t.Fatalf("first RequestRefresh: %v", err)
	}
	if !first.Queued || first.Coalesced {
		t.Fatalf("first refresh = %+v, want queued and not coalesced", first)
	}
	if !reflect.DeepEqual(first.Operations, []string{"poll", "reconcile"}) {
		t.Fatalf("first operations = %#v, want poll+reconcile", first.Operations)
	}

	second, err := o.RequestRefresh(context.Background())
	if err != nil {
		t.Fatalf("second RequestRefresh: %v", err)
	}
	if !second.Queued || !second.Coalesced {
		t.Fatalf("second refresh = %+v, want queued and coalesced", second)
	}

	select {
	case <-o.retryWakeCh():
	case <-time.After(time.Second):
		t.Fatal("refresh did not queue poll wake")
	}
	select {
	case <-o.retryWakeCh():
		t.Fatal("coalesced refresh queued a second poll wake")
	default:
	}

	third, err := o.RequestRefresh(context.Background())
	if err != nil {
		t.Fatalf("third RequestRefresh: %v", err)
	}
	if !third.Queued || third.Coalesced {
		t.Fatalf("third refresh after draining wake = %+v, want new non-coalesced wake", third)
	}
}

// TestRequestDispatch_ConcurrentSameIssueProducesOneRunning is the
// first of the two concurrency invariants the D21+D6 design names for
// PR 2: under N goroutines racing to claim the same issue, exactly one
// dispatch is accepted, exactly one Running entry exists, and the
// dispatcher is asked to spawn exactly one worker. SPEC §7.4 calls
// this the "duplicate-dispatch guard"; the actor's serialization is
// the mechanism that delivers it.
func TestRequestDispatch_ConcurrentSameIssueProducesOneRunning(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-1", Identifier: "ENG-1", Title: "race"}

	const N = 64
	var (
		wg       sync.WaitGroup
		accepted atomic.Int32
		denied   atomic.Int32
	)
	wg.Add(N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start
			err := o.RequestDispatch(context.Background(), iss, nil)
			switch {
			case err == nil:
				accepted.Add(1)
			case errors.Is(err, ErrNotDispatched):
				denied.Add(1)
			default:
				t.Errorf("unexpected RequestDispatch error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := accepted.Load(); got != 1 {
		t.Errorf("accepted dispatches = %d, want 1 (SPEC §7.4 duplicate-dispatch guard)", got)
	}
	if got := denied.Load(); got != N-1 {
		t.Errorf("denied dispatches = %d, want %d", got, N-1)
	}

	// Snapshot also serializes through the actor, so by the time it
	// returns the registerRunningOp from the accepted dispatch has
	// been applied.
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Running) != 1 {
		t.Errorf("Running view = %d entries, want 1: %+v", len(view.Running), view.Running)
	}
	if got := disp.count(); got != 1 {
		t.Errorf("Dispatcher.Spawn calls = %d, want 1", got)
	}
}

// TestScheduleRetry_TimerFireProducesExactlyOneReDispatch is the second
// invariant: when a retry timer fires, the actor's serialization
// guarantees exactly one re-dispatch. The classical race we're guarding
// against is "the timer callback submits retryFireOp; meanwhile a
// concurrent ScheduleRetry replaces the entry; the late retryFireOp
// arrives at the actor". The attempt-equality guard in retryFireOp
// makes the late fire a no-op, so the dispatcher sees exactly one Spawn
// for the latest scheduled attempt.
func TestScheduleRetry_TimerFireProducesExactlyOneReDispatch(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  RetryScheduler{MaxBackoff: 5 * time.Millisecond},
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-9", Identifier: "ENG-9", Title: "retry"}

	// Schedule the retry many times concurrently. Each call replaces
	// the prior entry's timer (via OrchestratorState.ScheduleRetry's
	// timer.Stop) but every call also submits a retryFireOp when its
	// timer fires. The actor's serialization is what makes the count
	// of re-dispatches deterministic: at most one for the entry that
	// "wins" the race.
	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			if err := o.ScheduleRetry(context.Background(), iss, iss.Identifier, i+1, "boom"); err != nil {
				t.Errorf("ScheduleRetry: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	// Wait long enough for every scheduled timer (delay 5ms) to fire
	// and for any retryFireOps to be processed by the actor.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		view, err := o.Snapshot(context.Background())
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if len(view.Retrying) == 0 && disp.count() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := disp.count(); got != 1 {
		t.Errorf("Dispatcher.Spawn calls after retry fires = %d, want exactly 1", got)
	}

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Retrying) != 0 {
		t.Errorf("Retrying view = %+v, want empty after re-dispatch", view.Retrying)
	}
	if len(view.Running) != 1 {
		t.Errorf("Running view = %+v, want 1 entry from the re-dispatch", view.Running)
	}
}

// TestReconcileStalledRunsCancelsWorkerPastTimeout pins SPEC §8.5 Part A:
// a running entry whose LastEventAt is older than the configured stall
// budget gets its worker context cancelled, and the finalize path treats
// the resulting context.Canceled as a normal worker failure (claim
// retained, retry scheduled), NOT as a reconciled cancel.
func TestReconcileStalledRunsCancelsWorkerPastTimeout(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "STALL-1", Identifier: "STALL-1", Title: "stuck", State: "Todo"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	// Force the running entry's LastEventAt into the past so the stall
	// budget (100ms) is comfortably exceeded.
	o.WithStateForTest(func(st *OrchestratorState) {
		st.Running["STALL-1"].LastEventAt = time.Now().Add(-10 * time.Second)
	})

	if err := o.ReconcileStalledRuns(context.Background(), 100, 0); err != nil {
		t.Fatalf("ReconcileStalledRuns: %v", err)
	}

	select {
	case <-disp.context().Done():
	case <-time.After(time.Second):
		t.Fatal("stalled worker context was not cancelled by ReconcileStalledRuns")
	}
}

// TestReconcileStalledRunsLeavesActiveRunsAlone pins the no-false-positive
// invariant: an entry with a recent LastEventAt must not be cancelled
// even when the stall budget is small.
func TestReconcileStalledRunsLeavesActiveRunsAlone(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ACTIVE-1", Identifier: "ACTIVE-1", State: "Todo"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	o.WithStateForTest(func(st *OrchestratorState) {
		st.Running["ACTIVE-1"].LastEventAt = time.Now()
	})

	if err := o.ReconcileStalledRuns(context.Background(), 5_000, 0); err != nil {
		t.Fatalf("ReconcileStalledRuns: %v", err)
	}

	select {
	case <-disp.context().Done():
		t.Fatal("active worker context was cancelled even though LastEventAt is recent")
	case <-time.After(100 * time.Millisecond):
		// Expected — context still live.
	}
}

// TestReconcileStalledRunsSkipsWhenTimeoutDisabled covers SPEC §8.5 Part A's
// "if stall_timeout_ms <= 0, skip stall detection entirely" clause.
func TestReconcileStalledRunsSkipsWhenTimeoutDisabled(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "OFF-1", Identifier: "OFF-1", State: "Todo"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	// Even with an ancient LastEventAt, stall detection is a no-op when
	// the budget is 0.
	o.WithStateForTest(func(st *OrchestratorState) {
		st.Running["OFF-1"].LastEventAt = time.Now().Add(-10 * time.Second)
	})
	if err := o.ReconcileStalledRuns(context.Background(), 0, 0); err != nil {
		t.Fatalf("ReconcileStalledRuns: %v", err)
	}
	select {
	case <-disp.context().Done():
		t.Fatal("stall_timeout_ms=0 should disable detection but the worker was cancelled")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRefreshActiveTrackerIssuesRefreshesIssueStateForCapacityCounting(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 10)
	st.MaxConcurrentAgentsByState = map[string]int{"rework": 1}
	o := New(st, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	iss := tracker.Issue{ID: "ENG-MOVE", Identifier: "ENG-MOVE", Title: "moves state", State: "In Progress"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	moved := iss
	moved.State = "Rework"
	active := map[string]tracker.Issue{moved.ID: moved}
	if err := o.RefreshActiveTrackerIssues(context.Background(), active, normalizedStates([]string{"In Progress", "Rework"})); err != nil {
		t.Fatalf("RefreshActiveTrackerIssues: %v", err)
	}

	other := tracker.Issue{ID: "ENG-OTHER", Identifier: "ENG-OTHER", Title: "second rework", State: "Rework"}
	if err := o.RequestDispatch(context.Background(), other, nil); !errors.Is(err, ErrCapacityFull) {
		t.Fatalf("dispatch second rework after reconciliation moved first into Rework err = %v, want ErrCapacityFull", err)
	}
	if got := disp.count(); got != 1 {
		t.Fatalf("dispatcher count after reconciliation = %d, want 1 (per-state cap must see refreshed state)", got)
	}
}

func TestFinalizeRunSchedulesRetryWithRefreshedIssueState(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 10)
	st.MaxConcurrentAgentsByState = map[string]int{"rework": 1}
	o := New(st, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Hour}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	moving := tracker.Issue{ID: "ENG-MOVE", Identifier: "ENG-MOVE", Title: "moves and fails", State: "In Progress"}
	if err := o.RequestDispatch(context.Background(), moving, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	moved := moving
	moved.State = "Rework"
	active := map[string]tracker.Issue{moved.ID: moved}
	if err := o.RefreshActiveTrackerIssues(context.Background(), active, normalizedStates([]string{"In Progress", "Rework"})); err != nil {
		t.Fatalf("RefreshActiveTrackerIssues: %v", err)
	}

	disp.finishAt(0, WorkerResult{Err: errors.New("transient"), Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Running) == 0 && len(v.Retrying) == 1
	}, time.Second)

	other := tracker.Issue{ID: "ENG-OTHER", Identifier: "ENG-OTHER", Title: "second rework", State: "Rework"}
	if err := o.RequestDispatch(context.Background(), other, nil); err != nil {
		t.Fatalf("dispatch other rework while moved retry is queued: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 2 }, time.Second)

	if err := o.submit(context.Background(), &retryFireOp{
		o:       o,
		id:      IssueID(moving.ID),
		issue:   moving, // captured dispatch-time issue; entry.Issue should override
		attempt: 1,
		kind:    RetryKindFailure,
	}); err != nil {
		t.Fatalf("submit retry fire: %v", err)
	}
	v, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := disp.count(); got != 2 {
		t.Fatalf("Dispatcher.Spawn calls = %d, want 2; retry must see refreshed Rework state and respect per-state cap", got)
	}
	if len(v.Retrying) != 1 {
		t.Fatalf("retry queue after fire = %+v, want refreshed retry still queued", v.Retrying)
	}
}

func TestRetryFireUsesRefreshedIssueWhenReconciledMidQueue(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 10)
	st.MaxConcurrentAgentsByState = map[string]int{"rework": 1}
	o := New(st, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Hour}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	ip := tracker.Issue{ID: "ENG-IP", Identifier: "ENG-IP", Title: "queued ip retry", State: "In Progress"}
	if err := o.RequestDispatch(context.Background(), ip, nil); err != nil {
		t.Fatalf("dispatch ip: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	disp.finishAt(0, WorkerResult{Err: errors.New("transient"), Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Running) == 0 && len(v.Retrying) == 1
	}, time.Second)

	rw := tracker.Issue{ID: "ENG-RW", Identifier: "ENG-RW", Title: "rework filler", State: "Rework"}
	if err := o.RequestDispatch(context.Background(), rw, nil); err != nil {
		t.Fatalf("dispatch rework filler: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 2 }, time.Second)

	moved := ip
	moved.State = "Rework"
	active := map[string]tracker.Issue{moved.ID: moved, rw.ID: rw}
	if err := o.RefreshActiveTrackerIssues(context.Background(), active, normalizedStates([]string{"In Progress", "Rework"})); err != nil {
		t.Fatalf("RefreshActiveTrackerIssues: %v", err)
	}

	if err := o.submit(context.Background(), &retryFireOp{
		o:       o,
		id:      IssueID(ip.ID),
		issue:   ip, // dispatch-time In Progress; refreshed entry.Issue says Rework
		attempt: 1,
		kind:    RetryKindFailure,
	}); err != nil {
		t.Fatalf("submit retry fire: %v", err)
	}
	v, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := disp.count(); got != 2 {
		t.Fatalf("Dispatcher.Spawn calls = %d, want 2; queued retry must see refreshed Rework state", got)
	}
	if len(v.Retrying) != 1 {
		t.Fatalf("retry queue after refresh-aware fire = %+v, want still queued behind Rework cap", v.Retrying)
	}
}

func TestTrackerRecheckedDispatchDoesNotConsumeFailureRetry(t *testing.T) {
	st := NewOrchestratorState(30000, 1)
	iss := tracker.Issue{ID: "ENG-FAIL-RETRY", Identifier: "ENG-FAIL-RETRY", Title: "failure retry", State: "Todo"}
	id := IssueID(iss.ID)
	st.ScheduleRetry(&RetryEntry{
		IssueID:    id,
		Identifier: iss.Identifier,
		Issue:      iss,
		Attempt:    3,
		Kind:       RetryKindFailure,
		DueAt:      time.Now().Add(-time.Second),
		Error:      "transient",
	})

	result := make(chan error, 1)
	followup := (&dispatchOp{
		issue:            iss,
		result:           result,
		trackerRechecked: true,
	}).apply(st)
	if followup != nil {
		t.Fatal("tracker-rechecked dispatch returned followup for failure retry; want retry to remain claimed")
	}
	if err := <-result; !errors.Is(err, ErrNotDispatched) {
		t.Fatalf("tracker-rechecked dispatch err = %v, want ErrNotDispatched for failure retry", err)
	}
	if _, ok := st.RetryAttempts[id]; !ok {
		t.Fatal("tracker-rechecked dispatch removed failure retry; want retryFireOp to own it")
	}
	if !st.IsClaimed(id) {
		t.Fatal("tracker-rechecked dispatch released failure retry claim; want retryFireOp to own it")
	}
}

func TestContinuationRetryTimerRequiresTrackerRecheckedDispatch(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Millisecond}},
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-10", Identifier: "ENG-10", Title: "continuation"}
	attempt := 2
	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, &attempt); err != nil {
		t.Fatalf("initial tracker-rechecked dispatch: %v", err)
	}
	if got := disp.count(); got != 1 {
		t.Fatalf("initial dispatch count = %d, want 1", got)
	}

	disp.finishAt(0, WorkerResult{Elapsed: time.Millisecond})
	time.Sleep(20 * time.Millisecond)

	if got := disp.count(); got != 1 {
		t.Fatalf("continuation retry timer spawned without tracker recheck: got %d dispatches, want 1", got)
	}
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Retrying) != 1 {
		t.Fatalf("retrying view = %+v, want continuation retry retained until tracker recheck", view.Retrying)
	}
	if view.Retrying[0].Attempt != 1 {
		t.Fatalf("continuation retry attempt = %d, want 1", view.Retrying[0].Attempt)
	}

	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, nil); err != nil {
		t.Fatalf("tracker-rechecked continuation dispatch: %v", err)
	}
	if got := disp.count(); got != 2 {
		t.Fatalf("dispatch count after tracker recheck = %d, want 2", got)
	}
	if got := disp.attemptValueAt(1); got != nil {
		t.Fatalf("tracker-rechecked continuation dispatch carried retry attempt = %d, want nil", *got)
	}
}

func TestFinalize_NormalExitResetsContinuationAttemptToOne(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Millisecond}},
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-CONT-RESET", Identifier: "ENG-CONT-RESET", Title: "reset continuation"}
	attempt := 7
	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, &attempt); err != nil {
		t.Fatalf("initial tracker-rechecked dispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	disp.finishAt(0, WorkerResult{Elapsed: time.Millisecond})

	waitFor(t, func() bool {
		view, err := o.Snapshot(context.Background())
		return err == nil && len(view.Retrying) == 1
	}, time.Second)
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := view.Retrying[0].Attempt; got != 1 {
		t.Fatalf("continuation retry attempt after clean exit = %d, want 1", got)
	}
}

func TestFinalize_FirstFailureAfterCleanContinuationUsesFirstFailureBackoff(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Millisecond, time.Hour}},
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-CONT-FAIL", Identifier: "ENG-CONT-FAIL", Title: "fail after continuation"}
	priorFailureAttempt := 7
	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, &priorFailureAttempt); err != nil {
		t.Fatalf("initial tracker-rechecked dispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	disp.finishAt(0, WorkerResult{Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		view, err := o.Snapshot(context.Background())
		return err == nil && len(view.Retrying) == 1 && !view.Retrying[0].DueAt.After(time.Now())
	}, time.Second)

	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, nil); err != nil {
		t.Fatalf("tracker-rechecked continuation dispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 2 }, time.Second)
	if got := disp.attemptValueAt(1); got != nil {
		t.Fatalf("clean continuation dispatch carried failure attempt = %d, want nil", *got)
	}

	disp.finishAt(1, WorkerResult{Err: errors.New("transient"), Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		view, err := o.Snapshot(context.Background())
		return err == nil && len(view.Retrying) == 1 && view.Retrying[0].Error == "transient"
	}, time.Second)
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := view.Retrying[0].Attempt; got != 1 {
		t.Fatalf("failure after clean continuation scheduled retry attempt = %d, want 1", got)
	}
}

func TestContinuationRetryRecheckedDispatchWaitsUntilDue(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{50 * time.Millisecond}},
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-11", Identifier: "ENG-11", Title: "continuation not due"}
	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, nil); err != nil {
		t.Fatalf("initial tracker-rechecked dispatch: %v", err)
	}
	if got := disp.count(); got != 1 {
		t.Fatalf("initial dispatch count = %d, want 1", got)
	}

	disp.finishAt(0, WorkerResult{Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		view, err := o.Snapshot(context.Background())
		return err == nil && len(view.Retrying) == 1
	}, time.Second)

	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, nil); !errors.Is(err, ErrNotDispatched) {
		t.Fatalf("early tracker-rechecked dispatch err = %v, want ErrNotDispatched", err)
	}
	if got := disp.count(); got != 1 {
		t.Fatalf("early tracker recheck spawned before continuation due time: got %d dispatches, want 1", got)
	}

	waitFor(t, func() bool {
		view, err := o.Snapshot(context.Background())
		return err == nil && len(view.Retrying) == 1 && !view.Retrying[0].DueAt.After(time.Now())
	}, time.Second)

	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, nil); err != nil {
		t.Fatalf("due tracker-rechecked continuation dispatch: %v", err)
	}
	if got := disp.count(); got != 2 {
		t.Fatalf("dispatch count after due tracker recheck = %d, want 2", got)
	}
}

func TestContinuationRetryRecheckedDispatchKeepsRetryWhenCapacityFull(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 1)
	o := New(st, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Millisecond}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	issueA := tracker.Issue{ID: "ENG-CONT-CAP", Identifier: "ENG-CONT-CAP", Title: "continuation waits"}
	issueB := tracker.Issue{ID: "ENG-RUNNING", Identifier: "ENG-RUNNING", Title: "running"}
	if err := o.RequestDispatch(context.Background(), issueB, nil); err != nil {
		t.Fatalf("dispatch B: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	if err := o.scheduleContinuationRetry(context.Background(), issueA, issueA.Identifier, 1, 0, Workspace{}); err != nil {
		t.Fatalf("schedule continuation retry: %v", err)
	}
	waitFor(t, func() bool {
		view, err := o.Snapshot(context.Background())
		return err == nil && len(view.Retrying) == 1 && !view.Retrying[0].DueAt.After(time.Now())
	}, time.Second)

	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), issueA, nil); !errors.Is(err, ErrCapacityFull) {
		t.Fatalf("tracker-rechecked continuation at capacity err = %v, want ErrCapacityFull", err)
	}
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Retrying) != 1 || view.Retrying[0].IssueID != IssueID(issueA.ID) {
		t.Fatalf("retrying after capacity rejection = %+v, want continuation retry preserved", view.Retrying)
	}
	if got := disp.count(); got != 1 {
		t.Fatalf("dispatch count after capacity rejection = %d, want only running issue B", got)
	}
}

func startRuntimeEventActor(t *testing.T, id string) (*Orchestrator, string, func()) {
	t.Helper()
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	iss := tracker.Issue{ID: id, Identifier: id, Title: id}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)
	return o, iss.ID, cancel
}

func TestRecordRuntimeEventUpdatesCodexTotalsAndRateLimits(t *testing.T) {
	o, issueID, cancel := startRuntimeEventActor(t, "ENG-METRICS")
	defer cancel()
	events := []task.RuntimeEvent{
		{Event: task.EventTurnCompleted, Payload: map[string]any{
			"usage":       map[string]any{"input_tokens": 2, "output_tokens": 3, "total_tokens": 5},
			"rate_limits": map[string]any{"primary": map[string]any{"remaining": 42}},
		}},
		{Event: task.EventTurnCompleted, Payload: map[string]any{
			"usage": map[string]any{"input_tokens": 3, "output_tokens": 5, "total_tokens": 8},
		}},
		{Event: task.EventNotification, Payload: map[string]any{
			"msg": map[string]any{"payload": map[string]any{"info": map[string]any{
				"total_token_usage": map[string]any{"input_tokens": "11", "output_tokens": "13", "total_tokens": "24"},
				"rate_limits":       map[string]any{"limit_id": "codex-primary", "primary": map[string]any{"remaining": 99}},
			}}},
		}},
	}
	for _, event := range events {
		if err := o.RecordRuntimeEvent(context.Background(), issueID, event); err != nil {
			t.Fatalf("RecordRuntimeEvent(%s): %v", event.Event, err)
		}
	}

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if view.CodexTotals.InputTokens != 11 || view.CodexTotals.OutputTokens != 13 || view.CodexTotals.TotalTokens != 24 {
		t.Fatalf("CodexTotals = %+v, want cumulative absolute telemetry totals", view.CodexTotals)
	}
	if len(view.Running) != 1 || view.Running[0].LastEventAt.IsZero() {
		t.Fatalf("Running LastEventAt not updated from runtime event: %+v", view.Running)
	}
	if view.CodexRateLimits == nil {
		t.Fatal("CodexRateLimits = nil, want latest rate_limits payload")
	}
	if got := (*view.CodexRateLimits)["primary"].(map[string]any)["remaining"]; got != 99 {
		t.Fatalf("rate_limits.primary.remaining = %#v, want 99", got)
	}
}

func TestRuntimeEventForwardingEmitterPreservesBaseEmitterAndUpdatesOrchestrator(t *testing.T) {
	o, issueID, cancel := startRuntimeEventActor(t, "ENG-EMITTER")
	defer cancel()

	base := &recordingWorkerEmitter{}
	emitter := runtimeEventForwardingEmitter{EventEmitter: base, Orchestrator: o, IssueID: issueID}
	payload := map[string]any{"usage": map[string]any{"total_tokens": 7}}
	if err := emitter.AddEventWithPayload(context.Background(), "task-1", task.EventTurnCompleted, task.EventTurnCompleted, payload); err != nil {
		t.Fatalf("AddEventWithPayload: %v", err)
	}

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(base.kinds) != 1 || base.kinds[0] != task.EventTurnCompleted || view.CodexTotals.TotalTokens != 7 {
		t.Fatalf("forwarded kinds=%v totals=%+v", base.kinds, view.CodexTotals)
	}
}

// TestRuntimeEventForwardingEmitterDropsNotificationFromOperatorLog pins #559:
// the high-frequency per-notification stream must reach the live state surface
// (/api/v1/state + TUI) via RecordRuntimeEvent but must NOT be echoed to the
// worker's process log (the wrapped EventEmitter), where it otherwise drowns
// the orchestrator lifecycle events. A non-notification event is still
// delegated to the log, so the suppression is scoped to EventNotification only.
func TestRuntimeEventForwardingEmitterDropsNotificationFromOperatorLog(t *testing.T) {
	o, issueID, cancel := startRuntimeEventActor(t, "ENG-559")
	defer cancel()

	base := &recordingWorkerEmitter{}
	emitter := runtimeEventForwardingEmitter{EventEmitter: base, Orchestrator: o, IssueID: issueID}

	notif := map[string]any{"message": "Working on it..."}
	if err := emitter.AddEventWithPayload(context.Background(), "task-1", task.EventNotification, task.EventNotification, notif); err != nil {
		t.Fatalf("AddEventWithPayload(notification): %v", err)
	}
	// A non-notification event must still be logged.
	if err := emitter.AddEventWithPayload(context.Background(), "task-1", task.EventTurnCompleted, task.EventTurnCompleted, map[string]any{"usage": map[string]any{"total_tokens": 3}}); err != nil {
		t.Fatalf("AddEventWithPayload(turn_completed): %v", err)
	}

	// Operator log: notification suppressed, turn_completed delegated.
	if len(base.kinds) != 1 || base.kinds[0] != task.EventTurnCompleted {
		t.Fatalf("operator-log kinds = %v; want only [%s] (notification must not reach the process log, #559)", base.kinds, task.EventTurnCompleted)
	}

	// Live state surface: the suppressed notification still updated the running row.
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Running) != 1 {
		t.Fatalf("running rows = %d, want 1", len(view.Running))
	}
	if got := view.Running[0].LastMessage; got != "Working on it..." {
		t.Errorf("Running[0].LastMessage = %q; want %q (notification still forwarded to /api/v1/state despite being dropped from the log)", got, "Working on it...")
	}
}

// TestRecordRuntimeEventPropagatesCodexAppServerPID pins the SPEC §4.1.6 /
// §10.4 round-trip: a `session_started` event carrying
// `codex_app_server_pid` populates RunningView.CodexAppServerPID so
// `/api/v1/state` can surface it.
func TestRecordRuntimeEventPropagatesCodexAppServerPID(t *testing.T) {
	o, issueID, cancel := startRuntimeEventActor(t, "ENG-PID")
	defer cancel()

	if err := o.RecordRuntimeEvent(context.Background(), issueID, task.RuntimeEvent{
		Event: task.EventSessionStarted,
		Payload: map[string]any{
			"session_id":           "thread-1-turn-1",
			"thread_id":            "thread-1",
			"turn_id":              "turn-1",
			"codex_app_server_pid": 42424,
		},
	}); err != nil {
		t.Fatalf("RecordRuntimeEvent: %v", err)
	}

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Running) != 1 {
		t.Fatalf("running rows = %d, want 1", len(view.Running))
	}
	if got := view.Running[0].CodexAppServerPID; got != 42424 {
		t.Fatalf("RunningView.CodexAppServerPID = %d, want 42424 (session_started payload)", got)
	}
}

// TestRecordRuntimeEventTracksLastEventAndMessage pins SPEC §13.7.2:
// last_event surfaces the most-recent runtime event kind on the running row,
// and last_message captures the payload `message` field when present (e.g.
// notification events) so operators can see what the agent is doing without
// tailing logs.
func TestRecordRuntimeEventTracksLastEventAndMessage(t *testing.T) {
	o, issueID, cancel := startRuntimeEventActor(t, "ENG-LAST-EVENT")
	defer cancel()

	if err := o.RecordRuntimeEvent(context.Background(), issueID, task.RuntimeEvent{
		Event:   task.EventNotification,
		Payload: map[string]any{"message": "Working on it..."},
	}); err != nil {
		t.Fatalf("RecordRuntimeEvent notification: %v", err)
	}
	if err := o.RecordRuntimeEvent(context.Background(), issueID, task.RuntimeEvent{
		Event:   task.EventTurnCompleted,
		Payload: map[string]any{"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}},
	}); err != nil {
		t.Fatalf("RecordRuntimeEvent turn_completed: %v", err)
	}

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Running) != 1 {
		t.Fatalf("running rows = %d, want 1", len(view.Running))
	}
	row := view.Running[0]
	if row.LastEvent != task.EventTurnCompleted {
		t.Errorf("LastEvent = %q, want most-recent event %q", row.LastEvent, task.EventTurnCompleted)
	}
	if row.LastMessage != "Working on it..." {
		t.Errorf("LastMessage = %q, want notification text preserved across later events that did not set message", row.LastMessage)
	}
}

func TestRecordRuntimeEventTreatsGenericUsageAsEventDelta(t *testing.T) {
	o, issueID, cancel := startRuntimeEventActor(t, "ENG-USAGE")
	defer cancel()

	for i := 0; i < 2; i++ {
		if err := o.RecordRuntimeEvent(context.Background(), issueID, task.RuntimeEvent{
			Event:   task.EventTurnCompleted,
			Payload: map[string]any{"usage": map[string]any{"total_tokens": 5}},
		}); err != nil {
			t.Fatalf("RecordRuntimeEvent: %v", err)
		}
	}
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if view.CodexTotals.TotalTokens != 10 {
		t.Fatalf("CodexTotals.TotalTokens = %d, want generic usage counted as per-event delta", view.CodexTotals.TotalTokens)
	}
}

func TestRecordRuntimeEventReadsNestedTurnUsagePayloads(t *testing.T) {
	o, issueID, cancel := startRuntimeEventActor(t, "ENG-NESTED-USAGE")
	defer cancel()

	events := []task.RuntimeEvent{
		{Event: task.EventTurnCompleted, Payload: map[string]any{
			"turn": map[string]any{
				"usage": map[string]any{"input_tokens": 2, "output_tokens": 3, "total_tokens": 5},
			},
		}},
		{Event: task.EventTurnCompleted, Payload: map[string]any{
			"turn": map[string]any{
				"token_usage": map[string]any{
					"total": map[string]any{"input_tokens": 11, "output_tokens": 13, "total_tokens": 24},
				},
			},
		}},
	}
	for _, event := range events {
		if err := o.RecordRuntimeEvent(context.Background(), issueID, event); err != nil {
			t.Fatalf("RecordRuntimeEvent: %v", err)
		}
	}
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if view.CodexTotals.InputTokens != 11 || view.CodexTotals.OutputTokens != 13 || view.CodexTotals.TotalTokens != 24 {
		t.Fatalf("CodexTotals = %+v, want nested turn usage folded into totals", view.CodexTotals)
	}
}

func TestSpawnRegistersRunningBeforeRuntimeEvents(t *testing.T) {
	disp := &earlyRuntimeEventDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	if err := o.RequestDispatch(context.Background(), tracker.Issue{ID: "ENG-EARLY", Identifier: "ENG-EARLY"}, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool {
		view, err := o.Snapshot(context.Background())
		return err == nil && view.CodexTotals.TotalTokens == 5
	}, time.Second)
}

// TestRequestDispatch_DedupesAgainstRunning verifies that once a
// dispatch is accepted and Running, a second RequestDispatch for the
// same issue is denied — even though the Claimed window between
// dispatchOp.apply and registerRunningOp could in principle be racy.
// The spawn helper's submit-then-spawn ordering is what closes that
// window; this test pins the behavior so a future refactor cannot
// silently regress it.
func TestRequestDispatch_DedupesAgainstRunning(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-2", Identifier: "ENG-2", Title: "dedupe"}

	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("first RequestDispatch: %v", err)
	}
	// At this point the accepted reply has been sent. The Running
	// entry is recorded asynchronously via registerRunningOp; before
	// it lands, IsClaimed still returns true via Claimed[id].
	if err := o.RequestDispatch(context.Background(), iss, nil); !errors.Is(err, ErrNotDispatched) {
		t.Errorf("second RequestDispatch err = %v, want ErrNotDispatched", err)
	}
	if got := disp.count(); got != 1 {
		t.Errorf("Dispatcher.Spawn calls = %d, want 1", got)
	}
}

// TestFinalize_NormalExitMarksCompletedAndSchedulesContinuationRetry covers
// the §7.3 normal exit branch end-to-end through the actor: dispatch, the
// dispatcher returns a successful WorkerResult, the finalize op records
// Completed and schedules a short continuation retry.
func TestFinalize_NormalExitMarksCompletedAndSchedulesContinuationRetry(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-3", Identifier: "ENG-3", Title: "ok"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	// Wait until the dispatcher has been called so we can complete it.
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	disp.finishAt(0, WorkerResult{Err: nil, Elapsed: 100 * time.Millisecond})

	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Running) == 0 && len(v.Completed) == 1 && len(v.Retrying) == 1
	}, time.Second)
}

// TestFinalize_NormalExitRecordsCompletedEvent pins the runtime-event KIND a
// clean worker exit records — `completed`, not `failed`. The sibling
// continuation test above asserts only the snapshot bucket (Completed), which
// FinishRunSucceeded drives independently of the recorded event; mutating the
// RecordEvent kind alone leaves that test green. This characterizes the
// clean-exit success signal before #499 splits the finalize routing.
func TestFinalize_NormalExitRecordsCompletedEvent(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-DONE", Identifier: "ENG-DONE", Title: "ok"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	disp.finishAt(0, WorkerResult{Err: nil, Elapsed: 100 * time.Millisecond})

	var kind RuntimeEventKind
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		for _, ev := range v.RecentEvents {
			if ev.IssueID == IssueID(iss.ID) && (ev.Kind == RuntimeEventCompleted || ev.Kind == RuntimeEventFailed) {
				kind = ev.Kind
				return true
			}
		}
		return false
	}, time.Second)
	if kind != RuntimeEventCompleted {
		t.Fatalf("clean worker exit recorded event kind = %q; want %q", kind, RuntimeEventCompleted)
	}
}

// TestFinalize_WorkerSetupFailureSchedulesBackoffRetry covers #584 (DEVIATIONS
// D29 closed): a deterministic worker-setup / task-build failure — formerly
// flagged NonRetryable and parked in the removed OrchestratorState.Failed
// suppression map — now rides the SPEC §8.4/§16.6 failure backoff like any
// abnormal exit. It is scheduled for retry (a later fire re-checks tracker
// eligibility), not suppressed; this matches upstream, where prompt_builder
// raises and the orchestrator routes the agent-down to schedule_issue_retry.
// The assertion fails if suppression is reintroduced: a suppressed failure
// would leave Retrying empty.
func TestFinalize_WorkerSetupFailureSchedulesBackoffRetry(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-SETUPFAIL", Identifier: "ENG-SETUPFAIL", Title: "worker-setup failure"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	disp.finishAt(0, WorkerResult{Err: errors.New("repo.clone_url missing in WORKFLOW.md"), Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, err := o.Snapshot(context.Background())
		return err == nil && len(v.Retrying) == 1
	}, time.Second)

	v, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(v.Retrying) != 1 {
		t.Fatalf("Snapshot().Retrying = %d; want 1 (failure rides §8.4 backoff, not suppressed)", len(v.Retrying))
	}
	if got := v.Retrying[0].IssueID; got != "ENG-SETUPFAIL" {
		t.Errorf("Retrying[0].IssueID = %q; want %q", got, "ENG-SETUPFAIL")
	}
	if got := v.Retrying[0].Kind; got != RetryKindFailure {
		t.Errorf("Retrying[0].Kind = %q; want %q (§8.4 failure backoff)", got, RetryKindFailure)
	}
	// The error is still surfaced as a RuntimeEventFailed for operator visibility.
	var sawFailed bool
	for _, ev := range v.RecentEvents {
		if ev.Kind == RuntimeEventFailed && ev.IssueID == "ENG-SETUPFAIL" {
			sawFailed = true
			if !strings.Contains(ev.Message, "repo.clone_url missing in WORKFLOW.md") {
				t.Errorf("RuntimeEventFailed.Message = %q; want the failure detail", ev.Message)
			}
		}
	}
	if !sawFailed {
		t.Errorf("RecentEvents = %+v; want a RuntimeEventFailed for ENG-SETUPFAIL", v.RecentEvents)
	}
}

// TestFinalize_ContinuationBudgetBlocksAtConfiguredLimit covers D34 / #621:
// upstream leaves clean continuation loops unbounded, but aiops-platform caps
// cumulative clean turns across re-dispatches so impossible active issues stop
// burning concurrency and quota.
func TestFinalize_ContinuationBudgetBlocksAtConfiguredLimit(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 100)
	st.MaxContinuationTurns = 3
	o := New(st, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Millisecond, time.Millisecond}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	iss := tracker.Issue{ID: "ENG-CONT-BUDGET", Identifier: "ENG-CONT-BUDGET", State: "In Progress", Title: "budgeted continuation"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)
	if got := disp.cleanTurnBudgetAt(0); got != 3 {
		t.Fatalf("fresh dispatch clean turn budget = %d; want full continuation budget 3", got)
	}

	for turn := 0; turn < 2; turn++ {
		if err := o.RecordRuntimeEvent(context.Background(), iss.ID, task.RuntimeEvent{Event: task.EventTurnCompleted}); err != nil {
			t.Fatalf("RecordRuntimeEvent turn %d: %v", turn, err)
		}
	}
	disp.finishAt(0, WorkerResult{Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, err := o.Snapshot(context.Background())
		return err == nil && len(v.Retrying) == 1 && v.Retrying[0].Attempt == 1 &&
			!v.Retrying[0].DueAt.After(time.Now())
	}, time.Second)
	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, nil); err != nil {
		t.Fatalf("tracker-rechecked continuation dispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 2 }, time.Second)
	if got := disp.cleanTurnBudgetAt(1); got != 1 {
		t.Fatalf("continuation dispatch clean turn budget = %d; want remaining continuation budget 1", got)
	}

	// This run records no turn_completed event; budget accounting must still
	// count a clean worker exit as one turn so zero-event loops cannot bypass D34.
	disp.finishAt(1, WorkerResult{Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, err := o.Snapshot(context.Background())
		return err == nil && len(v.Blocked) == 1 && len(v.Running) == 0 && len(v.Retrying) == 0
	}, time.Second)
	if err := o.RequestDispatch(context.Background(), iss, nil); !errors.Is(err, ErrNotDispatched) {
		t.Fatalf("budget-blocked issue dispatch err = %v, want ErrNotDispatched", err)
	}
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Blocked) != 1 {
		t.Fatalf("Blocked rows = %d, want 1; view=%+v", len(view.Blocked), view)
	}
	blocked := view.Blocked[0]
	if blocked.IssueID != IssueID(iss.ID) || blocked.Method != "continuation_budget" {
		t.Fatalf("blocked row = %+v, want issue %s with method continuation_budget", blocked, iss.ID)
	}
	for _, want := range []string{"cumulative_turns=3", "max_continuation_turns=3"} {
		if !strings.Contains(blocked.Error, want) {
			t.Fatalf("blocked error = %q, want %q", blocked.Error, want)
		}
	}
	var sawBudgetEvent bool
	for _, ev := range view.RecentEvents {
		if ev.IssueID == IssueID(iss.ID) && ev.Kind == RuntimeEventContinuationBudgetBlocked {
			sawBudgetEvent = true
			if !strings.Contains(ev.Message, "max_continuation_turns=3") {
				t.Fatalf("budget event message = %q, want budget detail", ev.Message)
			}
		}
	}
	if !sawBudgetEvent {
		t.Fatalf("RecentEvents = %+v; want continuation budget block event", view.RecentEvents)
	}
}

func TestFinalize_ContinuationBudgetDoesNotBlockSelfStopCleanExit(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 100)
	st.MaxContinuationTurns = 3
	o := New(st, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Millisecond}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	iss := tracker.Issue{ID: "ENG-CONT-SELFSTOP", Identifier: "ENG-CONT-SELFSTOP", State: "In Progress", Title: "self-stop at budget"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	for turn := 0; turn < 3; turn++ {
		if err := o.RecordRuntimeEvent(context.Background(), iss.ID, task.RuntimeEvent{Event: task.EventTurnCompleted}); err != nil {
			t.Fatalf("RecordRuntimeEvent turn %d: %v", turn, err)
		}
	}
	disp.finishAt(0, WorkerResult{Elapsed: time.Millisecond, IssueExitState: &runner.IssueStateSnapshot{
		Found:  true,
		State:  "In Review",
		Active: false,
	}})

	waitFor(t, func() bool {
		view, err := o.Snapshot(context.Background())
		return err == nil && len(view.Retrying) == 1 && len(view.Blocked) == 0 && len(view.Running) == 0
	}, time.Second)
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := view.Retrying[0].Kind; got != RetryKindContinuation {
		t.Fatalf("retry kind after self-stop = %q, want %q", got, RetryKindContinuation)
	}

	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, nil); !errors.Is(err, ErrNotDispatched) {
		t.Fatalf("tracker-rechecked exhausted self-stop continuation err = %v, want ErrNotDispatched", err)
	}
	waitFor(t, func() bool {
		view, err := o.Snapshot(context.Background())
		return err == nil && len(view.Blocked) == 1 && len(view.Retrying) == 0 && len(view.Running) == 0
	}, time.Second)
	view, err = o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot after recheck: %v", err)
	}
	if view.Blocked[0].Method != continuationBudgetBlockMethod {
		t.Fatalf("blocked method after active recheck = %q, want %q", view.Blocked[0].Method, continuationBudgetBlockMethod)
	}
}

func driveNearBudgetContinuation(t *testing.T, o *Orchestrator, disp *fakeDispatcher, iss tracker.Issue) {
	t.Helper()
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	for turn := 0; turn < 2; turn++ {
		if err := o.RecordRuntimeEvent(context.Background(), iss.ID, task.RuntimeEvent{Event: task.EventTurnCompleted}); err != nil {
			t.Fatalf("RecordRuntimeEvent turn %d: %v", turn, err)
		}
	}
	disp.finishAt(0, WorkerResult{Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, err := o.Snapshot(context.Background())
		return err == nil && len(v.Retrying) == 1 && v.Retrying[0].Kind == RetryKindContinuation &&
			!v.Retrying[0].DueAt.After(time.Now())
	}, time.Second)

	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, nil); err != nil {
		t.Fatalf("tracker-rechecked continuation dispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 2 }, time.Second)
}

func TestFinalize_FailureAfterNearBudgetCleanContinuationUsesFailureRetry(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 100)
	st.MaxContinuationTurns = 3
	o := New(st, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Millisecond, time.Hour}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	iss := tracker.Issue{ID: "ENG-CONT-FAIL-BUDGET", Identifier: "ENG-CONT-FAIL-BUDGET", State: "In Progress", Title: "fail near budget"}
	driveNearBudgetContinuation(t, o, disp, iss)

	disp.finishAt(1, WorkerResult{Err: errors.New("transient"), Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, err := o.Snapshot(context.Background())
		return err == nil && len(v.Retrying) == 1 && v.Retrying[0].Kind == RetryKindFailure
	}, time.Second)

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Blocked) != 0 {
		t.Fatalf("Blocked rows = %+v; want none because D34 only caps clean continuation exits", view.Blocked)
	}
	if len(view.Retrying) != 1 {
		t.Fatalf("Retrying rows = %d; want one failure retry", len(view.Retrying))
	}
	if got := view.Retrying[0].Attempt; got != 1 {
		t.Fatalf("failure retry attempt after near-budget clean continuation = %d, want 1", got)
	}
	if got := view.Retrying[0].Error; got != "transient" {
		t.Fatalf("failure retry error = %q; want %q", got, "transient")
	}
}

func TestFinalize_QuotaBackoffAfterNearBudgetCleanContinuationUsesQuotaRetry(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 100)
	st.MaxContinuationTurns = 3
	o := New(st, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Millisecond, time.Hour}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	iss := tracker.Issue{ID: "ENG-CONT-QUOTA-BUDGET", Identifier: "ENG-CONT-QUOTA-BUDGET", State: "In Progress", Title: "quota near budget"}
	driveNearBudgetContinuation(t, o, disp, iss)

	quotaErr := &runner.QuotaBackoffError{Message: "usage limit exceeded", RetryAfter: time.Hour}
	disp.finishAt(1, WorkerResult{Err: quotaErr, Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, err := o.Snapshot(context.Background())
		return err == nil && len(v.Retrying) == 1 && v.Retrying[0].Kind == RetryKindQuotaBackoff
	}, time.Second)

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Blocked) != 0 {
		t.Fatalf("Blocked rows = %+v; want none because D34 only caps clean continuation exits", view.Blocked)
	}
	if len(view.Retrying) != 1 {
		t.Fatalf("Retrying rows = %d; want one quota retry", len(view.Retrying))
	}
	if got := view.Retrying[0].Attempt; got != 0 {
		t.Fatalf("quota retry attempt after near-budget clean continuation = %d, want original attempt 0", got)
	}
	if got, want := view.Retrying[0].Error, quotaErr.Error(); got != want {
		t.Fatalf("quota retry error = %q; want %q", got, want)
	}
}

func TestDispatchRecheckBlocksQueuedContinuationWhenReloadedBudgetAlreadyExhausted(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 100)
	st.MaxContinuationTurns = 3
	o := New(st, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Millisecond}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	iss := tracker.Issue{ID: "ENG-CONT-RELOAD", Identifier: "ENG-CONT-RELOAD", State: "In Progress", Title: "reloaded budget"}
	if err := o.scheduleContinuationRetry(context.Background(), iss, iss.Identifier, 2, 3, Workspace{Path: "/tmp/eng-cont-reload"}); err != nil {
		t.Fatalf("scheduleContinuationRetry: %v", err)
	}
	waitFor(t, func() bool {
		v, err := o.Snapshot(context.Background())
		return err == nil && len(v.Retrying) == 1 && !v.Retrying[0].DueAt.After(time.Now())
	}, time.Second)

	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, nil); !errors.Is(err, ErrNotDispatched) {
		t.Fatalf("tracker-rechecked exhausted continuation err = %v, want ErrNotDispatched", err)
	}
	waitFor(t, func() bool {
		v, err := o.Snapshot(context.Background())
		return err == nil && len(v.Blocked) == 1 && len(v.Running) == 0 && len(v.Retrying) == 0
	}, time.Second)
	if got := disp.count(); got != 0 {
		t.Fatalf("dispatcher count = %d, want 0 after dispatch-time budget block", got)
	}
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	blocked := view.Blocked[0]
	if blocked.Method != "continuation_budget" {
		t.Fatalf("blocked method = %q, want continuation_budget", blocked.Method)
	}
	for _, want := range []string{"cumulative_turns=3", "max_continuation_turns=3"} {
		if !strings.Contains(blocked.Error, want) {
			t.Fatalf("blocked error = %q, want %q", blocked.Error, want)
		}
	}
	var sawBudgetEvent bool
	for _, ev := range view.RecentEvents {
		if ev.IssueID == IssueID(iss.ID) && ev.Kind == RuntimeEventContinuationBudgetBlocked {
			sawBudgetEvent = true
		}
	}
	if !sawBudgetEvent {
		t.Fatalf("RecentEvents = %+v; want continuation budget block event", view.RecentEvents)
	}
}

// TestFinalize_AbnormalExitSchedulesRetry covers the §7.3 abnormal exit
// branch: the dispatcher reports an error, finalize records elapsed
// without marking Completed, and schedules a retry through the actor.
// The retry's attempt is 1 (first retry of a first run, SPEC §4.1.5).
func TestFinalize_AbnormalExitSchedulesRetry(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  RetryScheduler{MaxBackoff: 250 * time.Millisecond},
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-4", Identifier: "ENG-4", Title: "fail"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	disp.finishAt(0, WorkerResult{Err: errors.New("kaboom"), Elapsed: 50 * time.Millisecond})

	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Running) == 0 && len(v.Retrying) == 1 && len(v.Completed) == 0
	}, time.Second)

	v, _ := o.Snapshot(context.Background())
	if len(v.Retrying) != 1 || v.Retrying[0].Attempt != 1 {
		t.Errorf("retry view = %+v, want one entry with Attempt=1", v.Retrying)
	}
	if v.Retrying[0].Error != "kaboom" {
		t.Errorf("retry Error = %q, want %q", v.Retrying[0].Error, "kaboom")
	}
}

// TestFinalize_QuotaBackoffReschedulesWithRetryAfter pins that a quota-backoff
// runner error reschedules the issue as a RetryKindQuotaBackoff entry with the
// error's RetryAfter delay and the original attempt preserved (never Failed).
func TestFinalize_QuotaBackoffReschedulesWithRetryAfter(t *testing.T) {
	disp := &fakeDispatcher{}
	scheduler := &recordingScheduler{delay: time.Hour}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  scheduler,
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-QUOTA", Identifier: "ENG-QUOTA", Title: "quota"}
	attempt := 1
	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, &attempt); err != nil {
		t.Fatalf("RequestDispatchAfterTrackerRecheck: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	quotaErr := &runner.QuotaBackoffError{
		Message:    "usage limit exceeded",
		RetryAfter: 90 * time.Second,
	}
	disp.finishAt(0, WorkerResult{Err: quotaErr, Elapsed: 50 * time.Millisecond})

	waitFor(t, func() bool {
		v, err := o.Snapshot(context.Background())
		return err == nil && len(v.Retrying) == 1
	}, time.Second)
	v, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if v.Retrying[0].Attempt != 1 {
		t.Fatalf("quota retry attempt = %d, want original attempt 1", v.Retrying[0].Attempt)
	}
	if v.Retrying[0].Error != quotaErr.Error() {
		t.Fatalf("quota retry error = %q, want %q", v.Retrying[0].Error, quotaErr.Error())
	}
	if v.Retrying[0].Kind != RetryKindQuotaBackoff {
		t.Fatalf("quota retry kind = %q, want %q", v.Retrying[0].Kind, RetryKindQuotaBackoff)
	}
	req, ok := scheduler.lastRequest()
	if !ok {
		t.Fatal("scheduler request missing")
	}
	if req.Kind != RetryKindQuotaBackoff {
		t.Fatalf("retry kind = %q, want %q", req.Kind, RetryKindQuotaBackoff)
	}
	if req.Attempt != 1 {
		t.Fatalf("retry scheduler attempt = %d, want original attempt 1", req.Attempt)
	}
	if req.DelayOverride != 90*time.Second {
		t.Fatalf("retry delay override = %s, want 90s", req.DelayOverride)
	}
}

func TestFinalize_QuotaBackoffDoesNotBumpNextFailureRetryAttempt(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Hour, time.Hour}},
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-QUOTA-FAIL", Identifier: "ENG-QUOTA-FAIL", Title: "quota then fail"}
	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatchAfterTrackerRecheck: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	quotaErr := &runner.QuotaBackoffError{Message: "usage limit exceeded", RetryAfter: time.Hour}
	disp.finishAt(0, WorkerResult{Err: quotaErr, Elapsed: 50 * time.Millisecond})
	waitFor(t, func() bool {
		v, err := o.Snapshot(context.Background())
		return err == nil && len(v.Retrying) == 1 && v.Retrying[0].Kind == RetryKindQuotaBackoff
	}, time.Second)

	if err := o.submit(context.Background(), &retryFireOp{
		o:       o,
		id:      IssueID(iss.ID),
		issue:   iss,
		attempt: 0,
		kind:    RetryKindQuotaBackoff,
	}); err != nil {
		t.Fatalf("submit quota retry fire: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 2 }, time.Second)

	disp.finishAt(1, WorkerResult{Err: errors.New("ordinary failure"), Elapsed: 50 * time.Millisecond})
	waitFor(t, func() bool {
		v, err := o.Snapshot(context.Background())
		return err == nil && len(v.Retrying) == 1 && v.Retrying[0].Kind == RetryKindFailure
	}, time.Second)
	v, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := v.Retrying[0].Attempt; got != 1 {
		t.Fatalf("ordinary failure retry attempt after quota = %d, want first failure retry attempt 1", got)
	}
}

// TestFinalize_AbnormalExitKeepsRetryingWithUnboundedDefault verifies the SPEC
// §8.4 / §16.6 contract: there is no failure-retry cap (the opt-in cap was
// removed in #577 / DEVIATIONS D29), so the orchestrator keeps scheduling
// retries past the historical default-1 threshold. Without this guard, a
// regression that re-introduces a finite retry cap would silently truncate
// retry sequences for issues a SPEC-conforming operator expects to keep tapping
// the backoff wall.
func TestFinalize_AbnormalExitKeepsRetryingWithUnboundedDefault(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-UNBOUNDED", Identifier: "ENG-UNBOUNDED", Title: "no cap"}
	// Simulate a second failure: the worker has already failed once and
	// rescheduled, and the tracker recheck redispatched at attempt=1.
	// Under the legacy default-1 cap this attempt+1 would exceed the
	// budget; under the SPEC default it must enter Retrying again.
	attempt := 1
	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, &attempt); err != nil {
		t.Fatalf("RequestDispatchAfterTrackerRecheck: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	disp.finishAt(0, WorkerResult{Err: errors.New("transient again"), Elapsed: 50 * time.Millisecond})

	waitFor(t, func() bool {
		v, err := o.Snapshot(context.Background())
		return err == nil && len(v.Retrying) == 1
	}, time.Second)
	v, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(v.Retrying) != 1 || v.Retrying[0].Attempt != 2 {
		t.Fatalf("retry view = %+v, want one entry with Attempt=2 under unbounded default", v.Retrying)
	}
}

func TestFinalize_InputRequiredExitBlocksIssueWithoutRetry(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  RetryScheduler{MaxBackoff: time.Minute},
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-INPUT", Identifier: "ENG-INPUT", State: "Todo", Title: "needs input"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)
	if err := o.RecordRuntimeEvent(context.Background(), iss.ID, task.RuntimeEvent{
		Event:   task.EventTurnInputRequired,
		Payload: map[string]any{"method": "item/tool/requestUserInput", "session_id": "thread-1-turn-1"},
	}); err != nil {
		t.Fatalf("RecordRuntimeEvent: %v", err)
	}

	disp.finishAt(0, WorkerResult{Err: errors.New("input required"), Elapsed: 50 * time.Millisecond})

	waitFor(t, func() bool {
		view, _ := o.Snapshot(context.Background())
		return len(view.Blocked) == 1 && len(view.Running) == 0 && len(view.Retrying) == 0
	}, time.Second)
	if err := o.RequestDispatch(context.Background(), iss, nil); !errors.Is(err, ErrNotDispatched) {
		t.Fatalf("blocked issue dispatch err = %v, want ErrNotDispatched", err)
	}
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Blocked) != 1 {
		t.Fatalf("blocked rows = %d, want 1; view=%+v", len(view.Blocked), view)
	}
	if view.Blocked[0].IssueID != "ENG-INPUT" || view.Blocked[0].SessionID != "thread-1-turn-1" {
		t.Fatalf("blocked row = %+v, want issue/session details", view.Blocked[0])
	}
}

func TestFinalize_NormalExitAfterInputRequiredBlocksInsteadOfContinuationRetry(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  RetryScheduler{MaxBackoff: time.Minute},
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-NORMAL-BLOCK", Identifier: "ENG-NORMAL-BLOCK", State: "Todo", Title: "needs input"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)
	if err := o.RecordRuntimeEvent(context.Background(), iss.ID, task.RuntimeEvent{
		Event:   task.EventTurnInputRequired,
		Payload: map[string]any{"method": "mcpServer/elicitation/request", "session_id": "thread-1-turn-1"},
	}); err != nil {
		t.Fatalf("RecordRuntimeEvent: %v", err)
	}

	disp.finishAt(0, WorkerResult{Elapsed: 50 * time.Millisecond})

	waitFor(t, func() bool {
		view, _ := o.Snapshot(context.Background())
		return len(view.Blocked) == 1 && len(view.Completed) == 0 && len(view.Retrying) == 0
	}, time.Second)
}

func TestReconcileBlockedIssuesRefreshesActiveAndReleasesInactive(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        RetryScheduler{MaxBackoff: time.Minute},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()

	blocked := tracker.Issue{ID: "ENG-BLOCK-A", Identifier: "ENG-BLOCK-A", State: "Todo", Title: "active block"}
	terminalWorkspace := t.TempDir()
	if err := o.RequestDispatch(context.Background(), blocked, nil); err != nil {
		t.Fatalf("RequestDispatch %s: %v", blocked.ID, err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)
	if err := o.RecordWorkspace(context.Background(), blocked.ID, Workspace{Path: terminalWorkspace}); err != nil {
		t.Fatalf("RecordWorkspace: %v", err)
	}
	if err := o.RecordRuntimeEvent(context.Background(), blocked.ID, task.RuntimeEvent{
		Event:   task.EventTurnInputRequired,
		Payload: map[string]any{"method": "item/tool/requestUserInput"},
	}); err != nil {
		t.Fatalf("RecordRuntimeEvent %s: %v", blocked.ID, err)
	}
	disp.finishAt(0, WorkerResult{Err: errors.New("input required"), Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		view, _ := o.Snapshot(context.Background())
		return len(view.Blocked) == 1
	}, time.Second)

	// Active pass: the blocked entry is still in an active state (moved to
	// Rework), so it is refreshed in place rather than released.
	activeStates := map[string]struct{}{"todo": {}, "rework": {}}
	if err := o.RefreshActiveTrackerIssues(context.Background(), map[string]tracker.Issue{
		blocked.ID: {ID: blocked.ID, Identifier: blocked.Identifier, State: "Rework", Title: blocked.Title},
	}, activeStates); err != nil {
		t.Fatalf("RefreshActiveTrackerIssues: %v", err)
	}
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Blocked) != 1 || view.Blocked[0].IssueID != IssueID(blocked.ID) || view.Blocked[0].State != "Rework" {
		t.Fatalf("blocked after active refresh = %+v, want refreshed in place at Rework", view.Blocked)
	}

	// Inactive pass: the blocked entry moves to a terminal state, so it is
	// released and its workspace removed through the WorkspaceCleaner
	// (before_remove hook), not a bare os.RemoveAll — matching upstream
	// reconcile_blocked_issue_state and the running-entry path (#331).
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		blocked.ID: {ID: blocked.ID, Identifier: blocked.Identifier, State: "Done", Title: blocked.Title},
	}, map[string]struct{}{"done": {}}, time.Second); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}
	view, err = o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Blocked) != 0 || len(view.Running) != 0 || len(view.Retrying) != 0 {
		t.Fatalf("state after terminal blocked release = running %+v retrying %+v blocked %+v", view.Running, view.Retrying, view.Blocked)
	}
	waitFor(t, func() bool { return len(cleaner.snapshot()) == 1 }, time.Second)
	got := cleaner.snapshot()[0]
	if got.IssueID != IssueID(blocked.ID) || got.Path != terminalWorkspace || got.Reason != "terminal" || got.State != "Done" {
		t.Fatalf("blocked cleanup = %+v, want terminal cleanup for %s at %q", got, blocked.ID, terminalWorkspace)
	}
}

// TestFinalize_AbnormalExitHoldsClaimAcrossScheduleRetryGap is a
// regression for the gap that Codex flagged on PR #102: finalizeRunOp's
// apply ran FinishRunFailed (which dropped Claimed[id]) and then
// returned a followup that enqueued ScheduleRetry asynchronously.
// Between the actor returning to its select loop and the
// scheduleRetryOp being processed, any RequestDispatch op already
// queued for the same id observed IsClaimed=false and dispatched the
// issue immediately — bypassing backoff and racing a phantom retry
// timer against a live worker.
//
// This test pins the fix: spam RequestDispatch from many goroutines
// recordingWorkspaceCleaner captures CleanupReconciledWorkspace calls so a
// test can assert SPEC §18.1 active-transition cleanup fired with the right
// workspace and reason.
type recordingWorkspaceCleaner struct {
	mu    sync.Mutex
	calls []ReconciledWorkspace
}

func (c *recordingWorkspaceCleaner) CleanupReconciledWorkspace(_ context.Context, w ReconciledWorkspace) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, w)
}

func (c *recordingWorkspaceCleaner) snapshot() []ReconciledWorkspace {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ReconciledWorkspace(nil), c.calls...)
}

// dispatchRunningIssue dispatches issue, waits for the worker spawn, and
// records its workspace path so the reconcile-cancel cleanup path has a
// directory to act on.
// testWorkspaceRoot is the dispatch-time root the helper records for each run,
// so cleanup tests can assert the captured root (not a live snapshot) reaches
// the cleaner.
const testWorkspaceRoot = "/var/aiops/workspaces"

func dispatchRunningIssue(t *testing.T, o *Orchestrator, disp *fakeDispatcher, issue tracker.Issue, wsPath string, wantSpawn int) {
	t.Helper()
	if err := o.RequestDispatch(context.Background(), issue, nil); err != nil {
		t.Fatalf("RequestDispatch %s: %v", issue.ID, err)
	}
	waitFor(t, func() bool { return disp.count() == wantSpawn }, time.Second)
	if err := o.RecordWorkspace(context.Background(), issue.ID, Workspace{Path: wsPath, Root: testWorkspaceRoot}); err != nil {
		t.Fatalf("RecordWorkspace %s: %v", issue.ID, err)
	}
}

// blockRunningIssue dispatches issue, records its workspace, then drives it into
// the Blocked set via an input-required turn so reconcile-cancel cleanup tests
// have a blocked entry with a workspace to act on. wantSpawn is the cumulative
// spawn count (and expected Blocked count) after this call.
// TestReconcileTerminalRunFiresActiveWorkspaceCleanup is the regression for
// #331: a running issue that moves to a terminal state mid-run must have its
// workspace removed (via the WorkspaceCleaner / before_remove hook) once the
// worker exits, instead of lingering until the next startup sweep.
func TestReconcileTerminalRunFiresActiveWorkspaceCleanup(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        RetryScheduler{MaxBackoff: time.Minute},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()

	issue := tracker.Issue{ID: "ENG-T1", Identifier: "ENG-T1", State: "In Progress"}
	const wsPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-T1"
	dispatchRunningIssue(t, o, disp, issue, wsPath, 1)

	// Issue flips to terminal Done mid-run. Fire-and-forget cancel (wait=0)
	// so the call returns before the worker exits; the fake worker then
	// reports its cancellation, driving the finalize → cleanup followup.
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		issue.ID: {ID: issue.ID, Identifier: issue.Identifier, State: "Done"},
	}, map[string]struct{}{"done": {}}, 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}
	disp.finishAt(0, WorkerResult{Err: context.Canceled, Elapsed: time.Millisecond})

	waitFor(t, func() bool { return len(cleaner.snapshot()) == 1 }, time.Second)
	got := cleaner.snapshot()[0]
	if got.IssueID != IssueID(issue.ID) || got.Path != wsPath || got.Reason != "terminal" || got.State != "Done" {
		t.Fatalf("cleanup = %+v, want terminal cleanup for %s at %q reason=terminal state=Done", got, issue.ID, wsPath)
	}
	// The dispatch-time root must reach the cleaner so removal is checked
	// against the root the path was created under, not a hot-reloaded one.
	if got.Root != testWorkspaceRoot {
		t.Fatalf("cleanup root = %q, want recorded dispatch-time root %q", got.Root, testWorkspaceRoot)
	}
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Running) != 0 {
		t.Fatalf("running not cleared after terminal cancel: %+v", view.Running)
	}
}

// TestReconcileTerminalRunAfterTurnCompletedCancelsWorkerButRecordsSuccess is
// the #448 regression: once app-server has emitted turn_completed, an
// agent-side move to Done should still cancel the worker so terminal
// reconciliation cannot leave a hung teardown occupying a running slot, but a
// clean worker result still records success and terminal cleanup after
// finalization.
func TestReconcileTerminalRunAfterTurnCompletedCancelsWorkerButRecordsSuccess(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        RetryScheduler{MaxBackoff: time.Minute},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()
	o.SetRetryTerminalStateResolver(staticStateRefresher{"ENG-TC1": "Done"}, []string{"Done"})

	issue := tracker.Issue{ID: "ENG-TC1", Identifier: "ENG-TC1", State: "In Progress"}
	const wsPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-TC1"
	dispatchRunningIssue(t, o, disp, issue, wsPath, 1)
	if err := o.RecordRuntimeEvent(context.Background(), issue.ID, task.RuntimeEvent{
		Event:   task.EventTurnCompleted,
		Payload: map[string]any{"turn_id": "turn-1"},
	}); err != nil {
		t.Fatalf("RecordRuntimeEvent: %v", err)
	}
	if err := o.RecordRuntimeEvent(context.Background(), issue.ID, task.RuntimeEvent{
		Event:   task.EventNotification,
		Payload: map[string]any{"message": "doing final local status check"},
	}); err != nil {
		t.Fatalf("RecordRuntimeEvent notification: %v", err)
	}

	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		issue.ID: {ID: issue.ID, Identifier: issue.Identifier, State: "Done"},
	}, map[string]struct{}{"done": {}}, 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}
	select {
	case <-disp.context().Done():
	case <-time.After(time.Second):
		t.Fatal("worker context was not canceled after terminal reconciliation")
	}

	disp.finishAt(0, WorkerResult{Elapsed: time.Millisecond})

	waitFor(t, func() bool { return len(cleaner.snapshot()) == 1 }, time.Second)
	got := cleaner.snapshot()[0]
	if got.IssueID != IssueID(issue.ID) || got.Path != wsPath || got.Reason != "terminal" || got.State != "Done" {
		t.Fatalf("cleanup = %+v, want terminal cleanup for %s at %q reason=terminal state=Done", got, issue.ID, wsPath)
	}
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Running) != 0 || len(view.Retrying) != 0 {
		t.Fatalf("state after clean terminal finalization = running %+v retrying %+v, want neither", view.Running, view.Retrying)
	}
	if len(view.Completed) != 1 || view.Completed[0] != IssueID(issue.ID) {
		t.Fatalf("completed = %+v, want %s", view.Completed, issue.ID)
	}
	if got := disp.count(); got != 1 {
		t.Fatalf("dispatcher spawn count = %d, want no continuation retry", got)
	}
}

func TestReconcileTerminalRunAfterTurnCompletedErrorDoesNotRetry(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	// MaxBackoff: time.Millisecond keeps the failure-backoff retry delay tiny so
	// the single flaky refresh failure is re-probed almost immediately.
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        RetryScheduler{MaxBackoff: time.Millisecond},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()
	o.SetRetryTerminalStateResolver(&flakyStateRefresher{states: staticStateRefresher{"ENG-TC2": "Done"}}, []string{"Done"})

	issue := tracker.Issue{ID: "ENG-TC2", Identifier: "ENG-TC2", State: "In Progress"}
	const wsPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-TC2"
	dispatchRunningIssue(t, o, disp, issue, wsPath, 1)
	if err := o.RecordRuntimeEvent(context.Background(), issue.ID, task.RuntimeEvent{Event: task.EventTurnCompleted}); err != nil {
		t.Fatalf("RecordRuntimeEvent: %v", err)
	}
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		issue.ID: {ID: issue.ID, Identifier: issue.Identifier, State: "Done"},
	}, map[string]struct{}{"done": {}}, 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}

	disp.finishAt(0, WorkerResult{Err: errors.New("app-server exited after terminal observation"), Elapsed: time.Millisecond})

	waitFor(t, func() bool { return len(cleaner.snapshot()) == 1 }, time.Second)
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Running) != 0 || len(view.Retrying) != 0 || len(view.Completed) != 0 {
		t.Fatalf("state after terminal errored finalization = running %+v retrying %+v completed %+v, want none", view.Running, view.Retrying, view.Completed)
	}
	if got := disp.count(); got != 1 {
		t.Fatalf("dispatcher spawn count = %d, want no failure retry", got)
	}
}

func TestReconcileTerminalRunAfterTurnCompletedRecordsStickyStopAndSkipsStaleCleanup(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        RetryScheduler{MaxBackoff: time.Minute},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()
	o.SetRetryTerminalStateResolver(staticStateRefresher{
		"ENG-TC3": "In Progress",
		"ENG-T5":  "Done",
	}, []string{"Done"})

	blip := tracker.Issue{ID: "ENG-TC3", Identifier: "ENG-TC3", State: "In Progress"}
	barrier := tracker.Issue{ID: "ENG-T5", Identifier: "ENG-T5", State: "In Progress"}
	const blipPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-TC3"
	const barrierPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-T5"
	dispatchRunningIssue(t, o, disp, blip, blipPath, 1)
	dispatchRunningIssue(t, o, disp, barrier, barrierPath, 2)
	for _, issue := range []tracker.Issue{blip, barrier} {
		if err := o.RecordRuntimeEvent(context.Background(), issue.ID, task.RuntimeEvent{Event: task.EventTurnCompleted}); err != nil {
			t.Fatalf("RecordRuntimeEvent %s: %v", issue.ID, err)
		}
	}

	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		blip.ID:    {ID: blip.ID, Identifier: blip.Identifier, State: "Done"},
		barrier.ID: {ID: barrier.ID, Identifier: barrier.Identifier, State: "Done"},
	}, map[string]struct{}{"done": {}}, 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}
	disp.finishAt(0, WorkerResult{Elapsed: time.Millisecond})
	disp.finishAt(1, WorkerResult{Elapsed: time.Millisecond})

	waitFor(t, func() bool {
		view, err := o.Snapshot(context.Background())
		return err == nil &&
			len(cleaner.snapshot()) == 1 &&
			len(view.Retrying) == 0 &&
			len(view.OperatorTerminalStops) == 2
	}, time.Second)
	calls := cleaner.snapshot()
	if len(calls) != 1 || calls[0].IssueID != IssueID(barrier.ID) || calls[0].Path != barrierPath {
		t.Fatalf("only the still-terminal barrier may be cleaned, got %+v", calls)
	}
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Retrying) != 0 {
		t.Fatalf("retrying after stale terminal recheck = %+v, want no continuation under sticky operator stop", view.Retrying)
	}
	if !sawRuntimeEvent(view.RecentEvents, RuntimeEventOperatorTerminalStop, IssueID(blip.ID)) {
		t.Fatalf("RecentEvents = %+v, want operator_terminal_stop for stale terminal issue %s", view.RecentEvents, blip.ID)
	}
}

// TestReconcileTerminalCleanupGivesUpAfterPersistentStateRefreshFailure is the
// #675 regression: when the deletion-time state recheck fails persistently (an
// unavailable tracker), the cleanup retry must back off and give up after a
// bounded number of attempts instead of re-probing every fixed second forever.
// The existing flakyStateRefresher only covers fail-once-then-succeed.
func TestReconcileTerminalCleanupGivesUpAfterPersistentStateRefreshFailure(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	// The cap is read, never mutated: a retry goroutine reads
	// maxReconciledCleanupStateRetries off-actor, so writing it from the test
	// would race the running loop (the -race detector flags exactly that). With
	// MaxBackoff: time.Millisecond every backed-off retry is near-instant, so
	// exercising the real default cap completes in milliseconds.
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        RetryScheduler{MaxBackoff: time.Millisecond},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()
	refresher := &countingFailRefresher{}
	o.SetRetryTerminalStateResolver(refresher, []string{"Done"})

	issue := tracker.Issue{ID: "ENG-GU1", Identifier: "ENG-GU1", State: "In Progress"}
	const wsPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-GU1"
	dispatchRunningIssue(t, o, disp, issue, wsPath, 1)
	if err := o.RecordRuntimeEvent(context.Background(), issue.ID, task.RuntimeEvent{Event: task.EventTurnCompleted}); err != nil {
		t.Fatalf("RecordRuntimeEvent: %v", err)
	}
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		issue.ID: {ID: issue.ID, Identifier: issue.Identifier, State: "Done"},
	}, map[string]struct{}{"done": {}}, 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}
	disp.finishAt(0, WorkerResult{Elapsed: time.Millisecond})

	// 1 initial recheck + maxReconciledCleanupStateRetries backed-off retries,
	// then the loop gives up rather than probing forever.
	wantCalls := maxReconciledCleanupStateRetries + 1
	waitFor(t, func() bool { return refresher.count() >= wantCalls }, 2*time.Second)
	settled := refresher.count()
	time.Sleep(50 * time.Millisecond)
	if got := refresher.count(); got != settled {
		t.Fatalf("resolver kept being probed after give-up: count went %d -> %d; want it to stop", settled, got)
	}
	if got := refresher.count(); got != wantCalls {
		t.Errorf("resolver call count = %d; want %d (1 initial recheck + %d backed-off retries)", got, wantCalls, maxReconciledCleanupStateRetries)
	}
	if calls := cleaner.snapshot(); len(calls) != 0 {
		t.Errorf("workspace cleaner called %d times; want 0 (state never confirmed terminal)", len(calls))
	}
}

// TestReconcileTerminalCleanupRetryUsesFailureBackoffSchedule asserts the #675
// acceptance criterion "backoff schedule matches the failure-retry path": each
// cleanup retry asks the scheduler for RetryKindFailure with an incrementing
// 1-based attempt (1..cap), i.e. the same exponential schedule as a failure
// retry — not the fixed-1s RetryKindContinuation delay the old loop used.
func TestReconcileTerminalCleanupRetryUsesFailureBackoffSchedule(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	scheduler := &recordingScheduler{delay: time.Millisecond}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        scheduler,
		WorkspaceCleaner: cleaner,
	})
	defer cancel()
	refresher := &countingFailRefresher{}
	o.SetRetryTerminalStateResolver(refresher, []string{"Done"})

	issue := tracker.Issue{ID: "ENG-BK1", Identifier: "ENG-BK1", State: "In Progress"}
	const wsPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-BK1"
	dispatchRunningIssue(t, o, disp, issue, wsPath, 1)
	if err := o.RecordRuntimeEvent(context.Background(), issue.ID, task.RuntimeEvent{Event: task.EventTurnCompleted}); err != nil {
		t.Fatalf("RecordRuntimeEvent: %v", err)
	}
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		issue.ID: {ID: issue.ID, Identifier: issue.Identifier, State: "Done"},
	}, map[string]struct{}{"done": {}}, 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}
	disp.finishAt(0, WorkerResult{Elapsed: time.Millisecond})

	wantRetries := maxReconciledCleanupStateRetries
	waitFor(t, func() bool { return refresher.count() >= wantRetries+1 }, 2*time.Second)
	time.Sleep(50 * time.Millisecond) // let the give-up settle before reading requests

	var failureAttempts []int
	for _, req := range scheduler.snapshotRequests() {
		if req.Kind != RetryKindFailure {
			// This flow carries a non-nil continuation into the give-up (verified:
			// the giveup log reports continuation_dropped=true). A give-up must
			// DROP that continuation, never reschedule it — resuming onto a
			// workspace whose state the recheck never confirmed would defeat the
			// §18.1 stale-terminal guard and the D35 operator-stop gate (#675).
			t.Errorf("cleanup retry scheduled a %s request %+v; want only failure-backoff retries (the carried continuation must be dropped on give-up, not resumed)", req.Kind, req)
			continue
		}
		failureAttempts = append(failureAttempts, req.Attempt)
	}
	want := make([]int, 0, wantRetries)
	for i := 1; i <= wantRetries; i++ {
		want = append(want, i)
	}
	if !reflect.DeepEqual(failureAttempts, want) {
		t.Errorf("cleanup retry scheduler requests = RetryKindFailure attempts %v; want %v (failure backoff schedule, 1..cap)", failureAttempts, want)
	}
}

// TestReconcileTerminalCleanupRetryRaceFreeWithSchedulerSwap is the #675
// race regression: the off-actor cleanup retry reads o.scheduler while
// UpdateRetryScheduler swaps it on the actor goroutine. Without the schedulerMu
// guard this fails under -race. The test drives a persistently-failing recheck
// (so the retry loop runs) while continuously swapping the scheduler.
func TestReconcileTerminalCleanupRetryRaceFreeWithSchedulerSwap(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        RetryScheduler{MaxBackoff: time.Millisecond},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()
	refresher := &countingFailRefresher{}
	o.SetRetryTerminalStateResolver(refresher, []string{"Done"})

	issue := tracker.Issue{ID: "ENG-RC1", Identifier: "ENG-RC1", State: "In Progress"}
	const wsPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-RC1"
	dispatchRunningIssue(t, o, disp, issue, wsPath, 1)
	if err := o.RecordRuntimeEvent(context.Background(), issue.ID, task.RuntimeEvent{Event: task.EventTurnCompleted}); err != nil {
		t.Fatalf("RecordRuntimeEvent: %v", err)
	}
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		issue.ID: {ID: issue.ID, Identifier: issue.Identifier, State: "Done"},
	}, map[string]struct{}{"done": {}}, 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}

	// Continuously swap the scheduler on the actor while the off-actor cleanup
	// retry loop reads it — maximizing overlap so -race reliably catches an
	// unsynchronized o.scheduler access.
	stop := make(chan struct{})
	swapDone := make(chan struct{})
	go func() {
		defer close(swapDone)
		for {
			select {
			case <-stop:
				return
			default:
				_ = o.UpdateRetryScheduler(context.Background(), RetryScheduler{MaxBackoff: time.Millisecond})
			}
		}
	}()

	disp.finishAt(0, WorkerResult{Elapsed: time.Millisecond})
	waitFor(t, func() bool { return refresher.count() >= maxReconciledCleanupStateRetries+1 }, 2*time.Second)
	close(stop)
	<-swapDone
	if calls := cleaner.snapshot(); len(calls) != 0 {
		t.Errorf("workspace cleaner called %d times; want 0 (state never confirmed terminal)", len(calls))
	}
}

type redispatchDuringCleanupRefresher struct {
	orch      *Orchestrator
	issue     tracker.Issue
	state     string
	attempted chan error
}

// countingFailRefresher fails every state refresh and counts calls, so a test
// can assert the cleanup retry loop is bounded.
type countingFailRefresher struct {
	mu    sync.Mutex
	calls int
}

func (f *countingFailRefresher) FetchIssueStatesByIDs(_ context.Context, _ []string) (map[string]tracker.IssueState, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return nil, errors.New("persistent tracker refresh failure")
}

func (f *countingFailRefresher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type flakyStateRefresher struct {
	states staticStateRefresher
	calls  int
}

func (f *flakyStateRefresher) FetchIssueStatesByIDs(ctx context.Context, ids []string) (map[string]tracker.IssueState, error) {
	f.calls++
	if f.calls == 1 {
		return nil, errors.New("temporary tracker refresh failure")
	}
	return f.states.FetchIssueStatesByIDs(ctx, ids)
}

func (r *redispatchDuringCleanupRefresher) FetchIssueStatesByIDs(ctx context.Context, ids []string) (map[string]tracker.IssueState, error) {
	return r.FetchIssueStatesByRefs(ctx, tracker.IssueRefsFromIDs(ids))
}

func (r *redispatchDuringCleanupRefresher) FetchIssueStatesByRefs(ctx context.Context, refs []tracker.IssueRef) (map[string]tracker.IssueState, error) {
	r.attempted <- r.orch.RequestDispatch(ctx, r.issue, nil)
	out := make(map[string]tracker.IssueState, len(refs))
	for _, ref := range refs {
		out[ref.ID] = tracker.IssueState{State: r.state}
	}
	return out, nil
}

func TestReconcileWorkspaceCleanupRechecksStateUnderReservation(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        &sequenceScheduler{delays: []time.Duration{time.Millisecond}},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()

	issue := tracker.Issue{ID: "ENG-TC4", Identifier: "ENG-TC4", State: "In Progress"}
	refresher := &redispatchDuringCleanupRefresher{
		orch:      o,
		issue:     issue,
		state:     "In Progress",
		attempted: make(chan error, 1),
	}
	o.SetRetryTerminalStateResolver(refresher, []string{"Done"})

	const wsPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-TC4"
	dispatchRunningIssue(t, o, disp, issue, wsPath, 1)
	if err := o.RecordRuntimeEvent(context.Background(), issue.ID, task.RuntimeEvent{Event: task.EventTurnCompleted}); err != nil {
		t.Fatalf("RecordRuntimeEvent: %v", err)
	}
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		issue.ID: {ID: issue.ID, Identifier: issue.Identifier, State: "Done"},
	}, map[string]struct{}{"done": {}}, 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}

	disp.finishAt(0, WorkerResult{Elapsed: time.Millisecond})

	select {
	case err := <-refresher.attempted:
		if !errors.Is(err, ErrNotDispatched) {
			t.Fatalf("dispatch during cleanup recheck = %v, want ErrNotDispatched", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup recheck did not attempt redispatch")
	}
	waitFor(t, func() bool {
		view, err := o.Snapshot(context.Background())
		return err == nil && len(view.Retrying) == 0 && len(view.OperatorTerminalStops) == 1
	}, time.Second)
	if err := o.RequestDispatch(context.Background(), issue, nil); !errors.Is(err, ErrNotDispatched) {
		t.Fatalf("plain dispatch after sticky stop = %v, want ErrNotDispatched", err)
	}
	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), issue, nil); !errors.Is(err, ErrNotDispatched) {
		t.Fatalf("tracker-rechecked dispatch after sticky stop = %v, want ErrNotDispatched", err)
	}
	if got := disp.count(); got != 1 {
		t.Fatalf("dispatcher spawn count = %d, want only the original run under sticky stop", got)
	}
	if calls := cleaner.snapshot(); len(calls) != 0 {
		t.Fatalf("cleanup calls = %+v, want none after state rechecked active", calls)
	}
}

// TestReconcileInactiveNonTerminalRunKeepsWorkspace pins the upstream gating:
// a run cancelled because its issue went to a merely-inactive (non-terminal)
// state must NOT have its workspace removed — the issue may return to active
// work and reuse it.
//
// A terminal sibling (ENG-T2) reconciled in the same pass acts as a progress
// barrier: each run's cleanup fires from its own fire-and-forget finalize
// followup, so we cannot prove a negative instantly. Waiting for the terminal
// sibling's cleanup to land gives the non-terminal run's followup (if the
// gating regressed and one were wrongly scheduled) ample opportunity to
// surface, then we assert the terminal sibling is the ONLY call. Not strictly
// race-free, but it reliably catches a reverted terminal gate (the wrongly
// scheduled call pushes the count to 2) and is stable under -race.
func TestReconcileInactiveNonTerminalRunKeepsWorkspace(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        RetryScheduler{MaxBackoff: time.Minute},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()

	inactiveIssue := tracker.Issue{ID: "ENG-I1", Identifier: "ENG-I1", State: "In Progress"}
	terminalIssue := tracker.Issue{ID: "ENG-T2", Identifier: "ENG-T2", State: "In Progress"}
	const inactivePath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-I1"
	const terminalPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-T2"
	dispatchRunningIssue(t, o, disp, inactiveIssue, inactivePath, 1)
	dispatchRunningIssue(t, o, disp, terminalIssue, terminalPath, 2)

	// One reconcile pass: ENG-I1 → Backlog (configured-inactive, non-terminal),
	// ENG-T2 → Done (terminal). Only the terminal one may be cleaned.
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		inactiveIssue.ID: {ID: inactiveIssue.ID, Identifier: inactiveIssue.Identifier, State: "Backlog"},
		terminalIssue.ID: {ID: terminalIssue.ID, Identifier: terminalIssue.Identifier, State: "Done"},
	}, map[string]struct{}{"done": {}}, 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}
	disp.finishAt(0, WorkerResult{Err: context.Canceled, Elapsed: time.Millisecond})
	disp.finishAt(1, WorkerResult{Err: context.Canceled, Elapsed: time.Millisecond})

	waitFor(t, func() bool { return len(cleaner.snapshot()) == 1 }, time.Second)
	calls := cleaner.snapshot()
	if len(calls) != 1 || calls[0].IssueID != IssueID(terminalIssue.ID) || calls[0].Path != terminalPath {
		t.Fatalf("only the terminal sibling may be cleaned, got %+v", calls)
	}
	view, _ := o.Snapshot(context.Background())
	if len(view.Running) != 0 {
		t.Fatalf("running not cleared after reconcile: %+v", view.Running)
	}
}

// TestReconcileTerminalContinuationRetryFiresWorkspaceCleanup is the
// regression for #341: a run that self-stops via the SPEC §16.5 per-turn
// refresher exits cleanly and schedules a continuation retry. If the issue is
// then observed terminal while that continuation is queued, the reconcile pass
// must clean its workspace through the WorkspaceCleaner (before_remove hook /
// reconcile_workspace reason=terminal) once the retry resolves, instead of
// leaking the directory until the next startup sweep. Mirrors upstream
// handle_retry_issue_lookup's terminal branch (orchestrator.ex:1082-1090).
func TestReconcileTerminalContinuationRetryFiresWorkspaceCleanup(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        RetryScheduler{MaxBackoff: time.Minute},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()

	issue := tracker.Issue{ID: "ENG-CR1", Identifier: "ENG-CR1", State: "In Progress", Title: "self-stop"}
	const wsPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-CR1"
	dispatchRunningIssue(t, o, disp, issue, wsPath, 1)

	// Clean exit (the §16.5 refresher observed the issue leave the active set)
	// → finalize finishes the run and schedules a continuation retry that
	// carries the finalized run's workspace.
	disp.finishAt(0, WorkerResult{Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		view, err := o.Snapshot(context.Background())
		return err == nil && len(view.Retrying) == 1 && len(view.Running) == 0
	}, time.Second)

	// The continuation is still queued when the issue is observed terminal.
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		issue.ID: {ID: issue.ID, Identifier: issue.Identifier, State: "Done"},
	}, map[string]struct{}{"done": {}}, 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}

	waitFor(t, func() bool { return len(cleaner.snapshot()) == 1 }, time.Second)
	got := cleaner.snapshot()[0]
	if got.IssueID != IssueID(issue.ID) || got.Path != wsPath || got.Reason != "terminal" || got.State != "Done" {
		t.Fatalf("cleanup = %+v, want terminal cleanup for %s at %q reason=terminal state=Done", got, issue.ID, wsPath)
	}
	// The dispatch-time root recorded for the run must reach the cleaner so
	// removal is checked against the root the path was created under.
	if got.Root != testWorkspaceRoot {
		t.Fatalf("cleanup root = %q, want recorded dispatch-time root %q", got.Root, testWorkspaceRoot)
	}
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Retrying) != 0 {
		t.Fatalf("retry not released after terminal reconcile: %+v", view.Retrying)
	}
}

// TestReconcileInactiveContinuationRetryKeepsWorkspace pins the other branch of
// the retry-fire release path (#341): a continuation retry whose issue went to
// a merely-inactive (non-terminal) state must be released WITHOUT removing its
// workspace — the issue may return to active work and reuse it. A terminal
// sibling reconciled in the same pass is the progress barrier that makes the
// negative observable: its cleanup fires from a fire-and-forget followup, so we
// wait for it to land, then assert it is the ONLY call. A reverted terminal
// gate would push the count to 2.
func TestReconcileInactiveContinuationRetryKeepsWorkspace(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        RetryScheduler{MaxBackoff: time.Minute},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()

	inactive := tracker.Issue{ID: "ENG-CR-I", Identifier: "ENG-CR-I", State: "In Progress", Title: "inactive self-stop"}
	terminal := tracker.Issue{ID: "ENG-CR-T", Identifier: "ENG-CR-T", State: "In Progress", Title: "terminal self-stop"}
	const inactivePath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-CR-I"
	const terminalPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-CR-T"
	dispatchRunningIssue(t, o, disp, inactive, inactivePath, 1)
	dispatchRunningIssue(t, o, disp, terminal, terminalPath, 2)

	// Both self-stop cleanly → two queued continuation retries.
	disp.finishAt(0, WorkerResult{Elapsed: time.Millisecond})
	disp.finishAt(1, WorkerResult{Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		view, err := o.Snapshot(context.Background())
		return err == nil && len(view.Retrying) == 2 && len(view.Running) == 0
	}, time.Second)

	// One reconcile pass: ENG-CR-I → Backlog (non-terminal inactive),
	// ENG-CR-T → Done (terminal). Only the terminal one may be cleaned.
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		inactive.ID: {ID: inactive.ID, Identifier: inactive.Identifier, State: "Backlog"},
		terminal.ID: {ID: terminal.ID, Identifier: terminal.Identifier, State: "Done"},
	}, map[string]struct{}{"done": {}}, 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}

	waitFor(t, func() bool { return len(cleaner.snapshot()) == 1 }, time.Second)
	calls := cleaner.snapshot()
	if len(calls) != 1 || calls[0].IssueID != IssueID(terminal.ID) || calls[0].Path != terminalPath {
		t.Fatalf("only the terminal sibling may be cleaned, got %+v", calls)
	}
	view, _ := o.Snapshot(context.Background())
	if len(view.Retrying) != 0 {
		t.Fatalf("retries not released after reconcile: %+v", view.Retrying)
	}
}

// TestReconcileTerminalContinuationRetryActiveRecheckKeepsStickyStop pins D35:
// once the operator's terminal tracker state has been observed for a queued
// continuation, a later cleanup-time active recheck must not resume that
// continuation. D34 still covers clean active continuation before any terminal
// observation; D35 makes the terminal observation process-authoritative.
func TestReconcileTerminalContinuationRetryActiveRecheckKeepsStickyStop(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		// Two short delays: one for the original continuation scheduled at
		// finalize, one for the continuation the recheck reschedules.
		Scheduler:        &sequenceScheduler{delays: []time.Duration{time.Millisecond, time.Millisecond}},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()

	issue := tracker.Issue{ID: "ENG-CRR1", Identifier: "ENG-CRR1", State: "In Progress", Title: "self-stop"}
	const wsPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-CRR1"
	dispatchRunningIssue(t, o, disp, issue, wsPath, 1)

	// Clean §16.5 self-stop → finalize schedules a continuation retry (attempt 1)
	// carrying the finalized run's workspace.
	disp.finishAt(0, WorkerResult{Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		view, err := o.Snapshot(context.Background())
		return err == nil && len(view.Running) == 0 && len(view.Retrying) == 1 && view.Retrying[0].Attempt == 1
	}, time.Second)

	// The recheck resolver reports the issue active again (not in the terminal
	// set). Under D35 this skips workspace removal but must not resume the
	// continuation because a terminal operator stop was already recorded.
	o.SetRetryTerminalStateResolver(staticStateRefresher{issue.ID: "In Progress"}, []string{"Done"})

	// The continuation is still queued when the inactive pass observes terminal.
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		issue.ID: {ID: issue.ID, Identifier: issue.Identifier, State: "Done"},
	}, map[string]struct{}{"done": {}}, 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}

	waitFor(t, func() bool {
		view, err := o.Snapshot(context.Background())
		return err == nil && len(view.Retrying) == 0 && len(view.OperatorTerminalStops) == 1
	}, time.Second)
	if err := o.RequestDispatch(context.Background(), issue, nil); !errors.Is(err, ErrNotDispatched) {
		t.Fatalf("RequestDispatch after sticky stop = %v, want ErrNotDispatched", err)
	}
	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), issue, nil); !errors.Is(err, ErrNotDispatched) {
		t.Fatalf("RequestDispatchAfterTrackerRecheck after sticky stop = %v, want ErrNotDispatched", err)
	}
	if got := disp.count(); got != 1 {
		t.Fatalf("dispatcher spawn count = %d, want no resumed continuation dispatch", got)
	}
	if calls := cleaner.snapshot(); len(calls) != 0 {
		t.Fatalf("cleanup calls = %+v, want none after state rechecked active", calls)
	}
}

// TestReconcileWorkspaceCleanupLockDeniesDispatch backs the Codex stop-time
// finding: the re-claim guard must be race-free. While a terminal workspace
// cleanup is in flight (reserved on the actor), dispatch onto the same issue —
// hence the same deterministic workspace path — must be denied, so the delayed
// removal cannot delete a freshly-dispatched run's workspace. Once cleanup
// ends, dispatch is allowed again.
func TestReconcileWorkspaceCleanupLockDeniesDispatch(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	issue := tracker.Issue{ID: "ENG-LK1", Identifier: "ENG-LK1", State: "In Progress"}
	id := IssueID(issue.ID)
	if !o.beginReconcileWorkspaceCleanup(id) {
		t.Fatal("begin must succeed for an unclaimed, not-cleaning issue")
	}
	// Reserved for cleanup → dispatch must be denied (no race window: both the
	// reservation and this dispatch check run on the actor goroutine).
	if err := o.RequestDispatch(context.Background(), issue, nil); !errors.Is(err, ErrNotDispatched) {
		t.Fatalf("dispatch during cleanup = %v, want ErrNotDispatched", err)
	}
	// A second cleanup for the same issue must also be refused.
	if o.beginReconcileWorkspaceCleanup(id) {
		t.Fatal("a second concurrent cleanup must be refused")
	}
	o.endReconcileWorkspaceCleanup(id)
	// Once the mark clears, dispatch proceeds.
	if err := o.RequestDispatch(context.Background(), issue, nil); err != nil {
		t.Fatalf("dispatch after cleanup ended: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)
}

// TestReconcileWorkspaceCleanupAbortsWhenReclaimed pins the other half of the
// guard: if the issue was re-dispatched (claimed) since it went terminal, the
// cleanup must abort rather than remove the new run's workspace.
func TestReconcileWorkspaceCleanupAbortsWhenReclaimed(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	issue := tracker.Issue{ID: "ENG-LK2", Identifier: "ENG-LK2", State: "In Progress"}
	if err := o.RequestDispatch(context.Background(), issue, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)
	if o.beginReconcileWorkspaceCleanup(IssueID(issue.ID)) {
		t.Fatal("cleanup must abort when the issue has been re-claimed")
	}
}

// TestReconcileTerminalBlipClearedByActiveRefreshKeepsWorkspace is the
// regression for Codex P2: a terminal observation flags the run for cleanup,
// but if a later refresh sees the still-running issue back in an active state
// the flag must be cleared so finalize does NOT remove a workspace that should
// be preserved for reuse. A terminal sibling (ENG-T3) that is NOT refreshed
// acts as a barrier so the negative assertion is observable.
func TestReconcileTerminalBlipClearedByActiveRefreshKeepsWorkspace(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        RetryScheduler{MaxBackoff: time.Minute},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()

	blip := tracker.Issue{ID: "ENG-B1", Identifier: "ENG-B1", State: "In Progress"}
	barrier := tracker.Issue{ID: "ENG-T3", Identifier: "ENG-T3", State: "In Progress"}
	const barrierPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-T3"
	dispatchRunningIssue(t, o, disp, blip, "/var/aiops/workspaces/acme/repo/linear_issue/ENG-B1", 1)
	dispatchRunningIssue(t, o, disp, barrier, barrierPath, 2)

	// Both observed terminal (flags + cancels, fire-and-forget so the entries
	// stay in Running until we finish their workers below).
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		blip.ID:    {ID: blip.ID, Identifier: blip.Identifier, State: "Done"},
		barrier.ID: {ID: barrier.ID, Identifier: barrier.Identifier, State: "Done"},
	}, map[string]struct{}{"done": {}}, 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}
	// ENG-B1 flips back to an active state before its worker exits; the refresh
	// must clear its pending cleanup flag. ENG-T3 is not in this set → stays flagged.
	if err := o.RefreshActiveTrackerIssues(context.Background(), map[string]tracker.Issue{
		blip.ID: {ID: blip.ID, Identifier: blip.Identifier, State: "In Progress"},
	}, map[string]struct{}{"in progress": {}}); err != nil {
		t.Fatalf("RefreshActiveTrackerIssues: %v", err)
	}
	disp.finishAt(0, WorkerResult{Err: context.Canceled, Elapsed: time.Millisecond})
	disp.finishAt(1, WorkerResult{Err: context.Canceled, Elapsed: time.Millisecond})

	// Barrier cleanup proves both finalizes ran; only the still-terminal
	// barrier may be cleaned — the refreshed blip must be left intact.
	waitFor(t, func() bool { return len(cleaner.snapshot()) == 1 }, time.Second)
	calls := cleaner.snapshot()
	if len(calls) != 1 || calls[0].IssueID != IssueID(barrier.ID) || calls[0].Path != barrierPath {
		t.Fatalf("only the still-terminal barrier may be cleaned, got %+v", calls)
	}
}

// TestReconcileTerminalBlipClearedByInactiveRefreshKeepsWorkspace is the
// regression for the Codex P2 follow-up: the clear must also happen on the
// inactive reconcile path. A run seen terminal (Done) is flagged, but if a
// later tick sees the same still-running issue in a non-terminal inactive
// state (Backlog) the flag must be cleared so finalize keeps the workspace.
func TestReconcileTerminalBlipClearedByInactiveRefreshKeepsWorkspace(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        RetryScheduler{MaxBackoff: time.Minute},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()

	blip := tracker.Issue{ID: "ENG-B2", Identifier: "ENG-B2", State: "In Progress"}
	barrier := tracker.Issue{ID: "ENG-T4", Identifier: "ENG-T4", State: "In Progress"}
	const barrierPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-T4"
	dispatchRunningIssue(t, o, disp, blip, "/var/aiops/workspaces/acme/repo/linear_issue/ENG-B2", 1)
	dispatchRunningIssue(t, o, disp, barrier, barrierPath, 2)

	terminalStates := map[string]struct{}{"done": {}}
	// Tick 1: both observed terminal (Done) → flagged + cancelled.
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		blip.ID:    {ID: blip.ID, Identifier: blip.Identifier, State: "Done"},
		barrier.ID: {ID: barrier.ID, Identifier: barrier.Identifier, State: "Done"},
	}, terminalStates, 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait tick 1: %v", err)
	}
	// Tick 2 (workers not yet exited): ENG-B2 reopened to a non-terminal
	// inactive state must clear its flag; ENG-T4 still terminal stays flagged.
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		blip.ID:    {ID: blip.ID, Identifier: blip.Identifier, State: "Backlog"},
		barrier.ID: {ID: barrier.ID, Identifier: barrier.Identifier, State: "Done"},
	}, terminalStates, 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait tick 2: %v", err)
	}
	disp.finishAt(0, WorkerResult{Err: context.Canceled, Elapsed: time.Millisecond})
	disp.finishAt(1, WorkerResult{Err: context.Canceled, Elapsed: time.Millisecond})

	waitFor(t, func() bool { return len(cleaner.snapshot()) == 1 }, time.Second)
	calls := cleaner.snapshot()
	if len(calls) != 1 || calls[0].IssueID != IssueID(barrier.ID) || calls[0].Path != barrierPath {
		t.Fatalf("only the still-terminal barrier may be cleaned, got %+v", calls)
	}
}

// while the worker is exiting abnormally, and verify the dispatcher
// is asked to spawn exactly once for the lifetime of the issue. With
// the bug present, additional Spawn calls slip through the gap; with
// the fix (Claimed re-set inside apply before the followup runs), the
// gap is invisible to other ops.
func TestFinalize_AbnormalExitHoldsClaimAcrossScheduleRetryGap(t *testing.T) {
	disp := &fakeDispatcher{}
	// Long delay so the retry timer never fires during the test;
	// we're testing the gap, not the eventual re-dispatch.
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-RACE", Identifier: "ENG-RACE"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("first RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	// Start spammers BEFORE finishAt so the actor's ops channel is
	// hot with dispatchOps the instant finalizeRunOp returns. Whoever
	// wins the channel-send race lands behind finalizeRunOp; with the
	// bug, those dispatchOps would see IsClaimed=false in the gap.
	stop := make(chan struct{})
	var spammerWG sync.WaitGroup
	for i := 0; i < 16; i++ {
		spammerWG.Add(1)
		go func() {
			defer spammerWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = o.RequestDispatch(context.Background(), iss, nil)
			}
		}()
	}
	// Give the spammers a moment to ramp up so several dispatchOps
	// are guaranteed to be queued behind finalizeRunOp by the time
	// the actor processes it.
	time.Sleep(10 * time.Millisecond)

	disp.finishAt(0, WorkerResult{Err: errors.New("fail"), Elapsed: 0})

	// Wait for the retry to be scheduled (the followup's
	// scheduleRetryOp has been processed by the actor).
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Retrying) == 1 && len(v.Running) == 0
	}, 2*time.Second)

	// Stop spammers and let in-flight RequestDispatch attempts drain.
	close(stop)
	spammerWG.Wait()

	// The single accepted dispatch from the start of the test is the
	// only one that should have reached the dispatcher. Any extra
	// Spawn means a spammer slipped through the gap.
	if got := disp.count(); got != 1 {
		t.Errorf("Dispatcher.Spawn calls = %d, want 1 (Claimed must be held across FinishRunFailed → ScheduleRetry gap)", got)
	}
}

func TestRequestDispatch_RespectsPerStateCapacityAndFallback(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 3)
	st.MaxConcurrentAgentsByState = map[string]int{"rework": 1}
	o := New(st, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx)
	if err := o.WaitStarted(ctx); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	if err := o.RequestDispatch(context.Background(), tracker.Issue{ID: "rw-1", Identifier: "RW-1", State: "Rework"}, nil); err != nil {
		t.Fatalf("dispatch first rework: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	if err := o.RequestDispatch(context.Background(), tracker.Issue{ID: "rw-2", Identifier: "RW-2", State: "rework"}, nil); !errors.Is(err, ErrCapacityFull) {
		t.Fatalf("dispatch second rework err = %v, want ErrCapacityFull", err)
	}
	if got := disp.count(); got != 1 {
		t.Fatalf("dispatcher count after capped rework = %d, want 1", got)
	}

	if err := o.RequestDispatch(context.Background(), tracker.Issue{ID: "ip-1", Identifier: "IP-1", State: "In Progress"}, nil); err != nil {
		t.Fatalf("dispatch fallback state: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 2 }, time.Second)
}

func TestRequestDispatch_PerStateCapacityNormalizesStateKey(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 10)
	st.MaxConcurrentAgentsByState = map[string]int{"in_progress": 1}
	o := New(st, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx)
	if err := o.WaitStarted(ctx); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	if err := o.RequestDispatch(context.Background(), tracker.Issue{ID: "ip-1", Identifier: "IP-1", State: "In Progress"}, nil); err != nil {
		t.Fatalf("dispatch first in-progress: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	if err := o.RequestDispatch(context.Background(), tracker.Issue{ID: "ip-2", Identifier: "IP-2", State: "in_progress"}, nil); !errors.Is(err, ErrCapacityFull) {
		t.Fatalf("dispatch normalized in-progress err = %v, want ErrCapacityFull", err)
	}
}

func TestContinuationRetryRecheckedDispatchIgnoresOwnPerStateClaim(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 10)
	st.MaxConcurrentAgentsByState = map[string]int{"rework": 1}
	o := New(st, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Millisecond}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	iss := tracker.Issue{ID: "ENG-CONT-CAP", Identifier: "ENG-CONT-CAP", State: "Rework", Title: "continue under cap"}
	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, nil); err != nil {
		t.Fatalf("initial tracker-rechecked dispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	disp.finishAt(0, WorkerResult{Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, err := o.Snapshot(context.Background())
		return err == nil && len(v.Retrying) == 1
	}, time.Second)
	waitFor(t, func() bool {
		v, err := o.Snapshot(context.Background())
		if err != nil {
			return false
		}
		for _, entry := range v.Retrying {
			if entry.IssueID == IssueID(iss.ID) && !entry.DueAt.After(time.Now()) {
				return true
			}
		}
		return false
	}, time.Second)

	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, nil); err != nil {
		t.Fatalf("tracker-rechecked continuation dispatch under own per-state cap: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 2 }, time.Second)
}

func TestUpdateMaxConcurrentAgentsByStateNormalizesStateKeys(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp})
	defer cancel()

	if err := o.UpdateMaxConcurrentAgentsByState(context.Background(), map[string]int{"In Progress": 1}); err != nil {
		t.Fatalf("UpdateMaxConcurrentAgentsByState: %v", err)
	}
	if err := o.RequestDispatch(context.Background(), tracker.Issue{ID: "ip-1", Identifier: "IP-1", State: "in_progress"}, nil); err != nil {
		t.Fatalf("dispatch first in-progress: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	if err := o.RequestDispatch(context.Background(), tracker.Issue{ID: "ip-2", Identifier: "IP-2", State: "In Progress"}, nil); !errors.Is(err, ErrCapacityFull) {
		t.Fatalf("dispatch second normalized in-progress err = %v, want ErrCapacityFull", err)
	}
}

func TestUpdatePollIntervalMsRefreshesSnapshotMetadata(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp})
	defer cancel()

	if err := o.UpdatePollIntervalMs(context.Background(), 42000); err != nil {
		t.Fatalf("UpdatePollIntervalMs: %v", err)
	}
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if view.PollIntervalMs != 42000 {
		t.Fatalf("PollIntervalMs = %d, want 42000", view.PollIntervalMs)
	}
}

func TestRetryFire_DropsStaleContinuationFireAfterFailureReplacement(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 10)
	o := New(st, Deps{Dispatcher: disp})

	issueID := IssueID("ENG-KIND")
	continuationIssue := tracker.Issue{ID: string(issueID), Identifier: "ENG-KIND", State: "Todo", Title: "old continuation"}
	failureIssue := tracker.Issue{ID: string(issueID), Identifier: "ENG-KIND", State: "Needs Fix", Title: "new failure"}
	st.ScheduleRetry(&RetryEntry{
		Issue:      failureIssue,
		IssueID:    issueID,
		Identifier: failureIssue.Identifier,
		Attempt:    1,
		Kind:       RetryKindFailure,
	})

	followup := (&retryFireOp{
		o:       o,
		id:      issueID,
		issue:   continuationIssue,
		attempt: 1,
		kind:    RetryKindContinuation,
	}).apply(st)
	if followup != nil {
		t.Fatal("stale continuation fire returned a followup, want it dropped before side effects")
	}

	if got := disp.count(); got != 0 {
		t.Fatalf("stale continuation fire spawned %d workers, want 0", got)
	}
	if len(st.Running) != 0 {
		t.Fatalf("running after stale continuation fire = %+v, want none", st.Running)
	}
	entry, ok := st.RetryAttempts[issueID]
	if !ok {
		t.Fatal("stale continuation fire consumed the replacement failure retry")
	}
	if entry.Kind != RetryKindFailure || entry.Issue.Title != failureIssue.Title {
		t.Fatalf("replacement retry = kind %q issue %+v, want failure issue preserved", entry.Kind, entry.Issue)
	}
	if !st.IsClaimed(issueID) {
		t.Fatal("stale continuation fire released claim for replacement failure retry")
	}
}

func TestRetryFire_DropsStaleFailureFireAfterContinuationReplacement(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 10)
	o := New(st, Deps{Dispatcher: disp})

	issueID := IssueID("ENG-KIND")
	failureIssue := tracker.Issue{ID: string(issueID), Identifier: "ENG-KIND", State: "Needs Fix", Title: "old failure"}
	continuationIssue := tracker.Issue{ID: string(issueID), Identifier: "ENG-KIND", State: "Todo", Title: "new continuation"}
	timer := time.AfterFunc(time.Hour, func() {})
	defer timer.Stop()
	st.ScheduleRetry(&RetryEntry{
		Issue:      continuationIssue,
		IssueID:    issueID,
		Identifier: continuationIssue.Identifier,
		Attempt:    1,
		Timer:      timer,
		Kind:       RetryKindContinuation,
	})

	followup := (&retryFireOp{
		o:       o,
		id:      issueID,
		issue:   failureIssue,
		attempt: 1,
		kind:    RetryKindFailure,
	}).apply(st)
	if followup != nil {
		t.Fatal("stale failure fire returned a followup, want it dropped before side effects")
	}

	if got := disp.count(); got != 0 {
		t.Fatalf("stale failure fire spawned %d workers, want 0", got)
	}
	if len(st.Running) != 0 {
		t.Fatalf("running after stale failure fire = %+v, want none", st.Running)
	}
	entry, ok := st.RetryAttempts[issueID]
	if !ok {
		t.Fatal("stale failure fire consumed the replacement continuation retry")
	}
	if entry.Kind != RetryKindContinuation || entry.Timer == nil {
		t.Fatalf("replacement retry = kind %q timer %v, want continuation timer preserved", entry.Kind, entry.Timer)
	}
	if !st.IsClaimed(issueID) {
		t.Fatal("stale failure fire released claim for replacement continuation retry")
	}
}

// TestRetryFire_DropsFireWhenEntryAlreadyConsumed pins the entry-absent guard of
// (*retryFireOp).apply (the upstream pop_retry_attempt_state :missing case,
// orchestrator.ex:1057-1058). A retry timer can fire after reconciliation's
// ReleaseClaim, or an earlier fire of the same retry, already removed the entry;
// the late fire must be a no-op rather than dispatching from the stale snapshot.
func TestRetryFire_DropsFireWhenEntryAlreadyConsumed(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 10)
	o := New(st, Deps{Dispatcher: disp})

	issueID := IssueID("ENG-GONE")
	followup := (&retryFireOp{
		o:       o,
		id:      issueID,
		issue:   tracker.Issue{ID: string(issueID), Identifier: "ENG-GONE", State: "Needs Fix"},
		attempt: 1,
		kind:    RetryKindFailure,
	}).apply(st)
	if followup != nil {
		t.Fatal("fire for already-consumed entry returned a followup, want nil no-op")
	}
	if got := disp.count(); got != 0 {
		t.Fatalf("Dispatcher.Spawn calls = %d, want 0 for absent retry entry", got)
	}
	if len(st.Running) != 0 {
		t.Fatalf("running after absent-entry fire = %+v, want none", st.Running)
	}
	if _, ok := st.RetryAttempts[issueID]; ok {
		t.Fatal("absent-entry fire created a retry entry, want none")
	}
}

// TestRetryFire_DropsStaleFireAfterAttemptBumpReplacement pins the attempt arm of
// the stale-fire guard (entry.Attempt != r.attempt). The existing stale-fire
// tests only vary Kind; this one keeps Kind constant and varies the attempt, so
// the (Attempt,Kind) identity check that catches a Stop()-missed late timer is
// pinned on both arms of the OR (mirrors upstream's make_ref token mismatch,
// orchestrator.ex:1047).
func TestRetryFire_DropsStaleFireAfterAttemptBumpReplacement(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 10)
	o := New(st, Deps{Dispatcher: disp})

	issueID := IssueID("ENG-ATT")
	issue := tracker.Issue{ID: string(issueID), Identifier: "ENG-ATT", State: "Needs Fix", Title: "bumped"}
	// Live entry is attempt 3 (a newer ScheduleRetry bumped it); the late timer
	// fires for attempt 2 of the same kind.
	st.ScheduleRetry(&RetryEntry{
		Issue:      issue,
		IssueID:    issueID,
		Identifier: issue.Identifier,
		Attempt:    3,
		Kind:       RetryKindFailure,
	})

	followup := (&retryFireOp{
		o:       o,
		id:      issueID,
		issue:   issue,
		attempt: 2,
		kind:    RetryKindFailure,
	}).apply(st)
	if followup != nil {
		t.Fatal("stale attempt fire returned a followup, want it dropped before side effects")
	}
	if got := disp.count(); got != 0 {
		t.Fatalf("stale attempt fire spawned %d workers, want 0", got)
	}
	entry, ok := st.RetryAttempts[issueID]
	if !ok {
		t.Fatal("stale attempt fire consumed the replacement retry")
	}
	if entry.Attempt != 3 {
		t.Fatalf("replacement retry attempt = %d, want 3 preserved", entry.Attempt)
	}
	if !st.IsClaimed(issueID) {
		t.Fatal("stale attempt fire released claim for replacement retry")
	}
}

// TestRetryFire_ContinuationFireWakesPollLoopWithoutDispatch pins the wake-only
// branch of (*retryFireOp).apply for a fired continuation timer. The companion
// TestContinuationRetryTimerRequiresTrackerRecheckedDispatch drives the eventual
// dispatch via an explicit RequestDispatchAfterTrackerRecheck — so deleting the
// wake-emission call would not fail it. This test fires a continuation timer
// straight into apply and asserts (a) the entry is retained for the tracker
// recheck, (b) its timer is cleared, (c) nothing dispatches from the cached
// snapshot, and (d) the followup actually queues a poll wake (closing the
// placebo gap). Continuation is now the only wake kind — the external-blocker arm
// of this branch was removed with the cooldown in #572.
func TestRetryFire_ContinuationFireWakesPollLoopWithoutDispatch(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 10)
	o := New(st, Deps{Dispatcher: disp})

	issueID := IssueID("ENG-CONT")
	issue := tracker.Issue{ID: string(issueID), Identifier: "ENG-CONT", State: "In Progress", Title: "continuation wake"}
	timer := time.AfterFunc(time.Hour, func() {})
	defer timer.Stop()
	st.ScheduleRetry(&RetryEntry{
		Issue:      issue,
		IssueID:    issueID,
		Identifier: issue.Identifier,
		Attempt:    3,
		Timer:      timer,
		Kind:       RetryKindContinuation,
	})

	followup := (&retryFireOp{
		o:       o,
		id:      issueID,
		issue:   issue,
		attempt: 3,
		kind:    RetryKindContinuation,
	}).apply(st)
	if followup == nil {
		t.Fatal("continuation fire returned nil, want a poll-wake followup")
	}

	entry, ok := st.RetryAttempts[issueID]
	if !ok {
		t.Fatal("continuation fire consumed the retry entry, want it retained for tracker recheck")
	}
	if entry.Timer != nil {
		t.Fatal("continuation fire left entry.Timer set, want it cleared after firing")
	}
	if got := disp.count(); got != 0 {
		t.Fatalf("continuation fire spawned %d workers, want 0 (must re-observe via poll)", got)
	}

	// The followup must actually wake the poll loop; otherwise the continuation
	// retry would never be re-observed and the issue would stall in the queue.
	followup()
	select {
	case <-o.retryWakeCh():
	default:
		t.Fatal("continuation fire followup did not queue a poll wake")
	}
}

func TestRetryFire_RespectsPerStateCapacityBeforeSpawning(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 10)
	st.MaxConcurrentAgentsByState = map[string]int{"rework": 1}
	o := New(st, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Hour}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	issueA := tracker.Issue{ID: "ENG-A", Identifier: "ENG-A", State: "Rework", Title: "retry later"}
	issueB := tracker.Issue{ID: "ENG-B", Identifier: "ENG-B", State: "Rework", Title: "running now"}
	if err := o.RequestDispatch(context.Background(), issueA, nil); err != nil {
		t.Fatalf("dispatch A: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	disp.finishAt(0, WorkerResult{Err: errors.New("transient"), Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Running) == 0 && len(v.Retrying) == 1
	}, time.Second)

	if err := o.RequestDispatch(context.Background(), issueB, nil); err != nil {
		t.Fatalf("dispatch B while A is retrying: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 2 }, time.Second)

	if err := o.submit(context.Background(), &retryFireOp{
		o:       o,
		id:      IssueID(issueA.ID),
		issue:   issueA,
		attempt: 1,
		kind:    RetryKindFailure,
	}); err != nil {
		t.Fatalf("submit retry fire: %v", err)
	}
	v, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := disp.count(); got != 2 {
		t.Fatalf("Dispatcher.Spawn calls = %d, want 2; retry must not spawn while per-state capacity is full", got)
	}
	if len(v.Running) != 1 || len(v.Retrying) != 1 {
		t.Fatalf("state after retry fire at per-state capacity: running=%+v retrying=%+v, want B running and A still retrying", v.Running, v.Retrying)
	}
}

// TestRetryFire_RespectsCapacityBeforeSpawning is a regression for retry
// timers bypassing the max_concurrent_agents gate. A failed issue may sit in
// RetryAttempts while another issue starts; when the retry timer fires, it must
// not spawn over the configured cap.
func TestRetryFire_RespectsCapacityBeforeSpawning(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 1)
	o := New(st, Deps{
		Dispatcher: disp,
		Scheduler:  RetryScheduler{MaxBackoff: 20 * time.Millisecond},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	issueA := tracker.Issue{ID: "ENG-A", Identifier: "ENG-A", Title: "retry later"}
	issueB := tracker.Issue{ID: "ENG-B", Identifier: "ENG-B", Title: "running now"}
	if err := o.RequestDispatch(context.Background(), issueA, nil); err != nil {
		t.Fatalf("dispatch A: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	disp.finishAt(0, WorkerResult{Err: errors.New("transient"), Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Running) == 0 && len(v.Retrying) == 1
	}, time.Second)

	if err := o.RequestDispatch(context.Background(), issueB, nil); err != nil {
		t.Fatalf("dispatch B while A is retrying: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 2 }, time.Second)

	time.Sleep(80 * time.Millisecond)
	v, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := disp.count(); got != 2 {
		t.Fatalf("Dispatcher.Spawn calls = %d, want 2; retry must not spawn while capacity is full", got)
	}
	if len(v.Running) != 1 || len(v.Retrying) != 1 {
		t.Fatalf("state after retry fire at capacity: running=%+v retrying=%+v, want B running and A still retrying", v.Running, v.Retrying)
	}
	if v.Running[0].IssueID != IssueID(issueB.ID) {
		t.Fatalf("running entries = %+v, want issue B to be the only running issue", v.Running)
	}
	if v.Retrying[0].IssueID != IssueID(issueA.ID) {
		t.Fatalf("retrying entries = %+v, want issue A preserved for later retry", v.Retrying)
	}
}

// TestRetryFire_GlobalCapacityFullReschedulesViaBackoff pins the SPEC §16.6
// algorithm-alignment fix for #306: when a fired failure-retry observes
// global capacity full, the orchestrator must reschedule via the
// configured backoff (attempt+1) and stamp the entry with the upstream
// "no available orchestrator slots" error — not arm a 100ms re-fire
// timer that leaves the attempt counter frozen and produces no runtime
// event for the cap-pressure case.
func TestRetryFire_GlobalCapacityFullReschedulesViaBackoff(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 1)
	o := New(st, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Hour, time.Hour}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	blocker := tracker.Issue{ID: "BLOCKER", Identifier: "BLOCKER", Title: "blocker"}
	if err := o.RequestDispatch(context.Background(), blocker, nil); err != nil {
		t.Fatalf("dispatch blocker: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	retryIssue := tracker.Issue{ID: "RETRY", Identifier: "RETRY", Title: "retry me"}
	if err := o.ScheduleRetry(context.Background(), retryIssue, retryIssue.Identifier, 1, "transient"); err != nil {
		t.Fatalf("ScheduleRetry: %v", err)
	}
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Retrying) == 1
	}, time.Second)

	if err := o.submit(context.Background(), &retryFireOp{
		o:       o,
		id:      IssueID(retryIssue.ID),
		issue:   retryIssue,
		attempt: 1,
		kind:    RetryKindFailure,
	}); err != nil {
		t.Fatalf("submit retry fire: %v", err)
	}

	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Retrying) == 1 &&
			v.Retrying[0].Attempt == 2 &&
			strings.Contains(v.Retrying[0].Error, "no available orchestrator slots")
	}, 2*time.Second)

	v, _ := o.Snapshot(context.Background())
	if got := disp.count(); got != 1 {
		t.Fatalf("Dispatcher.Spawn calls = %d, want 1 (no spawn while capacity full)", got)
	}
	if len(v.Retrying) != 1 {
		t.Fatalf("retrying entries = %d, want 1 (capacity-deferred reschedule)", len(v.Retrying))
	}
	if got := v.Retrying[0].Attempt; got != 2 {
		t.Fatalf("Retrying[0].Attempt = %d, want 2 (attempt+1 from capacity defer)", got)
	}
	if got := v.Retrying[0].Error; !strings.Contains(got, "no available orchestrator slots") {
		t.Fatalf("Retrying[0].Error = %q, want substring %q", got, "no available orchestrator slots")
	}
}

// TestRetryFire_PerStateCapacityFullReschedulesViaBackoff is the symmetric
// per-state version of the global-capacity test above: with
// max_concurrent_agents_by_state[Rework]=1 and one running Rework issue,
// firing a queued Rework retry must reschedule via the backoff with the
// same upstream-canonical error.
func TestRetryFire_PerStateCapacityFullReschedulesViaBackoff(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 100)
	st.MaxConcurrentAgentsByState = map[string]int{"rework": 1}
	o := New(st, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Hour, time.Hour}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	blocker := tracker.Issue{ID: "BLOCKER", Identifier: "BLOCKER", Title: "blocker", State: "Rework"}
	if err := o.RequestDispatch(context.Background(), blocker, nil); err != nil {
		t.Fatalf("dispatch blocker: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	retryIssue := tracker.Issue{ID: "RETRY", Identifier: "RETRY", Title: "retry me", State: "Rework"}
	if err := o.ScheduleRetry(context.Background(), retryIssue, retryIssue.Identifier, 1, "transient"); err != nil {
		t.Fatalf("ScheduleRetry: %v", err)
	}
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Retrying) == 1
	}, time.Second)

	if err := o.submit(context.Background(), &retryFireOp{
		o:       o,
		id:      IssueID(retryIssue.ID),
		issue:   retryIssue,
		attempt: 1,
		kind:    RetryKindFailure,
	}); err != nil {
		t.Fatalf("submit retry fire: %v", err)
	}

	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Retrying) == 1 &&
			v.Retrying[0].Attempt == 2 &&
			strings.Contains(v.Retrying[0].Error, "no available orchestrator slots")
	}, 2*time.Second)

	v, _ := o.Snapshot(context.Background())
	if got := disp.count(); got != 1 {
		t.Fatalf("Dispatcher.Spawn calls = %d, want 1 (no spawn while per-state cap full)", got)
	}
	if got := v.Retrying[0].Attempt; got != 2 {
		t.Fatalf("Retrying[0].Attempt = %d, want 2 (attempt+1 from per-state cap defer)", got)
	}
	if got := v.Retrying[0].Error; !strings.Contains(got, "no available orchestrator slots") {
		t.Fatalf("Retrying[0].Error = %q, want substring %q", got, "no available orchestrator slots")
	}
}

func TestScheduleRetryUsesReloadedSchedulerWithoutRacing(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Hour}},
	})
	defer cancel()

	if err := o.UpdateRetryScheduler(context.Background(), &sequenceScheduler{delays: []time.Duration{time.Millisecond}}); err != nil {
		t.Fatalf("UpdateRetryScheduler: %v", err)
	}
	iss := tracker.Issue{ID: "ENG-SCHED", Identifier: "ENG-SCHED", Title: "scheduler reload"}
	if err := o.ScheduleRetry(context.Background(), iss, iss.Identifier, 1, "boom"); err != nil {
		t.Fatalf("ScheduleRetry: %v", err)
	}

	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)
}

// TestApply_FollowupRunsOffActorAndCanResubmit pins the design's
// "apply must not block on the ops channel" invariant: a followup
// returned by apply runs on a fresh goroutine, so it can submit
// further ops without deadlocking against the actor. If a future
// refactor inlined the followup call into the actor loop, this test
// would deadlock and time out.
func TestApply_FollowupRunsOffActorAndCanResubmit(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	resubmitted := make(chan struct{})
	first := opFunc(func(*OrchestratorState) func() {
		return func() {
			// This runs in the goroutine the actor spawned for the
			// followup. Submitting another op here must not deadlock,
			// because the actor is already back in its select loop
			// reading from o.ops.
			second := opFunc(func(*OrchestratorState) func() {
				close(resubmitted)
				return nil
			})
			_ = o.submit(context.Background(), second)
		}
	})
	if err := o.submit(context.Background(), first); err != nil {
		t.Fatalf("submit first op: %v", err)
	}

	select {
	case <-resubmitted:
	case <-time.After(time.Second):
		t.Fatal("re-submitted op never ran — actor likely deadlocked on followup")
	}
}

// TestRun_ContextCancelStopsActor verifies the actor terminates when
// its run ctx is cancelled. After cancel, in-flight submits return
// promptly via their own ctx; ops queued before cancel may or may not
// be applied (the loop drains nothing extra after seeing ctx.Done).
func TestRun_ContextCancelStopsActor(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 4)
	o := New(st, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.Run(ctx)
		close(done)
	}()
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit within 1s of ctx cancel")
	}
}

// TestSnapshot_SerializedThroughActor demonstrates that Snapshot sees
// state mutations atomically: after RequestDispatch returns success,
// a Snapshot call serializes after the registerRunningOp the dispatch
// followup submitted, so the Running entry is always visible.
func TestSnapshot_SerializedThroughActor(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	for i := 0; i < 10; i++ {
		iss := tracker.Issue{ID: idForIndex(i), Identifier: idForIndex(i)}
		if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
			t.Fatalf("RequestDispatch[%d]: %v", i, err)
		}
		v, err := o.Snapshot(context.Background())
		if err != nil {
			t.Fatalf("Snapshot[%d]: %v", i, err)
		}
		if len(v.Running) != i+1 {
			t.Errorf("Running view after dispatch[%d] = %d entries, want %d", i, len(v.Running), i+1)
		}
	}
}

func TestRetryScheduler_FailureBackoffDoublesUntilCap(t *testing.T) {
	s := RetryScheduler{MaxBackoff: 25 * time.Second}
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: 10 * time.Second},
		{attempt: 2, want: 20 * time.Second},
		{attempt: 3, want: 25 * time.Second},
		{attempt: 4, want: 25 * time.Second},
	}
	for _, tt := range tests {
		if got := s.NextDelay(RetryRequest{Attempt: tt.attempt}); got != tt.want {
			t.Errorf("failure retry delay attempt %d = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// TestSpawn_FinalizeSubmitFailureClosesWorkerDone exercises the SIGTERM
// shutdown race: the worker goroutine receives a result and tries to submit
// finalizeRunOp, but the actor's runCtx is canceled before the actor accepts
// it. Without the fix, the submit error is discarded and workerDone is never
// closed, so any consumer waiting on entry.Done blocks until the process is
// killed. With the fix, the spawn goroutine closes workerDone itself when
// submit fails.
func TestSpawn_FinalizeSubmitFailureClosesWorkerDone(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 100)
	o := New(st, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	ctx, cancel := context.WithCancel(context.Background())
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}
	defer cancel()

	iss := tracker.Issue{ID: "ENG-shutdown", Identifier: "ENG-shutdown", Title: "shutdown race"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}

	entryCh := make(chan *RunningEntry, 1)
	if err := o.submit(context.Background(), opFunc(func(st *OrchestratorState) func() {
		entryCh <- st.Running[IssueID(iss.ID)]
		return nil
	})); err != nil {
		t.Fatalf("fetch running entry: %v", err)
	}
	entry := <-entryCh
	if entry == nil {
		t.Fatalf("Running[%s] = nil after dispatch", iss.ID)
	}

	// Pause the actor with a sentinel op so we can deterministically observe
	// the shutdown race: the worker goroutine's submit will be blocked on the
	// ops channel while we cancel runCtx.
	sentinelStarted := make(chan struct{})
	releaseSentinel := make(chan struct{})
	sentinelDone := make(chan struct{})
	go func() {
		defer close(sentinelDone)
		_ = o.submit(context.Background(), opFunc(func(*OrchestratorState) func() {
			close(sentinelStarted)
			<-releaseSentinel
			return nil
		}))
	}()
	<-sentinelStarted

	// Capture the per-worker context the dispatcher received. Spawn's watcher
	// goroutine calls `cancel()` on this context at actor.go:1289 the instant
	// it consumes a result from `resultCh`, before falling through to the
	// `o.submit(o.runCtx, &finalizeRunOp{...})` call. We use its Done channel
	// below as a deterministic signal that the watcher has moved past its
	// outer select and committed to the finalize-submit path — replacing the
	// earlier (broken) `len(o.ops)` check, since `o.ops` is unbuffered and its
	// length is always 0.
	workerCtx := disp.context()

	// Deliver the worker result while the actor is paused. The spawn goroutine
	// receives it and attempts to submit finalizeRunOp; that submit blocks on
	// the unbuffered ops channel.
	disp.finishAt(0, WorkerResult{Err: nil, Elapsed: 10 * time.Millisecond})

	// Wait for the watcher goroutine to consume the result. Once workerCtx is
	// canceled, the watcher has left its outer select and the only forward
	// path is `o.submit(o.runCtx, &finalizeRunOp{...})`, which we want to
	// observe failing.
	select {
	case <-workerCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("watcher never consumed dispatch result")
	}

	// Cancel runCtx. Once the actor finishes the sentinel op and re-enters its
	// select, it observes ctx.Done() and exits the for-loop without draining
	// the queued finalize submit. The worker goroutine's submit then returns
	// ctx.Err.
	cancel()
	close(releaseSentinel)
	<-sentinelDone

	select {
	case <-entry.Done:
	case <-time.After(2 * time.Second):
		t.Fatalf("entry.Done leaked after shutdown race: workerDone was never closed")
	}
}

func idForIndex(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "ENG-0"
	}
	out := []byte{'E', 'N', 'G', '-'}
	var buf []byte
	for i > 0 {
		buf = append([]byte{digits[i%10]}, buf...)
		i /= 10
	}
	return string(append(out, buf...))
}

// fakeCandidateLister is a stub ActiveIssueLister for SPEC §16.6 retry-fire
// tests. Tests configure issues and err, then assert the retryFireOp branch.
type fakeCandidateLister struct {
	mu     sync.Mutex
	issues []tracker.Issue
	err    error
	calls  int
}

func (f *fakeCandidateLister) ListActiveIssues(_ context.Context) ([]tracker.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	out := make([]tracker.Issue, len(f.issues))
	copy(out, f.issues)
	return out, f.err
}

func (f *fakeCandidateLister) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestRetryFire_ReleasesClaimWhenIssueAbsentFromCandidates is the SPEC §16.6
// step 3 / step 5 conformance check: when the candidate fetch returns
// successfully but the issue isn't present (either deleted or no longer in an
// active state), the retry releases the claim instead of re-dispatching.
func TestRetryFire_ReleasesClaimWhenIssueAbsentFromCandidates(t *testing.T) {
	disp := &fakeDispatcher{}
	lister := &fakeCandidateLister{}
	o, cancel := startActor(t, Deps{
		Dispatcher:      disp,
		Scheduler:       &sequenceScheduler{delays: []time.Duration{time.Hour}},
		CandidateLister: lister,
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-GONE", Identifier: "ENG-GONE", Title: "absent", State: "Todo"}
	if err := o.ScheduleRetry(context.Background(), iss, iss.Identifier, 2, "transient"); err != nil {
		t.Fatalf("ScheduleRetry: %v", err)
	}
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Retrying) != 1 {
		t.Fatalf("retrying entries = %d, want 1 queued retry", len(view.Retrying))
	}

	// Lister returns no issues: SPEC §16.6 step 3 — release the claim.
	if err := o.submit(context.Background(), &retryFireOp{
		o:       o,
		id:      IssueID(iss.ID),
		issue:   iss,
		attempt: 2,
		kind:    RetryKindFailure,
	}); err != nil {
		t.Fatalf("submit retry fire: %v", err)
	}

	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Retrying) == 0
	}, 2*time.Second)

	v, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot after fire: %v", err)
	}
	if got := disp.count(); got != 0 {
		t.Fatalf("Dispatcher.Spawn calls = %d, want 0; absent issue must not be re-dispatched", got)
	}
	if len(v.Retrying) != 0 {
		t.Fatalf("retrying view after absent fetch = %+v, want claim released", v.Retrying)
	}
	if lister.callCount() == 0 {
		t.Fatal("candidate lister was not consulted on retry fire")
	}
}

// staticStateRefresher is a read-only IssueStateRefresher for the §16.6
// retry-fire terminal-resolution tests (#341). Concurrent reads of the
// underlying map from multiple followup goroutines are safe because nothing
// mutates it after construction.
type staticStateRefresher map[string]string

func (s staticStateRefresher) FetchIssueStatesByIDs(_ context.Context, ids []string) (map[string]tracker.IssueState, error) {
	out := make(map[string]tracker.IssueState, len(ids))
	for _, id := range ids {
		if state, ok := s[id]; ok {
			out[id] = tracker.IssueState{State: state}
		}
	}
	return out, nil
}

type recordingRefStateRefresher struct {
	states map[string]string
	refs   [][]tracker.IssueRef
}

func (r *recordingRefStateRefresher) FetchIssueStatesByIDs(context.Context, []string) (map[string]tracker.IssueState, error) {
	return nil, errors.New("legacy ID-only retry terminal refresh should not be used")
}

func (r *recordingRefStateRefresher) FetchIssueStatesByRefs(_ context.Context, refs []tracker.IssueRef) (map[string]tracker.IssueState, error) {
	r.refs = append(r.refs, append([]tracker.IssueRef(nil), refs...))
	out := make(map[string]tracker.IssueState, len(refs))
	for _, ref := range refs {
		if state, ok := r.states[ref.ID]; ok {
			out[ref.ID] = tracker.IssueState{State: state}
		}
	}
	return out, nil
}

// TestRetryFire_TerminalIssueFiresWorkspaceCleanup is the failure-retry half of
// #341: a failure retry whose issue has since gone terminal must clean its
// workspace when the SPEC §16.6 retry timer fires. The active-only candidate
// fetch returns found==nil for a terminal issue, indistinguishable from a
// deleted one; the terminal-state resolver recovers upstream
// handle_retry_issue_lookup's terminal branch so the directory is removed
// through the WorkspaceCleaner seam (reason=terminal) instead of leaking.
func TestRetryFire_TerminalIssueFiresWorkspaceCleanup(t *testing.T) {
	disp := &fakeDispatcher{}
	lister := &fakeCandidateLister{} // empty active set → found==nil
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        &sequenceScheduler{delays: []time.Duration{time.Hour}},
		CandidateLister:  lister,
		WorkspaceCleaner: cleaner,
	})
	defer cancel()
	resolver := &recordingRefStateRefresher{states: map[string]string{"global-101": "Done"}}
	o.SetRetryTerminalStateResolver(resolver, []string{"Done"})

	issue := tracker.Issue{ID: "global-101", Identifier: "#7", State: "In Progress", Title: "fail then terminal"}
	const wsPath = "/var/aiops/workspaces/acme/repo/github_issue/7"
	dispatchRunningIssue(t, o, disp, issue, wsPath, 1)

	// Abnormal (retryable) exit → failure retry that carries the run's workspace.
	disp.finishAt(0, WorkerResult{Err: errors.New("transient"), Elapsed: time.Millisecond})
	var attempt int
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		if len(v.Retrying) == 1 && len(v.Running) == 0 {
			attempt = v.Retrying[0].Attempt
			return true
		}
		return false
	}, time.Second)

	// Fire the retry timer: candidate fetch is empty (issue not active), the
	// resolver reports the terminal state → clean + release.
	if err := o.submit(context.Background(), &retryFireOp{
		o:       o,
		id:      IssueID(issue.ID),
		issue:   issue,
		attempt: attempt,
		kind:    RetryKindFailure,
	}); err != nil {
		t.Fatalf("submit retry fire: %v", err)
	}

	waitFor(t, func() bool { return len(cleaner.snapshot()) == 1 }, 2*time.Second)
	if len(resolver.refs) != 2 {
		t.Fatalf("terminal resolver calls = %d, want retry-fire lookup plus cleanup recheck", len(resolver.refs))
	}
	for i, refs := range resolver.refs {
		if len(refs) != 1 || refs[0].ID != "global-101" || refs[0].Identifier != "#7" {
			t.Fatalf("terminal resolver refs[%d] = %#v, want global-101 with identifier #7", i, refs)
		}
	}
	got := cleaner.snapshot()[0]
	if got.IssueID != IssueID(issue.ID) || got.Path != wsPath || got.Reason != "terminal" || got.State != "Done" {
		t.Fatalf("cleanup = %+v, want terminal cleanup for %s at %q reason=terminal state=Done", got, issue.ID, wsPath)
	}
	if got.Root != testWorkspaceRoot {
		t.Fatalf("cleanup root = %q, want recorded dispatch-time root %q", got.Root, testWorkspaceRoot)
	}
	v, _ := o.Snapshot(context.Background())
	if len(v.Retrying) != 0 {
		t.Fatalf("retry not released after terminal fire: %+v", v.Retrying)
	}
}

// TestRetryFire_InactiveIssueKeepsWorkspace pins the other branch of the
// failure-retry §16.6 resolution (#341): a retry whose issue went to a
// merely-inactive (non-terminal) state is released WITHOUT removing its
// workspace. The assertion is deterministic: retryFireAfterFetchOp records the
// release reason in the SAME apply that releases the claim, so observing the
// "absent from active candidates" event (not the "issue terminal" one) proves
// the terminal branch was not taken — and that branch is the only path that
// schedules a cleanup. A reverted terminal gate (classifying Backlog as
// terminal) would record the "issue terminal" message and fail this assertion.
func TestRetryFire_InactiveIssueKeepsWorkspace(t *testing.T) {
	disp := &fakeDispatcher{}
	lister := &fakeCandidateLister{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        &sequenceScheduler{delays: []time.Duration{time.Hour}},
		CandidateLister:  lister,
		WorkspaceCleaner: cleaner,
	})
	defer cancel()
	o.SetRetryTerminalStateResolver(staticStateRefresher{"ENG-FR-I": "Backlog"}, []string{"Done"})

	issue := tracker.Issue{ID: "ENG-FR-I", Identifier: "ENG-FR-I", State: "In Progress", Title: "fail then inactive"}
	const wsPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-FR-I"
	dispatchRunningIssue(t, o, disp, issue, wsPath, 1)

	disp.finishAt(0, WorkerResult{Err: errors.New("transient"), Elapsed: time.Millisecond})
	var attempt int
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		if len(v.Retrying) == 1 {
			attempt = v.Retrying[0].Attempt
			return true
		}
		return false
	}, time.Second)

	if err := o.submit(context.Background(), &retryFireOp{
		o:       o,
		id:      IssueID(issue.ID),
		issue:   issue,
		attempt: attempt,
		kind:    RetryKindFailure,
	}); err != nil {
		t.Fatalf("submit retry fire: %v", err)
	}

	// The resolver reports Backlog (non-terminal) → release only. Wait for the
	// release event, which apply records synchronously with the ReleaseClaim.
	var msg string
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		if len(v.Retrying) != 0 {
			return false
		}
		for _, ev := range v.RecentEvents {
			if ev.IssueID == IssueID(issue.ID) && strings.HasPrefix(ev.Message, "retry released:") {
				msg = ev.Message
				return true
			}
		}
		return false
	}, 2*time.Second)
	if msg != "retry released: issue absent from active candidates" {
		t.Fatalf("release event = %q, want the non-terminal release-only message (no workspace cleanup)", msg)
	}
	if got := cleaner.snapshot(); len(got) != 0 {
		t.Fatalf("non-terminal retry must not clean its workspace, got %+v", got)
	}
}

// TestRetryFire_ReschedulesWithRetryPollFailedOnFetchError is the SPEC §16.6
// step 1 alt conformance check: when the candidate fetch fails, the retry is
// rescheduled with a typed "retry poll failed" error so the next backoff
// window can try the fetch again — the claim must not be released.
func TestRetryFire_ReschedulesWithRetryPollFailedOnFetchError(t *testing.T) {
	disp := &fakeDispatcher{}
	lister := &fakeCandidateLister{err: errors.New("tracker unreachable")}
	o, cancel := startActor(t, Deps{
		Dispatcher:      disp,
		Scheduler:       &sequenceScheduler{delays: []time.Duration{time.Hour, time.Hour}},
		CandidateLister: lister,
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-FETCH-FAIL", Identifier: "ENG-FETCH-FAIL", Title: "fetch err", State: "Todo"}
	if err := o.ScheduleRetry(context.Background(), iss, iss.Identifier, 1, "transient"); err != nil {
		t.Fatalf("ScheduleRetry: %v", err)
	}

	if err := o.submit(context.Background(), &retryFireOp{
		o:       o,
		id:      IssueID(iss.ID),
		issue:   iss,
		attempt: 1,
		kind:    RetryKindFailure,
	}); err != nil {
		t.Fatalf("submit retry fire: %v", err)
	}

	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Retrying) == 1 && v.Retrying[0].Attempt == 2
	}, 2*time.Second)

	v, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := disp.count(); got != 0 {
		t.Fatalf("Dispatcher.Spawn calls = %d, want 0; fetch failure must not dispatch", got)
	}
	if len(v.Retrying) != 1 {
		t.Fatalf("retrying entries = %d, want 1 rescheduled retry", len(v.Retrying))
	}
	got := v.Retrying[0]
	if got.Attempt != 2 {
		t.Fatalf("rescheduled attempt = %d, want attempt+1 = 2", got.Attempt)
	}
	if !strings.Contains(got.Error, "retry poll failed") {
		t.Fatalf("rescheduled error = %q, want it to contain \"retry poll failed\"", got.Error)
	}
	if !strings.Contains(got.Error, "tracker unreachable") {
		t.Fatalf("rescheduled error = %q, want the underlying fetch error wrapped in", got.Error)
	}
}

// TestRetryFire_DispatchesWhenCandidateFetchReturnsIssue is the SPEC §16.6
// step 4 conformance check: when the candidate fetch finds the issue, the
// retry consumes its entry and dispatches a worker — and the dispatched
// issue carries the fresh tracker state, not the dispatch-time snapshot.
func TestRetryFire_DispatchesWhenCandidateFetchReturnsIssue(t *testing.T) {
	disp := &fakeDispatcher{}
	freshState := tracker.Issue{ID: "ENG-FOUND", Identifier: "ENG-FOUND", Title: "refreshed", State: "Rework"}
	lister := &fakeCandidateLister{issues: []tracker.Issue{freshState}}
	o, cancel := startActor(t, Deps{
		Dispatcher:      disp,
		Scheduler:       &sequenceScheduler{delays: []time.Duration{time.Hour}},
		CandidateLister: lister,
	})
	defer cancel()

	stale := freshState
	stale.State = "Todo"
	if err := o.ScheduleRetry(context.Background(), stale, stale.Identifier, 1, "transient"); err != nil {
		t.Fatalf("ScheduleRetry: %v", err)
	}

	if err := o.submit(context.Background(), &retryFireOp{
		o:       o,
		id:      IssueID(stale.ID),
		issue:   stale,
		attempt: 1,
		kind:    RetryKindFailure,
	}); err != nil {
		t.Fatalf("submit retry fire: %v", err)
	}

	waitFor(t, func() bool { return disp.count() == 1 }, 2*time.Second)
	if got := disp.issueAt(0).State; got != "Rework" {
		t.Fatalf("dispatched issue state = %q, want Rework from refreshed candidate fetch", got)
	}
}

// blockingCandidateLister blocks ListActiveIssues until either the caller's
// context is cancelled or the explicit unblock channel is closed. It is the
// stand-in for a tracker client that ignores ctx cancellation: the retry
// fire's per-fetch timeout must still fire and reschedule the retry rather
// than leaving the issue stuck in Claimed forever.
type blockingCandidateLister struct {
	unblock <-chan struct{}
}

func (l *blockingCandidateLister) ListActiveIssues(ctx context.Context) ([]tracker.Issue, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.unblock:
		return nil, nil
	}
}

// TestRetryFire_FetchTimeoutReschedulesAsRetryPollFailed asserts the SPEC
// §16.6 hardening contract: when the candidate fetch outruns
// retryFetchTimeout (modeled here by a lister that only returns on context
// cancellation), the retry must reschedule via the "retry poll failed"
// branch instead of orphaning the claim. Without the per-fetch timeout
// the followup goroutine would pin indefinitely with entry.Timer already
// cleared, so this regression covers the hardening fix.
func TestRetryFire_FetchTimeoutReschedulesAsRetryPollFailed(t *testing.T) {
	prevTimeout := retryFetchTimeout
	retryFetchTimeout = 50 * time.Millisecond
	defer func() { retryFetchTimeout = prevTimeout }()

	disp := &fakeDispatcher{}
	lister := &blockingCandidateLister{unblock: make(chan struct{})}
	o, cancel := startActor(t, Deps{
		Dispatcher:      disp,
		Scheduler:       &sequenceScheduler{delays: []time.Duration{time.Hour, time.Hour}},
		CandidateLister: lister,
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-TIMEOUT", Identifier: "ENG-TIMEOUT", Title: "hang", State: "Todo"}
	if err := o.ScheduleRetry(context.Background(), iss, iss.Identifier, 1, "transient"); err != nil {
		t.Fatalf("ScheduleRetry: %v", err)
	}

	if err := o.submit(context.Background(), &retryFireOp{
		o:       o,
		id:      IssueID(iss.ID),
		issue:   iss,
		attempt: 1,
		kind:    RetryKindFailure,
	}); err != nil {
		t.Fatalf("submit retry fire: %v", err)
	}

	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Retrying) == 1 && v.Retrying[0].Attempt == 2
	}, 2*time.Second)

	v, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := disp.count(); got != 0 {
		t.Fatalf("Dispatcher.Spawn calls = %d, want 0; fetch timeout must not dispatch", got)
	}
	if len(v.Retrying) != 1 {
		t.Fatalf("retrying entries = %d, want 1 rescheduled retry; the claim must not orphan", len(v.Retrying))
	}
	got := v.Retrying[0]
	if got.Attempt != 2 {
		t.Fatalf("rescheduled attempt = %d, want attempt+1 = 2 per SPEC §16.6", got.Attempt)
	}
	if !strings.Contains(got.Error, "retry poll failed") {
		t.Fatalf("rescheduled error = %q, want the typed \"retry poll failed\" prefix", got.Error)
	}
	if !strings.Contains(got.Error, context.DeadlineExceeded.Error()) {
		t.Fatalf("rescheduled error = %q, want the deadline-exceeded reason wrapped in", got.Error)
	}
}

func TestRetryFire_QuotaBackoffFetchFailureReschedulesWithoutOrphaningClaim(t *testing.T) {
	disp := &fakeDispatcher{}
	lister := &fakeCandidateLister{err: errors.New("tracker unavailable")}
	o, cancel := startActor(t, Deps{
		Dispatcher:      disp,
		Scheduler:       &sequenceScheduler{delays: []time.Duration{time.Hour, time.Hour}},
		CandidateLister: lister,
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-QUOTA-POLL", Identifier: "ENG-QUOTA-POLL", Title: "quota poll", State: "Todo"}
	if err := o.scheduleQuotaBackoffRetry(context.Background(), iss, iss.Identifier, 0, "quota backoff", 0, Workspace{}); err != nil {
		t.Fatalf("scheduleQuotaBackoffRetry: %v", err)
	}

	if err := o.submit(context.Background(), &retryFireOp{
		o:       o,
		id:      IssueID(iss.ID),
		issue:   iss,
		attempt: 0,
		kind:    RetryKindQuotaBackoff,
	}); err != nil {
		t.Fatalf("submit quota retry fire: %v", err)
	}

	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Retrying) == 1 && strings.Contains(v.Retrying[0].Error, "retry poll failed")
	}, 2*time.Second)

	v, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := disp.count(); got != 0 {
		t.Fatalf("Dispatcher.Spawn calls = %d, want 0; failed retry poll must not dispatch", got)
	}
	if len(v.Retrying) != 1 {
		t.Fatalf("retrying entries = %d, want 1 rescheduled quota backoff", len(v.Retrying))
	}
	got := v.Retrying[0]
	if got.Kind != RetryKindQuotaBackoff {
		t.Fatalf("rescheduled kind = %q, want %q", got.Kind, RetryKindQuotaBackoff)
	}
	if got.Attempt != 0 {
		t.Fatalf("rescheduled quota attempt = %d, want unchanged attempt 0", got.Attempt)
	}
	if !strings.Contains(got.Error, "tracker unavailable") {
		t.Fatalf("rescheduled error = %q, want tracker failure reason", got.Error)
	}
}

// stubActiveIssueLister returns canned issues without applying any
// downstream filters. Tests stack the real production wrapper
// (eligibleActiveIssueLister) on top of it so the assertions exercise
// the actual filter code rather than a mock that pre-filters.
type stubActiveIssueLister struct {
	mu     sync.Mutex
	issues []tracker.Issue
	err    error
}

func (s *stubActiveIssueLister) ListActiveIssues(_ context.Context) ([]tracker.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]tracker.Issue, len(s.issues))
	copy(out, s.issues)
	return out, s.err
}

func (s *stubActiveIssueLister) replace(issues []tracker.Issue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues = issues
}

// TestRetryFire_TodoIssueBlockedByOpenDependencyReleasesClaim is the
// HIGH-severity regression test for the cross-cutting filter gap a
// post-merge adversarial audit found: the poll loop applies
// filterEligibleCandidates between active-state fetch and dispatch
// (poller.go), and the retry-fire lister chain must mirror it. Without
// the eligibleActiveIssueLister wrap, a Todo issue whose retry was
// scheduled when its blocker was terminal would still be dispatched
// after the blocker re-opens, because the active-state filter alone
// would not reject it. The wrap is in runtime_poller.go; this test
// pins the actor-side contract using the real production wrap stack.
func TestRetryFire_TodoIssueBlockedByOpenDependencyReleasesClaim(t *testing.T) {
	disp := &fakeDispatcher{}
	inner := &stubActiveIssueLister{}
	// Mirror the poll loop's filter chain — active → eligible. No routing
	// configured so this isolates the eligibility filter alone.
	lister := eligibleActiveIssueLister{inner: inner, terminalStates: []string{"Done", "Cancelled"}}

	o, cancel := startActor(t, Deps{
		Dispatcher:      disp,
		Scheduler:       &sequenceScheduler{delays: []time.Duration{time.Hour}},
		CandidateLister: lister,
	})
	defer cancel()

	// Retry was scheduled when the Todo issue's only blocker was terminal.
	iss := tracker.Issue{ID: "ENG-TODO", Identifier: "ENG-TODO", Title: "todo", State: "Todo"}
	if err := o.ScheduleRetry(context.Background(), iss, iss.Identifier, 1, "transient"); err != nil {
		t.Fatalf("ScheduleRetry: %v", err)
	}

	// Between schedule and fire the blocker re-opens — its state is now
	// "In Progress", which is non-terminal. The poll loop would refuse
	// this issue via filterEligibleCandidates; retry-fire must too.
	inner.replace([]tracker.Issue{{
		ID: "ENG-TODO", Identifier: "ENG-TODO", Title: "todo", State: "Todo",
		BlockedBy: []tracker.BlockerRef{{ID: "ENG-BLOCK", Identifier: "ENG-BLOCK", State: "In Progress"}},
	}})

	if err := o.submit(context.Background(), &retryFireOp{
		o:       o,
		id:      IssueID(iss.ID),
		issue:   iss,
		attempt: 1,
		kind:    RetryKindFailure,
	}); err != nil {
		t.Fatalf("submit retry fire: %v", err)
	}

	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Retrying) == 0
	}, 2*time.Second)

	v, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot after fire: %v", err)
	}
	if got := disp.count(); got != 0 {
		t.Fatalf("Dispatcher.Spawn calls = %d, want 0; Todo with non-terminal blocker must not be dispatched", got)
	}
	if len(v.Retrying) != 0 {
		t.Fatalf("retrying view after blocked-Todo fetch = %+v, want claim released to match poll-loop filter", v.Retrying)
	}
}

// TestRetryFireAfterFetch_RefreshedIdentifierSurfacesInSnapshot pins the
// LOW-severity audit finding: when the candidate fetch surfaces a refreshed
// issue with a renamed Identifier (Linear supports identifier renames on
// team key changes), retryFireAfterFetchOp.apply must mutate
// entry.Identifier — not just entry.Issue — so subsequent runtime events
// and retry reschedules stamp the live value, not the stale schedule-time
// snapshot.
//
// The previous version of this test asserted only against the dispatched
// issue (`disp.issueAt(0).Identifier`), which is fed by
// `entry.Issue = *r.found` — the pre-existing line. A mutation test
// (deleting the new `entry.Identifier = id` line) showed it still
// passed: the assertion never exercised the entry.Identifier field.
//
// The fix: block dispatch at the capacity gate so the entry stays in
// RetryAttempts after the fetch+refresh. The Retrying snapshot view
// reads RetryEntry.Identifier directly (Snapshot's Retrying loop in
// state_snapshot.go), so asserting
// against `v.Retrying[0].Identifier` does pin the new mutation. With
// the new line deleted, the entry retains the stale "OLD-1" and the
// test fails.
func TestRetryFireAfterFetch_RefreshedIdentifierSurfacesInSnapshot(t *testing.T) {
	disp := &fakeDispatcher{}
	fresh := tracker.Issue{ID: "ID-STABLE", Identifier: "NEW-1", Title: "renamed", State: "Rework"}
	lister := &fakeCandidateLister{issues: []tracker.Issue{fresh}}

	// Cap capacity at 1 and fill it with a different issue so the retry's
	// dispatch tail trips the global cap and reschedules via
	// capacityDeferRetry — leaving a refreshed entry in RetryAttempts
	// (carrying the same Identifier) for the snapshot to observe.
	st := NewOrchestratorState(15000, 1)
	o := New(st, Deps{
		Dispatcher:      disp,
		Scheduler:       &sequenceScheduler{delays: []time.Duration{time.Hour}},
		CandidateLister: lister,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	blocker := tracker.Issue{ID: "ID-BLOCKER", Identifier: "BLOCKER-1", Title: "blocker", State: "Rework"}
	if err := o.RequestDispatch(context.Background(), blocker, nil); err != nil {
		t.Fatalf("dispatch blocker: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	stale := tracker.Issue{ID: "ID-STABLE", Identifier: "OLD-1", Title: "renamed", State: "Todo"}
	if err := o.ScheduleRetry(context.Background(), stale, stale.Identifier, 1, "transient"); err != nil {
		t.Fatalf("ScheduleRetry: %v", err)
	}

	if err := o.submit(context.Background(), &retryFireOp{
		o:       o,
		id:      IssueID(stale.ID),
		issue:   stale,
		attempt: 1,
		kind:    RetryKindFailure,
	}); err != nil {
		t.Fatalf("submit retry fire: %v", err)
	}

	// The retry-fire chain: candidate fetch → retryFireAfterFetchOp.apply
	// (refreshes entry.Issue + entry.Identifier from r.found) →
	// retryFireDispatchTail observes RunningCount=1 >= cap=1 →
	// capacityDeferRetry submits a fresh ScheduleRetry that carries the
	// refreshed Identifier into a new RetryEntry. The Retrying snapshot
	// view must surface that identifier.
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Retrying) == 1 && v.Retrying[0].Identifier == "NEW-1"
	}, 2*time.Second)

	v, _ := o.Snapshot(context.Background())
	if got := disp.count(); got != 1 {
		t.Fatalf("Dispatcher.Spawn calls = %d, want 1 (blocker only); retry must not spawn under capacity", got)
	}
	if len(v.Retrying) != 1 {
		t.Fatalf("retrying entries = %d, want 1 (capacity-deferred refreshed entry)", len(v.Retrying))
	}
	if v.Retrying[0].Identifier != "NEW-1" {
		t.Fatalf("Retrying[0].Identifier = %q, want NEW-1 from refreshed candidate fetch — entry.Identifier mutation is missing", v.Retrying[0].Identifier)
	}
}
