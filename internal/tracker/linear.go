package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

const (
	linearIssuePageSize = 50
	maxLinearIssuePages = 200
)

type Category string

const (
	CategoryUnsupportedTrackerKind    Category = "unsupported_tracker_kind"
	CategoryMissingTrackerAPIKey      Category = "missing_tracker_api_key"
	CategoryMissingTrackerProjectSlug Category = "missing_tracker_project_slug"
	CategoryLinearAPIRequest          Category = "linear_api_request"
	CategoryLinearAPIStatus           Category = "linear_api_status"
	CategoryLinearGraphQLErrors       Category = "linear_graphql_errors"
	CategoryLinearUnknownPayload      Category = "linear_unknown_payload"
	CategoryLinearMissingEndCursor    Category = "linear_missing_end_cursor"
	CategoryIssueListingCapped        Category = "issue_listing_capped"
)

var (
	ErrUnsupportedTrackerKind    = &Error{Category: CategoryUnsupportedTrackerKind}
	ErrMissingTrackerAPIKey      = &Error{Category: CategoryMissingTrackerAPIKey}
	ErrMissingTrackerProjectSlug = &Error{Category: CategoryMissingTrackerProjectSlug}
	ErrLinearAPIRequest          = &Error{Category: CategoryLinearAPIRequest}
	ErrLinearAPIStatus           = &Error{Category: CategoryLinearAPIStatus}
	ErrLinearGraphQLErrors       = &Error{Category: CategoryLinearGraphQLErrors}
	ErrLinearUnknownPayload      = &Error{Category: CategoryLinearUnknownPayload}
	ErrLinearMissingEndCursor    = &Error{Category: CategoryLinearMissingEndCursor}
	// ErrIssueListingCapped is returned by ListIssuesByStates when pagination
	// cap is hit on any collection, so the returned issue set would be partial.
	// Callers that rely on listing completeness (e.g. startup reconciliation,
	// which deletes workspaces not seen in the active list) must treat this as
	// a fetch failure and skip cleanup.
	ErrIssueListingCapped = &Error{Category: CategoryIssueListingCapped}
)

type Error struct {
	Category Category
	Message  string
	Err      error
}

func NewError(category Category, message string, err error) *Error {
	return &Error{Category: category, Message: message, Err: err}
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	msg := e.Message
	if msg == "" {
		msg = string(e.Category)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *Error) Is(target error) bool {
	if e == nil || e.Category == "" || target == nil {
		return false
	}
	var targetErr *Error
	return errors.As(target, &targetErr) && targetErr.Category == e.Category
}

func ErrorCategory(err error) (Category, bool) {
	var trackerErr *Error
	if errors.As(err, &trackerErr) {
		return trackerErr.Category, trackerErr.Category != ""
	}
	return "", false
}

type LinearClient struct {
	APIKey  string
	BaseURL string
	Config  workflow.TrackerConfig
	HTTP    *http.Client
	// RequestTimeout caps each Linear GraphQL request per SPEC §11.2.
	// Defaults to 30s when zero.
	RequestTimeout time.Duration
}

const defaultLinearRequestTimeout = 30 * time.Second

// DefaultLinearEndpoint is the Linear GraphQL endpoint per SPEC §5.3.1.
const DefaultLinearEndpoint = "https://api.linear.app/graphql"

func NewLinearClient(cfg workflow.TrackerConfig) *LinearClient {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = DefaultLinearEndpoint
	}
	return &LinearClient{
		APIKey:         cfg.APIKey,
		BaseURL:        endpoint,
		Config:         cfg,
		HTTP:           http.DefaultClient,
		RequestTimeout: defaultLinearRequestTimeout,
	}
}

func (c *LinearClient) ListActiveIssues(ctx context.Context) ([]Issue, error) {
	return c.ListIssuesByStates(ctx, c.Config.ActiveStates)
}

