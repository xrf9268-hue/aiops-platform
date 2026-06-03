package task_test

import (
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

func TestRunAttemptPhasesMatchSymphonySpec(t *testing.T) {
	got := task.RunAttemptPhases()
	want := []task.RunAttemptPhase{
		task.PhasePreparingWorkspace,
		task.PhaseBuildingPrompt,
		task.PhaseLaunchingAgentProcess,
		task.PhaseInitializingSession,
		task.PhaseStreamingTurn,
		task.PhaseFinishing,
		task.PhaseSucceeded,
		task.PhaseFailed,
		task.PhaseTimedOut,
		task.PhaseStalled,
		task.PhaseCanceledByReconciliation,
	}
	if len(got) != len(want) {
		t.Fatalf("phase count = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("phase[%d] = %q, want %q; all=%#v", i, got[i], want[i], got)
		}
	}
}

func TestRuntimeEventVocabularyIncludesRunnerAppServerEvents(t *testing.T) {
	want := []string{
		task.EventSessionStarted,
		task.EventStartupFailed,
		task.EventTurnStarted,
		task.EventTurnCompleted,
		task.EventTurnFailed,
		task.EventTurnCancelled,
		task.EventTurnEndedWithError,
		task.EventTurnInputRequired,
		task.EventApprovalAutoApproved,
		task.EventUnsupportedToolCall,
		task.EventNotification,
		task.EventOtherMessage,
		task.EventMalformed,
	}
	vocab := map[string]bool{}
	for _, event := range task.RuntimeEvents() {
		vocab[event] = true
	}
	for _, event := range want {
		if !vocab[event] {
			t.Fatalf("RuntimeEvents missing %q; got %#v", event, task.RuntimeEvents())
		}
	}
}

func TestPhaseTransitionEventsUseSpecPhasePayload(t *testing.T) {
	e := task.PhaseTransitionEvent(task.PhaseInitializingSession, task.PhaseStreamingTurn)
	if e.Event != task.EventRunPhaseTransition {
		t.Fatalf("event = %q, want %q", e.Event, task.EventRunPhaseTransition)
	}
	if e.From != task.PhaseInitializingSession || e.To != task.PhaseStreamingTurn {
		t.Fatalf("transition = %q -> %q, want InitializingSession -> StreamingTurn", e.From, e.To)
	}
}
