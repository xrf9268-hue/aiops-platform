package tracker

import (
	"context"
	"strings"
	"time"
)

type Issue struct {
	ID          string
	Identifier  string
	Title       string
	Description string
	URL         string
	State       string
	Labels      []string
	BranchName  string
	Priority    int
	CreatedAt   time.Time
	UpdatedAt   time.Time
	BlockedBy   []BlockerRef
}

// IssueState is the narrow SPEC §11.2 state-refresh fact: an issue's current
// workflow state plus its normalized labels. Labels are carried so the SPEC §6.4
// required_labels "continue" gate can observe label removal on already-claimed
// issues that may sit beyond the active-listing page (#682). Producing clients
// lowercase/trim each label so matching is case-insensitive.
type IssueState struct {
	State  string
	Labels []string
}

// IssueRef carries the stable tracker ID plus the human identifier captured at
// dispatch time. GitHub and Gitea need the identifier as a repo issue-number
// fallback because their REST state-refresh endpoints cannot address issues by
// the normalized global ID directly.
type IssueRef struct {
	ID         string
	Identifier string
}

func IssueRefsFromIDs(issueIDs []string) []IssueRef {
	refs := make([]IssueRef, 0, len(issueIDs))
	for _, id := range issueIDs {
		if id = strings.TrimSpace(id); id != "" {
			refs = append(refs, IssueRef{ID: id})
		}
	}
	return refs
}

// BlockerRef is the minimal tracker dependency metadata the orchestrator needs
// to filter blocked Todo candidates. State is the blocker issue's workflow
// state; callers decide which states are terminal in their workflow.
type BlockerRef struct {
	ID         string
	Identifier string
	State      string
}

// TimeString returns the canonical string form used by workspace keys for a
// tracker timestamp.
func TimeString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
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
	FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]IssueState, error)
}
