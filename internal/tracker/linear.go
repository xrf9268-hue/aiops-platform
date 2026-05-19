package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

const (
	linearIssuePageSize = 50
	maxLinearIssuePages = 200
)

type LinearClient struct {
	APIKey  string
	BaseURL string
	Config  workflow.TrackerConfig
	HTTP    *http.Client
}

func NewLinearClient(cfg workflow.TrackerConfig) *LinearClient {
	base := "https://api.linear.app/graphql"
	return &LinearClient{APIKey: cfg.APIKey, BaseURL: base, Config: cfg, HTTP: http.DefaultClient}
}

func (c *LinearClient) ListActiveIssues(ctx context.Context) ([]Issue, error) {
	return c.ListIssuesByStates(ctx, c.Config.ActiveStates)
}

func (c *LinearClient) ListIssuesByStates(ctx context.Context, states []string) ([]Issue, error) {
	if c.APIKey == "" {
		return nil, fmt.Errorf("Linear API key is required")
	}
	projectSlug := strings.TrimSpace(c.Config.ProjectSlug)
	if projectSlug == "" {
		return nil, fmt.Errorf("Linear project slug is required")
	}
	query := `query ListIssues($projectSlug: String!, $states: [String!], $first: Int!, $after: String) {
  issues(filter: { project: { slugId: { eq: $projectSlug } }, state: { name: { in: $states } } }, first: $first, after: $after) {
    nodes {
      id
      identifier
      title
      description
      url
      priority
      createdAt
      updatedAt
      project { slugId }
      team { key }
      labels(first: 50) { nodes { name } }
      customFieldValues(first: 50) { nodes { customField { name } value } }
      state { name }
    }
    pageInfo { hasNextPage endCursor }
  }
}`
	var issues []Issue
	var after any
	for page := 0; page < maxLinearIssuePages; page++ {
		var out struct {
			Data struct {
				Issues struct {
					Nodes []struct {
						ID          string `json:"id"`
						Identifier  string `json:"identifier"`
						Title       string `json:"title"`
						Description string `json:"description"`
						URL         string `json:"url"`
						Priority    int    `json:"priority"`
						CreatedAt   string `json:"createdAt"`
						UpdatedAt   string `json:"updatedAt"`
						Project     struct {
							SlugID string `json:"slugId"`
						} `json:"project"`
						Team struct {
							Key string `json:"key"`
						} `json:"team"`
						Labels struct {
							Nodes []struct {
								Name string `json:"name"`
							} `json:"nodes"`
						} `json:"labels"`
						CustomFieldValues struct {
							Nodes []struct {
								CustomField struct {
									Name string `json:"name"`
								} `json:"customField"`
								Value json.RawMessage `json:"value"`
							} `json:"nodes"`
						} `json:"customFieldValues"`
						State struct {
							Name string `json:"name"`
						} `json:"state"`
					} `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"issues"`
			} `json:"data"`
			Errors []map[string]any `json:"errors"`
		}
		if err := c.graphql(ctx, query, map[string]any{"projectSlug": projectSlug, "states": states, "first": linearIssuePageSize, "after": after}, &out); err != nil {
			return nil, err
		}
		if len(out.Errors) > 0 {
			return nil, fmt.Errorf("linear errors: %v", out.Errors)
		}
		for _, n := range out.Data.Issues.Nodes {
			var blockers []Blocker
			if isTodoState(n.State.Name) {
				var err error
				blockers, err = c.linearBlockersForIssue(ctx, n.ID)
				if err != nil {
					return nil, err
				}
			}
			labels := make([]string, 0, len(n.Labels.Nodes))
			for _, label := range n.Labels.Nodes {
				if strings.TrimSpace(label.Name) != "" {
					labels = append(labels, strings.ToLower(strings.TrimSpace(label.Name)))
				}
			}
			customFields := make(map[string]string, len(n.CustomFieldValues.Nodes))
			for _, field := range n.CustomFieldValues.Nodes {
				if strings.TrimSpace(field.CustomField.Name) == "" {
					continue
				}
				customFields[strings.TrimSpace(field.CustomField.Name)] = stringifyLinearCustomFieldValue(field.Value)
			}
			issues = append(issues, Issue{ID: n.ID, Identifier: n.Identifier, Title: n.Title, Description: n.Description, URL: n.URL, Priority: n.Priority, CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt, ProjectSlug: n.Project.SlugID, TeamKey: n.Team.Key, Labels: labels, CustomFields: customFields, State: n.State.Name, BlockedBy: blockers})
		}
		if !out.Data.Issues.PageInfo.HasNextPage {
			return issues, nil
		}
		if out.Data.Issues.PageInfo.EndCursor == "" {
			return nil, fmt.Errorf("linear pagination missing endCursor")
		}
		after = out.Data.Issues.PageInfo.EndCursor
	}
	return nil, fmt.Errorf("linear pagination exceeded %d pages", maxLinearIssuePages)
}

func stringifyLinearCustomFieldValue(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(string(raw))
}

func (c *LinearClient) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error) {
	if c.APIKey == "" {
		return nil, fmt.Errorf("Linear API key is required")
	}
	if len(issueIDs) == 0 {
		return map[string]string{}, nil
	}
	query := `query IssueStatesByIDs($ids: [ID!]!, $first: Int!) {
  issues(filter: { id: { in: $ids } }, first: $first) {
    nodes {
      id
      state { name }
    }
  }
}`
	states := make(map[string]string, len(issueIDs))
	for start := 0; start < len(issueIDs); start += linearIssuePageSize {
		end := start + linearIssuePageSize
		if end > len(issueIDs) {
			end = len(issueIDs)
		}
		chunk := issueIDs[start:end]
		var out struct {
			Data struct {
				Issues struct {
					Nodes []struct {
						ID    string `json:"id"`
						State struct {
							Name string `json:"name"`
						} `json:"state"`
					} `json:"nodes"`
				} `json:"issues"`
			} `json:"data"`
			Errors []map[string]any `json:"errors"`
		}
		if err := c.graphql(ctx, query, map[string]any{"ids": chunk, "first": len(chunk)}, &out); err != nil {
			return nil, err
		}
		if len(out.Errors) > 0 {
			return nil, fmt.Errorf("linear errors: %v", out.Errors)
		}
		for _, n := range out.Data.Issues.Nodes {
			states[n.ID] = n.State.Name
		}
	}
	return states, nil
}

