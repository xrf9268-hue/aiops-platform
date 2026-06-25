// Package stateapi defines the /api/v1/state response wire DTOs (SPEC §13.7.2).
//
// It is the single source of truth for the JSON contract shared by the worker
// (producer, cmd/worker) and the TUI (consumer, cmd/tui): a field added here
// surfaces in both binaries from one definition, so a new field can no longer
// be added to the producer while the consumer silently never renders it, and a
// JSON-tag typo can no longer drop data on only one side (#793).
//
// It deliberately holds only the wire shape — plain string / map[string]any /
// time.Time, no internal/orchestrator import — so the thin TUI binary need not
// pull in the scheduler core to decode a status snapshot. The worker's
// orchestrator.StateView -> wire mappers stay in cmd/worker (single-consumer
// projection); the /api/v1/<issue> and error DTOs stay there too for the same
// reason.
package stateapi

import "time"

// StateResponse is the /api/v1/state body (SPEC §13.7.2).
type StateResponse struct {
	// Version is the worker build stamp (the -ldflags -X main.version value, the
	// VCS-revision fallback, or "devel"), so a bug report can name the exact
	// build. omitempty drops the key only when the resolved version is empty —
	// which the "devel" default normally prevents (#796).
	Version string `json:"version,omitempty"`
	// AgentDefault is the worker's configured default runner/provider
	// (`agent.default`, e.g. "codex-app-server"), surfaced as the dashboard's
	// top-summary worker default provider (#977). The model is resolved per-run
	// by the agent (see Running.AgentModel), so there is no worker-default
	// model. omitempty keeps older payloads (and the mock/unconfigured default)
	// from forcing the key; the dashboard renders a missing value as "unknown".
	AgentDefault               string         `json:"agent_default,omitempty"`
	GeneratedAt                time.Time      `json:"generated_at"`
	PollIntervalMs             int64          `json:"poll_interval_ms"`
	MaxConcurrentAgents        int            `json:"max_concurrent_agents"`
	MaxConcurrentAgentsByState map[string]int `json:"max_concurrent_agents_by_state,omitempty"`
	Counts                     Counts         `json:"counts"`
	Running                    []Running      `json:"running"`
	Blocked                    []Blocked      `json:"blocked"`
	Retrying                   []Retry        `json:"retrying"`
	Completed                  []string       `json:"completed"`
	// ReconcileStoppedWithProgress lists reconcile-stopped runs that had completed ≥1
	// agent turn (made progress — usually the agent's handoff, but turn_completed
	// fires after every turn, so it can also be a run stopped after an intermediate
	// turn; inspect to confirm, it is not a guaranteed success) before the per-tick
	// reconcile reaped them — surfaced so a progressed-but-reaped run is visible
	// rather than absent from completed (#557). It does not overlap
	// completed: a reconcile-stopped run is not a clean §16.5 exit, matching
	// upstream's accounting, so completed stays unchanged.
	ReconcileStoppedWithProgress []string               `json:"reconcile_stopped_with_progress"`
	AgentHandoffReconcileStopped []string               `json:"agent_handoff_reconcile_stopped"`
	OperatorTerminalStops        []OperatorTerminalStop `json:"operator_terminal_stops"`
	CodexTotals                  CodexTotals            `json:"codex_totals"`
	// RateLimits is the latest Codex rate-limit payload (SPEC §13.7.2). It
	// is emitted unconditionally — `null` until a `rate_limit_updated`
	// notification is observed — so operators can rely on the key always
	// being present (upstream parity, #328). A nil map marshals to JSON
	// null, not an omitted key.
	RateLimits map[string]any `json:"rate_limits"`
}

// CodexTotals mirrors SPEC §13.7.2's aggregate `codex_totals` object.
type CodexTotals struct {
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	TotalTokens    int64   `json:"total_tokens"`
	SecondsRunning float64 `json:"seconds_running"`
}

// Counts mirrors SPEC §13.7.2's `counts` object.
type Counts struct {
	Running int `json:"running"`
	Blocked int `json:"blocked"`
	// Retrying is the current retry-backoff queue depth.
	Retrying int `json:"retrying"`
	// Completed is the size of the FIFO-bounded recent-completed set
	// (the same set published as `state.completed`). For lifetime
	// totals across worker restarts and FIFO evictions, use
	// completed_total. SPEC §13.7 §4.1.8.
	Completed int `json:"completed"`
	// CompletedTotal is a monotonic counter of every observed Succeeded
	// transition since process start, independent of FIFO eviction. Added
	// for #234 so long-running deployments still expose a true lifetime
	// number when the bounded set has rotated.
	CompletedTotal int64 `json:"completed_total"`
	// ReconcileStoppedWithProgress is the size of the FIFO-bounded recent set of
	// reconcile-stopped runs that had made progress (≥1 completed turn; the same set
	// published as `state.reconcile_stopped_with_progress`);
	// ReconcileStoppedWithProgressTotal is the lifetime monotonic counter that
	// survives FIFO eviction (#557).
	ReconcileStoppedWithProgress      int   `json:"reconcile_stopped_with_progress"`
	ReconcileStoppedWithProgressTotal int64 `json:"reconcile_stopped_with_progress_total"`
	AgentHandoffReconcileStopped      int   `json:"agent_handoff_reconcile_stopped"`
	AgentHandoffReconcileStoppedTotal int64 `json:"agent_handoff_reconcile_stopped_total"`
	// OperatorTerminalStops is the size of the FIFO-bounded recent D35 latch set
	// (the same set published as `state.operator_terminal_stops`).
	// OperatorTerminalStopsTotal is the lifetime monotonic counter that survives
	// the #667 cap eviction, so a long-running worker that has rotated past the
	// bound still exposes the true number of distinct operator-terminal-stops.
	OperatorTerminalStops      int   `json:"operator_terminal_stops"`
	OperatorTerminalStopsTotal int64 `json:"operator_terminal_stops_total"`
}

