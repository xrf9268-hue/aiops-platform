package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestLogRescheduleErrSuppressesBenignShutdown(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "nil", err: nil},
		{name: "context canceled", err: context.Canceled},
		{name: "wrapped context canceled", err: fmt.Errorf("submit stopped: %w", context.Canceled)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var o Orchestrator
			got := captureOrchestratorLog(t, func() {
				o.logRescheduleErr(tt.err, IssueID("issue-636"), "ENG-636")
			})
			if got != "" {
				t.Fatalf("logRescheduleErr(%v) log = %q; want empty", tt.err, got)
			}
		})
	}
}

func TestLogRescheduleErrEmitsDroppedRetryDiagnostic(t *testing.T) {
	var o Orchestrator
	got := captureOrchestratorLog(t, func() {
		o.logRescheduleErr(errors.New("actor unavailable"), IssueID("issue-636"), "ENG-636")
	})

	for _, want := range []string{
		"event=reschedule_submit_failed",
		"issue_id=issue-636",
		"issue_identifier=ENG-636",
		`error="actor unavailable"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("logRescheduleErr unexpected log = %q; want substring %q", got, want)
		}
	}
}
