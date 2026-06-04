package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	currentIssueRejectActiveStateUpdate = "current_issue_active_state_update"
	currentIssueRejectUnsupportedShape  = "unsupported_issue_update_shape"
	currentIssueRejectRefreshFailed     = "current_issue_state_refresh_failed"
	currentIssueRejectMissing           = "current_issue_state_missing"
	currentIssueRejectTerminal          = "current_issue_terminal"
	currentIssueRejectNotActive         = "current_issue_not_active"
	currentIssueRejectOperatorStop      = "operator_terminal_stop"
	currentIssueRejectStateLookupFailed = "active_state_lookup_failed"
)

const linearGraphQLStateLookupTimeout = 30 * time.Second

type currentIssueMutationGuard struct {
	issueID                    string
	issueIdentifier            string
	activeStates               []string
	teamKey                    string
	refresh                    IssueStateRefresher
	operatorTerminalStopLookup OperatorTerminalStopLookup
	cache                      *activeStateIDCache
}

type activeStateIDCache struct {
	mu     sync.Mutex
	loaded bool
	ids    map[string]struct{}
}

func (g currentIssueMutationGuard) enabled() bool {
	return strings.TrimSpace(g.issueID) != "" && g.refresh != nil
}

func (p linearGraphQLProxy) checkCurrentIssueUpdate(ctx context.Context, query string, variables map[string]any) (linearGraphQLMutationRejected, bool, bool) {
	if !p.currentIssueGuard.enabled() {
		return linearGraphQLMutationRejected{}, false, false
	}
	argumentTexts, err := issueUpdateArgumentTexts(query)
	if err != nil {
		if errors.Is(err, errIssueUpdateNotFound) {
			return linearGraphQLMutationRejected{}, false, false
		}
		return linearGraphQLMutationRejected{OperationField: "issueUpdate", Reason: currentIssueRejectUnsupportedShape}, true, false
	}
	currentIssueHandoff := false
	for _, args := range argumentTexts {
		rejection, reject, handoff := p.checkCurrentIssueUpdateArgs(ctx, args, variables)
		if reject {
			return rejection, true, false
		}
		currentIssueHandoff = currentIssueHandoff || handoff
	}
	return linearGraphQLMutationRejected{}, false, currentIssueHandoff
}

func (p linearGraphQLProxy) checkCurrentIssueUpdateArgs(ctx context.Context, args string, variables map[string]any) (linearGraphQLMutationRejected, bool, bool) {
	issueID, err := parseIssueUpdateIssueID(args, variables)
	if err != nil {
		return currentIssueUnsupportedUpdateRejection(), true, false
	}
	if !p.currentIssueGuard.matchesCurrentIssue(issueID) {
		return linearGraphQLMutationRejected{}, false, false
	}
	change, err := parseIssueUpdateArguments(args, variables)
	if err != nil {
		return currentIssueUnsupportedUpdateRejection(), true, false
	}
	return p.checkCurrentIssueStateChange(ctx, change)
}

func (p linearGraphQLProxy) checkCurrentIssueStateChange(ctx context.Context, change issueUpdateStateChange) (linearGraphQLMutationRejected, bool, bool) {
	snapshot, err := p.currentIssueGuard.refresh(ctx)
	if err != nil {
		return linearGraphQLMutationRejected{OperationField: "issueUpdate", Reason: currentIssueRejectRefreshFailed}, true, false
	}
	rejection := currentIssueSnapshotRejection("issueUpdate", snapshot)
	if rejection.Reason != "" {
		return rejection, true, false
	}
	activeIDs, err := p.currentIssueGuard.resolveActiveStateIDs(ctx, p)
	if err != nil {
		return currentIssueStateLookupRejection(snapshot), true, false
	}
	if _, activeTarget := activeIDs[change.StateID]; activeTarget {
		return currentIssueActiveStateRejection(snapshot), true, false
	}
	return linearGraphQLMutationRejected{}, false, true
}

func (g currentIssueMutationGuard) matchesCurrentIssue(issueID string) bool {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return false
	}
	for _, candidate := range []string{g.issueID, g.issueIdentifier} {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" && strings.EqualFold(issueID, candidate) {
			return true
		}
	}
	return false
}

func currentIssueUnsupportedUpdateRejection() linearGraphQLMutationRejected {
	return linearGraphQLMutationRejected{
		OperationField: "issueUpdate",
		Reason:         currentIssueRejectUnsupportedShape,
	}
}

func currentIssueStateLookupRejection(snapshot IssueStateSnapshot) linearGraphQLMutationRejected {
	return linearGraphQLMutationRejected{
		OperationField: "issueUpdate",
		Reason:         currentIssueRejectStateLookupFailed,
		Found:          snapshot.Found,
		State:          snapshot.State,
		Terminal:       snapshot.Terminal,
	}
}

func currentIssueActiveStateRejection(snapshot IssueStateSnapshot) linearGraphQLMutationRejected {
	return linearGraphQLMutationRejected{
		OperationField: "issueUpdate",
		Reason:         currentIssueRejectActiveStateUpdate,
		Found:          snapshot.Found,
		State:          snapshot.State,
		Terminal:       snapshot.Terminal,
	}
}

func currentIssueSnapshotRejection(operationField string, snapshot IssueStateSnapshot) linearGraphQLMutationRejected {
	rejection := linearGraphQLMutationRejected{
		OperationField: operationField,
		Found:          snapshot.Found,
		State:          snapshot.State,
		Terminal:       snapshot.Terminal,
	}
	switch {
	case snapshot.OperatorTerminalStop:
		rejection.Reason = currentIssueRejectOperatorStop
	case !snapshot.Found:
		rejection.Reason = currentIssueRejectMissing
	case snapshot.Terminal:
		rejection.Reason = currentIssueRejectTerminal
	case !snapshot.Active:
		rejection.Reason = currentIssueRejectNotActive
	}
	return rejection
}

