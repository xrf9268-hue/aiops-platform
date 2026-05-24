package queue_test

// Integration tests for the postgres-backed queue.Store.
//
// These tests require a migrated Postgres database pointed at by
// TEST_DATABASE_URL.  When the variable is absent (e.g. in the standard CI
// "Go build and test" job) TestMain exits 0 immediately so that
// go test -covermode=atomic compiles and exits without invoking covdata on
// a package with no runnable tests.
//
// Run locally:
//   TEST_DATABASE_URL="postgres://..." go test -race ./internal/queue/...

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xrf9268-hue/aiops-platform/internal/queue"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

var (
	testDB   *queue.Store
	testPool *pgxpool.Pool
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		// No DB configured — skip all integration tests.  The binary still
		// compiles and exits 0 so go test -covermode=atomic does not look up
		// the covdata tool on a package with no runnable tests.
		os.Exit(0)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pgxpool.New:", err)
		os.Exit(1)
	}
	defer pool.Close()
	testPool = pool
	testDB = queue.New(pool)
	os.Exit(m.Run())
}

// uniqueID returns a collision-resistant source_event_id for test isolation.
func uniqueID(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}

// minimalTask returns a Task with the minimum fields needed to enqueue.
func minimalTask(eventID string) task.Task {
	return task.Task{
		SourceType:    "test",
		SourceEventID: eventID,
		RepoOwner:     "test-owner",
		RepoName:      "test-repo",
		CloneURL:      "https://example.com/test-repo.git",
		BaseBranch:    "main",
		Title:         "test: " + eventID,
		MaxAttempts:   2,
	}
}

// TestEnqueueDefaults verifies that Enqueue populates default fields when
// the caller leaves them zero: ID, WorkBranch, Model, Priority, MaxAttempts.
func TestEnqueueDefaults(t *testing.T) {
	ctx := context.Background()

	in := task.Task{
		SourceType:    "test",
		SourceEventID: uniqueID("defaults"),
		RepoOwner:     "o",
		RepoName:      "r",
		CloneURL:      "https://example.com/r.git",
		BaseBranch:    "main",
		Title:         "defaults test",
		// ID, WorkBranch, Model, Priority, MaxAttempts all zero/empty
	}
	out, deduped, err := testDB.Enqueue(ctx, in)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if deduped {
		t.Fatal("first enqueue should not be deduped")
	}
	// Clean up so the queued row does not interfere with later tests that call
	// Claim (e.g. TestFIFO, TestClaimEmpty, TestFailRetryAndTerminal).
	t.Cleanup(func() { _ = testDB.Complete(context.Background(), out.ID) })
	if out.ID == "" {
		t.Error("ID should be populated by Enqueue")
	}
	if out.WorkBranch == "" {
		t.Error("WorkBranch should be populated by Enqueue")
	}
	if out.Model == "" {
		t.Error("Model should be populated by Enqueue")
	}
	if out.Priority == 0 {
		t.Error("Priority should be populated by Enqueue")
	}
	if out.MaxAttempts == 0 {
		t.Error("MaxAttempts should be populated by Enqueue")
	}
}

// TestEnqueueDeduplication verifies the ON CONFLICT idempotency: enqueueing
// the same (source_type, source_event_id) twice returns deduped=true the
// second time and does not create a second row.
func TestEnqueueDeduplication(t *testing.T) {
	ctx := context.Background()

	t1 := minimalTask(uniqueID("dedup"))
	_, dup1, err := testDB.Enqueue(ctx, t1)
	if err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	if dup1 {
		t.Fatal("first enqueue: deduped=true, want false")
	}

	_, dup2, err := testDB.Enqueue(ctx, t1)
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}
	if !dup2 {
		t.Fatal("second enqueue with same source_event_id: deduped=false, want true")
	}
}

// TestClaimEmpty verifies the empty-dequeue invariant: Claim returns nil
// without error when no queued tasks are available.
//
// To avoid any side effects on non-test rows in a shared database, we check
// for non-test queued rows with a read-only count query before calling Claim.
// If non-test rows exist we skip rather than risk claiming and mutating them.
// Test-owned rows are drained via a targeted UPDATE that does not go through
// Claim (so no running-state transition occurs on test fixtures).
func TestClaimEmpty(t *testing.T) {
	ctx := context.Background()

	// Drain only test-owned queued rows without calling Claim (no state
	// transition side effects on test rows beyond marking them succeeded).
	if _, err := testPool.Exec(ctx,
		"UPDATE tasks SET status='succeeded', updated_at=now() WHERE source_type='test' AND status='queued'"); err != nil {
		t.Fatalf("drain test tasks: %v", err)
	}

	// Check for non-test queued rows without claiming them.  If any exist
	// we cannot enforce the empty-dequeue invariant in this shared DB.
	var nonTestCount int
	if err := testPool.QueryRow(ctx,
		"SELECT count(*) FROM tasks WHERE source_type != 'test' AND status = 'queued'",
	).Scan(&nonTestCount); err != nil {
		t.Fatalf("count non-test queued tasks: %v", err)
	}
	if nonTestCount > 0 {
		t.Skipf("non-test queued tasks exist (count=%d); run with an isolated DB schema to validate empty-dequeue invariant", nonTestCount)
	}

	// Queue contains only test-owned rows and we just drained them; Claim
	// must return nil.
	got, err := testDB.Claim(ctx)
	if err != nil {
		t.Fatalf("Claim on empty queue: %v", err)
	}
	if got != nil {
		t.Errorf("Claim on empty queue returned id=%s source_type=%s, want nil", got.ID, got.SourceType)
	}
}

