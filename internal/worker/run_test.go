package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	worker "github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
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
	worker.Emit(context.Background(), ev, "tsk_1", "", task.EventRunnerStart, "runner started", map[string]any{"model": "mock"})
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
	worker.Emit(context.Background(), nil, "tsk_1", "", task.EventRunnerStart, "ignored", nil)
}

func TestEmitLogsEmitterError(t *testing.T) {
	ev := &fakeEmitter{err: errors.New("db down")}
	worker.Emit(context.Background(), ev, "tsk_1", "", task.EventRunnerStart, "runner_start", nil)
	if len(ev.events) != 1 {
		t.Fatalf("event should still be recorded by fake even when error returned")
	}
}

func TestErrSummaryTruncatesLongMessages(t *testing.T) {
	if worker.ErrSummary(nil) != "" {
		t.Fatalf("nil error should map to empty string")
	}
	long := strings.Repeat("x", 600)
	got := worker.ErrSummary(errors.New(long))
	if len(got) > 600 {
		t.Fatalf("errSummary did not truncate: len=%d", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("truncated message should end with ellipsis, got %q", got[len(got)-10:])
	}
}

// TestRunTaskUsesConfiguredServiceWorkflowInsteadOfRepoWorkflow verifies the
// worker runs the startup-selected service workflow and its agent default, not
// a WORKFLOW.md committed inside the cloned repo.
func TestRunTaskUsesConfiguredServiceWorkflowInsteadOfRepoWorkflow(t *testing.T) {
	cloneURL, tk := initBareUpstreamWithWorkflow(t, `---
repo:
  owner: acme
  name: demo
  clone_url: $REPO_URL
tracker:
  kind: linear
  project_slug: platform
agent:
  default: definitely-not-a-runner
---
repo prompt should not run
`)
	t.Setenv("REPO_URL", cloneURL)

	serviceWorkflow := workflow.Workflow{
		Config:         workflow.DefaultConfig(),
		PromptTemplate: "service workflow for {{task.title}}",
		Source:         workflow.SourceFile,
		Path:           filepath.Join(t.TempDir(), "WORKFLOW.md"),
	}
	serviceWorkflow.Config.Agent.Default = "mock"
	serviceWorkflow.Config.Repo.CloneURL = cloneURL
	serviceWorkflow.Config.Repo.Owner = "acme"
	serviceWorkflow.Config.Repo.Name = "demo"

	ev := &fakeEmitter{}
	cfg := workerCfgForIntegration(t)
	cfg.Workflow = &serviceWorkflow
	tk.Model = ""

	if rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg); rterr != nil {
		t.Fatalf("runTask: %v", rterr.Err)
	}

	resolved := ev.byKind(task.EventWorkflowResolved)
	if len(resolved) != 1 {
		t.Fatalf("workflow_resolved events = %d, want 1; events=%#v", len(resolved), ev.events)
	}
	if got := payloadField(t, resolved[0].Payload, "path"); got != serviceWorkflow.Path {
		t.Fatalf("workflow path payload = %v, want service workflow path %q", got, serviceWorkflow.Path)
	}
	if got := payloadField(t, resolved[0].Payload, "agent_default"); got != "mock" {
		t.Fatalf("agent_default payload = %v, want service workflow default", got)
	}
}

