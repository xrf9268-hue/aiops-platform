package orchestrator

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	st := NewOrchestratorState(15000, 4)
	o := New(st, deps)
	ctx, cancel := context.WithCancel(context.Background())
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}
	return o, cancel
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
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: FixedDelayScheduler{Delay: time.Minute}})
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
		Scheduler:  FixedDelayScheduler{Delay: 5 * time.Millisecond},
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

// TestRequestDispatch_DedupesAgainstRunning verifies that once a
// dispatch is accepted and Running, a second RequestDispatch for the
// same issue is denied — even though the Claimed window between
// dispatchOp.apply and registerRunningOp could in principle be racy.
// The spawn helper's submit-then-spawn ordering is what closes that
// window; this test pins the behavior so a future refactor cannot
// silently regress it.
func TestRequestDispatch_DedupesAgainstRunning(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: FixedDelayScheduler{Delay: time.Minute}})
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

// TestFinalize_NormalExitMarksCompletedNoRetry covers the §7.3 normal
// exit branch end-to-end through the actor: dispatch, the dispatcher
// returns a successful WorkerResult, the finalize op moves the entry
// into Completed and does not schedule a retry.
func TestFinalize_NormalExitMarksCompletedNoRetry(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: FixedDelayScheduler{Delay: time.Minute}})
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
		return len(v.Running) == 0 && len(v.Completed) == 1 && len(v.Retrying) == 0
	}, time.Second)
}

// TestFinalize_AbnormalExitSchedulesRetry covers the §7.3 abnormal exit
// branch: the dispatcher reports an error, finalize records elapsed
// without marking Completed, and schedules a retry through the actor.
// The retry's attempt is 1 (first retry of a first run, SPEC §4.1.5).
func TestFinalize_AbnormalExitSchedulesRetry(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  FixedDelayScheduler{Delay: 250 * time.Millisecond},
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

// TestApply_FollowupRunsOffActorAndCanResubmit pins the design's
// "apply must not block on the ops channel" invariant: a followup
// returned by apply runs on a fresh goroutine, so it can submit
// further ops without deadlocking against the actor. If a future
// refactor inlined the followup call into the actor loop, this test
// would deadlock and time out.
func TestApply_FollowupRunsOffActorAndCanResubmit(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: FixedDelayScheduler{Delay: time.Minute}})
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
	o := New(st, Deps{Dispatcher: disp, Scheduler: FixedDelayScheduler{Delay: time.Minute}})

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
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: FixedDelayScheduler{Delay: time.Minute}})
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

// TestFixedDelayScheduler_NextDelay is a one-line guard so the legacy
// 60-second behavior the PR 4 cutover preserves cannot drift silently.
func TestFixedDelayScheduler_NextDelay(t *testing.T) {
	s := FixedDelayScheduler{Delay: 60 * time.Second}
	for attempt := 1; attempt <= 5; attempt++ {
		if got := s.NextDelay(attempt); got != 60*time.Second {
			t.Errorf("NextDelay(%d) = %v, want 60s", attempt, got)
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
