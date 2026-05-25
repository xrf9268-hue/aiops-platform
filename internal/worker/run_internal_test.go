package worker

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTerminalUpdateContext_SurvivesParentCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	if parent.Err() == nil {
		t.Fatalf("parent should be canceled")
	}

	cleanup, cancelCleanup := terminalUpdateContext(parent)
	defer cancelCleanup()

	if cleanup.Err() != nil {
		t.Fatalf("cleanup ctx should not be canceled even though parent is; got %v", cleanup.Err())
	}

	deadline, ok := cleanup.Deadline()
	if !ok {
		t.Fatalf("cleanup ctx should carry a deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 5*time.Second {
		t.Fatalf("cleanup deadline out of expected range; remaining=%v", remaining)
	}
}

func TestTerminalUpdateContext_RespectsItsOwnCancel(t *testing.T) {
	parent := context.Background()
	cleanup, cancel := terminalUpdateContext(parent)
	cancel()
	if !errors.Is(cleanup.Err(), context.Canceled) {
		t.Fatalf("cleanup ctx should be Canceled after its cancel func runs; got %v", cleanup.Err())
	}
}

func TestAnalysisOnlyArtifactAllowedIsExplicit(t *testing.T) {
	cases := map[string]bool{
		".aiops/PLAN.md": true, ".aiops/RUN_SUMMARY.md": true, ".aiops/CHANGED_FILES.txt": true, ".aiops/VERIFICATION.txt": true,
		".aiops/PROMPT.md": false, ".aiops/TASK.md": false, ".aiops/WORKFLOW.md": false, ".aiops/debug.md": false, ".aiops/logs/runner.log": false,
	}
	for path, want := range cases {
		if got := analysisOnlyArtifactAllowed(path); got != want {
			t.Fatalf("analysisOnlyArtifactAllowed(%q) = %v, want %v", path, got, want)
		}
	}
}
