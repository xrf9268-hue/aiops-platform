package worker

import (
	"context"
	"fmt"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// Transitioner is the worker-facing alias for tracker.Transitioner. The
// alias keeps internal/worker depending on the abstraction (which is
// trivially fakeable in tests) without forcing every call site to import
// internal/tracker just to type a parameter.
type Transitioner = tracker.Transitioner

// LinearIssueID returns the Linear issue.ID a task originated from,
// stripping the `|rework|<updatedAt>` suffix the poller adds when it
// re-enqueues a task on a Rework transition (cmd/linear-poller). The
// suffix exists for the queue-side dedupe key only; the API mutations
// always need the bare issue.ID.
//
// Returns ok=false when the task did not come from Linear or the source
// ID is empty, so callers can no-op cleanly without scattering source
// checks across each lifecycle hook.
func LinearIssueID(t task.Task) (string, bool) {
	if t.SourceType != "linear_issue" || t.SourceEventID == "" {
		return "", false
	}
	id := t.SourceEventID
	if i := strings.Index(id, "|"); i >= 0 {
		id = id[:i]
	}
	if id == "" {
		return "", false
	}
	return id, true
}

// OnClaim moves the linked Linear issue to the configured "in progress"
// state. No-op when the task did not come from Linear, when the worker
// was started without a transitioner factory, or when the configured
// status name is empty. Errors are recorded as a tracker_transition_error
// event and swallowed: a tracker hiccup must not abort the actual code
// work.
func OnClaim(ctx context.Context, ev EventEmitter, tr Transitioner, t task.Task, cfg workflow.Config) {
	transitionTo(ctx, ev, tr, t, cfg, cfg.Tracker.Statuses.InProgress, "claimed")
}

// OnPRCreated moves the linked Linear issue to the configured
// "human review" state. The worker calls this whenever CreatePR returns
// nil, which covers both the create-new and reuse-existing paths so a
// retried task that lands on an existing PR still flips the issue
// forward.
func OnPRCreated(ctx context.Context, ev EventEmitter, tr Transitioner, t task.Task, cfg workflow.Config) {
	transitionTo(ctx, ev, tr, t, cfg, cfg.Tracker.Statuses.HumanReview, "pr_created")
}

// OnFailure routes a task failure to the linked Linear issue. The
// preferred path is moving the issue to the configured "rework" state
// because the poller's Rework re-enqueue logic (cmd/linear-poller's
// sourceEventID) depends on that transition to retry. When the move
// itself fails (state name typo, API error, missing permission) we
// fall back to attaching a comment so the human still has visibility
// into what happened — better than silent tracker drift.
func OnFailure(ctx context.Context, ev EventEmitter, tr Transitioner, t task.Task, cfg workflow.Config, runErr error) {
	if tr == nil || cfg.Tracker.Kind != "linear" {
		return
	}
	issueID, ok := LinearIssueID(t)
	if !ok {
		return
	}
	if state := cfg.Tracker.Statuses.Rework; state != "" {
		err := tr.MoveIssueToState(ctx, issueID, state)
		if err == nil {
			Emit(ctx, ev, t.ID, "tracker_transition", "issue moved to rework", map[string]any{
				"issue_id":     issueID,
				"target_state": state,
				"reason":       "failure",
			})
			return
		}
		// Record the move failure, then fall through to the comment path so
		// the human still sees the failure on the issue.
		Emit(ctx, ev, t.ID, "tracker_transition_error", "move to rework failed", map[string]any{
			"issue_id":     issueID,
			"target_state": state,
			"error":        ErrSummary(err),
		})
	}
	body := fmt.Sprintf("AI run failed for task `%s`: %s", t.ID, ErrSummary(runErr))
	if err := tr.AddComment(ctx, issueID, body); err != nil {
		Emit(ctx, ev, t.ID, "tracker_transition_error", "failure comment failed", map[string]any{
			"issue_id": issueID,
			"error":    ErrSummary(err),
		})
		return
	}
	Emit(ctx, ev, t.ID, "tracker_comment", "failure comment posted", map[string]any{
		"issue_id": issueID,
	})
}

// transitionTo is the shared body of OnClaim and OnPRCreated. It guards
// the no-op cases (no transitioner, non-Linear tracker, empty target,
// non-Linear task) and emits a single tracker_transition or
// tracker_transition_error event with a uniform payload shape so the
// debug API can surface the lifecycle without cross-event reconciliation.
func transitionTo(ctx context.Context, ev EventEmitter, tr Transitioner, t task.Task, cfg workflow.Config, target, reason string) {
	if tr == nil || cfg.Tracker.Kind != "linear" || target == "" {
		return
	}
	issueID, ok := LinearIssueID(t)
	if !ok {
		return
	}
	payload := map[string]any{
		"issue_id":     issueID,
		"target_state": target,
		"reason":       reason,
	}
	if err := tr.MoveIssueToState(ctx, issueID, target); err != nil {
		payload["error"] = ErrSummary(err)
		Emit(ctx, ev, t.ID, "tracker_transition_error", "issue transition failed", payload)
		return
	}
	Emit(ctx, ev, t.ID, "tracker_transition", "issue transitioned", payload)
}
