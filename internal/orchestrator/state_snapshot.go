package orchestrator

// state_snapshot.go is the SPEC §13.3 / §13.7 snapshot projection: StateView
// and the Snapshot() read path that publishes the orchestrator's in-memory
// state for /api/v1/state, CLI status, and tests. The projection row types
// (RunningView, BlockedView, RetryView, TokensView, OperatorTerminalStopView)
// live in views.go; the state being projected lives in state.go.

import (
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

// StateView is the SPEC §13.3 / §13.7 shape the orchestrator publishes
// for /api/v1/state, CLI status, and tests. It is intentionally a
// value type with slices so callers cannot mutate the orchestrator's
// internal maps through it. The HTTP wrapper is deferred (see the
// design doc's "HTTP surface" section) but Snapshot() ships now so
// tests have a stable read API.
type StateView struct {
	GeneratedAt                time.Time
	PollIntervalMs             int64
	MaxConcurrentAgents        int
	MaxConcurrentAgentsByState map[string]int
	// AgentDefault is the worker's configured default runner/provider
	// (`agent.default`), surfaced as the top-summary worker default on
	// /api/v1/state (#977).
	AgentDefault string

	Running  []RunningView
	Blocked  []BlockedView
	Retrying []RetryView
	// Completed is bounded by MaxRecentCompleted on the source state; the
	// slice here is FIFO-ordered (oldest first). For the lifetime total that
	// survives eviction, read CumulativeCompletedTotal.
	Completed []IssueID
	// ReconcileStoppedWithProgress is the FIFO-bounded recent set of reconcile-stopped
	// runs that had completed ≥1 agent turn (made progress — usually the agent's
	// handoff, but inspect to confirm) — surfaced so a progressed-but-reaped run is
	// visible rather than absent from Completed (#557).
	// CumulativeReconcileStoppedWithProgressTotal is the lifetime monotonic counter
	// that survives FIFO eviction.
	ReconcileStoppedWithProgress []IssueID
	// AgentHandoffReconcileStopped is the FIFO-bounded recent set of
	// reconcile-stopped runs that observed a guarded current-issue Linear state
	// handoff. It may overlap ReconcileStoppedWithProgress when the handoff also
	// completed a turn before reconcile reaped it.
	AgentHandoffReconcileStopped []IssueID
	// ActiveSuccessNoHandoff is the FIFO-bounded recent set of clean exits that
	// left the issue active with no guarded handoff signal. These exits still
	// re-dispatch through the normal continuation retry and also remain in
	// Completed per SPEC §16.6.
	ActiveSuccessNoHandoff []IssueID
	// OperatorTerminalStops is the FIFO-bounded recent set of D35 latches (oldest
	// first), capped by MaxRecentOperatorTerminalStops. For the lifetime total that
	// survives eviction, read CumulativeOperatorTerminalStopsTotal.
	OperatorTerminalStops []OperatorTerminalStopView
	// CompletedSessionUsage keeps the established API field name for clean,
	// failed, and reconcile-ineligible run outcomes. Blocked claims stay in Blocked.
	CompletedSessionUsage                       []SessionUsageView
	BudgetGuardrails                            BudgetGuardrailsView
	CumulativeCompletedTotal                    int64
	CumulativeReconcileStoppedWithProgressTotal int64
	CumulativeAgentHandoffReconcileStoppedTotal int64
	CumulativeActiveSuccessNoHandoffTotal       int64
	CumulativeOperatorTerminalStopsTotal        int64
	CodexTotals                                 CodexTotals
	CodexRateLimits                             *RateLimitSnapshot
	// RecentEvents is the bounded orchestrator-wide event log (capped at
	// MaxRuntimeEvents). It carries SPEC §13.7 operator-visible signals
	// like dispatch_preflight_failed that are not scoped to one issue.
	RecentEvents []RuntimeEvent
}

// Snapshot returns a read-only view of the orchestrator state. The
// returned slices are freshly allocated so the caller may sort or
// truncate them without affecting future snapshots. Map iteration
// order is unspecified in Go, but downstream consumers (the §13.7 HTTP
// handler, CLI status) sort by IssueID before display.
// snapshotRunningViews projects s.Running into RunningView rows. It deep-copies
// the RetryAttempt pointer so a snapshot consumer mutating the pointee cannot
// reach back into orchestrator state; the pointer-vs-nil distinction is the SPEC
// §4.1.5 first-run semantic, so it cannot be flattened to an int.
func (s *OrchestratorState) snapshotRunningViews(now time.Time) []RunningView {
	rows := make([]RunningView, 0, len(s.Running))
	for id, r := range s.Running {
		var retryAttempt *int
		if r.RetryAttempt != nil {
			n := *r.RetryAttempt
			retryAttempt = &n
		}
		rows = append(rows, RunningView{
			IssueID:        id,
			Identifier:     r.Identifier,
			IssueURL:       r.Issue.URL,
			State:          r.Issue.State,
			SessionID:      r.Session.SessionID,
			TurnCount:      r.Session.TurnCount,
			LastEvent:      r.LastCodexEvent,
			LastMessage:    r.LastCodexMessage,
			StartedAt:      r.StartedAt,
			RuntimeSeconds: runtimeSecondsSince(now, r.StartedAt),
			LastEventAt:    r.LastEventAt,
			RetryAttempt:   retryAttempt,
			WorkspacePath:  r.Workspace.Path,
			Tokens: TokensView{
				InputTokens:  r.CodexInputTokens,
				OutputTokens: r.CodexOutputTokens,
				TotalTokens:  r.CodexTotalTokens,
			},
			CodexAppServerPID: r.Session.CodexAppServerPID,
			AgentProvider:     r.Session.AgentProvider,
			AgentModel:        r.Session.AgentModel,
			WorkflowSource:    r.Session.WorkflowSource,
			WorkflowPath:      r.Session.WorkflowPath,
		})
	}
	return rows
}

func (s *OrchestratorState) snapshotBlockedViews() []BlockedView {
	rows := make([]BlockedView, 0, len(s.Blocked))
	for id, b := range s.Blocked {
		rows = append(rows, BlockedView{
			IssueID:        id,
			Identifier:     b.Identifier,
			IssueURL:       b.Issue.URL,
			State:          b.Issue.State,
			BlockedAt:      b.BlockedAt,
			WorkspacePath:  b.Workspace.Path,
			SessionID:      b.Session.SessionID,
			LastEventAt:    b.LastEventAt,
			RuntimeSeconds: b.RuntimeSeconds,
			Tokens: TokensView{
				InputTokens:  b.CodexInputTokens,
				OutputTokens: b.CodexOutputTokens,
				TotalTokens:  b.CodexTotalTokens,
			},
			Method:            b.Method,
			Error:             b.Error,
			CodexAppServerPID: b.Session.CodexAppServerPID,
		})
	}
	return rows
}

func (s *OrchestratorState) snapshotEndedSessionUsage() []SessionUsageView {
	rows := make([]SessionUsageView, 0, len(s.endedSessionUsage))
	for _, usage := range s.endedSessionUsage {
		rows = append(rows, SessionUsageView{
			IssueID:        usage.IssueID,
			Identifier:     usage.Identifier,
			IssueURL:       usage.IssueURL,
			State:          usage.State,
			SessionID:      usage.Session.SessionID,
			WorkflowSource: usage.Session.WorkflowSource,
			WorkflowPath:   usage.Session.WorkflowPath,
			AgentProvider:  usage.Session.AgentProvider,
			AgentModel:     usage.Session.AgentModel,
			Tokens:         usage.Tokens,
			RuntimeSeconds: usage.RuntimeSeconds,
			CompletedAt:    usage.CompletedAt,
			Outcome:        usage.Outcome,
		})
	}
	return rows
}

func runtimeSecondsSince(now, startedAt time.Time) float64 {
	if startedAt.IsZero() {
		return 0
	}
	elapsed := now.Sub(startedAt)
	if elapsed <= 0 {
		return 0
	}
	return elapsed.Seconds()
}

func (s *OrchestratorState) snapshotRetryViews() []RetryView {
	rows := make([]RetryView, 0, len(s.RetryAttempts))
	for id, r := range s.RetryAttempts {
		rows = append(rows, RetryView{
			IssueID:        id,
			Identifier:     r.Identifier,
			IssueURL:       r.Issue.URL,
			Attempt:        r.Attempt,
			DueAt:          r.DueAt,
			Error:          r.Error,
			Kind:           r.Kind,
			StartupFailure: task.CopyStartupFailure(r.StartupFailure),
		})
	}
	return rows
}

func (s *OrchestratorState) snapshotOperatorTerminalStopViews() []OperatorTerminalStopView {
	rows := make([]OperatorTerminalStopView, 0, len(s.operatorTerminalStopOrder))
	for _, id := range s.operatorTerminalStopOrder {
		entry, ok := s.LookupOperatorTerminalStop(id)
		if !ok {
			continue
		}
		rows = append(rows, OperatorTerminalStopView{
			IssueID:               id,
			Identifier:            entry.Identifier,
			State:                 entry.State,
			StoppedAt:             entry.StoppedAt,
			SuppressedDispatches:  entry.SuppressedDispatches,
			FirstSuppressedAt:     entry.FirstSuppressedAt,
			FirstSuppressedState:  entry.FirstSuppressedState,
			FirstSuppressedReason: entry.FirstSuppressedReason,
		})
	}
	return rows
}

func (s *OrchestratorState) Snapshot() StateView {
	// Keep the live-aggregate math on the monotonic clock: `time.Now()`
	// carries a monotonic component that `time.Time.Sub` uses for elapsed
	// calculations, but `time.Time.UTC()` strips it (Go stdlib documents
	// the loss explicitly). RunningEntry.StartedAt is set from `time.Now()`
	// in spawn (with monotonic reading intact), so subtracting two
	// monotonic-bearing times keeps the elapsed reading invariant under
	// wall-clock NTP steps. `GeneratedAt` is the operator-visible
	// timestamp and needs UTC; do the strip only when populating it.
	now := time.Now()
	// SPEC §13.5 Runtime accounting: report runtime as a live aggregate at
	// snapshot/render time. CodexTotals.SecondsRunning increments only on
	// session end (AddSeconds in the worker-exit finalization path); without
	// the per-snapshot fold a dashboard polling /api/v1/state would see a
	// flat counter while a long run is in flight and a sudden jump on exit.
	// Add elapsed time for each currently-running entry, measured against
	// the same monotonic `now` so all rows share one instant. The
	// ended-session counter and the live aggregate cannot double-count
	// because a finished run is removed from s.Running before AddSeconds
	// runs for it.
	totals := s.CodexTotals
	for _, r := range s.Running {
		if r.StartedAt.IsZero() {
			continue
		}
		if elapsed := now.Sub(r.StartedAt); elapsed > 0 {
			totals.SecondsRunning += elapsed.Seconds()
		}
	}
	view := StateView{
		GeneratedAt:                  now.UTC(),
		PollIntervalMs:               s.PollIntervalMs,
		MaxConcurrentAgents:          s.MaxConcurrentAgents,
		MaxConcurrentAgentsByState:   copyStateConcurrencyLimits(s.MaxConcurrentAgentsByState),
		AgentDefault:                 s.AgentDefault,
		Blocked:                      s.snapshotBlockedViews(),
		Retrying:                     s.snapshotRetryViews(),
		Completed:                    make([]IssueID, 0, len(s.completedOrder)),
		ReconcileStoppedWithProgress: make([]IssueID, 0, len(s.reconcileStoppedWithProgressOrder)),
		AgentHandoffReconcileStopped: make([]IssueID, 0, len(s.agentHandoffReconcileStoppedOrder)),
		ActiveSuccessNoHandoff:       make([]IssueID, 0, len(s.activeSuccessNoHandoffOrder)),
		OperatorTerminalStops:        s.snapshotOperatorTerminalStopViews(),
		CompletedSessionUsage:        s.snapshotEndedSessionUsage(),
		BudgetGuardrails: BudgetGuardrailsView{
			MaxTokensPerClaim:         s.BudgetGuardrails.MaxTokensPerClaim,
			MaxRuntimeSecondsPerClaim: s.BudgetGuardrails.MaxRuntimeSecondsPerClaim,
		},
		CumulativeCompletedTotal:                    s.CumulativeCompletedTotal,
		CumulativeReconcileStoppedWithProgressTotal: s.CumulativeReconcileStoppedWithProgressTotal,
		CumulativeAgentHandoffReconcileStoppedTotal: s.CumulativeAgentHandoffReconcileStoppedTotal,
		CumulativeActiveSuccessNoHandoffTotal:       s.CumulativeActiveSuccessNoHandoffTotal,
		CumulativeOperatorTerminalStopsTotal:        s.CumulativeOperatorTerminalStopsTotal,
		CodexTotals:                                 totals,
		CodexRateLimits:                             copyRateLimitSnapshot(s.CodexRateLimits),
		RecentEvents:                                append([]RuntimeEvent(nil), s.RecentEvents...),
	}
	view.Running = s.snapshotRunningViews(now)
	// Iterate the FIFO order slices, not the map. The map's iteration
	// order is undefined in Go; using the slices preserves observed
	// insertion order so /api/v1/state consumers see "oldest first"
	// stably, and the bounded slice matches MaxRecent* exactly.
	view.Completed = append(view.Completed, s.completedOrder...)
	view.ReconcileStoppedWithProgress = append(view.ReconcileStoppedWithProgress, s.reconcileStoppedWithProgressOrder...)
	view.AgentHandoffReconcileStopped = append(view.AgentHandoffReconcileStopped, s.agentHandoffReconcileStoppedOrder...)
	view.ActiveSuccessNoHandoff = append(view.ActiveSuccessNoHandoff, s.activeSuccessNoHandoffOrder...)
	return view
}

func copyRateLimitSnapshot(in *RateLimitSnapshot) *RateLimitSnapshot {
	if in == nil {
		return nil
	}
	copied := RateLimitSnapshot(copyAnyMap(map[string]any(*in)))
	return &copied
}
