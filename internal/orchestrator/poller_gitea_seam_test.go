package orchestrator

import (
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// TestGiteaPreDispatchStateIsBlockerGated pins the producer/consumer seam
// behind #739: the Gitea adapter's pre-dispatch state (derived from the
// aiops/todo label) must be the literal "Todo" state the SPEC §8.2 blocker
// rule keys on. If the adapter ever renames that state again (it shipped as
// "AI Ready"), blocked Gitea issues dispatch while their dependencies are
// still open and this test fails.
func TestGiteaPreDispatchStateIsBlockerGated(t *testing.T) {
	mappings := gitea.DefaultStateLabelMappings()
	preDispatch, diags := gitea.IssueStateFromLabels([]gitea.Label{{Name: "aiops/todo"}}, mappings)
	if len(diags) != 0 {
		t.Fatalf("IssueStateFromLabels(aiops/todo) diagnostics = %v, want none", diags)
	}
	terminal, diags := gitea.IssueStateFromLabels([]gitea.Label{{Name: "aiops/done"}}, mappings)
	if len(diags) != 0 {
		t.Fatalf("IssueStateFromLabels(aiops/done) diagnostics = %v, want none", diags)
	}
	terminalStates := []string{terminal}

	// Field repro from the v0.1.0 lifecycle test: issue #10 (aiops/todo)
	// declared "Depends on #4" while #4 was still aiops/todo.
	blocked := tracker.Issue{
		ID:         "10",
		Identifier: "#10",
		Title:      "blocked by open dependency",
		State:      preDispatch,
		BlockedBy:  []tracker.BlockerRef{{ID: "4", Identifier: "#4", State: preDispatch}},
	}
	if out := filterEligibleCandidates([]tracker.Issue{blocked}, terminalStates, nil); len(out) != 0 {
		t.Fatalf("filterEligibleCandidates(issue in %q blocked by non-terminal %q) = %d issues, want 0", preDispatch, preDispatch, len(out))
	}

	released := blocked
	released.BlockedBy = []tracker.BlockerRef{{ID: "4", Identifier: "#4", State: terminal}}
	if out := filterEligibleCandidates([]tracker.Issue{released}, terminalStates, nil); len(out) != 1 {
		t.Fatalf("filterEligibleCandidates(issue in %q blocked by terminal %q) = %d issues, want 1", preDispatch, terminal, len(out))
	}
}