// Running mirrors SPEC §13.7.2's per-running-row object.
type Running struct {
	IssueID    string `json:"issue_id"`
	Identifier string `json:"issue_identifier,omitempty"`
	// IssueURL is the tracker-provided issue URL, emitted only when available
	// (SPEC §13.7 SHOULD). omitempty keeps URL-less rows (mock tracker) clean.
	IssueURL string `json:"issue_url,omitempty"`
	// State / SessionID / TurnCount / LastEvent / LastMessage are part of
	// the SPEC §13.7.2 running-row contract — the sample literally shows
	// `"last_message": ""` and `"turn_count": 7`, so a freshly-dispatched
	// run with zero/empty values must still emit the keys. omitempty would
	// let consumers confuse "known zero/empty" with "field missing".
	State             string        `json:"state"`
	SessionID         string        `json:"session_id"`
	TurnCount         int           `json:"turn_count"`
	LastEvent         string        `json:"last_event"`
	LastMessage       string        `json:"last_message"`
	StartedAt         *time.Time    `json:"started_at,omitempty"`
	LastEventAt       *time.Time    `json:"last_event_at,omitempty"`
	RetryAttempt      *int          `json:"retry_attempt,omitempty"`
	WorkspacePath     string        `json:"workspace_path,omitempty"`
	Tokens            RunningTokens `json:"tokens"`
	CodexAppServerPID int           `json:"codex_app_server_pid,omitempty"`
	// AgentProvider is the runtime/runner driving this claim (e.g.
	// "codex-app-server") and AgentModel the resolved model (e.g.
	// "gpt-5.3-codex-spark"), so an operator can tell which model/runtime
	// produced a run rather than reconstructing it from workflow files or logs
	// (#977). omitempty drops the key when unknown; the dashboard renders a
	// missing value as "unknown", so older payloads stay usable.
	AgentProvider string `json:"agent_provider,omitempty"`
	AgentModel    string `json:"agent_model,omitempty"`
	// WorkflowSource / WorkflowPath identify which WORKFLOW.md (the profile, e.g.
	// reviewer vs maker) produced this run, so an operator running maker +
	// reviewer workflows can correlate a failure with the workflow that produced
	// it, not just the model (#983). WorkflowSource is one of file / prompt_only
	// / default; WorkflowPath is absent when the run used built-in defaults
	// (Source == default). omitempty drops each key when unknown; the dashboard
	// renders a missing value as "unknown", so older payloads stay usable.
	WorkflowSource string `json:"workflow_source,omitempty"`
	WorkflowPath   string `json:"workflow_path,omitempty"`
}

// RunningTokens mirrors SPEC §13.7.2's per-running-row `tokens` object.
type RunningTokens struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// Blocked mirrors SPEC §13.7.2's per-blocked-row object.
type Blocked struct {
	IssueID           string     `json:"issue_id"`
	Identifier        string     `json:"issue_identifier,omitempty"`
	IssueURL          string     `json:"issue_url,omitempty"`
	State             string     `json:"state,omitempty"`
	BlockedAt         *time.Time `json:"blocked_at,omitempty"`
	WorkspacePath     string     `json:"workspace_path,omitempty"`
	SessionID         string     `json:"session_id,omitempty"`
	LastEventAt       *time.Time `json:"last_event_at,omitempty"`
	Method            string     `json:"method,omitempty"`
	Error             string     `json:"error,omitempty"`
	CodexAppServerPID int        `json:"codex_app_server_pid,omitempty"`
}

// Retry mirrors SPEC §13.7.2's per-retry-row object. Kind is the wire form of
// orchestrator.RetryKind (a string enum).
type Retry struct {
	IssueID        string          `json:"issue_id"`
	Identifier     string          `json:"issue_identifier,omitempty"`
	IssueURL       string          `json:"issue_url,omitempty"`
	Attempt        int             `json:"attempt"`
	DueAt          *time.Time      `json:"due_at,omitempty"`
	Error          string          `json:"error,omitempty"`
	Kind           string          `json:"kind"`
	StartupFailure *StartupFailure `json:"startup_failure,omitempty"`
}

// StartupFailure is the wire form of the runner startup phase that failed
// before the issue entered a usable app-server session.
type StartupFailure struct {
	Phase string `json:"phase"`
	Error string `json:"error,omitempty"`
}

// OperatorTerminalStop mirrors SPEC §13.7.2's per-operator-terminal-stop row.
type OperatorTerminalStop struct {
	IssueID               string     `json:"issue_id"`
	Identifier            string     `json:"issue_identifier,omitempty"`
	State                 string     `json:"state,omitempty"`
	StoppedAt             *time.Time `json:"stopped_at,omitempty"`
	SuppressedDispatches  int        `json:"suppressed_dispatches"`
	FirstSuppressedAt     *time.Time `json:"first_suppressed_at,omitempty"`
	FirstSuppressedState  string     `json:"first_suppressed_state,omitempty"`
	FirstSuppressedReason string     `json:"first_suppressed_reason,omitempty"`
}
