package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestValidateDispatchPreflight_HappyPathReturnsNil(t *testing.T) {
	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "linear", APIKey: "lin_xxxx", ProjectSlug: "team-x"},
		Codex:   workflow.CommandConfig{Command: "codex app-server"},
	}
	if err := validateDispatchPreflight(cfg); err != nil {
		t.Fatalf("happy-path preflight: %v", err)
	}
}

func TestValidateDispatchPreflight_EmptyAPIKeyAfterVarResolution(t *testing.T) {
	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "linear", APIKey: "", ProjectSlug: "team-x"},
		Codex:   workflow.CommandConfig{Command: "codex app-server"},
	}
	err := validateDispatchPreflight(cfg)
	if err == nil {
		t.Fatalf("expected error for empty api_key")
	}
	if !strings.Contains(err.Error(), "tracker.api_key empty") {
		t.Errorf("unexpected reason: %v", err)
	}
}

func TestValidateDispatchPreflight_MissingCodexCommand(t *testing.T) {
	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "linear", APIKey: "lin_xxxx", ProjectSlug: "team-x"},
		Codex:   workflow.CommandConfig{Command: ""},
	}
	err := validateDispatchPreflight(cfg)
	if err == nil {
		t.Fatalf("expected error for empty codex.command")
	}
	if !strings.Contains(err.Error(), "codex.command empty") {
		t.Errorf("unexpected reason: %v", err)
	}
}

func TestValidateDispatchPreflight_UnsupportedTrackerKind(t *testing.T) {
	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "jira", APIKey: "x", ProjectSlug: "p"},
		Codex:   workflow.CommandConfig{Command: "codex app-server"},
	}
	err := validateDispatchPreflight(cfg)
	if err == nil {
		t.Fatalf("expected error for unsupported tracker.kind")
	}
	if !strings.Contains(err.Error(), "tracker.kind unsupported") {
		t.Errorf("unexpected reason: %v", err)
	}
}

func TestValidateDispatchPreflight_LinearProjectSlugMissingFailsButServiceRouteCovers(t *testing.T) {
	missing := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "linear", APIKey: "lin_xxxx"},
		Codex:   workflow.CommandConfig{Command: "codex app-server"},
	}
	if err := validateDispatchPreflight(missing); err == nil {
		t.Fatalf("expected error for missing linear project_slug")
	}
	covered := workflow.Config{
		Tracker:  workflow.TrackerConfig{Kind: "linear", APIKey: "lin_xxxx"},
		Codex:    workflow.CommandConfig{Command: "codex app-server"},
		Services: []workflow.ServiceConfig{{Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "service-x"}}},
	}
	if err := validateDispatchPreflight(covered); err != nil {
		t.Fatalf("service-routed project_slug should satisfy preflight: %v", err)
	}
}

func TestValidateDispatchPreflight_JoinsMultipleReasons(t *testing.T) {
	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "", APIKey: ""},
		Codex:   workflow.CommandConfig{Command: ""},
	}
	err := validateDispatchPreflight(cfg)
	if err == nil {
		t.Fatalf("expected joined error")
	}
	for _, want := range []string{"tracker.kind missing", "tracker.api_key empty", "codex.command empty"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error missing %q: %v", want, err)
		}
	}
}

func TestPollOncePreflightFailureSkipsDispatchAndEmitsRuntimeEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}}
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
		TerminalStates:    []string{"Cancelled", "Done"},
		WorkerExitTimeout: time.Second,
	})
	// Empty api_key + missing codex.command — preflight must catch both.
	preflightCfg := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "linear", APIKey: "", ProjectSlug: "team-x"},
		Codex:   workflow.CommandConfig{Command: ""},
	}
	poller.preflight = &preflightCfg

	err := poller.PollOnce(ctx)
	if err == nil {
		t.Fatalf("expected preflight failure error")
	}
	if !strings.Contains(err.Error(), "dispatch preflight failed") {
		t.Errorf("unexpected error shape: %v", err)
	}
	if dispatcher.count() != 0 {
		t.Errorf("dispatch should be skipped on preflight failure, got count=%d", dispatcher.count())
	}
	view, snapErr := orch.Snapshot(ctx)
	if snapErr != nil {
		t.Fatalf("snapshot: %v", snapErr)
	}
	var saw bool
	for _, ev := range view.RecentEvents {
		if ev.Kind == RuntimeEventDispatchPreflightFailed {
			saw = true
			if !strings.Contains(ev.Message, "tracker.api_key empty") || !strings.Contains(ev.Message, "codex.command empty") {
				t.Errorf("preflight event message does not carry both joined reasons: %q", ev.Message)
			}
		}
	}
	if !saw {
		t.Errorf("RuntimeEventDispatchPreflightFailed not recorded in RecentEvents (have %d events)", len(view.RecentEvents))
	}
}

func TestPollOncePreflightSuccessProceedsToFetch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}}
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
		TerminalStates:    []string{"Cancelled", "Done"},
		WorkerExitTimeout: time.Second,
	})
	preflightCfg := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "linear", APIKey: "lin_xxxx", ProjectSlug: "team-x"},
		Codex:   workflow.CommandConfig{Command: "codex app-server"},
	}
	poller.preflight = &preflightCfg

	if err := poller.PollOnce(ctx); err != nil && !errors.Is(err, ErrNotDispatched) {
		// Best-effort: dispatch may or may not occur depending on test
		// fakes; the assertion that matters is no preflight error.
		if strings.Contains(err.Error(), "dispatch preflight failed") {
			t.Fatalf("preflight should have passed: %v", err)
		}
	}
}
