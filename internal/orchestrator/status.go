package orchestrator

import "time"

// RuntimeEventKind is the SPEC-aligned runtime event vocabulary used by the
// lightweight status surface. It describes orchestrator runtime state, not rows
// in the transitional queue.
type RuntimeEventKind string

const (
	RuntimeEventCandidate        RuntimeEventKind = "candidate"
	RuntimeEventRunning          RuntimeEventKind = "running"
	RuntimeEventCompleted        RuntimeEventKind = "completed"
	RuntimeEventFailed           RuntimeEventKind = "failed"
	RuntimeEventCandidateBlocked RuntimeEventKind = "blocked"
	RuntimeEventInputBlocked     RuntimeEventKind = "input_blocked"
)

// RuntimeEvent is an operator-facing event observed by the orchestrator runtime.
// Branch and PRURL are optional discoveries from agent output/events; their
// presence does not imply the worker created or pushed anything itself.
type RuntimeEvent struct {
	Kind       RuntimeEventKind `json:"kind"`
	IssueID    IssueID          `json:"issue_id,omitempty"`
	Identifier string           `json:"identifier,omitempty"`
	Message    string           `json:"message,omitempty"`
	Branch     string           `json:"branch,omitempty"`
	PRURL      string           `json:"pr_url,omitempty"`
	At         time.Time        `json:"at"`
}

// RecordEvent appends ev to the bounded in-memory event log.
func (s *OrchestratorState) RecordEvent(ev RuntimeEvent) {
	if s == nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	s.RecentEvents = append(s.RecentEvents, ev)
	const maxRuntimeEvents = 200
	if len(s.RecentEvents) > maxRuntimeEvents {
		copy(s.RecentEvents, s.RecentEvents[len(s.RecentEvents)-maxRuntimeEvents:])
		s.RecentEvents = s.RecentEvents[:maxRuntimeEvents]
	}
}
