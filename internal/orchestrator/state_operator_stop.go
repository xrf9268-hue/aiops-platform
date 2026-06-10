package orchestrator

// state_operator_stop.go holds the D35 operator-terminal-stop latch: the entry
// type plus the pure state-transition methods that record, query, and account
// for suppressed dispatches against it. The latch fields and their cap live on
// OrchestratorState (state.go); the dispatch/retry consumers are
// actor_dispatch.go and actor_retry.go.

import (
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// OperatorTerminalStopEntry is a process-local latch recording that this
// worker observed an operator move an issue to a terminal tracker state. D35
// makes that observation authoritative for this worker process so a later
// agent-side issueUpdate cannot re-activate and re-dispatch the same issue.
type OperatorTerminalStopEntry struct {
	Issue                 tracker.Issue
	Identifier            string
	State                 string
	StoppedAt             time.Time
	SuppressedDispatches  int
	FirstSuppressedAt     time.Time
	FirstSuppressedState  string
	FirstSuppressedReason string
}

func (s *OrchestratorState) RecordOperatorTerminalStop(id IssueID, issue tracker.Issue, at time.Time) (OperatorTerminalStopEntry, bool) {
	if s.OperatorTerminalStops == nil {
		s.OperatorTerminalStops = map[IssueID]*OperatorTerminalStopEntry{}
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	identifier := issue.Identifier
	if identifier == "" {
		identifier = string(id)
	}
	state := issue.State
	if existing, ok := s.OperatorTerminalStops[id]; ok {
		if strings.TrimSpace(existing.Identifier) == "" {
			existing.Identifier = identifier
		}
		if strings.TrimSpace(state) != "" {
			existing.State = state
			existing.Issue = issue
		}
		return *existing, false
	}
	entry := &OperatorTerminalStopEntry{
		Issue:      issue,
		Identifier: identifier,
		State:      state,
		StoppedAt:  at,
	}
	s.OperatorTerminalStops[id] = entry
	s.operatorTerminalStopOrder = append(s.operatorTerminalStopOrder, id)
	// Counted here, after the repeat early-return — intentionally unlike
	// recordCompleted, which counts every call. A re-observed stop is not a new stop.
	s.CumulativeOperatorTerminalStopsTotal++
	if s.MaxRecentOperatorTerminalStops > 0 && len(s.operatorTerminalStopOrder) > s.MaxRecentOperatorTerminalStops {
		oldest := s.operatorTerminalStopOrder[0]
		s.operatorTerminalStopOrder = s.operatorTerminalStopOrder[1:]
		delete(s.OperatorTerminalStops, oldest)
	}
	return *entry, true
}

func (s *OrchestratorState) IsOperatorTerminalStopped(id IssueID) bool {
	_, ok := s.OperatorTerminalStops[id]
	return ok
}

func (s *OrchestratorState) LookupOperatorTerminalStop(id IssueID) (OperatorTerminalStopEntry, bool) {
	entry, ok := s.OperatorTerminalStops[id]
	if !ok || entry == nil {
		return OperatorTerminalStopEntry{}, false
	}
	return *entry, true
}

func (s *OrchestratorState) RecordOperatorTerminalDispatchSuppressed(id IssueID, issue tracker.Issue, reason string) (OperatorTerminalStopEntry, bool) {
	entry, ok := s.OperatorTerminalStops[id]
	if !ok || entry == nil {
		return OperatorTerminalStopEntry{}, false
	}
	entry.SuppressedDispatches++
	first := entry.FirstSuppressedAt.IsZero()
	if first {
		entry.FirstSuppressedAt = time.Now().UTC()
		entry.FirstSuppressedState = issue.State
		entry.FirstSuppressedReason = reason
	}
	return *entry, first
}
