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
	// BlockedBy carries the refreshed blocker dependencies so dispatch-time
	// revalidation can re-apply the SPEC §8.2 Todo blocker gate, matching
	// upstream retry_candidate_issue? (orchestrator.ex:1602-1604), which
	// re-checks todo_issue_blocked_by_non_terminal? on the refreshed issue
	// (#750). nil means the adapter supplied no blocker knowledge for this
	// issue and the consumer must keep its listing-time verdict; a non-nil
	// empty slice is a positive "no blockers" answer. Unresolvable blocker
	// knowledge fails closed as an empty-state placeholder the gate treats
	// as open — never as nil, which would read as "safe". Linear resolves
	// blockers only for refreshed Todo-state issues (the only state the gate
	// applies to), placeholding the whole Todo batch when the
	// inverse-relations query fails; Gitea derives them from the issue
	// body's `Depends on #N` references, placeholding
	// transiently-unresolvable references and skipping definitively-deleted
	// blockers; GitHub has no blocker concept and always leaves this nil.
	BlockedBy []BlockerRef
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

// BlockedByNonTerminal reports whether any blocker in blockedBy is still open
// per the SPEC §8.2 rule: an empty/unknown blocker state blocks, and a state
// outside terminalStates (lowercased, trimmed keys) blocks. It is the single
// source of truth for the "is this blocker open" judgment (#750). The
// empty-state branch is what makes the Gitea adapter's fail-closed
// placeholder refs (unresolvable `Depends on #N` lookups) block at the gate.
func BlockedByNonTerminal(blockedBy []BlockerRef, terminalStates map[string]struct{}) bool {
	for _, blocker := range blockedBy {
		state := strings.ToLower(strings.TrimSpace(blocker.State))
		if state == "" {
			return true
		}
		if _, terminal := terminalStates[state]; !terminal {
			return true
		}
	}
	return false
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