type linearRelationNode struct {
	Type  string `json:"type"`
	Issue struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
		State      struct {
			Name string `json:"name"`
		} `json:"state"`
	} `json:"issue"`
}

func isTodoState(state string) bool {
	return strings.EqualFold(strings.TrimSpace(state), "Todo")
}

func (c *LinearClient) linearBlockersForIssue(ctx context.Context, issueID string) ([]Blocker, error) {
	var out struct {
		Data struct {
			Issue struct {
				InverseRelations struct {
					Nodes    []linearRelationNode `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"inverseRelations"`
			} `json:"issue"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	query := `query ListIssueInverseRelations($id: String!, $after: String) {
  issue(id: $id) {
    inverseRelations(first: 50, after: $after) { nodes { type issue { id identifier state { name } } } pageInfo { hasNextPage endCursor } }
  }
}`
	if err := c.graphql(ctx, query, map[string]any{"id": issueID, "after": nil}, &out); err != nil {
		return nil, err
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("linear errors: %v", out.Errors)
	}
	return c.linearBlockersFromInverseRelations(ctx, issueID, out.Data.Issue.InverseRelations.Nodes, out.Data.Issue.InverseRelations.PageInfo.HasNextPage, out.Data.Issue.InverseRelations.PageInfo.EndCursor)
}

func (c *LinearClient) linearBlockersFromInverseRelations(ctx context.Context, issueID string, nodes []linearRelationNode, hasNextPage bool, endCursor string) ([]Blocker, error) {
	blockers := make([]Blocker, 0, len(nodes))
	appendBlockers := func(nodes []linearRelationNode) {
		for _, r := range nodes {
			if r.Type != "blocks" {
				continue
			}
			blockers = append(blockers, Blocker{ID: r.Issue.ID, Identifier: r.Issue.Identifier, State: r.Issue.State.Name})
		}
	}
	appendBlockers(nodes)
	for page := 0; hasNextPage && page < maxLinearIssuePages; page++ {
		if endCursor == "" {
			return nil, fmt.Errorf("linear inverse relation pagination missing endCursor for issue %s", issueID)
		}
		var out struct {
			Data struct {
				Issue struct {
					InverseRelations struct {
						Nodes    []linearRelationNode `json:"nodes"`
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"inverseRelations"`
				} `json:"issue"`
			} `json:"data"`
			Errors []map[string]any `json:"errors"`
		}
		query := `query ListIssueInverseRelations($id: String!, $after: String) {
  issue(id: $id) {
    inverseRelations(first: 50, after: $after) { nodes { type issue { id identifier state { name } } } pageInfo { hasNextPage endCursor } }
  }
}`
		if err := c.graphql(ctx, query, map[string]any{"id": issueID, "after": endCursor}, &out); err != nil {
			return nil, err
		}
		if len(out.Errors) > 0 {
			return nil, fmt.Errorf("linear errors: %v", out.Errors)
		}
		appendBlockers(out.Data.Issue.InverseRelations.Nodes)
		hasNextPage = out.Data.Issue.InverseRelations.PageInfo.HasNextPage
		endCursor = out.Data.Issue.InverseRelations.PageInfo.EndCursor
	}
	if hasNextPage {
		return nil, fmt.Errorf("linear inverse relation pagination exceeded %d pages for issue %s", maxLinearIssuePages, issueID)
	}
	return blockers, nil
}

