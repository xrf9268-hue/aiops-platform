package orchestrator

import (
	"testing"
	"time"
)

// TestResolveDispatchClaim_Table characterizes every branch of the dispatch
// claim gate extracted from (*dispatchOp).apply: fresh vs tracker-rechecked,
// present vs absent retry entry, consumable (due continuation / external
// blocker) vs non-consumable (wrong kind / not due) entry, and the
// already-claimed deny paths. The tracker-rechecked + absent + claimed deny had
// no dedicated coverage before this extraction.
func TestResolveDispatchClaim_Table(t *testing.T) {
	const id = IssueID("ENG-DISP")
	due := time.Now().Add(-time.Hour)
	notDue := time.Now().Add(time.Hour)
	entry := func(kind RetryKind, attempt int, dueAt time.Time) *RetryEntry {
		return &RetryEntry{IssueID: id, Identifier: string(id), Kind: kind, Attempt: attempt, DueAt: dueAt, ContinuationTurnCount: 4}
	}
	cases := []struct {
		name              string
		rechecked         bool
		setup             func(st *OrchestratorState)
		wantConsumed      bool
		wantConsumedKind  RetryKind // checked only when wantConsumed
		wantContAttempt   int
		wantContTurnCount int
		wantDeny          bool
	}{
		{name: "fresh unclaimed proceeds", rechecked: false, setup: func(*OrchestratorState) {}, wantDeny: false},
		{name: "fresh claimed denied", rechecked: false, setup: func(st *OrchestratorState) { st.Claimed[id] = struct{}{} }, wantDeny: true},
		{name: "rechecked absent unclaimed proceeds", rechecked: true, setup: func(*OrchestratorState) {}, wantDeny: false},
		{name: "rechecked absent claimed denied", rechecked: true, setup: func(st *OrchestratorState) { st.Claimed[id] = struct{}{} }, wantDeny: true},
		{name: "rechecked failure retry denied", rechecked: true, setup: func(st *OrchestratorState) {
			st.RetryAttempts[id] = entry(RetryKindFailure, 2, due)
		}, wantDeny: true},
		{name: "rechecked quota retry denied", rechecked: true, setup: func(st *OrchestratorState) {
			st.RetryAttempts[id] = entry(RetryKindQuotaBackoff, 1, due)
		}, wantDeny: true},
		{name: "rechecked due continuation consumed", rechecked: true, setup: func(st *OrchestratorState) {
			st.RetryAttempts[id] = entry(RetryKindContinuation, 3, due)
		}, wantConsumed: true, wantConsumedKind: RetryKindContinuation, wantContAttempt: 3, wantContTurnCount: 4, wantDeny: false},
		{name: "rechecked due continuation attempt zero", rechecked: true, setup: func(st *OrchestratorState) {
			st.RetryAttempts[id] = entry(RetryKindContinuation, 0, due)
		}, wantConsumed: true, wantConsumedKind: RetryKindContinuation, wantContAttempt: 0, wantContTurnCount: 4, wantDeny: false},
		{name: "rechecked not-due continuation denied", rechecked: true, setup: func(st *OrchestratorState) {
			st.RetryAttempts[id] = entry(RetryKindContinuation, 3, notDue)
		}, wantDeny: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := NewOrchestratorState(15000, 10)
			tc.setup(st)
			consumed, contAttempt, contTurnCount, deny := resolveDispatchClaim(st, id, tc.rechecked)
			if deny != tc.wantDeny {
				t.Fatalf("resolveDispatchClaim deny = %v; want %v", deny, tc.wantDeny)
			}
			switch {
			case tc.wantConsumed && (consumed == nil || consumed.Kind != tc.wantConsumedKind):
				t.Fatalf("resolveDispatchClaim consumed = %+v; want a %q entry", consumed, tc.wantConsumedKind)
			case !tc.wantConsumed && consumed != nil:
				t.Fatalf("resolveDispatchClaim consumed = %+v; want nil (no entry consumed)", consumed)
			}
			if contAttempt != tc.wantContAttempt {
				t.Fatalf("resolveDispatchClaim continuationAttempt = %d; want %d", contAttempt, tc.wantContAttempt)
			}
			if contTurnCount != tc.wantContTurnCount {
				t.Fatalf("resolveDispatchClaim continuationTurnCount = %d; want %d", contTurnCount, tc.wantContTurnCount)
			}
		})
	}
}
