package orchestrator

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// fakeDispatcher records every Spawn call and lets the test control
// when each spawned worker "exits" by closing/sending on the per-call
// result channel. Concurrent calls are safe: spawnCount and the
// results slice are guarded by a mutex.
type fakeDispatcher struct {
	mu         sync.Mutex
	spawnCount int
	results    []chan WorkerResult
	contexts   []context.Context
	issues     []tracker.Issue
	attempts   []*int

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

func (d *earlyRuntimeEventDispatcher) Spawn(ctx context.Context, issue tracker.Issue, _ *int) <-chan WorkerResult {
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

func (f *fakeDispatcher) Spawn(ctx context.Context, issue tracker.Issue, attempt *int) <-chan WorkerResult {
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

func (f *fakeDispatcher) contextAt(i int) context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.contexts[i]
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
// a running entry whose LastCodexAt is older than the configured stall
// budget gets its worker context cancelled, and the finalize path treats
// the resulting context.Canceled as a normal worker failure (claim
// retained, retry scheduled), NOT as a reconciled cancel.
func TestReconcileStalledRunsCancelsWorkerPastTimeout(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "STALL-1", Identifier: "STALL-1", Title: "stuck", State: "AI Ready"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	// Force the running entry's LastCodexAt into the past so the stall
	// budget (100ms) is comfortably exceeded.
	o.WithStateForTest(func(st *OrchestratorState) {
		st.Running["STALL-1"].LastCodexAt = time.Now().Add(-10 * time.Second)
	})

	if err := o.ReconcileStalledRuns(context.Background(), 100, 0); err != nil {
		t.Fatalf("ReconcileStalledRuns: %v", err)
	}

	select {
	case <-disp.contextAt(0).Done():
	case <-time.After(time.Second):
		t.Fatal("stalled worker context was not cancelled by ReconcileStalledRuns")
	}
}

// TestReconcileStalledRunsLeavesActiveRunsAlone pins the no-false-positive
// invariant: an entry with a recent LastCodexAt must not be cancelled
// even when the stall budget is small.
func TestReconcileStalledRunsLeavesActiveRunsAlone(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ACTIVE-1", Identifier: "ACTIVE-1", State: "AI Ready"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	o.WithStateForTest(func(st *OrchestratorState) {
		st.Running["ACTIVE-1"].LastCodexAt = time.Now()
	})

	if err := o.ReconcileStalledRuns(context.Background(), 5_000, 0); err != nil {
		t.Fatalf("ReconcileStalledRuns: %v", err)
	}

	select {
	case <-disp.contextAt(0).Done():
		t.Fatal("active worker context was cancelled even though LastCodexAt is recent")
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

	iss := tracker.Issue{ID: "OFF-1", Identifier: "OFF-1", State: "AI Ready"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	// Even with an ancient LastCodexAt, stall detection is a no-op when
	// the budget is 0.
	o.WithStateForTest(func(st *OrchestratorState) {
		st.Running["OFF-1"].LastCodexAt = time.Now().Add(-10 * time.Second)
	})
	if err := o.ReconcileStalledRuns(context.Background(), 0, 0); err != nil {
		t.Fatalf("ReconcileStalledRuns: %v", err)
	}
	select {
	case <-disp.contextAt(0).Done():
		t.Fatal("stall_timeout_ms=0 should disable detection but the worker was cancelled")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestReconcileTrackerIssuesCancelsRunWhenServiceRouteChanges(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "LIN-16", Identifier: "LIN-16", Title: "route", State: "AI Ready", ServiceName: "api"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	active := map[string]tracker.Issue{
		"LIN-16": {ID: "LIN-16", Identifier: "LIN-16", Title: "route", State: "AI Ready", ServiceName: "web"},
	}
	if err := o.ReconcileTrackerIssues(context.Background(), active, normalizedStates([]string{"AI Ready"})); err != nil {
		t.Fatalf("ReconcileTrackerIssues: %v", err)
	}
	select {
	case <-disp.contexts[0].Done():
	case <-time.After(time.Second):
		t.Fatal("route-changed worker context was not canceled")
	}

	if err := o.RequestDispatch(context.Background(), active["LIN-16"], nil); err == nil {
		t.Fatal("RequestDispatch while canceled worker is exiting returned nil error, want duplicate guard to keep old run claimed")
	}
	if got := disp.count(); got != 1 {
		t.Fatalf("dispatch count while canceled worker is exiting = %d, want duplicate guard to suppress new worker", got)
	}

	disp.finishAt(0, WorkerResult{Err: context.Canceled})
	waitFor(t, func() bool {
		view, err := o.Snapshot(context.Background())
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		return len(view.Running) == 0
	}, time.Second)

	if err := o.RequestDispatch(context.Background(), active["LIN-16"], nil); err != nil {
		t.Fatalf("RequestDispatch after canceled worker exit: %v", err)
	}
	if got := disp.count(); got != 2 {
		t.Fatalf("dispatch count after route change = %d, want new routed worker", got)
	}
	if got := disp.issueAt(1).ServiceName; got != "web" {
		t.Fatalf("redispatched service = %q, want web", got)
	}
}

func TestReconcileTrackerIssuesRefreshesIssueStateForCapacityCounting(t *testing.T) {
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
	if err := o.ReconcileTrackerIssues(context.Background(), active, normalizedStates([]string{"In Progress", "Rework"})); err != nil {
		t.Fatalf("ReconcileTrackerIssues: %v", err)
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
	if err := o.ReconcileTrackerIssues(context.Background(), active, normalizedStates([]string{"In Progress", "Rework"})); err != nil {
		t.Fatalf("ReconcileTrackerIssues: %v", err)
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
	if err := o.ReconcileTrackerIssues(context.Background(), active, normalizedStates([]string{"In Progress", "Rework"})); err != nil {
		t.Fatalf("ReconcileTrackerIssues: %v", err)
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

func TestReconcileTrackerIssuesReleasesRetryWhenServiceRouteChanges(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "LIN-16", Identifier: "LIN-16", Title: "route", State: "AI Ready", ServiceName: "api"}
	if err := o.ScheduleRetry(context.Background(), iss, iss.Identifier, 1, "transient"); err != nil {
		t.Fatalf("ScheduleRetry: %v", err)
	}
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Retrying) != 1 {
		t.Fatalf("retrying entries = %d, want queued retry", len(view.Retrying))
	}

	active := map[string]tracker.Issue{
		"LIN-16": {ID: "LIN-16", Identifier: "LIN-16", Title: "route", State: "AI Ready", ServiceName: "web"},
	}
	if err := o.ReconcileTrackerIssues(context.Background(), active, normalizedStates([]string{"AI Ready"})); err != nil {
		t.Fatalf("ReconcileTrackerIssues: %v", err)
	}
	view, err = o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Retrying) != 0 {
		t.Fatalf("retrying entries after route change = %+v, want released retry", view.Retrying)
	}
}

func TestTrackerRecheckedDispatchDoesNotConsumeFailureRetry(t *testing.T) {
	st := NewOrchestratorState(30000, 1)
	iss := tracker.Issue{ID: "ENG-FAIL-RETRY", Identifier: "ENG-FAIL-RETRY", Title: "failure retry", State: "AI Ready"}
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
		return err == nil && len(view.Retrying) == 1
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

	if err := o.scheduleContinuationRetry(context.Background(), issueA, issueA.Identifier, 1); err != nil {
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
	if len(view.Running) != 1 || view.Running[0].LastCodexAt.IsZero() {
		t.Fatalf("Running LastCodexAt not updated from runtime event: %+v", view.Running)
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

func TestFinalize_NormalExitStopsAfterMaxTurns(t *testing.T) {
	disp := &fakeDispatcher{}
	maxTurns := 2
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Millisecond}},
		MaxTurns:   &maxTurns,
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-CLEAN-BUDGET", Identifier: "ENG-CLEAN-BUDGET", Title: "clean budget"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	disp.finishAt(0, WorkerResult{Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, err := o.Snapshot(context.Background())
		return err == nil && len(v.Retrying) == 1 && v.Retrying[0].Attempt == 1
	}, time.Second)

	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, nil); err != nil {
		t.Fatalf("tracker-rechecked continuation dispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 2 }, time.Second)

	disp.finishAt(1, WorkerResult{Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, err := o.Snapshot(context.Background())
		return err == nil && len(v.Running) == 0 && len(v.Retrying) == 0 && len(v.Failed) == 1
	}, time.Second)

	v, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(v.Retrying) != 0 {
		t.Fatalf("retrying entries after clean budget exhausted = %+v, want none", v.Retrying)
	}
	if got := disp.count(); got != 2 {
		t.Fatalf("Dispatcher.Spawn calls = %d, want no additional continuation after max turns", got)
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

func TestFinalize_AbnormalExitStopsAfterDefaultFailureRetryBudget(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-BUDGET", Identifier: "ENG-BUDGET", Title: "retry budget"}
	attempt := 1
	if err := o.RequestDispatchAfterTrackerRecheck(context.Background(), iss, &attempt); err != nil {
		t.Fatalf("RequestDispatchAfterTrackerRecheck: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	disp.finishAt(0, WorkerResult{Err: errors.New("still failing"), Elapsed: 50 * time.Millisecond})

	waitFor(t, func() bool {
		v, err := o.Snapshot(context.Background())
		return err == nil && len(v.Running) == 0 && len(v.Retrying) == 0
	}, time.Second)
	v, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(v.Retrying) != 0 {
		t.Fatalf("retrying entries after exhausted budget = %+v, want none", v.Retrying)
	}
	if got := disp.count(); got != 1 {
		t.Fatalf("Dispatcher.Spawn calls = %d, want no additional retry after exhausted budget", got)
	}
}

func TestFinalize_InputRequiredExitBlocksIssueWithoutRetry(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  RetryScheduler{MaxBackoff: time.Minute},
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-INPUT", Identifier: "ENG-INPUT", State: "AI Ready", Title: "needs input"}
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

	iss := tracker.Issue{ID: "ENG-NORMAL-BLOCK", Identifier: "ENG-NORMAL-BLOCK", State: "AI Ready", Title: "needs input"}
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
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  RetryScheduler{MaxBackoff: time.Minute},
	})
	defer cancel()

	active := tracker.Issue{ID: "ENG-BLOCK-A", Identifier: "ENG-BLOCK-A", State: "AI Ready", Title: "active block"}
	inactive := tracker.Issue{ID: "ENG-BLOCK-B", Identifier: "ENG-BLOCK-B", State: "AI Ready", Title: "inactive block"}
	terminalWorkspace := t.TempDir()
	for _, iss := range []tracker.Issue{active, inactive} {
		if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
			t.Fatalf("RequestDispatch %s: %v", iss.ID, err)
		}
	}
	waitFor(t, func() bool { return disp.count() == 2 }, time.Second)
	if err := o.RecordWorkspace(context.Background(), active.ID, Workspace{Path: terminalWorkspace}); err != nil {
		t.Fatalf("RecordWorkspace: %v", err)
	}
	for i, iss := range []tracker.Issue{active, inactive} {
		if err := o.RecordRuntimeEvent(context.Background(), iss.ID, task.RuntimeEvent{
			Event:   task.EventTurnInputRequired,
			Payload: map[string]any{"method": "item/tool/requestUserInput"},
		}); err != nil {
			t.Fatalf("RecordRuntimeEvent %s: %v", iss.ID, err)
		}
		disp.finishAt(i, WorkerResult{Err: errors.New("input required"), Elapsed: time.Millisecond})
	}
	waitFor(t, func() bool {
		view, _ := o.Snapshot(context.Background())
		return len(view.Blocked) == 2
	}, time.Second)

	activeStates := map[string]struct{}{"ai ready": {}, "rework": {}}
	if err := o.ReconcileTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		active.ID: {ID: active.ID, Identifier: active.Identifier, State: "Rework", Title: active.Title},
	}, activeStates, time.Second); err != nil {
		t.Fatalf("ReconcileTrackerIssuesAndWait: %v", err)
	}
	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Blocked) != 1 || view.Blocked[0].IssueID != IssueID(active.ID) || view.Blocked[0].State != "Rework" {
		t.Fatalf("blocked after active reconcile = %+v, want refreshed active block only", view.Blocked)
	}
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		active.ID: {ID: active.ID, Identifier: active.Identifier, State: "Done", Title: active.Title},
	}, time.Second); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}
	view, err = o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Blocked) != 0 || len(view.Running) != 0 || len(view.Retrying) != 0 {
		t.Fatalf("state after terminal blocked release = running %+v retrying %+v blocked %+v", view.Running, view.Retrying, view.Blocked)
	}
	if _, err := os.Stat(terminalWorkspace); !os.IsNotExist(err) {
		t.Fatalf("terminal blocked workspace should be removed, stat err=%v", err)
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
	continuationIssue := tracker.Issue{ID: string(issueID), Identifier: "ENG-KIND", State: "AI Ready", Title: "old continuation"}
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
	continuationIssue := tracker.Issue{ID: string(issueID), Identifier: "ENG-KIND", State: "AI Ready", Title: "new continuation"}
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

// TestRetryFire_CapacityDeferralUsesShortRecheckDelay is a regression for
// capacity-blocked retries being re-enqueued through the normal scheduler
// backoff. Temporary capacity pressure should recheck soon, not wait a full
// retry interval while the issue stays claimed and poll ticks cannot help it.
func TestRetryFire_CapacityDeferralUsesShortRecheckDelay(t *testing.T) {
	disp := &fakeDispatcher{}
	st := NewOrchestratorState(15000, 1)
	o := New(st, Deps{
		Dispatcher: disp,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{50 * time.Millisecond}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	issueA := tracker.Issue{ID: "ENG-A", Identifier: "ENG-A", Title: "retry soon"}
	issueB := tracker.Issue{ID: "ENG-B", Identifier: "ENG-B", Title: "running now"}
	if err := o.RequestDispatch(context.Background(), issueA, nil); err != nil {
		t.Fatalf("dispatch A: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)
	disp.finishAt(0, WorkerResult{Err: errors.New("transient"), Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Retrying) == 1
	}, time.Second)

	if err := o.RequestDispatch(context.Background(), issueB, nil); err != nil {
		t.Fatalf("dispatch B while A is retrying: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 2 }, time.Second)

	// Let A's retry timer fire while B occupies the only slot; then free B.
	time.Sleep(150 * time.Millisecond)
	disp.finishAt(1, WorkerResult{Err: nil, Elapsed: time.Millisecond})

	waitFor(t, func() bool { return disp.count() == 3 }, time.Second)
	v, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(v.Running) != 1 || v.Running[0].IssueID != IssueID(issueA.ID) {
		t.Fatalf("running entries after capacity frees = %+v, want retry issue A re-dispatched by short recheck", v.Running)
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
	workerCtx := disp.contextAt(0)

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

	iss := tracker.Issue{ID: "ENG-GONE", Identifier: "ENG-GONE", Title: "absent", State: "AI Ready"}
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

	iss := tracker.Issue{ID: "ENG-FETCH-FAIL", Identifier: "ENG-FETCH-FAIL", Title: "fetch err", State: "AI Ready"}
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
	stale.State = "AI Ready"
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

	iss := tracker.Issue{ID: "ENG-TIMEOUT", Identifier: "ENG-TIMEOUT", Title: "hang", State: "AI Ready"}
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

// routingFilteringCandidateLister rejects issues whose ID does not match
// the allowed set, modelling the SPEC §16.6 retry-fire candidate fetch
// after selectRoutedCandidates has stripped issues that have since routed
// to another service. The routed wrapper installed by RuntimePoller does
// the real filtering; this lister just lets the actor-level test assert
// that a "routed away" issue surfaces as absent rather than dispatched.
type routingFilteringCandidateLister struct {
	mu      sync.Mutex
	allowed map[string]tracker.Issue
}

func (l *routingFilteringCandidateLister) ListActiveIssues(_ context.Context) ([]tracker.Issue, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]tracker.Issue, 0, len(l.allowed))
	for _, iss := range l.allowed {
		out = append(out, iss)
	}
	return out, nil
}

// TestRetryFire_RoutedAwayIssueReleasesClaim asserts the SPEC §16.6
// candidate-fetch contract holds when the lister applies service routing
// on top of the active-state filter: an issue that is still in an active
// state but has routed to another service must look absent to the retry
// timer (no dispatch, claim released) instead of being dispatched against
// a workflow that no longer owns it. The companion runtime-poller wiring
// (routedActiveIssueLister) is what makes this fall out in production;
// this test pins the actor-side contract so a future refactor cannot
// silently regress it.
func TestRetryFire_RoutedAwayIssueReleasesClaim(t *testing.T) {
	disp := &fakeDispatcher{}
	// The retry was scheduled when the issue still matched this workflow's
	// service route; by the time the timer fires the route has changed,
	// so the lister no longer surfaces it.
	lister := &routingFilteringCandidateLister{allowed: map[string]tracker.Issue{}}
	o, cancel := startActor(t, Deps{
		Dispatcher:      disp,
		Scheduler:       &sequenceScheduler{delays: []time.Duration{time.Hour}},
		CandidateLister: lister,
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-ROUTED", Identifier: "ENG-ROUTED", Title: "moved", State: "AI Ready", ServiceName: "old-service"}
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
		return len(v.Retrying) == 0
	}, 2*time.Second)

	v, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot after fire: %v", err)
	}
	if got := disp.count(); got != 0 {
		t.Fatalf("Dispatcher.Spawn calls = %d, want 0; routed-away issue must not be dispatched", got)
	}
	if len(v.Retrying) != 0 {
		t.Fatalf("retrying view after routed-away fetch = %+v, want claim released", v.Retrying)
	}
}