func currentIssueMutationRejectMessage(reason string) string {
	switch reason {
	case currentIssueRejectActiveStateUpdate:
		return "linear_graphql issueUpdate for the current issue cannot move it into a configured active state after the operator stop guard is enabled"
	case currentIssueRejectUnsupportedShape:
		return "linear_graphql issueUpdate uses an unsupported issueUpdate shape; provide id and input.stateId as literals or variables"
	case currentIssueRejectRefreshFailed:
		return "linear_graphql issueUpdate for the current issue was rejected because the current issue state could not be refreshed"
	case currentIssueRejectMissing:
		return "linear_graphql issueUpdate for the current issue was rejected because the issue state is unknown"
	case currentIssueRejectTerminal:
		return "linear_graphql issueUpdate for the current issue was rejected because the issue is terminal"
	case currentIssueRejectNotActive:
		return "linear_graphql issueUpdate for the current issue was rejected because the issue is not active"
	case currentIssueRejectOperatorStop:
		return "linear_graphql issueUpdate for the current issue was rejected because Operator Terminal Stop is active"
	case currentIssueRejectStateLookupFailed:
		return "linear_graphql issueUpdate for the current issue was rejected because active state IDs could not be resolved"
	default:
		return "linear_graphql issueUpdate for the current issue was rejected"
	}
}

func (g currentIssueMutationGuard) postOperatorTerminalStop(ctx context.Context) bool {
	if strings.TrimSpace(g.issueID) == "" || g.operatorTerminalStopLookup == nil {
		return false
	}
	snapshot, ok := g.operatorTerminalStopLookup(ctx)
	if !ok {
		return false
	}
	return snapshot.OperatorTerminalStop
}

func (g currentIssueMutationGuard) resolveActiveStateIDs(ctx context.Context, p linearGraphQLProxy) (map[string]struct{}, error) {
	cache := g.cache
	if cache == nil {
		cache = &activeStateIDCache{}
	}
	if ids, ok := cache.copyLoaded(); ok {
		return ids, nil
	}
	ids, err := g.lookupActiveStateIDs(ctx, p)
	if err != nil {
		return nil, err
	}
	cache.store(ids)
	return ids, nil
}

func (g currentIssueMutationGuard) lookupActiveStateIDs(ctx context.Context, p linearGraphQLProxy) (map[string]struct{}, error) {
	ids := map[string]struct{}{}
	for _, state := range g.activeStates {
		state = strings.TrimSpace(state)
		if state == "" {
			continue
		}
		stateIDs, err := p.lookupWorkflowStateIDs(ctx, state, g.teamKey)
		if err != nil {
			return nil, err
		}
		for _, id := range stateIDs {
			ids[id] = struct{}{}
		}
	}
	return ids, nil
}

func (c *activeStateIDCache) copyLoaded() (map[string]struct{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		return nil, false
	}
	return copyStringSet(c.ids), true
}

func (c *activeStateIDCache) store(ids map[string]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = copyStringSet(ids)
	c.loaded = true
}

func copyStringSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for key := range in {
		out[key] = struct{}{}
	}
	return out
}

func (p linearGraphQLProxy) lookupWorkflowStateIDs(ctx context.Context, stateName, teamKey string) ([]string, error) {
	query, vars := workflowStateLookupQuery(stateName, teamKey)
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return nil, fmt.Errorf("build Linear workflowStates lookup: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, linearGraphQLStateLookupTimeout)
	defer cancel()
	req, err := p.workflowStateLookupRequest(reqCtx, body)
	if err != nil {
		return nil, err
	}
	resp, err := p.linearHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("send Linear workflowStates lookup: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("linear workflowStates lookup failed: %s", resp.Status)
	}
	return decodeWorkflowStateIDs(resp.Body, stateName)
}

func (p linearGraphQLProxy) workflowStateLookupRequest(ctx context.Context, body []byte) (*http.Request, error) {
	endpoint := linearGraphQLEndpoint(p.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build Linear workflowStates lookup request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", linearAuthorizationHeader(p.apiKey))
	return req, nil
}

func workflowStateLookupQuery(stateName, teamKey string) (string, map[string]any) {
	vars := map[string]any{"name": stateName}
	if strings.TrimSpace(teamKey) == "" {
		return `query StateByName($name: String!) {
  workflowStates(filter: { name: { eq: $name } }, first: 50) {
    nodes { id name }
  }
}`, vars
	}
	vars["teamKey"] = strings.TrimSpace(teamKey)
	return `query StateByName($name: String!, $teamKey: String!) {
  workflowStates(filter: { name: { eq: $name }, team: { key: { eq: $teamKey } } }, first: 50) {
    nodes { id name }
  }
}`, vars
}

func decodeWorkflowStateIDs(r io.Reader, stateName string) ([]string, error) {
	var out struct {
		Data struct {
			WorkflowStates struct {
				Nodes []struct {
					ID string `json:"id"`
				} `json:"nodes"`
			} `json:"workflowStates"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode Linear workflowStates lookup: %w", err)
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("linear workflowStates lookup returned errors for %q", stateName)
	}
	ids := make([]string, 0, len(out.Data.WorkflowStates.Nodes))
	for _, node := range out.Data.WorkflowStates.Nodes {
		id := strings.TrimSpace(node.ID)
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no Linear workflow state matches %q", stateName)
	}
	return ids, nil
}