func (c *LinearClient) ListIssuesByStates(ctx context.Context, states []string) ([]Issue, error) { //nolint:gocognit,funlen // baseline (#521)
	if c.APIKey == "" {
		return nil, NewError(CategoryMissingTrackerAPIKey, "Linear API key is required", nil)
	}
	projectSlug := strings.TrimSpace(c.Config.ProjectSlug)
	if projectSlug == "" {
		return nil, NewError(CategoryMissingTrackerProjectSlug, "Linear project slug is required", nil)
	}
	// SPEC §17.3: empty fetch_issues_by_states([]) returns empty without API call.
	nonEmpty := make([]string, 0, len(states))
	for _, s := range states {
		if t := strings.TrimSpace(s); t != "" {
			nonEmpty = append(nonEmpty, t)
		}
	}
	if len(nonEmpty) == 0 {
		return nil, nil
	}
	// customFieldValues is intentionally NOT requested. Linear's GraphQL
	// schema does not expose a customFieldValues field on Issue (only
	// customerTicketCount matches `custom*`); requesting it produced
	// HTTP 400 GRAPHQL_VALIDATION_FAILED on every poll (#326). The
	// upstream Elixir reference (elixir/lib/symphony_elixir/linear/client.ex)
	// also omits any custom-field fragment for the same reason.
	// `services[].tracker.custom_fields` route predicates are rejected at
	// workflow load time until Linear surfaces a working query field.
	query := `query ListIssues($projectSlug: String!, $states: [String!], $first: Int!, $after: String) {
  issues(filter: { project: { slugId: { eq: $projectSlug } }, state: { name: { in: $states } } }, first: $first, after: $after) {
    nodes {
      id
      identifier
      title
      description
      url
      priority
      branchName
      createdAt
      updatedAt
      project { slugId }
      team { key }
      labels(first: 50) { nodes { name } }
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
						BranchName  string `json:"branchName"`
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
		if err := c.graphql(ctx, query, map[string]any{"projectSlug": projectSlug, "states": nonEmpty, "first": linearIssuePageSize, "after": after}, &out); err != nil {
			return nil, err
		}
		if len(out.Errors) > 0 {
			return nil, linearGraphQLErrors(out.Errors)
		}
		for _, n := range out.Data.Issues.Nodes {
			createdAt, err := parseLinearIssueTime("createdAt", n.CreatedAt)
			if err != nil {
				return nil, err
			}
			updatedAt, err := parseLinearIssueTime("updatedAt", n.UpdatedAt)
			if err != nil {
				return nil, err
			}
			var blockers []BlockerRef
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
			// Issue.CustomFields stays nil — Linear's GraphQL schema does not
			// expose any custom-field data on Issue (introspection confirms
			// only `customerTicketCount` matches `custom*`). See #326.
			issues = append(issues, Issue{ID: n.ID, Identifier: n.Identifier, Title: n.Title, Description: n.Description, URL: n.URL, Priority: n.Priority, BranchName: n.BranchName, CreatedAt: createdAt, UpdatedAt: updatedAt, ProjectSlug: n.Project.SlugID, TeamKey: n.Team.Key, Labels: labels, State: n.State.Name, BlockedBy: blockers})
		}
		if !out.Data.Issues.PageInfo.HasNextPage {
			return issues, nil
		}
		if out.Data.Issues.PageInfo.EndCursor == "" {
			return nil, NewError(CategoryLinearMissingEndCursor, "linear pagination missing endCursor", nil)
		}
		after = out.Data.Issues.PageInfo.EndCursor
	}
	return nil, fmt.Errorf("linear pagination exceeded %d pages", maxLinearIssuePages)
}

func parseLinearIssueTime(field, value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse Linear issue %s %q: %w", field, value, err)
	}
	return parsed, nil
}

func (c *LinearClient) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error) { //nolint:gocognit // baseline (#521)
	if c.APIKey == "" {
		return nil, NewError(CategoryMissingTrackerAPIKey, "Linear API key is required", nil)
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
			return nil, linearGraphQLErrors(out.Errors)
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

func (c *LinearClient) linearBlockersForIssue(ctx context.Context, issueID string) ([]BlockerRef, error) {
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
		return nil, linearGraphQLErrors(out.Errors)
	}
	return c.linearBlockersFromInverseRelations(ctx, issueID, out.Data.Issue.InverseRelations.Nodes, out.Data.Issue.InverseRelations.PageInfo.HasNextPage, out.Data.Issue.InverseRelations.PageInfo.EndCursor)
}

func (c *LinearClient) linearBlockersFromInverseRelations(ctx context.Context, issueID string, nodes []linearRelationNode, hasNextPage bool, endCursor string) ([]BlockerRef, error) { //nolint:gocognit // baseline (#521)
	blockers := make([]BlockerRef, 0, len(nodes))
	appendBlockers := func(nodes []linearRelationNode) {
		for _, r := range nodes {
			if r.Type != "blocks" {
				continue
			}
			blockers = append(blockers, BlockerRef{ID: r.Issue.ID, Identifier: r.Issue.Identifier, State: r.Issue.State.Name})
		}
	}
	appendBlockers(nodes)
	for page := 0; hasNextPage && page < maxLinearIssuePages; page++ {
		if endCursor == "" {
			return nil, NewError(CategoryLinearMissingEndCursor, fmt.Sprintf("linear inverse relation pagination missing endCursor for issue %s", issueID), nil)
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
			return nil, linearGraphQLErrors(out.Errors)
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
		return NewError(CategoryMissingTrackerAPIKey, "Linear API key is required", nil)
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
		return linearGraphQLErrors(out.Errors)
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
		return NewError(CategoryMissingTrackerAPIKey, "Linear API key is required", nil)
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
		return linearGraphQLErrors(out.Errors)
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
		return "", linearGraphQLErrors(out.Errors)
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
		return NewError(CategoryLinearAPIRequest, "build Linear GraphQL request", err)
	}
	timeout := c.RequestTimeout
	if timeout <= 0 {
		timeout = defaultLinearRequestTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.BaseURL, bytes.NewReader(b))
	if err != nil {
		return NewError(CategoryLinearAPIRequest, "build Linear GraphQL request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.APIKey)
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return NewError(CategoryLinearAPIRequest, "send Linear GraphQL request", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return NewError(CategoryLinearAPIStatus, fmt.Sprintf("linear request failed: %s", resp.Status), nil)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return NewError(CategoryLinearUnknownPayload, "decode Linear GraphQL response", err)
	}
	return nil
}

func linearGraphQLErrors(errs []map[string]any) error {
	return NewError(CategoryLinearGraphQLErrors, fmt.Sprintf("linear errors: %v", errs), nil)
}
