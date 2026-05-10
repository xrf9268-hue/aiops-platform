package worker_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	worker "github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// fakeTransitioner records every call so tests can assert the worker
// hooks fired with the right issue ID and target state. Errors can be
// scripted per-method to drive the comment-fallback and event-emission
// branches.
type fakeTransitioner struct {
	mu sync.Mutex

	moveErr    error
	commentErr error

	moves    []moveCall
	comments []commentCall
}

type moveCall struct {
	IssueID, State string
}

type commentCall struct {
	IssueID, Body string
}

func (f *fakeTransitioner) MoveIssueToState(_ context.Context, issueID, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moves = append(f.moves, moveCall{IssueID: issueID, State: state})
	return f.moveErr
}

func (f *fakeTransitioner) AddComment(_ context.Context, issueID, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, commentCall{IssueID: issueID, Body: body})
	return f.commentErr
}

// linearTask is a small constructor for the source_type=linear_issue
// shape the hooks key off of. The default ID maps to a bare issue UUID
// (no rework suffix).
func linearTask(eventID string) task.Task {
	return task.Task{
		ID:            "tsk_lin",
		SourceType:    "linear_issue",
		SourceEventID: eventID,
	}
}

// linearCfg returns a workflow.Config wired for Linear with the
// stock status names. Tests that need to vary the names override the
// fields after this returns.
func linearCfg() workflow.Config {
	return workflow.Config{
		Tracker: workflow.TrackerConfig{
			Kind: "linear",
			Statuses: workflow.TrackerStatusConfig{
				InProgress:  "In Progress",
				HumanReview: "Human Review",
				Rework:      "Rework",
			},
		},
	}
}

