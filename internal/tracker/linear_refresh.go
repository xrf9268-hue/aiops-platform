package tracker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
)

// labels(first:250) mirrors listLinearIssuesQuery: the SPEC §6.4 required_labels
// gate consults the refresh's label set to stop/release already-claimed work on
// label removal, so the projection must be wide enough that a required label
// cannot sort past the cap and look removed (#705).
const issueStatesByIDsQuery = `query IssueStatesByIDs($ids: [ID!]!, $first: Int!) {
  issues(filter: { id: { in: $ids } }, first: $first) {
    nodes {
      id
      state { name }
      labels(first: 250) { nodes { name } }
    }
  }
}`

type linearIssueStateNode struct {
	ID    *string `json:"id"`
	State *struct {
		Name *string `json:"name"`
	} `json:"state"`
	Labels *struct {
		Nodes *[]struct {
			Name *string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
}

type linearIssueStatesResponse struct {
	Data *struct {
		Issues *struct {
			Nodes *[]linearIssueStateNode `json:"nodes"`
		} `json:"issues"`
	} `json:"data"`
	Errors []map[string]any `json:"errors"`
}

func (c *LinearClient) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]IssueState, error) { //nolint:gocognit // baseline (#521)
	states, ids := unknownIssueStatesByIDs(issueIDs)
	if c.APIKey == "" {
		return states, NewError(CategoryMissingTrackerAPIKey, "Linear API key is required", nil)
	}
	if len(ids) == 0 {
		return states, nil
	}
	var refreshErrs []error
	halted := false
	for start := 0; start < len(ids); start += linearIssuePageSize {
		end := start + linearIssuePageSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		var out linearIssueStatesResponse
		if err := c.graphql(ctx, issueStatesByIDsQuery, map[string]any{"ids": chunk, "first": len(chunk)}, &out); err != nil {
			refreshErrs = append(refreshErrs, err)
			if ShouldStopIssueStateRefresh(ctx, err) {
				halted = true
				break
			}
			continue
		}
		if len(out.Errors) > 0 {
			refreshErrs = append(refreshErrs, linearGraphQLErrors(out.Errors))
			continue
		}
		chunkStates, err := classifyLinearIssueStateChunk(chunk, out)
		if err != nil {
			refreshErrs = append(refreshErrs, err)
			continue
		}
		for id, state := range chunkStates {
			states[id] = state
		}
	}
	if halted {
		failClosedTodoBlockers(states)
	} else if err := c.attachRefreshedTodoBlockers(ctx, states); err != nil {
		// Blocker data is consumed only by dispatch-time revalidation; the
		// reconcile and §16.5 per-turn refreshers share this method and must
		// not fail on it — their state/label result is already complete, and
		// an error here would short-circuit a healthy run's turn loop on a
		// transient inverse-relations failure (PR #752 review). Instead,
		// every refreshed Todo entry fails closed with an empty-state
		// placeholder blocker — the same shape the Gitea adapter uses for an
		// unresolvable reference — so the revalidation gate skips the
		// candidate for this tick and the next tick retries, rather than
		// dispatching past a blocker the failed query could not see (a
		// listing-time "unblocked" verdict may predate a newly added
		// relation). State-only consumers ignore BlockedBy and are
		// unaffected.
		log.Printf("event=linear_blocker_refresh_failed error=%q detail=\"refreshed Todo issues fail closed with a placeholder blocker this batch\"", err.Error())
		failClosedTodoBlockers(states)
	}
	return states, errors.Join(refreshErrs...)
}