// TestAppendVerifyDirective pins the SPEC §1 verify hand-off contract: the
// operator-declared verify.commands are surfaced to the agent's prompt (the
// worker no longer runs them), joined by "; ", appended exactly once, and a
// no-op when no commands are configured.
func TestAppendVerifyDirective(t *testing.T) {
	const marker = "**Verification (you own this):**"
	plain := "do the work"
	cmds := []string{"go build ./...", "go test ./..."}

	out := worker.AppendVerifyDirective(plain, cmds)
	if !strings.Contains(out, marker) {
		t.Fatalf("AppendVerifyDirective(%q, %v) = %q; want directive marker present", plain, cmds, out)
	}
	if !strings.Contains(out, "go build ./...; go test ./...") {
		t.Fatalf("AppendVerifyDirective(%q, %v) = %q; want commands joined by \"; \"", plain, cmds, out)
	}
	if got := strings.Count(out, marker); got != 1 {
		t.Fatalf("AppendVerifyDirective(%q, %v) marker count = %d; want 1", plain, cmds, got)
	}

	// No commands configured: the prompt is returned unchanged.
	if got := worker.AppendVerifyDirective(plain, nil); got != plain {
		t.Fatalf("AppendVerifyDirective(%q, nil) = %q; want unchanged %q", plain, got, plain)
	}

	// All-whitespace commands filter to empty and must also be a no-op.
	if got := worker.AppendVerifyDirective(plain, []string{"  ", "\t"}); got != plain {
		t.Fatalf("AppendVerifyDirective(%q, [whitespace]) = %q; want unchanged %q", plain, got, plain)
	}

	// Idempotent: a second call when the marker is already present is a no-op.
	if gotAgain := worker.AppendVerifyDirective(out, cmds); gotAgain != out {
		t.Fatalf("AppendVerifyDirective(<already has directive>, %v) = %q; want unchanged %q", cmds, gotAgain, out)
	}
}

func TestAppendAnalysisOnlyDirectiveOnlyForAnalysisMode(t *testing.T) {
	plain := "inspect the issue"
	got := worker.AppendAnalysisOnlyDirective(plain, "analysis_only")
	for _, want := range []string{"Analysis-only mode", ".aiops/PLAN.md", "do not edit source files"} {
		if !strings.Contains(got, want) {
			t.Fatalf("analysis-only directive missing %q: %q", want, got)
		}
	}
	if again := worker.AppendAnalysisOnlyDirective(got, "analysis_only"); again != got {
		t.Fatalf("analysis-only directive should be idempotent; got %q", again)
	}
	if draft := worker.AppendAnalysisOnlyDirective(plain, "draft_pr"); draft != plain {
		t.Fatalf("draft_pr prompt should not receive analysis-only directive: %q", draft)
	}
}

