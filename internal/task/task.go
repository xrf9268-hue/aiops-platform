package task

import (
	"encoding/json"
	"errors"
	"time"
)

var ErrNotFound = errors.New("task not found")

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

// RunAttemptPhase is the SPEC §7.2 run-attempt lifecycle vocabulary.
type RunAttemptPhase string

const (
	PhasePreparingWorkspace       RunAttemptPhase = "PreparingWorkspace"
	PhaseBuildingPrompt           RunAttemptPhase = "BuildingPrompt"
	PhaseLaunchingAgentProcess    RunAttemptPhase = "LaunchingAgentProcess"
	PhaseInitializingSession      RunAttemptPhase = "InitializingSession"
	PhaseStreamingTurn            RunAttemptPhase = "StreamingTurn"
	PhaseFinishing                RunAttemptPhase = "Finishing"
	PhaseSucceeded                RunAttemptPhase = "Succeeded"
	PhaseFailed                   RunAttemptPhase = "Failed"
	PhaseTimedOut                 RunAttemptPhase = "TimedOut"
	PhaseStalled                  RunAttemptPhase = "Stalled"
	PhaseCanceledByReconciliation RunAttemptPhase = "CanceledByReconciliation"
)

// RunAttemptPhases returns the SPEC §7.2 phases in normative order.
func RunAttemptPhases() []RunAttemptPhase {
	return []RunAttemptPhase{
		PhasePreparingWorkspace,
		PhaseBuildingPrompt,
		PhaseLaunchingAgentProcess,
		PhaseInitializingSession,
		PhaseStreamingTurn,
		PhaseFinishing,
		PhaseSucceeded,
		PhaseFailed,
		PhaseTimedOut,
		PhaseStalled,
		PhaseCanceledByReconciliation,
	}
}

// Event kinds emitted by the queue store and the worker. Keep these in sync
// with the docs in docs/runbooks/task-api.md and the cmd/worker stage helpers.
const (
	EventRunPhaseTransition = "run_phase_transition"

	EventSessionStarted       = "session_started"
	EventStartupFailed        = "startup_failed"
	EventTurnStarted          = "turn_started"
	EventTurnCompleted        = "turn_completed"
	EventTurnFailed           = "turn_failed"
	EventTurnCancelled        = "turn_cancelled"
	EventTurnEndedWithError   = "turn_ended_with_error"
	EventTurnInputRequired    = "turn_input_required"
	EventApprovalAutoApproved = "approval_auto_approved"
	EventUnsupportedToolCall  = "unsupported_tool_call"
	// EventToolCallMutation records a Linear GraphQL mutation dispatched
	// through the agent-visible linear_graphql tool (SPEC §15.5 harness
	// narrowing, #298). The payload carries the mutation field name only
	// and NEVER the query body or variables, so prompt-injected query
	// strings are not echoed onto the operator-visible status surface
	// (see #295).
	EventToolCallMutation = "tool_call_mutation"
	EventNotification     = "notification"
	EventOtherMessage     = "other_message"
	EventMalformed        = "malformed"

	EventEnqueued           = "enqueued"
	EventClaimed            = "claimed"
	EventWorkflowResolved   = "workflow_resolved"
	EventWorkspaceHookStart = "hook_start"
	EventWorkspaceHookEnd   = "hook_end"
	EventRunnerStart        = "runner_start"
	EventRunnerEnd          = "runner_end"
	// EventRunnerStopped marks a run the orchestrator stopped because its
	// tracker issue left the active set (eligibility reconcile / agent PR
	// handoff), distinct from runner_end's success/failure so it is not counted
	// as either (SPEC §7.3 supervised stop; #543).
	EventRunnerStopped        = "runner_stopped"
	EventRunnerTimeout        = "runner_timeout"
	EventStalled              = "stalled"
	EventPRReused             = "pr_reused"
	EventSucceeded            = "succeeded"
	EventFailedAttempt        = "failed_attempt"
	EventReconcileStart       = "reconcile_start"
	EventReconcileWorkspace   = "reconcile_workspace"
	EventReconcileEnd         = "reconcile_end"
	EventWorkflowReloaded     = "workflow_reload"
	EventWorkflowReloadFailed = "workflow_reload_failed"

	// Tracker transition events are implementation extensions retained for
	// operator visibility while tracker writes remain tool-driven.
	EventTrackerTransition      = "tracker_transition"
	EventTrackerTransitionError = "tracker_transition_error"
	EventTrackerComment         = "tracker_comment"
)

// RuntimeEvents returns the app-server runtime event vocabulary this
// implementation forwards or synthesizes into task events.
func RuntimeEvents() []string {
	return []string{
		EventSessionStarted,
		EventStartupFailed,
		EventTurnStarted,
		EventTurnCompleted,
		EventTurnFailed,
		EventTurnCancelled,
		EventTurnEndedWithError,
		EventTurnInputRequired,
		EventApprovalAutoApproved,
		EventUnsupportedToolCall,
		EventToolCallMutation,
		EventNotification,
		EventOtherMessage,
		EventMalformed,
	}
}

type PhaseTransition struct {
	Event string          `json:"event"`
	From  RunAttemptPhase `json:"from,omitempty"`
	To    RunAttemptPhase `json:"to"`
}

func PhaseTransitionEvent(from, to RunAttemptPhase) PhaseTransition {
	return PhaseTransition{Event: EventRunPhaseTransition, From: from, To: to}
}

// RuntimeEvent is an app-server event emitted by an agent runtime, or a
// runner-synthesized marker for the same live session, forwarded into the task
// event stream by the worker.
type RuntimeEvent struct {
	Event   string `json:"event"`
	Payload any    `json:"payload,omitempty"`
}

type Task struct {
	ID            string    `json:"id"`
	Status        Status    `json:"status"`
	SourceType    string    `json:"source_type"`
	SourceEventID string    `json:"source_event_id"`
	RepoOwner     string    `json:"repo_owner"`
	RepoName      string    `json:"repo_name"`
	CloneURL      string    `json:"clone_url"`
	BaseBranch    string    `json:"base_branch"`
	WorkBranch    string    `json:"work_branch"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	Actor         string    `json:"actor"`
	Model         string    `json:"model"`
	Priority      int       `json:"priority"`
	Attempts      int       `json:"attempts"`
	MaxAttempts   int       `json:"max_attempts"`
	AvailableAt   time.Time `json:"available_at"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	// IssueRender is the SPEC §4.1.1 normalized issue snapshot, pre-built at
	// dispatch time so the worker's runtask can populate the prompt
	// template's `issue` variable without re-importing tracker.Issue. SPEC
	// §12.1 requires the full normalized field set to be visible to the
	// template; §5.4 fails rendering on unknown variables, so a workflow
	// referencing `issue.priority` or `{% for label in issue.labels %}`
	// would crash without this. Excluded from JSON because it is
	// reconstructed per dispatch.
	IssueRender map[string]any `json:"-"`
}

type Event struct {
	ID        int64           `json:"id"`
	TaskID    string          `json:"task_id"`
	EventType string          `json:"event_type"`
	Message   string          `json:"message,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}
