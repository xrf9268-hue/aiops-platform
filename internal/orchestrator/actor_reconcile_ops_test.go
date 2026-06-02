package orchestrator

// actor_reconcile_ops_test.go pins the two smallest #499 reconcile ops —
// refreshActiveTrackerIssuesOp and reconcileStalledRunsOp — at their apply
// boundary before #499 decomposes them. Both are exercised here directly on an
// OrchestratorState (no live actor), which pins the per-collection refresh
// actions and the stall-timing reference the integration suite only samples.

import (
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// TestRefreshActiveTrackerIssuesOpApply pins that the refresh pass updates the
// stored issue + ClaimedIssues snapshot for every in-process entry still
// observed active (running/retry/blocked), and leaves an entry absent from the
// (possibly partial) refresh listing untouched.
func TestRefreshActiveTrackerIssuesOpApply(t *testing.T) {
	st := NewOrchestratorState(15000, 10)
	st.Running[IssueID("R1")] = &RunningEntry{Identifier: "R1", Issue: tracker.Issue{ID: "R1", Identifier: "R1", State: "In Progress"}}
	st.RetryAttempts[IssueID("T1")] = &RetryEntry{IssueID: IssueID("T1"), Identifier: "T1", Issue: tracker.Issue{ID: "T1", Identifier: "T1", State: "In Progress"}}
	st.Blocked[IssueID("B1")] = &BlockedEntry{Identifier: "B1", Issue: tracker.Issue{ID: "B1", Identifier: "B1", State: "In Progress"}}
	// Absent from the (possibly partial) refresh listing: "no information", not
	// inactive — must be left untouched.
	st.Running[IssueID("R3")] = &RunningEntry{Identifier: "R3", Issue: tracker.Issue{ID: "R3", Identifier: "R3", State: "In Progress"}}

	refreshed := map[string]tracker.Issue{
		"R1": {ID: "R1", Identifier: "R1", State: "Rework"},
		"T1": {ID: "T1", Identifier: "T1", State: "Rework"},
		"B1": {ID: "B1", Identifier: "B1", State: "Rework"},
		// R3 intentionally omitted (absent from the listing).
	}
	done := make(chan struct{}, 1)
	op := &refreshActiveTrackerIssuesOp{issuesByID: refreshed, activeStates: normalizedStates([]string{"In Progress", "Rework"}), done: done}
	op.apply(st)()

	for _, id := range []IssueID{"R1", "T1", "B1"} {
		if got := st.ClaimedIssues[id].State; got != "Rework" {
			t.Errorf("ClaimedIssues[%s].State = %q; want Rework (refreshed)", id, got)
		}
	}
	if got := st.Running[IssueID("R1")].Issue.State; got != "Rework" {
		t.Errorf("running R1 Issue.State = %q; want Rework (refreshRunningIssue must update it)", got)
	}
	if got := st.RetryAttempts[IssueID("T1")].Issue.State; got != "Rework" {
		t.Errorf("retry T1 Issue.State = %q; want Rework (refresh must update it)", got)
	}
	if got := st.Blocked[IssueID("B1")].Issue.State; got != "Rework" {
		t.Errorf("blocked B1 Issue.State = %q; want Rework (refresh must update it)", got)
	}
	if got := st.Running[IssueID("R3")].Issue.State; got != "In Progress" {
		t.Errorf("running R3 Issue.State = %q; want In Progress (absent from listing -> untouched)", got)
	}
	if _, ok := st.ClaimedIssues[IssueID("R3")]; ok {
		t.Errorf("ClaimedIssues[R3] was set; want untouched (absent from listing)")
	}
	select {
	case <-done:
	default:
		t.Errorf("apply followup did not close done")
	}
}

// TestReconcileStalledRunsOpTimingReference pins the stall-timing reference: a
// run is stalled when now - (LastEventAt, or StartedAt before any event) exceeds
// the budget, and a run with neither timestamp is skipped rather than anchored
// at the zero time (which would cancel every fresh fixture).
func TestReconcileStalledRunsOpTimingReference(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name         string
		last         time.Time
		started      time.Time
		wantCanceled bool
	}{
		{"stalled by LastEventAt", now.Add(-time.Hour), now.Add(-2 * time.Hour), true},
		{"stalled by StartedAt when no event observed", time.Time{}, now.Add(-time.Hour), true},
		{"within budget", now.Add(-time.Millisecond), now.Add(-time.Hour), false},
		// Boundary pair pinning strict-greater-than (elapsed == budget is NOT
		// stalled; one tick over is): catches a `>` -> `>=` regression.
		{"exactly at budget is not stalled", now.Add(-time.Minute), time.Time{}, false},
		{"one tick over budget is stalled", now.Add(-time.Minute - time.Nanosecond), time.Time{}, true},
		{"skipped when no timing reference", time.Time{}, time.Time{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := NewOrchestratorState(15000, 10)
			st.Running[IssueID("S")] = &RunningEntry{Identifier: "S", LastEventAt: tt.last, StartedAt: tt.started, CancelWorker: func(error) {}}
			result := make(chan []*RunningEntry, 1)
			op := &reconcileStalledRunsOp{timeout: time.Minute, now: now, result: result}
			op.apply(st)()
			canceled := <-result
			if got := len(canceled) == 1; got != tt.wantCanceled {
				t.Errorf("reconcileStalledRuns(last=%v, started=%v) canceled=%v (n=%d); want canceled=%v",
					tt.last, tt.started, got, len(canceled), tt.wantCanceled)
			}
		})
	}
}
