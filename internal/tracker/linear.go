package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

const maxLinearIssuePages = 200

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
	query := `query ListIssues($states: [String!], $after: String) {
  issues(filter: { state: { name: { in: $states } } }, first: 50, after: $after) {
    nodes { id identifier title description url updatedAt state { name } }
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
						UpdatedAt   string `json:"updatedAt"`
						State       struct {
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
		if err := c.graphql(ctx, query, map[string]any{"states": states, "after": after}, &out); err != nil {
			return nil, err
		}
		if len(out.Errors) > 0 {
			return nil, fmt.Errorf("linear errors: %v", out.Errors)
		}
		for _, n := range out.Data.Issues.Nodes {
			issues = append(issues, Issue{ID: n.ID, Identifier: n.Identifier, Title: n.Title, Description: n.Description, URL: n.URL, UpdatedAt: n.UpdatedAt, State: n.State.Name})
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