// TestFIFO verifies that Claim returns tasks in priority-DESC, created_at-ASC
// order.  Two tasks with different priorities are enqueued; the higher-priority
// task must be claimed first.
func TestFIFO(t *testing.T) {
	ctx := context.Background()

	low := minimalTask(uniqueID("fifo_low"))
	low.Priority = 10
	outLow, _, err := testDB.Enqueue(ctx, low)
	if err != nil {
		t.Fatalf("Enqueue low-priority: %v", err)
	}

	high := minimalTask(uniqueID("fifo_high"))
	high.Priority = 90
	outHigh, _, err := testDB.Enqueue(ctx, high)
	if err != nil {
		t.Fatalf("Enqueue high-priority: %v", err)
	}

	first, err := testDB.Claim(ctx)
	if err != nil {
		t.Fatalf("Claim first: %v", err)
	}
	if first == nil {
		t.Fatal("Claim first: got nil, want high-priority task")
	}
	if first.ID != outHigh.ID {
		t.Errorf("Claim first = id=%s priority=%d, want id=%s priority=%d",
			first.ID, first.Priority, outHigh.ID, outHigh.Priority)
	}

	second, err := testDB.Claim(ctx)
	if err != nil {
		t.Fatalf("Claim second: %v", err)
	}
	if second == nil {
		t.Fatal("Claim second: got nil, want low-priority task")
	}
	if second.ID != outLow.ID {
		t.Errorf("Claim second = id=%s priority=%d, want id=%s priority=%d",
			second.ID, second.Priority, outLow.ID, outLow.Priority)
	}

	// Cleanup: mark both succeeded so they do not affect subsequent tests.
	_ = testDB.Complete(ctx, outHigh.ID)
	_ = testDB.Complete(ctx, outLow.ID)
}

// TestFailRetryAndTerminal verifies the retry-budget invariant:
//   - Fail while attempts < max_attempts → task is re-queued (terminal=false).
//   - Fail while attempts >= max_attempts → task is permanently failed (terminal=true).
func TestFailRetryAndTerminal(t *testing.T) {
	ctx := context.Background()

	t1 := minimalTask(uniqueID("retry"))
	t1.MaxAttempts = 2
	out, _, err := testDB.Enqueue(ctx, t1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Claim attempt 1.
	claimed, err := testDB.Claim(ctx)
	if err != nil || claimed == nil {
		t.Fatalf("Claim attempt 1: err=%v task=%v", err, claimed)
	}
	if claimed.ID != out.ID {
		// Another task was claimed; fail it back to re-queue so it is not
		// stranded in 'running', then skip.
		_, _ = testDB.Fail(ctx, claimed.ID, "test collision cleanup")
		t.Skipf("unexpected task claimed (id=%s); shared-DB collision — re-run with isolated schema", claimed.ID)
	}

	// First failure: attempts(1) < max_attempts(2) → re-queued.
	terminal, err := testDB.Fail(ctx, out.ID, "attempt 1 error")
	if err != nil {
		t.Fatalf("Fail attempt 1: %v", err)
	}
	if terminal {
		t.Error("Fail attempt 1: terminal=true, want false (task should be re-queued)")
	}

	// Fail sets available_at = now()+60s.  Reset it so Claim picks the task
	// up immediately without the test waiting 60 seconds.
	if _, err := testPool.Exec(ctx,
		"UPDATE tasks SET available_at=now() WHERE id=$1", out.ID); err != nil {
		t.Fatalf("reset available_at: %v", err)
	}

	// Claim attempt 2.
	claimed2, err := testDB.Claim(ctx)
	if err != nil || claimed2 == nil {
		t.Fatalf("Claim attempt 2: err=%v task=%v", err, claimed2)
	}
	if claimed2.ID != out.ID {
		// Fail the unexpected task back to re-queue so it is not left in 'running'.
		_, _ = testDB.Fail(ctx, claimed2.ID, "test collision cleanup")
		t.Skipf("unexpected task claimed on attempt 2 (id=%s); shared-DB collision", claimed2.ID)
	}

	// Second failure: attempts(2) >= max_attempts(2) → terminal.
	terminal2, err := testDB.Fail(ctx, out.ID, "attempt 2 error")
	if err != nil {
		t.Fatalf("Fail attempt 2: %v", err)
	}
	if !terminal2 {
		t.Error("Fail attempt 2: terminal=false, want true (task should be permanently failed)")
	}

	// Verify the task row reached 'failed' status.
	got, err := testDB.GetTask(ctx, out.ID)
	if err != nil {
		t.Fatalf("GetTask after terminal fail: %v", err)
	}
	if got.Status != task.StatusFailed {
		t.Errorf("status = %s, want %s", got.Status, task.StatusFailed)
	}
}
