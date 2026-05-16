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

// Event kinds emitted by the queue store and the worker. Keep these in sync
// with the docs in docs/runbooks/task-api.md and the cmd/worker stage helpers.
const (
	EventEnqueued           = "enqueued"
	EventClaimed            = "claimed"
	EventWorkflowResolved   = "workflow_resolved"
	EventRunnerStart        = "runner_start"
	EventRunnerEnd          = "runner_end"
	EventRunnerTimeout      = "runner_timeout"
	EventVerifyStart        = "verify_start"
	EventVerifyEnd          = "verify_end"
	EventPush               = "push"
	EventPRCreated          = "pr_created"
	EventPRReused           = "pr_reused"
	EventSucceeded          = "succeeded"
	EventFailedAttempt      = "failed_attempt"
	EventReconcileStart     = "reconcile_start"
	EventReconcileWorkspace = "reconcile_workspace"
	EventReconcileEnd       = "reconcile_end"

	EventTrackerTransition      = "tracker_transition"
	EventTrackerTransitionError = "tracker_transition_error"
	EventTrackerComment         = "tracker_comment"
)

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
}

type Event struct {
	ID        int64           `json:"id"`
	TaskID    string          `json:"task_id"`
	EventType string          `json:"event_type"`
	Message   string          `json:"message,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}
