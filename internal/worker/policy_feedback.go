package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/policy"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
	"github.com/xrf9268-hue/aiops-platform/internal/workspace"
)

type policyViolationFeedback struct {
	IssueID    string             `json:"issue_id"`
	Identifier string             `json:"identifier,omitempty"`
	Count      int                `json:"count"`
	UpdatedAt  string             `json:"updated_at"`
	Violations []policy.Violation `json:"violations"`
	Summary    string             `json:"summary"`
}

func policyViolationFeedbackPath(workspaceRoot string, t task.Task) string {
	sourceType := strings.TrimSpace(t.SourceType)
	if sourceType == "" {
		sourceType = "task"
	}
	sourceID := strings.TrimSpace(t.SourceEventID)
	if sourceID == "" {
		sourceID = t.ID
	}
	return filepath.Join(
		workspaceRoot,
		workspace.SanitizeComponent(t.RepoOwner),
		workspace.SanitizeComponent(t.RepoName),
		".aiops-policy-feedback",
		workspace.SanitizeComponent(sourceType),
		workspace.SanitizeComponent(sourceID)+".json",
	)
}

func readPolicyViolationFeedback(workspaceRoot string, t task.Task) (*policyViolationFeedback, string, error) {
	path := policyViolationFeedbackPath(workspaceRoot, t)
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, path, nil
		}
		return nil, path, err
	}
	var feedback policyViolationFeedback
	if err := json.Unmarshal(body, &feedback); err != nil {
		return nil, path, err
	}
	if feedback.Count <= 0 {
		return nil, path, nil
	}
	return &feedback, path, nil
}

func writePolicyViolationFeedback(workspaceRoot string, t task.Task, err error) (*policyViolationFeedback, string, error) {
	path := policyViolationFeedbackPath(workspaceRoot, t)
	previous, _, readErr := readPolicyViolationFeedback(workspaceRoot, t)
	if readErr != nil {
		return nil, path, readErr
	}
	feedback := &policyViolationFeedback{
		IssueID:    t.ID,
		Identifier: t.SourceEventID,
		Count:      1,
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
		Summary:    ErrSummary(err),
	}
	if previous != nil {
		feedback.Count = previous.Count + 1
	}
	var perr *workspace.PolicyError
	if errors.As(err, &perr) {
		feedback.Violations = perr.Violations
		feedback.Summary = perr.Error()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, path, err
	}
	body, err := json.MarshalIndent(feedback, "", "  ")
	if err != nil {
		return nil, path, err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, append(body, '\n'), 0o644); err != nil {
		return nil, path, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, path, err
	}
	return feedback, path, nil
}

func clearPolicyViolationFeedback(workspaceRoot string, t task.Task) {
	_ = os.Remove(policyViolationFeedbackPath(workspaceRoot, t))
}

func appendPolicyViolationFeedback(prompt string, feedback *policyViolationFeedback, cfg workflow.Config) string {
	if feedback == nil || feedback.Count <= 0 {
		return prompt
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(prompt, "\n"))
	b.WriteString("\n\n---\n\n")
	b.WriteString("### Previous worker policy violation\n\n")
	b.WriteString("A previous attempt for this same issue was rejected by the worker policy. Keep the retry tightly scoped and do not repeat the broad diff.\n\n")
	b.WriteString(fmt.Sprintf("- Violation count: %d\n", feedback.Count))
	if cfg.Policy.MaxChangedFiles > 0 {
		b.WriteString(fmt.Sprintf("- max_changed_files: %d\n", cfg.Policy.MaxChangedFiles))
	}
	if limit := cfg.Policy.LineLimit(); limit > 0 {
		b.WriteString(fmt.Sprintf("- max_changed_lines: %d\n", limit))
	}
	if len(feedback.Violations) > 0 {
		b.WriteString("- Previous violations:\n")
		for _, violation := range feedback.Violations {
			b.WriteString("  - ")
			b.WriteString(string(violation.Kind))
			if violation.Path != "" {
				b.WriteString(" ")
				b.WriteString(violation.Path)
			}
			b.WriteString(": ")
			b.WriteString(violation.Message)
			b.WriteString("\n")
		}
	} else if feedback.Summary != "" {
		b.WriteString("- Previous violation summary: ")
		b.WriteString(feedback.Summary)
		b.WriteString("\n")
	}
	b.WriteString("- Before continuing, run a cheap diff-size check such as `git diff --shortstat` and `git diff --name-only` against the base branch.\n")
	// The terminality hint must match the actual runtime budget: with budget=0
	// (suppression disabled) there is no "next attempt stops" backstop, and
	// when the current count is still below budget-1 the next violation
	// only consumes one more slot. Steering the agent with a false
	// terminality assumption (the original hardcoded sentence) coaxed it
	// into unnecessary behavior — see #230 review.
	budget := cfg.Agent.PolicyViolationBudgetValue()
	switch {
	case budget == 0:
		b.WriteString("- agent.policy_violation_budget = 0: the worker will keep retrying repeated policy violations; aim for the smallest correct diff but do not panic over a single retry.\n")
	case feedback.Count+1 >= budget:
		b.WriteString("- Next repeated policy violation will stop without another retry.\n")
	default:
		b.WriteString(fmt.Sprintf("- Policy violation budget %d/%d already consumed; the run stops non-retryably once the count reaches %d.\n", feedback.Count, budget, budget))
	}
	return b.String()
}
