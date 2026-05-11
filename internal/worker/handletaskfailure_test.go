package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// fakeFailingStore mocks the queue.Store subset handleTaskFailure
// uses. Tests script Fail/FailTimeout return values to drive the
// terminality decision the worker hands to OnFailure.
type fakeFailingStore struct {
	failResult      bool
	failErr         error
	failTimeoutReq  bool
	failTimeoutErr  error
	failCalls       []failCall
	failTimoutCalls []failTimeoutCall
}

type failCall struct {
	ID, Msg string
}

type failTimeoutCall struct {
	ID, Msg           string
	MaxTimeoutRetries int
}

func (f *fakeFailingStore) Fail(_ context.Context, id, msg string) (bool, error) {
	f.failCalls = append(f.failCalls, failCall{ID: id, Msg: msg})
	return f.failResult, f.failErr
}

func (f *fakeFailingStore) FailTimeout(_ context.Context, id, msg string, max int) (bool, error) {
	f.failTimoutCalls = append(f.failTimoutCalls, failTimeoutCall{ID: id, Msg: msg, MaxTimeoutRetries: max})
	return f.failTimeoutReq, f.failTimeoutErr
}

func sampleTask() task.Task {
	return task.Task{ID: "tsk_failtest"}
}

// TestHandleTaskFailure_NonTimeoutTerminalReportsTrue locks the
// contract that the queue's Fail returning terminal=true propagates
// back to the caller, which is what gates the OnFailure tracker move.
func TestHandleTaskFailure_NonTimeoutTerminalReportsTrue(t *testing.T) {
	store := &fakeFailingStore{failResult: true}
	got := handleTaskFailure(context.Background(), store, sampleTask(), workflow.Config{}, errors.New("policy violation"))
	if !got {
		t.Fatalf("terminal = false, want true when Fail reports terminal")
	}
	if len(store.failCalls) != 1 {
		t.Fatalf("Fail calls = %d, want 1", len(store.failCalls))
	}
	if len(store.failTimoutCalls) != 0 {
		t.Fatalf("FailTimeout must not run on a non-timeout error; got %d calls", len(store.failTimoutCalls))
	}
}

// TestHandleTaskFailure_NonTimeoutRequeueReportsFalse pins the
// behaviour the spec criterion 3 hinges on: when the queue re-queues
// the task for another attempt, the worker must not announce a
// terminal failure (otherwise OnFailure would move the Linear issue to
// Rework, the poller would re-enqueue, and the original re-queued task
// plus the new poller task would race on the same issue).
func TestHandleTaskFailure_NonTimeoutRequeueReportsFalse(t *testing.T) {
	store := &fakeFailingStore{failResult: false}
	got := handleTaskFailure(context.Background(), store, sampleTask(), workflow.Config{}, errors.New("verify failed"))
	if got {
		t.Fatalf("terminal = true, want false when Fail re-queues the task")
	}
}

// TestHandleTaskFailure_NonTimeoutErrorIsConservative covers the rare
// path where the queue write itself fails. We can't tell whether the
// task is terminal, so we report false and skip the tracker mutation
// — better to leave the Linear issue stale than to falsely announce a
// final failure based on indeterminate queue state.
func TestHandleTaskFailure_NonTimeoutErrorIsConservative(t *testing.T) {
	store := &fakeFailingStore{failErr: errors.New("db down")}
	got := handleTaskFailure(context.Background(), store, sampleTask(), workflow.Config{}, errors.New("boom"))
	if got {
		t.Fatalf("terminal = true, want false on Fail error")
	}
}

// TestHandleTaskFailure_TimeoutRequeueReportsFalse mirrors the
// non-timeout case for the dedicated timeout retry budget. A re-queued
// timeout (within budget) is not terminal; the next attempt reuses the
// same Linear issue without status churn.
func TestHandleTaskFailure_TimeoutRequeueReportsFalse(t *testing.T) {
	store := &fakeFailingStore{failTimeoutReq: true}
	cfg := workflow.Config{}
	terr := &runner.TimeoutError{Timeout: time.Second, Elapsed: 2 * time.Second}
	got := handleTaskFailure(context.Background(), store, sampleTask(), cfg, terr)
	if got {
		t.Fatalf("terminal = true, want false when FailTimeout re-queues")
	}
	if len(store.failCalls) != 0 {
		t.Fatalf("Fail must not run on a timeout error; got %d calls", len(store.failCalls))
	}
	if len(store.failTimoutCalls) != 1 {
		t.Fatalf("FailTimeout calls = %d, want 1", len(store.failTimoutCalls))
	}
}

// TestHandleTaskFailure_TimeoutBudgetExhaustedReportsTrue confirms
// timeouts that exhaust the dedicated retry budget do reach Linear:
// the operator needs the issue surfaced so they can intervene
// (raise the budget, switch runners, etc.) rather than have the
// worker silently drop the failure.
func TestHandleTaskFailure_TimeoutBudgetExhaustedReportsTrue(t *testing.T) {
	store := &fakeFailingStore{failTimeoutReq: false} // permanently failed
	cfg := workflow.Config{}
	terr := &runner.TimeoutError{Timeout: time.Second, Elapsed: 2 * time.Second}
	got := handleTaskFailure(context.Background(), store, sampleTask(), cfg, terr)
	if !got {
		t.Fatalf("terminal = false, want true when FailTimeout exhausts the budget")
	}
}

// TestHandleTaskFailure_TimeoutHonorsConfiguredBudget makes sure the
// configured agent.max_timeout_retries is the value passed to the
// queue, not a hard-coded constant. Regression guard for cases where
// the routing logic could accidentally short-circuit the config plumb.
func TestHandleTaskFailure_TimeoutHonorsConfiguredBudget(t *testing.T) {
	store := &fakeFailingStore{}
	budget := 7
	cfg := workflow.Config{Agent: workflow.AgentConfig{MaxTimeoutRetries: &budget}}
	terr := &runner.TimeoutError{Timeout: time.Second, Elapsed: 2 * time.Second}
	_ = handleTaskFailure(context.Background(), store, sampleTask(), cfg, terr)
	if len(store.failTimoutCalls) != 1 {
		t.Fatalf("FailTimeout calls = %d, want 1", len(store.failTimoutCalls))
	}
	if got := store.failTimoutCalls[0].MaxTimeoutRetries; got != budget {
		t.Fatalf("FailTimeout budget = %d, want %d", got, budget)
	}
}