func classifyLinearIssueStateChunk(chunk []string, out linearIssueStatesResponse) (map[string]IssueState, error) {
	if out.Data == nil || out.Data.Issues == nil || out.Data.Issues.Nodes == nil {
		return nil, incompleteLinearIssueStateResponse("missing data.issues.nodes")
	}
	classified := make(map[string]IssueState, len(chunk))
	for _, id := range chunk {
		classified[id] = IssueState{Outcome: IssueStateOutcomeAbsent}
	}
	seen := make(map[string]struct{}, len(*out.Data.Issues.Nodes))
	for _, node := range *out.Data.Issues.Nodes {
		if err := classifyLinearIssueStateNode(node, classified, seen); err != nil {
			return nil, err
		}
	}
	return classified, nil
}

func classifyLinearIssueStateNode(node linearIssueStateNode, classified map[string]IssueState, seen map[string]struct{}) error {
	if node.ID == nil || node.State == nil || node.State.Name == nil || node.Labels == nil || node.Labels.Nodes == nil {
		return incompleteLinearIssueStateResponse("issue node missing id, state, or labels")
	}
	id := strings.TrimSpace(*node.ID)
	state := strings.TrimSpace(*node.State.Name)
	if id == "" || state == "" {
		return incompleteLinearIssueStateResponse("issue node has empty id or state")
	}
	if _, requested := classified[id]; !requested {
		return incompleteLinearIssueStateResponse("issue node id was not requested")
	}
	if _, duplicate := seen[id]; duplicate {
		return incompleteLinearIssueStateResponse("duplicate issue node id")
	}
	labels := make([]string, 0, len(*node.Labels.Nodes))
	for _, label := range *node.Labels.Nodes {
		if label.Name == nil || strings.TrimSpace(*label.Name) == "" {
			return incompleteLinearIssueStateResponse("issue label missing non-empty name")
		}
		labels = append(labels, strings.ToLower(strings.TrimSpace(*label.Name)))
	}
	classified[id] = IssueState{Outcome: IssueStateOutcomeCurrent, State: *node.State.Name, Labels: labels}
	seen[id] = struct{}{}
	return nil
}

func incompleteLinearIssueStateResponse(detail string) error {
	return NewError(CategoryLinearUnknownPayload, "incomplete Linear issue state response", fmt.Errorf("%w: %s", ErrIssueStateRefreshIncomplete, detail))
}

// failClosedTodoBlockers marks every Todo-state entry with one empty-state
// placeholder blocker, which tracker.BlockedByNonTerminal treats as open.
func failClosedTodoBlockers(states map[string]IssueState) {
	for id, state := range states {
		if !isTodoState(state.State) {
			continue
		}
		state.BlockedBy = []BlockerRef{{}}
		states[id] = state
	}
}

// attachRefreshedTodoBlockers resolves blocker data for the refreshed issues
// whose state is Todo — the only state the SPEC §8.2 blocker gate applies to,
// mirroring the listing path's Todo-only resolution (#672) — so dispatch-time
// revalidation can re-apply the gate on refreshed data like upstream
// retry_candidate_issue? (orchestrator.ex:1602-1604) does (#750). Non-Todo
// entries keep a nil BlockedBy ("no blocker knowledge supplied"); Todo
// entries get the non-nil (possibly empty) result linearBlockersForIssues
// guarantees for every requested id.
//
// Dispatch revalidation is the only consumer of the blocker data; the
// reconcile and §16.5 per-turn refreshers share FetchIssueStatesByIDs and
// inherit the extra Todo-only batched query as a side effect but ignore
// BlockedBy (reconcile cancellation and the per-turn continue gate are
// state/label-only by design). Upstream's fetch_issue_states_by_ids returns
// fully normalized issues including blocked_by on every caller too, so the
// cost profile matches the reference.
func (c *LinearClient) attachRefreshedTodoBlockers(ctx context.Context, states map[string]IssueState) error {
	ids := make([]string, 0, len(states))
	for id, state := range states {
		if isTodoState(state.State) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	blockers, err := c.linearBlockersForIssues(ctx, ids)
	if err != nil {
		return err
	}
	for _, id := range ids {
		state := states[id]
		state.BlockedBy = blockers[id]
		states[id] = state
	}
	return nil
}
