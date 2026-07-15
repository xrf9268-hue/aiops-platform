package orchestrator

import (
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

func TestApplyBudgetExceededBlockFallbackNamesObservedScope(t *testing.T) {
	st := NewOrchestratorState(15000, 100)
	id := IssueID("ENG-BUDGET-FALLBACK")
	entry := &RunningEntry{
		BudgetExceeded: true,
		Issue:          tracker.Issue{ID: string(id), Identifier: string(id)},
		Identifier:     string(id),
	}
	st.Running[id] = entry
	done := make(chan struct{})
	op := finalizeRunOp{id: id, entry: entry, done: done}

	if !op.applyBudgetExceededBlock(st, time.Second) {
		t.Fatal("applyBudgetExceededBlock() = false; want true")
	}
	want := "worker-observed, runner-reported Codex claim budget exceeded; observed total and configured limit unavailable; external review and otherwise unreported nested or subagent usage are excluded"
	if got := st.Blocked[id].Error; got != want {
		t.Fatalf("blocked fallback error = %q; want %q", got, want)
	}
	select {
	case <-done:
	default:
		t.Fatal("applyBudgetExceededBlock did not close done")
	}
}