// TestLinearIssueID_StripsReworkSuffix pins the parser the hooks rely
// on to recover the bare Linear issue.ID even after the poller's
// rework re-enqueue has rewritten source_event_id.
func TestLinearIssueID_StripsReworkSuffix(t *testing.T) {
	cases := []struct {
		name string
		t    task.Task
		want string
		ok   bool
	}{
		{"plain", task.Task{SourceType: "linear_issue", SourceEventID: "abc"}, "abc", true},
		{"rework suffix", task.Task{SourceType: "linear_issue", SourceEventID: "abc|rework|2026-05-08T10:00:00Z"}, "abc", true},
		{"non-linear", task.Task{SourceType: "gitea_issue", SourceEventID: "abc"}, "", false},
		{"empty source id", task.Task{SourceType: "linear_issue", SourceEventID: ""}, "", false},
	}
	for _, tc := range cases {
		got, ok := worker.LinearIssueID(tc.t)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: LinearIssueID = (%q, %v), want (%q, %v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// TestOnClaim_MovesIssueToInProgress is the core happy-path: a Linear
// task triggers a single MoveIssueToState call and one tracker_transition
// event with reason=claimed.
func TestOnClaim_MovesIssueToInProgress(t *testing.T) {
	ev := &fakeEmitter{}
	tr := &fakeTransitioner{}
	worker.OnClaim(context.Background(), ev, tr, linearTask("issue-uuid"), linearCfg())

	if len(tr.moves) != 1 {
		t.Fatalf("MoveIssueToState calls = %d, want 1", len(tr.moves))
	}
	if tr.moves[0].IssueID != "issue-uuid" || tr.moves[0].State != "In Progress" {
		t.Fatalf("move call = %#v, want issue-uuid -> In Progress", tr.moves[0])
	}
	transitions := ev.byKind("tracker_transition")
	if len(transitions) != 1 {
		t.Fatalf("tracker_transition events = %d, want 1", len(transitions))
	}
	payload := transitions[0].Payload.(map[string]any)
	if payload["target_state"] != "In Progress" {
		t.Fatalf("payload.target_state = %v", payload["target_state"])
	}
	if payload["reason"] != "claimed" {
		t.Fatalf("payload.reason = %v", payload["reason"])
	}
}

// TestOnClaim_NoOpWhenTrackerNil locks the documented "missing
// transitioner = no-op" contract. The factory returns nil for
// non-Linear or unkeyed tracker configs, and the hook must tolerate
// that without panicking or emitting events.
func TestOnClaim_NoOpWhenTrackerNil(t *testing.T) {
	ev := &fakeEmitter{}
	worker.OnClaim(context.Background(), ev, nil, linearTask("issue-uuid"), linearCfg())
	if len(ev.events) != 0 {
		t.Fatalf("expected zero events with nil transitioner, got %d", len(ev.events))
	}
}

// TestOnClaim_NoOpForNonLinearTask covers the gitea-only path. Even
// with a configured transitioner, a task originating from gitea must
// not trigger Linear mutations.
func TestOnClaim_NoOpForNonLinearTask(t *testing.T) {
	ev := &fakeEmitter{}
	tr := &fakeTransitioner{}
	t1 := task.Task{SourceType: "gitea_issue", SourceEventID: "evt-1"}
	worker.OnClaim(context.Background(), ev, tr, t1, linearCfg())

	if len(tr.moves) != 0 {
		t.Fatalf("non-linear task must not call MoveIssueToState; got %d", len(tr.moves))
	}
	if len(ev.events) != 0 {
		t.Fatalf("expected zero events for non-linear task, got %d", len(ev.events))
	}
}

// TestOnClaim_RecordsErrorEventOnMutationFailure pins the safety
// contract: the hook never aborts the task, but it must surface the
// API failure so an operator can investigate after the fact.
func TestOnClaim_RecordsErrorEventOnMutationFailure(t *testing.T) {
	ev := &fakeEmitter{}
	tr := &fakeTransitioner{moveErr: errors.New("403 Forbidden")}
	worker.OnClaim(context.Background(), ev, tr, linearTask("issue-uuid"), linearCfg())

	errEvents := ev.byKind("tracker_transition_error")
	if len(errEvents) != 1 {
		t.Fatalf("tracker_transition_error events = %d, want 1; events=%#v", len(errEvents), ev.events)
	}
	if got := ev.byKind("tracker_transition"); len(got) != 0 {
		t.Fatalf("success event must not fire on error path, got %d", len(got))
	}
	payload := errEvents[0].Payload.(map[string]any)
	if !strings.Contains(payload["error"].(string), "403 Forbidden") {
		t.Fatalf("error payload should include underlying error: %v", payload["error"])
	}
}

// TestOnClaim_StripsReworkSuffixBeforeAPI mirrors the integration the
// poller depends on: tasks created on a Rework re-enqueue have
// source_event_id="<id>|rework|<updatedAt>", but the API call must
// target the bare issue.ID.
func TestOnClaim_StripsReworkSuffixBeforeAPI(t *testing.T) {
	ev := &fakeEmitter{}
	tr := &fakeTransitioner{}
	worker.OnClaim(context.Background(), ev, tr, linearTask("issue-uuid|rework|2026-05-08T10:00:00Z"), linearCfg())

	if len(tr.moves) != 1 {
		t.Fatalf("MoveIssueToState calls = %d, want 1", len(tr.moves))
	}
	if tr.moves[0].IssueID != "issue-uuid" {
		t.Fatalf("issue id = %q, want \"issue-uuid\" (suffix should be stripped)", tr.moves[0].IssueID)
	}
}

// TestOnPRCreated_MovesToHumanReview is the sister test of
// TestOnClaim_MovesIssueToInProgress, just for the PR handoff stage.
func TestOnPRCreated_MovesToHumanReview(t *testing.T) {
	ev := &fakeEmitter{}
	tr := &fakeTransitioner{}
	worker.OnPRCreated(context.Background(), ev, tr, linearTask("issue-uuid"), linearCfg())

	if len(tr.moves) != 1 {
		t.Fatalf("MoveIssueToState calls = %d, want 1", len(tr.moves))
	}
	if tr.moves[0].State != "Human Review" {
		t.Fatalf("target state = %q, want \"Human Review\"", tr.moves[0].State)
	}
	transitions := ev.byKind("tracker_transition")
	if len(transitions) != 1 {
		t.Fatalf("tracker_transition events = %d, want 1", len(transitions))
	}
	payload := transitions[0].Payload.(map[string]any)
	if payload["reason"] != "pr_created" {
		t.Fatalf("payload.reason = %v, want \"pr_created\"", payload["reason"])
	}
}

// TestOnFailure_MovesToReworkOnSuccess covers the preferred failure
// path: a single MoveIssueToState call with reason=failure and no
// fallback comment.
func TestOnFailure_MovesToReworkOnSuccess(t *testing.T) {
	ev := &fakeEmitter{}
	tr := &fakeTransitioner{}
	worker.OnFailure(context.Background(), ev, tr, linearTask("issue-uuid"), linearCfg(), errors.New("verify failed"))

	if len(tr.moves) != 1 || tr.moves[0].State != "Rework" {
		t.Fatalf("expected one move to Rework, got %#v", tr.moves)
	}
	if len(tr.comments) != 0 {
		t.Fatalf("expected zero comments on successful state move, got %d", len(tr.comments))
	}
	transitions := ev.byKind("tracker_transition")
	if len(transitions) != 1 {
		t.Fatalf("tracker_transition events = %d, want 1", len(transitions))
	}
	payload := transitions[0].Payload.(map[string]any)
	if payload["reason"] != "failure" {
		t.Fatalf("payload.reason = %v, want \"failure\"", payload["reason"])
	}
}

// TestOnFailure_FallsBackToCommentWhenMoveFails pins the spec language
// "move issue to Rework or comment with failure": when the state mutation
// errors (e.g. wrong status name, revoked permission), the hook attaches
// a comment so the human still sees the failure on the issue.
func TestOnFailure_FallsBackToCommentWhenMoveFails(t *testing.T) {
	ev := &fakeEmitter{}
	tr := &fakeTransitioner{moveErr: errors.New("no matching state")}
	cfg := linearCfg()
	worker.OnFailure(context.Background(), ev, tr, linearTask("issue-uuid"), cfg, errors.New("policy violation"))

	if len(tr.moves) != 1 {
		t.Fatalf("expected the move to be attempted, got %d", len(tr.moves))
	}
	if len(tr.comments) != 1 {
		t.Fatalf("expected fallback comment, got %d", len(tr.comments))
	}
	if !strings.Contains(tr.comments[0].Body, "policy violation") {
		t.Fatalf("comment body should include the run error; got %q", tr.comments[0].Body)
	}
	if !strings.Contains(tr.comments[0].Body, "tsk_lin") {
		t.Fatalf("comment body should mention the task ID; got %q", tr.comments[0].Body)
	}

	// The error event for the move and the success event for the comment
	// should both be recorded, in that order.
	errEvents := ev.byKind("tracker_transition_error")
	if len(errEvents) != 1 {
		t.Fatalf("tracker_transition_error count = %d, want 1", len(errEvents))
	}
	commentEvents := ev.byKind("tracker_comment")
	if len(commentEvents) != 1 {
		t.Fatalf("tracker_comment count = %d, want 1", len(commentEvents))
	}
}

// TestOnFailure_EmptyReworkStateAttemptsMoveThenComments locks the
// behavior when a caller hands OnFailure a Config whose Rework name is
// blank (which workflow.Load always populates with the default, but
// a direct caller could miss). The hook attempts the move — the
// Linear client surfaces a "state name is required" error — and then
// falls back to the comment path. This guards against the failure
// path silently swallowing such configs.
func TestOnFailure_EmptyReworkStateAttemptsMoveThenComments(t *testing.T) {
	ev := &fakeEmitter{}
	tr := &fakeTransitioner{moveErr: errors.New("state name is required")}
	cfg := linearCfg()
	cfg.Tracker.Statuses.Rework = ""

	worker.OnFailure(context.Background(), ev, tr, linearTask("issue-uuid"), cfg, errors.New("boom"))

	if len(tr.moves) != 1 {
		t.Fatalf("expected one move attempt, got %d", len(tr.moves))
	}
	if len(tr.comments) != 1 {
		t.Fatalf("expected single comment fallback, got %d", len(tr.comments))
	}
}

// TestOnFailure_RecordsErrorWhenCommentFails locks the worst-case
// path: both mutations fail. The hook must not panic, must not loop,
// and must leave a tracker_transition_error event behind.
func TestOnFailure_RecordsErrorWhenCommentFails(t *testing.T) {
	ev := &fakeEmitter{}
	tr := &fakeTransitioner{
		moveErr:    errors.New("403"),
		commentErr: errors.New("500"),
	}
	worker.OnFailure(context.Background(), ev, tr, linearTask("issue-uuid"), linearCfg(), errors.New("verify failed"))

	if got := len(tr.moves); got != 1 {
		t.Fatalf("moves = %d, want 1", got)
	}
	if got := len(tr.comments); got != 1 {
		t.Fatalf("comments = %d, want 1", got)
	}
	// Both error events should be recorded — one for the move, one for the comment.
	errEvents := ev.byKind("tracker_transition_error")
	if len(errEvents) != 2 {
		t.Fatalf("tracker_transition_error events = %d, want 2", len(errEvents))
	}
}

// TestOnClaim_HonoursCustomStatusName confirms acceptance criterion 4
// end-to-end: a workflow.Config with a non-default in_progress label
// flows through to the API call.
func TestOnClaim_HonoursCustomStatusName(t *testing.T) {
	ev := &fakeEmitter{}
	tr := &fakeTransitioner{}
	cfg := linearCfg()
	cfg.Tracker.Statuses.InProgress = "Coding"
	worker.OnClaim(context.Background(), ev, tr, linearTask("issue-uuid"), cfg)

	if len(tr.moves) != 1 || tr.moves[0].State != "Coding" {
		t.Fatalf("expected single move to \"Coding\", got %#v", tr.moves)
	}
}
