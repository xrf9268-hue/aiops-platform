package tracker

import "context"

type Issue struct {
	ID           string
	Identifier   string
	Title        string
	Description  string
	URL          string
	State        string
	ProjectSlug  string
	TeamKey      string
	Labels       []string
	CustomFields map[string]string
	ServiceName  string
	Priority     int
	CreatedAt    string
	UpdatedAt    string
	BlockedBy    []Blocker
}

// Blocker is the minimal tracker dependency metadata the orchestrator needs to
// filter blocked Todo candidates. State is the blocker issue's workflow state;
// callers decide which states are terminal in their workflow.
type Blocker struct {
	ID         string
	Identifier string
	State      string
}

type Client interface {
	ListActiveIssues(ctx context.Context) ([]Issue, error)
}

type StateIssueLister interface {
	ListIssuesByStates(ctx context.Context, states []string) ([]Issue, error)
}

// IssueStateRefresher fetches the current tracker state for explicit issue IDs.
// Poll-tick reconciliation uses this to refresh already-running issues without
// relying on candidate pagination side effects.
type IssueStateRefresher interface {
	FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error)
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
