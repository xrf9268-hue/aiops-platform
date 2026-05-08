package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
	"github.com/xrf9268-hue/aiops-platform/internal/workspace"
)

type recordedEvent struct {
	Kind    string
	Message string
	Payload any
}

type fakeEmitter struct {
	mu     sync.Mutex
	events []recordedEvent
	err    error
}

func (f *fakeEmitter) AddEvent(_ context.Context, _ string, kind, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, recordedEvent{Kind: kind, Message: msg})
	return f.err
}

func (f *fakeEmitter) AddEventWithPayload(_ context.Context, _ string, kind, msg string, payload any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, recordedEvent{Kind: kind, Message: msg, Payload: payload})
	return f.err
}

// byKind returns the recorded events whose Kind matches.
func (f *fakeEmitter) byKind(kind string) []recordedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedEvent
	for _, e := range f.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestEmitRecordsEventOnFakeEmitter(t *testing.T) {
	ev := &fakeEmitter{}
	emit(context.Background(), ev, "tsk_1", task.EventRunnerStart, "runner started", map[string]any{"model": "mock"})
	if len(ev.events) != 1 {
		t.Fatalf("events = %d, want 1", len(ev.events))
	}
	got := ev.events[0]
	if got.Kind != task.EventRunnerStart {
		t.Fatalf("kind = %q, want %q", got.Kind, task.EventRunnerStart)
	}
	// Payload must round-trip through JSON since the queue store stores it as jsonb.
	b, err := json.Marshal(got.Payload)
	if err != nil {
		t.Fatalf("payload marshal error: %v", err)
	}
	if !strings.Contains(string(b), `"model":"mock"`) {
		t.Fatalf("payload JSON = %s, want model=mock", b)
	}
}

func TestEmitNilEmitterIsNoop(t *testing.T) {
	// Should not panic when worker is started without an emitter (e.g. tests).
	emit(context.Background(), nil, "tsk_1", task.EventRunnerStart, "ignored", nil)
}

func TestEmitLogsEmitterError(t *testing.T) {
	ev := &fakeEmitter{err: errors.New("db down")}
	emit(context.Background(), ev, "tsk_1", task.EventPush, "push", nil)
	if len(ev.events) != 1 {
		t.Fatalf("event should still be recorded by fake even when error returned")
	}
}

func TestErrSummaryTruncatesLongMessages(t *testing.T) {
	if errSummary(nil) != "" {
		t.Fatalf("nil error should map to empty string")
	}
	long := strings.Repeat("x", 600)
	got := errSummary(errors.New(long))
	if len(got) > 600 {
		t.Fatalf("errSummary did not truncate: len=%d", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("truncated message should end with ellipsis, got %q", got[len(got)-10:])
	}
}

func TestSummarizeVerifyResultsIncludesError(t *testing.T) {
	results := []workspace.VerifyResult{
		{Command: "go test", ExitCode: 0, Duration: 10 * time.Millisecond},
		{Command: "make lint", ExitCode: 2, Err: errors.New("lint failed"), Duration: 5 * time.Millisecond},
	}
	got := summarizeVerifyResults(results)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0]["command"] != "go test" || got[0]["exit_code"] != 0 {
		t.Fatalf("entry 0 = %+v", got[0])
	}
	if got[1]["error"] != "lint failed" {
		t.Fatalf("entry 1 should propagate error, got %+v", got[1])
	}
	// Round-trip JSON to ensure it is a valid jsonb payload.
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("verify summary JSON: %v", err)
	}
}

func TestRunSummaryContainsKeyFields(t *testing.T) {
	t1 := task.Task{ID: "tsk_99", Title: "fix bug", Actor: "octo", Model: "claude", BaseBranch: "main", WorkBranch: "ai/tsk_99"}
	res := runner.Result{Summary: "claude completed"}
	changed := []string{"a.go", "b.go"}
	verify := []workspace.VerifyResult{{Command: "go test", ExitCode: 0, Duration: time.Millisecond}}
	out := runSummary(t1, res, changed, verify)
	for _, want := range []string{"tsk_99", "fix bug", "claude completed", "main -> ai/tsk_99", "Changed files: 2", "a.go", "go test"} {
		if !strings.Contains(out, want) {
			t.Fatalf("run summary missing %q:\n%s", want, out)
		}
	}
}

func TestEventKindConstantsAreSnakeCase(t *testing.T) {
	required := []string{
		task.EventRunnerStart,
		task.EventRunnerEnd,
		task.EventRunnerTimeout,
		task.EventVerifyStart,
		task.EventVerifyEnd,
		task.EventPush,
		task.EventPRCreated,
	}
	for _, kind := range required {
		if kind == "" {
			t.Fatalf("event kind constant is empty")
		}
		if strings.ToLower(kind) != kind {
			t.Fatalf("event kind %q must be lowercase snake_case", kind)
		}
	}
}

// stubRunner lets tests control the runner's outcome (sleep + final
// error) without invoking a subprocess.
type stubRunner struct {
	sleep time.Duration
	err   error
}

func (s stubRunner) Run(ctx context.Context, _ runner.RunInput) (runner.Result, error) {
	if s.sleep > 0 {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return runner.Result{}, &runner.TimeoutError{
					Timeout: 25 * time.Millisecond,
					Elapsed: 25 * time.Millisecond,
					Cause:   ctx.Err(),
				}
			}
			return runner.Result{}, ctx.Err()
		case <-time.After(s.sleep):
		}
	}
	return runner.Result{Summary: "ok"}, s.err
}

