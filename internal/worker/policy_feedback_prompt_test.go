package worker

import (
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// TestAppendPolicyViolationFeedbackTerminalityMatchesBudget pins #230 review:
// the prompt augmentation must describe the actual runtime stop condition for
// the configured `agent.policy_violation_budget`. The hardcoded "next
// violation stops" sentence is correct only when the next attempt would hit
// the cap; a budget of 0 has no stop condition; a higher budget leaves
// headroom that should be communicated to the agent.
func TestAppendPolicyViolationFeedbackTerminalityMatchesBudget(t *testing.T) {
	feedback := &policyViolationFeedback{Count: 1}

	cases := []struct {
		name        string
		budget      *int
		wantContain string
		wantAbsent  []string
	}{
		{
			name:        "default budget 2, count 1: next attempt would hit cap",
			budget:      nil,
			wantContain: "Next repeated policy violation will stop",
			wantAbsent:  []string{"budget = 0", "budget 1/"},
		},
		{
			name:        "explicit budget 5, count 1: 3 more attempts remain",
			budget:      ptrInt(5),
			wantContain: "Policy violation budget 1/5 already consumed",
			wantAbsent:  []string{"Next repeated policy violation will stop", "budget = 0"},
		},
		{
			name:        "budget 0 disables suppression entirely",
			budget:      ptrInt(0),
			wantContain: "agent.policy_violation_budget = 0",
			wantAbsent:  []string{"Next repeated policy violation will stop", "budget 1/"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := workflow.Config{Agent: workflow.AgentConfig{PolicyViolationBudget: tc.budget}}
			out := appendPolicyViolationFeedback("base prompt", feedback, cfg)
			if !strings.Contains(out, tc.wantContain) {
				t.Fatalf("prompt missing %q\n---\n%s", tc.wantContain, out)
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(out, absent) {
					t.Fatalf("prompt unexpectedly contains %q\n---\n%s", absent, out)
				}
			}
		})
	}
}

func ptrInt(v int) *int { return &v }
