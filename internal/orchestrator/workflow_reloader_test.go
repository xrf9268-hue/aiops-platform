package orchestrator

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type recordingWorkflowReloadEmitter struct {
	mu     sync.Mutex
	events []recordedWorkflowReloadEvent
}

type recordedWorkflowReloadEvent struct {
	kind    string
	message string
	payload any
}

func (e *recordingWorkflowReloadEmitter) AddEvent(_ context.Context, _ string, kind, msg string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, recordedWorkflowReloadEvent{kind: kind, message: msg})
	return nil
}

func (e *recordingWorkflowReloadEmitter) AddEventWithPayload(_ context.Context, _ string, kind, msg string, payload any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, recordedWorkflowReloadEvent{kind: kind, message: msg, payload: payload})
	return nil
}

func (e *recordingWorkflowReloadEmitter) count(kind string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	count := 0
	for _, ev := range e.events {
		if ev.kind == kind {
			count++
		}
	}
	return count
}

func TestWorkflowRuntimeReloadSuccessAtomicallySwapsConfigAndEmitsEvent(t *testing.T) {
	path := writeWorkflowForReloadTest(t, "linear", 30000, "AI Ready")
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	emitter := &recordingWorkflowReloadEmitter{}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{
		Initial:        initial,
		Path:           path,
		Source:         workflow.SourceFile,
		ReloadInterval: time.Millisecond,
		Emitter:        emitter,
		EventTaskID:    "workflow-runtime",
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	writeWorkflowForReloadTestAt(t, path, "linear", 45000, "Rework")
	if err := runtime.ReloadOnce(context.Background()); err != nil {
		t.Fatalf("reload once: %v", err)
	}

	snap := runtime.Current()
	if got := snap.Workflow.Config.Tracker.PollIntervalMs; got != 45000 {
		t.Fatalf("poll interval after reload = %d, want 45000", got)
	}
	if got := snap.Workflow.Config.Tracker.ActiveStates; len(got) != 1 || got[0] != "Rework" {
		t.Fatalf("active states after reload = %#v, want [Rework]", got)
	}
	if got := emitter.count(task.EventWorkflowReloaded); got != 1 {
		t.Fatalf("workflow_reload event count = %d, want 1", got)
	}
}

func TestWorkflowRuntimeReloadFailureKeepsPreviousConfigAndEmitsFailureEvent(t *testing.T) {
	path := writeWorkflowForReloadTest(t, "linear", 30000, "AI Ready")
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	emitter := &recordingWorkflowReloadEmitter{}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{
		Initial:        initial,
		Path:           path,
		Source:         workflow.SourceFile,
		ReloadInterval: time.Millisecond,
		Emitter:        emitter,
		EventTaskID:    "workflow-runtime",
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	writeWorkflowForReloadTestAt(t, path, "unsupported", 45000, "Rework")
	if err := runtime.ReloadOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "tracker.kind") {
		t.Fatalf("reload error = %v, want tracker.kind validation error", err)
	}

	snap := runtime.Current()
	if got := snap.Workflow.Config.Tracker.PollIntervalMs; got != 30000 {
		t.Fatalf("poll interval after failed reload = %d, want previous 30000", got)
	}
	if got := snap.Workflow.Config.Tracker.ActiveStates; len(got) != 1 || got[0] != "AI Ready" {
		t.Fatalf("active states after failed reload = %#v, want previous [AI Ready]", got)
	}
	if got := emitter.count(task.EventWorkflowReloadFailed); got != 1 {
		t.Fatalf("workflow_reload_failed event count = %d, want 1", got)
	}
}

func TestRunWorkflowReloadLoopPollFallbackReloadsChangedWorkflow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := writeWorkflowForReloadTest(t, "linear", 30000, "AI Ready")
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	sleeper := &reloadLoopTestSleeper{
		afterFirst: func() {
			writeWorkflowForReloadTestAt(t, path, "linear", 42000, "Rework")
		},
	}

	err = RunWorkflowReloadLoop(ctx, runtime, WorkflowReloadLoopOptions{Sleep: sleeper.sleep, StopAfterChecks: 2})
	if err != nil {
		t.Fatalf("run reload loop: %v", err)
	}

	snap := runtime.Current()
	if got := snap.Workflow.Config.Tracker.PollIntervalMs; got != 42000 {
		t.Fatalf("poll interval after polling reload loop = %d, want 42000", got)
	}
	if got := snap.Workflow.Config.Tracker.ActiveStates; len(got) != 1 || got[0] != "Rework" {
		t.Fatalf("active states after polling reload loop = %#v, want [Rework]", got)
	}
}

func TestRunPollLoopWithRuntimeUsesReloadedPollingCadence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := writeWorkflowForReloadTest(t, "linear", 25, "AI Ready")
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	poller := &countingPollOnce{afterFirst: func() {
		writeWorkflowForReloadTestAt(t, path, "linear", 75, "AI Ready")
		_ = runtime.ReloadOnce(context.Background())
	}}
	sleeper := &recordingPollSleeper{}

	err = RunPollLoopWithRuntime(ctx, poller, runtime, PollLoopRuntimeOptions{Sleep: sleeper.sleep, StopAfterPolls: 2})
	if err != nil {
		t.Fatalf("run poll loop: %v", err)
	}
	if got, want := sleeper.durations, []time.Duration{25 * time.Millisecond, 75 * time.Millisecond}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("sleep durations = %v, want %v", got, want)
	}
}

type countingPollOnce struct {
	calls      int
	afterFirst func()
}

func (p *countingPollOnce) PollOnce(_ context.Context) error {
	p.calls++
	if p.calls == 1 && p.afterFirst != nil {
		p.afterFirst()
	}
	return nil
}

type recordingPollSleeper struct {
	durations []time.Duration
}

func (s *recordingPollSleeper) sleep(_ context.Context, d time.Duration) error {
	s.durations = append(s.durations, d)
	return nil
}

type reloadLoopTestSleeper struct {
	calls      int
	afterFirst func()
}

func (s *reloadLoopTestSleeper) sleep(_ context.Context, _ time.Duration) error {
	s.calls++
	if s.calls == 1 && s.afterFirst != nil {
		s.afterFirst()
	}
	return nil
}

func writeWorkflowForReloadTest(t *testing.T, trackerKind string, pollIntervalMs int, activeState string) string {
	t.Helper()
	path := t.TempDir() + "/WORKFLOW.md"
	writeWorkflowForReloadTestAt(t, path, trackerKind, pollIntervalMs, activeState)
	return path
}

func writeWorkflowForReloadTestAt(t *testing.T, path, trackerKind string, pollIntervalMs int, activeState string) {
	t.Helper()
	content := "---\n" +
		"repo:\n" +
		"  owner: xrf9268-hue\n" +
		"  name: aiops-platform\n" +
		"  clone_url: https://github.com/xrf9268-hue/aiops-platform.git\n" +
		"tracker:\n" +
		"  kind: " + trackerKind + "\n" +
		"  active_states: [\"" + activeState + "\"]\n" +
		"  terminal_states: [\"Done\"]\n" +
		"  poll_interval_ms: " + itoaForReloadTest(pollIntervalMs) + "\n" +
		"agent:\n" +
		"  default: mock\n" +
		"---\n" +
		"Prompt body\n"
	if err := osWriteFileForReloadTest(path, []byte(content)); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
}

var osWriteFileForReloadTest = func(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

func itoaForReloadTest(v int) string {
	return strconv.Itoa(v)
}