// payloadField round-trips a recorded event payload through JSON and
// returns the value at key. Centralising this here keeps the
// runRunnerWithTimeout assertions resilient to the eventEmitter accepting
// `any` payload (map[string]any in this code path).
func payloadField(t *testing.T, p any, key string) any {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return m[key]
}

// TestRunRunnerWithTimeoutEmitsTimeoutEvent confirms that a runner
// killed by the per-task timeout produces exactly one runner_start +
// one runner_timeout event (and no runner_end), with payload fields the
// debug API can rely on.
func TestRunRunnerWithTimeoutEmitsTimeoutEvent(t *testing.T) {
	t.Parallel()
	ev := &fakeEmitter{}
	in := runner.RunInput{
		Task:     task.Task{ID: "tsk_to", Model: "mock"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	}
	_, err := runRunnerWithTimeout(context.Background(), ev, stubRunner{sleep: 5 * time.Second}, in, 30*time.Millisecond)
	if !runner.IsTimeout(err) {
		t.Fatalf("expected TimeoutError from runRunnerWithTimeout, got %v", err)
	}
	if got := len(ev.byKind(task.EventRunnerStart)); got != 1 {
		t.Fatalf("runner_start count: got=%d want=1", got)
	}
	if got := len(ev.byKind(task.EventRunnerTimeout)); got != 1 {
		t.Fatalf("runner_timeout count: got=%d want=1", got)
	}
	if got := len(ev.byKind(task.EventRunnerEnd)); got != 0 {
		t.Fatalf("runner_end must not fire on timeout, got=%d", got)
	}

	// Payload sanity: timeout_ms and elapsed_ms must be present.
	pe := ev.byKind(task.EventRunnerTimeout)[0]
	if got := payloadField(t, pe.Payload, "timeout_ms"); got == nil {
		t.Fatal("runner_timeout payload missing timeout_ms")
	}
	if got := payloadField(t, pe.Payload, "elapsed_ms"); got == nil {
		t.Fatal("runner_timeout payload missing elapsed_ms")
	}
}

// TestRunRunnerWithTimeoutHappyPath emits runner_start + runner_end
// (with ok=true) when the runner completes within budget.
func TestRunRunnerWithTimeoutHappyPath(t *testing.T) {
	t.Parallel()
	ev := &fakeEmitter{}
	in := runner.RunInput{
		Task:     task.Task{ID: "tsk_ok", Model: "mock"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	}
	if _, err := runRunnerWithTimeout(context.Background(), ev, stubRunner{}, in, time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(ev.byKind(task.EventRunnerStart)); got != 1 {
		t.Fatalf("runner_start count: got=%d want=1", got)
	}
	if got := len(ev.byKind(task.EventRunnerEnd)); got != 1 {
		t.Fatalf("runner_end count: got=%d want=1", got)
	}
	if got := len(ev.byKind(task.EventRunnerTimeout)); got != 0 {
		t.Fatalf("runner_timeout must not fire on success, got=%d", got)
	}
	pe := ev.byKind(task.EventRunnerEnd)[0]
	ok, _ := payloadField(t, pe.Payload, "ok").(bool)
	if !ok {
		t.Fatalf("runner_end payload ok=true expected, got %v", payloadField(t, pe.Payload, "ok"))
	}
}

// TestRunRunnerWithTimeoutNonTimeoutError keeps verify-vs-timeout
// retry buckets disjoint: a generic runner error must surface as
// runner_end with ok=false (not runner_timeout).
func TestRunRunnerWithTimeoutNonTimeoutError(t *testing.T) {
	t.Parallel()
	ev := &fakeEmitter{}
	wantErr := errors.New("agent crashed")
	in := runner.RunInput{
		Task:     task.Task{ID: "tsk_err", Model: "mock"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	}
	_, err := runRunnerWithTimeout(context.Background(), ev, stubRunner{err: wantErr}, in, time.Second)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err propagation broken: got %v want %v", err, wantErr)
	}
	if runner.IsTimeout(err) {
		t.Fatal("non-timeout error must not be classified as timeout")
	}
	if got := len(ev.byKind(task.EventRunnerTimeout)); got != 0 {
		t.Fatalf("runner_timeout must not fire on plain error, got=%d", got)
	}
	if got := len(ev.byKind(task.EventRunnerEnd)); got != 1 {
		t.Fatalf("runner_end count: got=%d want=1", got)
	}
}

// TestRunRunnerWithTimeoutZeroBudgetUsesDefault ensures we never call
// context.WithTimeout(0), which would fire instantly. A zero budget
// should fall back to the schema default (30m).
func TestRunRunnerWithTimeoutZeroBudgetUsesDefault(t *testing.T) {
	t.Parallel()
	ev := &fakeEmitter{}
	in := runner.RunInput{
		Task:     task.Task{ID: "tsk_zero", Model: "mock"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	}
	if _, err := runRunnerWithTimeout(context.Background(), ev, stubRunner{sleep: 10 * time.Millisecond}, in, 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pe := ev.byKind(task.EventRunnerStart)[0]
	got, _ := payloadField(t, pe.Payload, "timeout_ms").(float64)
	want := float64((30 * time.Minute).Milliseconds())
	if got != want {
		t.Fatalf("expected default timeout %v ms, got %v", want, got)
	}
}