func TestEventKindConstantsAreSnakeCase(t *testing.T) {
	required := []string{
		task.EventRunnerStart,
		task.EventRunnerEnd,
		task.EventRunnerTimeout,
		task.EventPRReused,
		task.EventTrackerTransition,
		task.EventTrackerTransitionError,
		task.EventTrackerComment,
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
	sleep      time.Duration
	err        error
	respectCtx bool
	result     runner.Result
}

func (s stubRunner) Run(ctx context.Context, _ runner.RunInput) (runner.Result, error) {
	result := s.result
	if result.Summary == "" && len(result.RuntimeEvents) == 0 && result.IssueExitState == nil && result.OutputBytes == 0 && result.OutputDropped == 0 && result.OutputHead == "" && result.OutputTail == "" {
		result = runner.Result{Summary: "ok"}
	}
	if s.sleep > 0 {
		if !s.respectCtx {
			time.Sleep(s.sleep)
			return result, s.err
		}
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
	return result, s.err
}

type callbackRunner struct {
	run func(context.Context, runner.RunInput) (runner.Result, error)
}

func (c callbackRunner) Run(ctx context.Context, in runner.RunInput) (runner.Result, error) {
	return c.run(ctx, in)
}

// payloadField round-trips a recorded event payload through JSON and
// returns the value at key. Centralising this here keeps the
// RunRunnerWithTimeout assertions resilient to the EventEmitter accepting
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
	_, err := worker.RunRunnerWithTimeout(context.Background(), ev, stubRunner{sleep: 5 * time.Second, respectCtx: true}, in, 30*time.Millisecond, "file")
	if !runner.IsTimeout(err) {
		t.Fatalf("expected TimeoutError from RunRunnerWithTimeout, got %v", err)
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

// TestRunRunnerWithTimeoutNormalizesDeadlineExceededFromStubbornRunner covers
// runners that return context.DeadlineExceeded directly after their process
// context is canceled. DeadlineExceeded from the worker's own runCtx is still
// a timeout even if the runner does not wrap it in runner.TimeoutError.
func TestRunRunnerWithTimeoutNormalizesDeadlineExceededFromStubbornRunner(t *testing.T) {
	t.Parallel()
	ev := &fakeEmitter{}
	in := runner.RunInput{
		Task:     task.Task{ID: "tsk_deadline", Model: "mock"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	}

	_, err := worker.RunRunnerWithTimeout(context.Background(), ev, stubRunner{sleep: 60 * time.Millisecond, err: context.DeadlineExceeded}, in, 30*time.Millisecond, "file")
	if !runner.IsTimeout(err) {
		t.Fatalf("expected normalized TimeoutError from RunRunnerWithTimeout, got %T %[1]v", err)
	}
	if got := len(ev.byKind(task.EventRunnerTimeout)); got != 1 {
		t.Fatalf("runner_timeout count: got=%d want=1", got)
	}
	if got := len(ev.byKind(task.EventRunnerEnd)); got != 0 {
		t.Fatalf("runner_end must not fire on normalized timeout, got=%d", got)
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
	if _, err := worker.RunRunnerWithTimeout(context.Background(), ev, stubRunner{}, in, time.Second, "file"); err != nil {
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

func TestRunRunnerWithTimeoutPreservesIssueExitStateResult(t *testing.T) {
	t.Parallel()
	ev := &fakeEmitter{}
	in := runner.RunInput{
		Task:     task.Task{ID: "tsk_issue_inactive", Model: "codex-app-server"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	}

	res, err := worker.RunRunnerWithTimeout(context.Background(), ev, stubRunner{
		result: runner.Result{Summary: "ok", IssueExitState: &runner.IssueStateSnapshot{
			Found:  true,
			State:  "In Review",
			Active: false,
		}},
	}, in, time.Second, "file")
	if err != nil {
		t.Fatalf("RunRunnerWithTimeout: %v", err)
	}
	if res.IssueExitState == nil || res.IssueExitState.State != "In Review" {
		t.Fatalf("IssueExitState = %+v, want In Review snapshot", res.IssueExitState)
	}
}

func TestRunRunnerWithTimeoutEmitsSpecPhaseTransitions(t *testing.T) {
	t.Parallel()
	ev := &fakeEmitter{}
	in := runner.RunInput{
		Task:     task.Task{ID: "tsk_phase", Model: "codex-app-server"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	}
	r := callbackRunner{run: func(ctx context.Context, in runner.RunInput) (runner.Result, error) {
		in.PhaseTransitionSink(task.PhaseLaunchingAgentProcess, task.PhaseInitializingSession)
		in.PhaseTransitionSink(task.PhaseInitializingSession, task.PhaseStreamingTurn)
		return runner.Result{Summary: "ok"}, nil
	}}
	if _, err := worker.RunRunnerWithTimeout(context.Background(), ev, r, in, time.Second, "file"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	transitions := ev.byKind(task.EventRunPhaseTransition)
	want := []task.RunAttemptPhase{
		task.PhaseLaunchingAgentProcess,
		task.PhaseInitializingSession,
		task.PhaseStreamingTurn,
		task.PhaseFinishing,
	}
	if len(transitions) != len(want) {
		t.Fatalf("phase transition count = %d, want %d; events=%#v", len(transitions), len(want), ev.events)
	}
	for i, phase := range want {
		if got := payloadField(t, transitions[i].Payload, "to"); got != string(phase) {
			t.Fatalf("transition[%d].to = %#v, want %q; transitions=%#v", i, got, phase, transitions)
		}
	}
}

func TestRunRunnerWithTimeoutForwardsSpecRuntimeEvents(t *testing.T) {
	t.Parallel()
	ev := &fakeEmitter{}
	in := runner.RunInput{
		Task:     task.Task{ID: "tsk_runtime", Model: "codex-app-server"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	}
	runtimeEvents := []task.RuntimeEvent{
		{
			Event: task.EventSessionStarted,
			Payload: map[string]any{
				"thread_id": "thread-1",
				"turn_id":   "turn-1",
			},
		},
		{
			Event: task.EventTurnCompleted,
			Payload: map[string]any{
				"turn_id": "turn-1",
				"usage":   map[string]any{"total_tokens": 3},
			},
		},
	}
	r := stubRunner{result: runner.Result{RuntimeEvents: runtimeEvents}}
	if _, err := worker.RunRunnerWithTimeout(context.Background(), ev, r, in, time.Second, "file"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertForwardedRuntimeEventsOnce(t, ev)
}

func TestRunRunnerWithTimeoutForwardsRuntimeEventsNotAlreadyEmittedThroughSink(t *testing.T) {
	t.Parallel()
	ev := &fakeEmitter{}
	in := runner.RunInput{
		Task:     task.Task{ID: "tsk_runtime_sink", Model: "codex-app-server"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	}
	sinkEvent := task.RuntimeEvent{
		Event: task.EventSessionStarted,
		Payload: map[string]any{
			"thread_id": "thread-1",
			"turn_id":   "turn-1",
		},
	}
	resultOnlyEvent := task.RuntimeEvent{
		Event: task.EventTurnCompleted,
		Payload: map[string]any{
			"turn_id": "turn-1",
			"usage":   map[string]any{"total_tokens": 3},
		},
	}
	r := callbackRunner{run: func(ctx context.Context, in runner.RunInput) (runner.Result, error) {
		in.RuntimeEventSink(sinkEvent)
		return runner.Result{RuntimeEvents: []task.RuntimeEvent{sinkEvent, resultOnlyEvent}}, nil
	}}
	if _, err := worker.RunRunnerWithTimeout(context.Background(), ev, r, in, time.Second, "file"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertForwardedRuntimeEventsOnce(t, ev)
}

func TestRunRunnerWithTimeoutEmitsTerminalErrorPhases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		to   task.RunAttemptPhase
	}{
		{
			name: "stall",
			err:  &runner.StallError{Timeout: 30 * time.Millisecond, Elapsed: 60 * time.Millisecond},
			to:   task.PhaseStalled,
		},
		{
			name: "turn_timeout",
			err:  &runner.TurnTimeoutError{Timeout: 30 * time.Millisecond, Elapsed: 60 * time.Millisecond},
			to:   task.PhaseTimedOut,
		},
		{
			// Locks the #507 item-3 rationale: an outer-deadline *TimeoutError
			// routes to the same terminal phase as the *TurnTimeoutError above,
			// so the classifier reporting one as the other in the coinciding
			// deadline window is a cosmetic budget difference, not a mis-route.
			name: "outer_timeout",
			err:  &runner.TimeoutError{Timeout: 30 * time.Millisecond, Elapsed: 60 * time.Millisecond},
			to:   task.PhaseTimedOut,
		},
		{
			name: "read_timeout",
			err:  &runner.ReadTimeoutError{Timeout: 30 * time.Millisecond},
			to:   task.PhaseTimedOut,
		},
		{
			name: "plain_error",
			err:  errors.New("agent crashed"),
			to:   task.PhaseFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := &fakeEmitter{}
			in := runner.RunInput{
				Task:     task.Task{ID: "tsk_" + tc.name, Model: "codex-app-server"},
				Workflow: workflow.Workflow{},
				Workdir:  t.TempDir(),
			}
			_, err := worker.RunRunnerWithTimeout(context.Background(), ev, stubRunner{err: tc.err}, in, time.Second, "file")
			if err == nil {
				t.Fatal("expected runner error")
			}
			transitions := ev.byKind(task.EventRunPhaseTransition)
			if len(transitions) < 2 {
				t.Fatalf("phase transition count = %d, want launch plus terminal; events=%#v", len(transitions), ev.events)
			}
			terminal := transitions[len(transitions)-1]
			if got := payloadField(t, terminal.Payload, "from"); got != string(task.PhaseLaunchingAgentProcess) {
				t.Fatalf("terminal from = %#v, want %q; transitions=%#v", got, task.PhaseLaunchingAgentProcess, transitions)
			}
			if got := payloadField(t, terminal.Payload, "to"); got != string(tc.to) {
				t.Fatalf("terminal to = %#v, want %q; transitions=%#v", got, tc.to, transitions)
			}
		})
	}
}

func TestRunTaskEmitsFailedPhaseWhenWorkflowResolutionFails(t *testing.T) {
	ev := &fakeEmitter{}
	rterr := worker.RunTaskForTest(context.Background(), ev, task.Task{ID: "tsk_missing_workflow"}, worker.Config{})
	if rterr == nil {
		t.Fatal("RunTaskForTest succeeded, want workflow resolution failure")
	}
	assertLastPhaseTransition(t, ev, task.PhasePreparingWorkspace, task.PhaseFailed)
}

func TestRunTaskEmitsFailedPhaseWhenPromptRenderingFails(t *testing.T) {
	cloneURL, tk := initBareUpstreamWithWorkflow(t, linearWorkflowBody)
	t.Setenv("REPO_URL", cloneURL)

	ev := &fakeEmitter{}
	cfg := workerCfgForIntegration(t)
	cfg.Workflow.PromptTemplate = "bad template {{ missing }}"

	rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg)
	if rterr == nil {
		t.Fatal("RunTaskForTest succeeded, want prompt render failure")
	}
	assertLastPhaseTransition(t, ev, task.PhaseBuildingPrompt, task.PhaseFailed)
}

func assertLastPhaseTransition(t *testing.T, ev *fakeEmitter, from, to task.RunAttemptPhase) {
	t.Helper()
	transitions := ev.byKind(task.EventRunPhaseTransition)
	if len(transitions) == 0 {
		t.Fatalf("expected phase transitions; events=%#v", ev.events)
	}
	terminal := transitions[len(transitions)-1]
	if got := payloadField(t, terminal.Payload, "from"); got != string(from) {
		t.Fatalf("terminal transition from = %#v, want %q; transitions=%#v", got, from, transitions)
	}
	if got := payloadField(t, terminal.Payload, "to"); got != string(to) {
		t.Fatalf("terminal transition to = %#v, want %q; transitions=%#v", got, to, transitions)
	}
}

func assertForwardedRuntimeEventsOnce(t *testing.T, ev *fakeEmitter) {
	t.Helper()
	if got := len(ev.byKind(task.EventSessionStarted)); got != 1 {
		t.Fatalf("session_started count: got=%d want=1; events=%#v", got, ev.events)
	}
	if got := len(ev.byKind(task.EventTurnCompleted)); got != 1 {
		t.Fatalf("turn_completed count: got=%d want=1; events=%#v", got, ev.events)
	}
	if got := payloadField(t, ev.byKind(task.EventSessionStarted)[0].Payload, "thread_id"); got != "thread-1" {
		t.Fatalf("session_started thread_id = %#v, want thread-1", got)
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
	_, err := worker.RunRunnerWithTimeout(context.Background(), ev, stubRunner{err: wantErr}, in, time.Second, "file")
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

func TestRunRunnerWithTimeoutStallEmitsTimeoutBudgetEvent(t *testing.T) {
	t.Parallel()
	ev := &fakeEmitter{}
	in := runner.RunInput{
		Task:     task.Task{ID: "tsk_stall", Model: "codex"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	}
	stall := &runner.StallError{Timeout: 30 * time.Millisecond, Elapsed: 60 * time.Millisecond}
	_, err := worker.RunRunnerWithTimeout(context.Background(), ev, stubRunner{err: stall}, in, time.Second, "file")
	if !runner.IsStall(err) {
		t.Fatalf("expected StallError from RunRunnerWithTimeout, got %v", err)
	}
	if got := len(ev.byKind(task.EventStalled)); got != 1 {
		t.Fatalf("stalled count: got=%d want=1", got)
	}
	if got := len(ev.byKind(task.EventRunnerTimeout)); got != 1 {
		t.Fatalf("runner_timeout count for stall retry budget: got=%d want=1", got)
	}
	if got := len(ev.byKind(task.EventRunnerEnd)); got != 0 {
		t.Fatalf("runner_end must not fire on stall, got=%d", got)
	}
}

func TestRunRunnerWithTimeoutTurnTimeoutEmitsTimeoutBudgetEvent(t *testing.T) {
	t.Parallel()
	ev := &fakeEmitter{}
	in := runner.RunInput{
		Task:     task.Task{ID: "tsk_turn_timeout", Model: "codex"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	}
	terr := &runner.TurnTimeoutError{Timeout: 30 * time.Millisecond, Elapsed: 60 * time.Millisecond}
	_, err := worker.RunRunnerWithTimeout(context.Background(), ev, stubRunner{err: terr}, in, time.Second, "file")
	if !runner.IsTurnTimeout(err) {
		t.Fatalf("expected TurnTimeoutError from RunRunnerWithTimeout, got %v", err)
	}
	if got := len(ev.byKind(task.EventRunnerTimeout)); got != 1 {
		t.Fatalf("runner_timeout count for turn-timeout retry budget: got=%d want=1", got)
	}
	if got := len(ev.byKind(task.EventRunnerEnd)); got != 0 {
		t.Fatalf("runner_end must not fire on turn timeout, got=%d", got)
	}
}

func TestRunRunnerWithTimeoutReadTimeoutEmitsTimeoutBudgetEvent(t *testing.T) {
	t.Parallel()
	ev := &fakeEmitter{}
	in := runner.RunInput{
		Task:     task.Task{ID: "tsk_read_timeout", Model: "codex"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	}
	terr := &runner.ReadTimeoutError{Timeout: 30 * time.Millisecond}
	_, err := worker.RunRunnerWithTimeout(context.Background(), ev, stubRunner{err: terr}, in, time.Second, "file")
	if !runner.IsReadTimeout(err) {
		t.Fatalf("expected ReadTimeoutError from RunRunnerWithTimeout, got %v", err)
	}
	if got := len(ev.byKind(task.EventRunnerTimeout)); got != 1 {
		t.Fatalf("runner_timeout count for read-timeout retry budget: got=%d want=1", got)
	}
	if got := len(ev.byKind(task.EventRunnerEnd)); got != 0 {
		t.Fatalf("runner_end must not fire on read timeout, got=%d", got)
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
	if _, err := worker.RunRunnerWithTimeout(context.Background(), ev, stubRunner{sleep: 10 * time.Millisecond}, in, 0, "default"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pe := ev.byKind(task.EventRunnerStart)[0]
	got, _ := payloadField(t, pe.Payload, "timeout_ms").(float64)
	want := float64((30 * time.Minute).Milliseconds())
	if got != want {
		t.Fatalf("expected default timeout %v ms, got %v", want, got)
	}
}

// TestResolveWorkflow_EmitsResolvedEvent verifies the worker emits a
// workflow_resolved event whose payload carries Source, Path, and the
// effective config quick-look fields (agent_default, policy_mode,
// tracker_kind). These four fields are what the spec promises for the
// post-hoc inspection contract.
func TestResolveWorkflow_EmitsResolvedEvent(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\nagent:\n  default: codex-app-server\npolicy:\n  mode: draft_pr\ntracker:\n  kind: linear\n  project_slug: platform\n---\nprompt\n"
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := workflow.Load(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	ev := &fakeEmitter{}
	wf, src, err := worker.ResolveWorkflow(context.Background(), ev, "tsk_1", "", loaded)
	if err != nil {
		t.Fatalf("ResolveWorkflow: %v", err)
	}
	if src != "file" {
		t.Fatalf("workflow_source = %q, want %q", src, "file")
	}
	if wf.Config.Agent.Default != "codex-app-server" {
		t.Fatalf("agent.default not loaded: %q", wf.Config.Agent.Default)
	}
	got := ev.byKind(task.EventWorkflowResolved)
	if len(got) != 1 {
		t.Fatalf("workflow_resolved events = %d, want 1", len(got))
	}
	payload, _ := got[0].Payload.(map[string]any)
	for _, key := range []string{"source", "path", "agent_default", "policy_mode", "tracker_kind"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("payload missing key %q: %#v", key, payload)
		}
	}
	if payload["source"] != "file" {
		t.Fatalf("payload.source = %v, want \"file\"", payload["source"])
	}
	if payload["path"] != workflowPath {
		t.Fatalf("payload.path = %v, want %q", payload["path"], workflowPath)
	}
	if payload["agent_default"] != "codex-app-server" {
		t.Fatalf("payload.agent_default = %v, want \"codex-app-server\"", payload["agent_default"])
	}
	if _, present := payload["shadowed_by"]; present {
		t.Fatalf("payload should omit shadowed_by when empty: %#v", payload)
	}
}

// TestResolveWorkflow_LogsResolutionLine pins the observability
// requirement from issue #69: every workflow resolution emits a single
// info-level log line that summarizes source, path, and the shadow set.
// The standalone log line lets an operator answer "which file is in
// effect?" by tailing worker logs, without parsing the structured event
// stream. When nothing is shadowed, the `shadowed=` segment is omitted
// so the common case stays terse.
func TestResolveWorkflow_LogsResolutionLine(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n  project_slug: platform\n---\nprompt\n"
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write root: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".aiops"), 0o755); err != nil {
		t.Fatalf("mkdir .aiops: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aiops", "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write .aiops: %v", err)
	}
	wf, err := workflow.Load(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}

	var buf strings.Builder
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	})

	if _, _, err := worker.ResolveWorkflow(context.Background(), &fakeEmitter{}, "tsk_log", "", wf); err != nil {
		t.Fatalf("ResolveWorkflow: %v", err)
	}

	got := buf.String()
	wantSubstrings := []string{
		"event=workflow_resolved",
		"task_id=tsk_log",
		"issue_id=tsk_log",
		"source=file",
		"path=" + workflowPath,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(got, want) {
			t.Fatalf("log line missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "shadowed=") {
		t.Fatalf("service workflow resolution must not report per-repo shadow paths:\n%s", got)
	}
}

// TestResolveWorkflow_LogsResolutionLineOmitsEmptyShadowed keeps the
// common no-shadow case readable: when only the canonical path exists,
// the line carries `source=` and `path=` but no `shadowed=` segment.
func TestResolveWorkflow_LogsResolutionLineOmitsEmptyShadowed(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n  project_slug: platform\n---\nprompt\n"
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	wf, err := workflow.Load(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}

	var buf strings.Builder
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	})

	if _, _, err := worker.ResolveWorkflow(context.Background(), &fakeEmitter{}, "tsk_log2", "", wf); err != nil {
		t.Fatalf("ResolveWorkflow: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "source=file") || !strings.Contains(got, "path="+workflowPath) {
		t.Fatalf("missing source/path in log line:\n%s", got)
	}
	if strings.Contains(got, "shadowed=") {
		t.Fatalf("shadowed= must be omitted when empty:\n%s", got)
	}
}

// TestResolveWorkflow_DefaultSourceOmitsPath checks that when no
// WORKFLOW.md exists, the resolved event records source=default and
// does not emit an empty path key.
func TestResolveWorkflow_DefaultSourceOmitsPath(t *testing.T) {
	ev := &fakeEmitter{}
	wf := &workflow.Workflow{Config: workflow.DefaultConfig(), PromptTemplate: workflow.DefaultPrompt(), Source: workflow.SourceDefault}
	_, src, err := worker.ResolveWorkflow(context.Background(), ev, "tsk_2", "", wf)
	if err != nil {
		t.Fatalf("ResolveWorkflow: %v", err)
	}
	if src != "default" {
		t.Fatalf("workflow_source = %q, want %q", src, "default")
	}
	got := ev.byKind(task.EventWorkflowResolved)
	if len(got) != 1 {
		t.Fatalf("workflow_resolved events = %d, want 1", len(got))
	}
	payload, _ := got[0].Payload.(map[string]any)
	if _, present := payload["path"]; present {
		t.Fatalf("payload should omit path when source=default: %#v", payload)
	}
	if _, present := payload["shadowed_by"]; present {
		t.Fatalf("payload should omit shadowed_by when empty: %#v", payload)
	}
}

// TestRunRunnerWithTimeout_StampsWorkflowSource verifies the
// runner_start payload carries workflow_source as a quick-look field.
// The full provenance is on workflow_resolved; this stamp lets a
// timeline viewer color the runner stage by source without joining
// against the earlier event.
func TestRunRunnerWithTimeout_StampsWorkflowSource(t *testing.T) {
	ev := &fakeEmitter{}
	in := runner.RunInput{Task: task.Task{ID: "tsk_1", Model: "mock"}}
	if _, err := worker.RunRunnerWithTimeout(context.Background(), ev, stubRunner{}, in, time.Second, "prompt_only"); err != nil {
		t.Fatalf("RunRunnerWithTimeout: %v", err)
	}
	got := ev.byKind(task.EventRunnerStart)
	if len(got) != 1 {
		t.Fatalf("runner_start events = %d, want 1", len(got))
	}
	payload, _ := got[0].Payload.(map[string]any)
	if payload["workflow_source"] != "prompt_only" {
		t.Fatalf("workflow_source = %v, want %q", payload["workflow_source"], "prompt_only")
	}
}

// fakeOutputRunner returns a fixed Result with non-zero output fields so we
// can assert RunRunnerWithTimeout forwards them onto the runner_end payload.
type fakeOutputRunner struct{}

func (fakeOutputRunner) Run(_ context.Context, _ runner.RunInput) (runner.Result, error) {
	return runner.Result{
		Summary:       "fake done",
		OutputBytes:   42,
		OutputDropped: 7,
		OutputHead:    "head-canary",
		OutputTail:    "tail-canary",
	}, nil
}

// TestRunRunnerWithTimeout_EmitsOutputFieldsOnRunnerEnd verifies that when a
// runner returns non-zero output telemetry, the runner_end payload carries
// output_bytes, output_dropped, output_head, and output_tail.
func TestRunRunnerWithTimeout_EmitsOutputFieldsOnRunnerEnd(t *testing.T) {
	ev := &fakeEmitter{}
	in := runner.RunInput{Task: task.Task{ID: "tsk_payload", Model: "codex"}}
	if _, err := worker.RunRunnerWithTimeout(context.Background(), ev, fakeOutputRunner{}, in, 5*time.Second, "file"); err != nil {
		t.Fatalf("RunRunnerWithTimeout: %v", err)
	}
	ends := ev.byKind(task.EventRunnerEnd)
	if len(ends) == 0 {
		t.Fatal("no runner_end event recorded")
	}
	pe := ends[len(ends)-1]

	wantInt := map[string]float64{
		"output_bytes":   42,
		"output_dropped": 7,
	}
	for k, want := range wantInt {
		got := payloadField(t, pe.Payload, k)
		if got != want {
			t.Fatalf("payload[%q] = %v (%T), want %v", k, got, got, want)
		}
	}
	wantStr := map[string]string{
		"output_head": "head-canary",
		"output_tail": "tail-canary",
	}
	for k, want := range wantStr {
		got, _ := payloadField(t, pe.Payload, k).(string)
		if got != want {
			t.Fatalf("payload[%q] = %q, want %q", k, got, want)
		}
	}
}

// TestRunRunnerWithTimeout_OmitsOutputFieldsForMockRunner verifies that the
// MockRunner (which leaves all Output* fields zero) does not pollute the
// runner_end payload with output_* keys.
func TestRunRunnerWithTimeout_OmitsOutputFieldsForMockRunner(t *testing.T) {
	ev := &fakeEmitter{}
	in := runner.RunInput{Task: task.Task{ID: "tsk_mock_payload", Model: "mock"}, Workdir: t.TempDir()}
	if _, err := worker.RunRunnerWithTimeout(context.Background(), ev, runner.MockRunner{}, in, 5*time.Second, "file"); err != nil {
		t.Fatalf("RunRunnerWithTimeout: %v", err)
	}
	ends := ev.byKind(task.EventRunnerEnd)
	if len(ends) == 0 {
		t.Fatal("no runner_end event recorded")
	}
	pe := ends[len(ends)-1]
	for _, k := range []string{"output_bytes", "output_dropped", "output_head", "output_tail"} {
		if got := payloadField(t, pe.Payload, k); got != nil {
			t.Fatalf("payload should not contain %q for mock runner; got %v", k, got)
		}
	}
}
