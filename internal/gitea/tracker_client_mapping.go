package gitea

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func parseGiteaIssueTime(field, value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse Gitea issue %s %q: %w", field, value, err)
	}
	return parsed, nil
}

// extractGiteaLabels returns lowercased label names per SPEC §11.3
// normalization. The Gitea label payload (`Label.Name`) is already deserialized
// by IssueStateFromLabels; we just project it onto the cross-tracker Issue
// shape.
func extractGiteaLabels(labels []Label) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		name := strings.TrimSpace(l.Name)
		if name == "" {
			continue
		}
		out = append(out, strings.ToLower(name))
	}
	return out
}

func giteaIssueID(issue Issue) string {
	if issue.ID != 0 {
		return strconv.FormatInt(issue.ID, 10)
	}
	if issue.Number != 0 {
		return strconv.Itoa(issue.Number)
	}
	return ""
}

func giteaAPIStateForWorkflowStates(states, terminalStateNames []string) string {
	terminalStates := normalizedStateSet(terminalStateNames)
	if len(terminalStates) == 0 {
		terminalStates = normalizedStateSet(workflow.DefaultConfig().Tracker.TerminalStates)
	}
	for _, state := range states {
		if _, ok := terminalStates[strings.ToLower(strings.TrimSpace(state))]; ok {
			return "all"
		}
	}
	return "open"
}

func (c *TrackerClient) logDiagnostics(issue Issue, diagnostics []StateDiagnostic) {
	if c.Logf == nil {
		return
	}
	identifier := issue.Number
	for _, diagnostic := range diagnostics {
		c.Logf("gitea issue #%d label diagnostic %s: %s", identifier, diagnostic.Code, diagnostic.Message)
	}
}

func normalizedStateSet(states []string) map[string]struct{} {
	set := make(map[string]struct{}, len(states))
	for _, state := range states {
		state = strings.ToLower(strings.TrimSpace(state))
		if state != "" {
			set[state] = struct{}{}
		}
	}
	return set
}
