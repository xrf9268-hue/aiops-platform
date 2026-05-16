package tracker

import "context"

type Issue struct {
	ID          string
	Identifier  string
	Title       string
	Description string
	URL         string
	State       string
	UpdatedAt   string
}

type Client interface {
	ListActiveIssues(ctx context.Context) ([]Issue, error)
}

type StateIssueLister interface {
	ListIssuesByStates(ctx context.Context, states []string) ([]Issue, error)
}

// Transitioner is the subset of a tracker client used by the worker to
// drive the linked issue through its lifecycle. The worker calls these
// methods at task claim, PR handoff, and failure so the tracker view
// stays in sync with the queue. Implementations must be safe to call
// from a single goroutine (the worker loop) and should use the supplied
// context for both deadlines and cancellation.
type Transitioner interface {
	// MoveIssueToState updates the issue identified by issueID so its
	// workflow state matches stateName. The state name is matched as
	// the human-readable label visible in the tracker UI; resolution
	// from name to internal ID is the implementation's responsibility.
	MoveIssueToState(ctx context.Context, issueID, stateName string) error
	// AddComment attaches a comment to the issue identified by issueID.
	// Used as the fallback when the worker cannot move the issue to a
	// configured failure state but still wants the human to see the
	// failure on the issue.
	AddComment(ctx context.Context, issueID, body string) error
}
