package gitea

import (
	"slices"
	"strings"
	"testing"
)

func TestIssueStateFromLabelsMapsSingleAIOpsLabel(t *testing.T) {
	state, diagnostics := IssueStateFromLabels([]Label{{Name: "bug"}, {Name: "aiops/in-progress"}}, DefaultStateLabelMappings())

	if state != "In Progress" {
		t.Fatalf("state = %q, want In Progress", state)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v, want none", diagnostics)
	}
}

func TestIssueStateFromLabelsMapsMergingAndPrioritizesItOverHumanReview(t *testing.T) {
	state, diagnostics := IssueStateFromLabels([]Label{{Name: "aiops/human-review"}, {Name: "aiops/merging"}}, DefaultStateLabelMappings())

	if state != "Merging" {
		t.Fatalf("state = %q, want Merging to win conflict priority over Human Review", state)
	}
	if !hasDiagnostic(diagnostics, "conflicting_aiops_state_labels") {
		t.Fatalf("diagnostics = %#v, want conflicting_aiops_state_labels", diagnostics)
	}
	if !strings.Contains(diagnostics[0].Message, "aiops/merging") || !strings.Contains(diagnostics[0].Message, "aiops/human-review") {
		t.Fatalf("diagnostic message = %q, want conflicting labels named", diagnostics[0].Message)
	}
}

func TestIssueStateFromLabelsReportsMissingState(t *testing.T) {
	state, diagnostics := IssueStateFromLabels([]Label{{Name: "bug"}}, DefaultStateLabelMappings())

	if state != "" {
		t.Fatalf("state = %q, want empty for missing aiops state label", state)
	}
	if !hasDiagnostic(diagnostics, "missing_aiops_state_label") {
		t.Fatalf("diagnostics = %#v, want missing_aiops_state_label", diagnostics)
	}
}

func TestIssueStateFromLabelsUsesDeterministicPriorityForConflicts(t *testing.T) {
	state, diagnostics := IssueStateFromLabels([]Label{{Name: "aiops/done"}, {Name: "aiops/rework"}}, DefaultStateLabelMappings())

	if state != "Rework" {
		t.Fatalf("state = %q, want Rework to win conflict priority over Done", state)
	}
	if !hasDiagnostic(diagnostics, "conflicting_aiops_state_labels") {
		t.Fatalf("diagnostics = %#v, want conflicting_aiops_state_labels", diagnostics)
	}
	if !strings.Contains(diagnostics[0].Message, "aiops/rework") || !strings.Contains(diagnostics[0].Message, "aiops/done") {
		t.Fatalf("diagnostic message = %q, want conflicting labels named", diagnostics[0].Message)
	}
}

func TestIssueStateFromLabelsIgnoresUnknownAIOpsLabels(t *testing.T) {
	state, diagnostics := IssueStateFromLabels([]Label{{Name: "aiops/backlog"}, {Name: "aiops/todo"}}, DefaultStateLabelMappings())

	if state != "Todo" {
		t.Fatalf("state = %q, want Todo", state)
	}
	if !hasDiagnostic(diagnostics, "unknown_aiops_label") {
		t.Fatalf("diagnostics = %#v, want unknown_aiops_label", diagnostics)
	}
}

func TestStateLabelNamesForStatesFiltersConfiguredStates(t *testing.T) {
	got := StateLabelNamesForStates([]string{"Todo", "Merging", "Rework", "Done", "Not Configured"}, DefaultStateLabelMappings())
	want := []string{"aiops/todo", "aiops/merging", "aiops/rework", "aiops/done"}

	if !slices.Equal(got, want) {
		t.Fatalf("StateLabelNamesForStates = %#v, want %#v", got, want)
	}
}

func hasDiagnostic(diagnostics []StateDiagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
