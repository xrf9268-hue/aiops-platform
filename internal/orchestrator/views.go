package orchestrator

import "time"

// This file holds the public projection types that StateView exposes for the
// SPEC §13.7 observability surface (the /api/v1/state and /api/v1/<id>
// handlers). They are split from the state machine so it and its read-only
// projections stay separately legible (#521 burn-down); the builders live
// alongside Snapshot in state_snapshot.go.

// RunningView is the per-running-entry projection in StateView. It
// omits unexported / non-serializable fields like CancelWorker and Done.
// SPEC §13.7.2 dictates the exposed field set; State / SessionID /
// TurnCount / LastEvent / LastMessage / Tokens were added per #209 so
// operators inspecting `/api/v1/state` or `/api/v1/<id>` can see live
// coding-agent activity without tailing logs.
type RunningView struct {
	IssueID           IssueID
	Identifier        string
	IssueURL          string
	State             string
	SessionID         string
	TurnCount         int
	LastEvent         string
	LastMessage       string
	StartedAt         time.Time
	LastEventAt       time.Time
	RetryAttempt      *int
	WorkspacePath     string
	Tokens            TokensView
	CodexAppServerPID int
	// AgentProvider / AgentModel expose the runtime and resolved model driving
	// this claim (#977); empty until the runner reports them.
	AgentProvider string
	AgentModel    string
}

// TokensView mirrors the SPEC §13.7.2 per-issue `tokens` object: the
// input/output/total triple of cumulative codex tokens observed for the
// active session.
type TokensView struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

// BlockedView is the public projection of a blocked claim.
type BlockedView struct {
	IssueID           IssueID
	Identifier        string
	IssueURL          string
	State             string
	BlockedAt         time.Time
	WorkspacePath     string
	SessionID         string
	LastEventAt       time.Time
	Method            string
	Error             string
	CodexAppServerPID int
}

// RetryView is the per-retry-entry projection in StateView. Omits the
// *time.Timer handle because it is not meaningful outside the process.
type RetryView struct {
	IssueID    IssueID
	Identifier string
	IssueURL   string
	Attempt    int
	DueAt      time.Time
	Error      string
	Kind       RetryKind
}

type OperatorTerminalStopView struct {
	IssueID               IssueID
	Identifier            string
	State                 string
	StoppedAt             time.Time
	SuppressedDispatches  int
	FirstSuppressedAt     time.Time
	FirstSuppressedState  string
	FirstSuppressedReason string
}
