package gitea

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

const stateLabelPrefix = "aiops/"

// Label is the subset of a Gitea issue label needed to derive the
// orchestrator-visible workflow state.
type Label struct {
	Name string `json:"name"`
}

// StateLabelMapping maps one mutually-exclusive aiops/* label to the workflow
// state name exposed through tracker.Issue.State.
type StateLabelMapping struct {
	Label string
	State string
}

// StateDiagnostic records non-fatal label-state problems discovered while
// reading a Gitea issue. The poller can log these diagnostics without turning a
// malformed issue into an orchestrator-side write.
type StateDiagnostic struct {
	Code    string
	Message string
	Labels  []string
}

// DefaultStateLabelMappings defines the repo's Gitea tracker label convention.
// The order is also the deterministic conflict priority: active/rework labels
// win over terminal labels so an issue is not silently dropped when a stale
// terminal label remains alongside a label that requests work.
//
// aiops/todo MUST map to the SPEC state name "Todo": the §8.2 blocker rule
// only gates issues whose state is literally Todo, so a renamed pre-dispatch
// state silently disables dependency blocking for this tracker (#739).
func DefaultStateLabelMappings() []StateLabelMapping {
	return []StateLabelMapping{
		{Label: "aiops/rework", State: "Rework"},
		{Label: "aiops/in-progress", State: "In Progress"},
		{Label: "aiops/human-review", State: "Human Review"},
		{Label: "aiops/todo", State: "Todo"},
		{Label: "aiops/done", State: "Done"},
		{Label: "aiops/canceled", State: "Canceled"},
	}
}

// IssueStateFromLabels derives a workflow state from Gitea aiops/* labels. It
// is read-only by design: conflicts and malformed labels are reported as
// diagnostics for humans/agents, but this function never mutates labels.
func IssueStateFromLabels(labels []Label, mappings []StateLabelMapping) (string, []StateDiagnostic) { //nolint:gocognit // baseline (#521)
	if len(mappings) == 0 {
		mappings = DefaultStateLabelMappings()
	}
	known := make(map[string]string, len(mappings))
	priority := make(map[string]int, len(mappings))
	for i, mapping := range mappings {
		label := strings.ToLower(strings.TrimSpace(mapping.Label))
		if label == "" || mapping.State == "" {
			continue
		}
		known[label] = mapping.State
		priority[label] = i
	}

	matches := make([]string, 0, 1)
	unknown := make([]string, 0)
	for _, label := range labels {
		name := strings.ToLower(strings.TrimSpace(label.Name))
		if name == "" || !strings.HasPrefix(name, stateLabelPrefix) {
			continue
		}
		if _, ok := known[name]; ok {
			if !slices.Contains(matches, name) {
				matches = append(matches, name)
			}
			continue
		}
		if !slices.Contains(unknown, name) {
			unknown = append(unknown, name)
		}
	}

	var diagnostics []StateDiagnostic
	if len(unknown) > 0 {
		sort.Strings(unknown)
		diagnostics = append(diagnostics, StateDiagnostic{
			Code:    "unknown_aiops_label",
			Message: fmt.Sprintf("unknown aiops state label(s): %s", strings.Join(unknown, ", ")),
			Labels:  slices.Clone(unknown),
		})
	}
	if len(matches) == 0 {
		diagnostics = append(diagnostics, StateDiagnostic{
			Code:    "missing_aiops_state_label",
			Message: "issue has no configured aiops/* state label",
		})
		return "", diagnostics
	}

	sort.SliceStable(matches, func(i, j int) bool { return priority[matches[i]] < priority[matches[j]] })
	if len(matches) > 1 {
		diagnostics = append(diagnostics, StateDiagnostic{
			Code:    "conflicting_aiops_state_labels",
			Message: fmt.Sprintf("multiple aiops state labels present; using %s by deterministic priority: %s", matches[0], strings.Join(matches, ", ")),
			Labels:  slices.Clone(matches),
		})
	}
	return known[matches[0]], diagnostics
}

// StateLabelNamesForStates returns configured aiops/* label names for the named
// workflow states. It lets pollers query the tracker by active or terminal
// state sets without hard-coding label names outside the Gitea adapter.
func StateLabelNamesForStates(states []string, mappings []StateLabelMapping) []string {
	if len(mappings) == 0 {
		mappings = DefaultStateLabelMappings()
	}
	byState := make(map[string]string, len(mappings))
	for _, mapping := range mappings {
		state := strings.ToLower(strings.TrimSpace(mapping.State))
		label := strings.ToLower(strings.TrimSpace(mapping.Label))
		if state != "" && label != "" {
			byState[state] = label
		}
	}
	labels := make([]string, 0, len(states))
	for _, state := range states {
		if label, ok := byState[strings.ToLower(strings.TrimSpace(state))]; ok && !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	return labels
}
