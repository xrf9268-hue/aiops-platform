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
