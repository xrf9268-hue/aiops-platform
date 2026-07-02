package orchestrator

import "time"

// RuntimeEventKind is the SPEC-aligned runtime event vocabulary used by the
// lightweight status surface. It describes orchestrator runtime state.
type RuntimeEventKind string

const (
	RuntimeEventCandidate    RuntimeEventKind = "candidate"
	RuntimeEventRunning      RuntimeEventKind = "running"
	RuntimeEventCompleted    RuntimeEventKind = "completed"
	RuntimeEventFailed       RuntimeEventKind = "failed"
	RuntimeEventInputBlocked RuntimeEventKind = "input_blocked"
	// RuntimeEventContinuationBudgetBlocked marks D34's local harness-hardening
	// deviation: a still-active issue exhausted its cumulative clean-turn budget
	// and was parked in Blocked instead of being re-dispatched indefinitely.
	RuntimeEventContinuationBudgetBlocked              RuntimeEventKind = "continuation_budget_blocked"
	RuntimeEventOperatorTerminalStop                   RuntimeEventKind = "operator_terminal_stop"
	RuntimeEventOperatorTerminalStopDispatchSuppressed RuntimeEventKind = "operator_terminal_stop_dispatch_suppressed"
	// RuntimeEventReconcileStopped marks a run the per-tick reconcile stopped
	// after it had completed ≥1 agent turn (made progress — usually the agent's
	// handoff). Recorded so the run appears in the event stream and is drillable
	// by IDENTIFIER (not just global id) via /api/v1/<issue>, mirroring how
	// completed/failed surface there — the reconcile-cancel finalize path records
	// no completed/failed event otherwise, so an id in reconcile_stopped_with_progress
	// would only be resolvable by global id (#557). The string value matches the
	// /api/v1 status so string(kind) maps straight through.
	RuntimeEventReconcileStopped RuntimeEventKind = "reconcile_stopped_with_progress"
	// RuntimeEventAgentHandoffReconcileStopped marks a reconcile-stopped run
	// that observed a current-issue state handoff through an agent-visible
	// tracker mutation tool (linear_graphql, linear_ai_workpad,
	// gitea_issue_labels — #748) before the tracker made the issue inactive.
	// This is a scheduler/reader observability signal only: completed accounting
	// stays separate from reconcile-stopped handoffs, and tracker writes remain
	// agent-owned.
	RuntimeEventAgentHandoffReconcileStopped RuntimeEventKind = "agent_handoff_reconcile_stopped"
	// RuntimeEventActiveSuccessNoHandoff marks a clean worker exit whose issue
	// still has the active tracker state last observed by the orchestrator, with
	// no guarded current-issue handoff event. The scheduler still queues the
	// normal continuation; this is an operator-visible "no handoff yet" outcome,
	// not a tracker write.
	RuntimeEventActiveSuccessNoHandoff RuntimeEventKind = "active_success_no_handoff"
	// RuntimeEventBudgetExceeded marks a local worker budget guardrail stopping
	// a run before it can continue burning tokens/runtime. It is observability
	// only; tracker writes and PR handoff remain agent-owned.
	RuntimeEventBudgetExceeded RuntimeEventKind = "budget_exceeded"
	// RuntimeEventDispatchPreflightFailed flags SPEC §8.1 step 2 failures:
	// the per-tick dispatch preflight could not validate the workflow's
	// tracker / agent / API-key config, so the orchestrator skipped
	// candidate fetch/sort/dispatch for the tick. The Message field carries
	// the joined preflight reason(s); IssueID is empty because the gate is
	// not scoped to a single issue.
	RuntimeEventDispatchPreflightFailed RuntimeEventKind = "dispatch_preflight_failed"
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
