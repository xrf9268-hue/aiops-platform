package orchestrator

import (
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

func TestApplyBudgetExceededBlockFallbackNamesObservedScope(t *testing.T) {
	st := NewOrchestratorState(15000, 100)
	st.BudgetGuardrails = BudgetGuardrails{MaxTokensPerClaim: 10, MaxRuntimeSecondsPerClaim: 7200}
	id := IssueID("ENG-BUDGET-FALLBACK")
	entry := &RunningEntry{
		BudgetExceeded:   true,
		CodexTotalTokens: 12,
		Issue:            tracker.Issue{ID: string(id), Identifier: string(id)},
		Identifier:       string(id),
	}
	st.Running[id] = entry
	done := make(chan struct{})
	op := finalizeRunOp{id: id, entry: entry, done: done}

	if !op.applyBudgetExceededBlock(st, time.Second) {
		t.Fatal("applyBudgetExceededBlock() = false; want true")
	}
	want := "worker-observed, runner-reported Codex claim budget exceeded: current_claim_total_tokens=12 max_tokens_per_claim=10 current_claim_runtime_seconds=1 max_runtime_seconds_per_claim=7200; recorded exceedance reason missing; external GitHub @codex review and otherwise unreported nested or subagent usage are excluded from token totals"
	if got := st.Blocked[id].Error; got != want {
		t.Fatalf("blocked fallback error = %q; want %q", got, want)
	}
	select {
	case <-done:
	default:
		t.Fatal("applyBudgetExceededBlock did not close done")
	}
}
