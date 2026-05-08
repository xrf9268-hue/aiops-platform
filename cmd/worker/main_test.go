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