// MoveIssueToState updates the named Linear issue so its workflowState
// matches stateName. Linear's GraphQL requires a state ID rather than a
// name, so this performs a workflowStates lookup first (scoped to the
// configured TeamKey when present so identically-named states from
// other teams cannot match by accident). A non-nil error here is
// surfaced as a tracker_transition_error event by the worker; it never
// aborts the underlying task.
func (c *LinearClient) MoveIssueToState(ctx context.Context, issueID, stateName string) error {
	if c.APIKey == "" {
		return fmt.Errorf("Linear API key is required")
	}
	if issueID == "" {
		return fmt.Errorf("issue id is required")
	}
	if stateName == "" {
		return fmt.Errorf("state name is required")
	}
	stateID, err := c.lookupStateID(ctx, stateName)
	if err != nil {
		return fmt.Errorf("lookup state %q: %w", stateName, err)
	}
	mutation := `mutation IssueUpdate($id: String!, $stateId: String!) {
  issueUpdate(id: $id, input: { stateId: $stateId }) { success }
}`
	var out struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	if err := c.graphql(ctx, mutation, map[string]any{"id": issueID, "stateId": stateID}, &out); err != nil {
		return err
	}
	if len(out.Errors) > 0 {
		return fmt.Errorf("linear errors: %v", out.Errors)
	}
	if !out.Data.IssueUpdate.Success {
		return fmt.Errorf("linear: issueUpdate did not report success")
	}
	return nil
}

// AddComment posts a comment on the named Linear issue. Used as the
// failure fallback when MoveIssueToState fails so a failure is still
// visible on the issue even if the workflow state could not be moved.
func (c *LinearClient) AddComment(ctx context.Context, issueID, body string) error {
	if c.APIKey == "" {
		return fmt.Errorf("Linear API key is required")
	}
	if issueID == "" {
		return fmt.Errorf("issue id is required")
	}
	mutation := `mutation CommentCreate($issueId: String!, $body: String!) {
  commentCreate(input: { issueId: $issueId, body: $body }) { success }
}`
	var out struct {
		Data struct {
			CommentCreate struct {
				Success bool `json:"success"`
			} `json:"commentCreate"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	if err := c.graphql(ctx, mutation, map[string]any{"issueId": issueID, "body": body}, &out); err != nil {
		return err
	}
	if len(out.Errors) > 0 {
		return fmt.Errorf("linear errors: %v", out.Errors)
	}
	if !out.Data.CommentCreate.Success {
		return fmt.Errorf("linear: commentCreate did not report success")
	}
	return nil
}

// lookupStateID resolves a workflowState name to its UUID. When TeamKey
// is configured the filter pins the lookup to that team so a state with
// the same label in another team cannot be picked. When TeamKey is empty
// (operators that have not set it) the first matching state is used,
// which is acceptable for personal/single-team boards.
func (c *LinearClient) lookupStateID(ctx context.Context, stateName string) (string, error) {
	var query string
	vars := map[string]any{"name": stateName}
	if c.Config.TeamKey != "" {
		query = `query StateByName($name: String!, $teamKey: String!) {
  workflowStates(filter: { name: { eq: $name }, team: { key: { eq: $teamKey } } }, first: 5) {
    nodes { id name }
  }
}`
		vars["teamKey"] = c.Config.TeamKey
	} else {
		query = `query StateByName($name: String!) {
  workflowStates(filter: { name: { eq: $name } }, first: 5) {
    nodes { id name }
  }
}`
	}
	var out struct {
		Data struct {
			WorkflowStates struct {
				Nodes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"workflowStates"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	if err := c.graphql(ctx, query, vars, &out); err != nil {
		return "", err
	}
	if len(out.Errors) > 0 {
		return "", fmt.Errorf("linear errors: %v", out.Errors)
	}
	if len(out.Data.WorkflowStates.Nodes) == 0 {
		return "", fmt.Errorf("no workflow state matches %q", stateName)
	}
	return out.Data.WorkflowStates.Nodes[0].ID, nil
}

// graphql issues a single GraphQL POST against c.BaseURL and decodes the
// response into out. It is unexported because callers should go through
// one of the typed methods (ListActiveIssues / MoveIssueToState /
// AddComment) so the request shape and error semantics are consistent.
func (c *LinearClient) graphql(ctx context.Context, query string, variables map[string]any, out any) error {
	payload := map[string]any{"query": query, "variables": variables}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.APIKey)
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("linear request failed: %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
