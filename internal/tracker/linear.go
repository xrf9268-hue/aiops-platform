package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	CategoryRateLimited               Category = "rate_limited"
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
	// ErrRateLimited classifies a rate-limited response from any tracker
	// client — HTTP 429 (Linear, Gitea, GitHub), plus GitHub's documented
	// 403 limit shapes (see githubRateLimited). The wrapped
	// *RateLimitedError carries the parsed retry hint; see
	// NewRateLimitedError.
	ErrRateLimited = &Error{Category: CategoryRateLimited}
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

// listLinearIssuesQuery is the fixed ListIssues GraphQL query.
//
// customFieldValues is intentionally NOT requested. Linear's GraphQL
// schema does not expose a customFieldValues field on Issue (only
// customerTicketCount matches `custom*`); requesting it produced
// HTTP 400 GRAPHQL_VALIDATION_FAILED on every poll (#326). The
// upstream Elixir reference (elixir/lib/symphony_elixir/linear/client.ex)
// also omits any custom-field fragment for the same reason.
//
// labels are projected at first:250 (Linear's connection maximum). The SPEC
// §6.4 required_labels gate matches against exactly this projection, and the
// IssueStatesByIDs refresh below now also cancels running work on label
// removal, so the cap must be high enough that a required label cannot sort
// past it and look removed (#705). Keep both label projections in step.
const listLinearIssuesQuery = `query ListIssues($projectSlug: String!, $states: [String!], $first: Int!, $after: String) {
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
      labels(first: 250) { nodes { name } }
      state { name }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

// linearIssueNode is one node of the ListIssues response. The json tags are
// the wire contract; keep them byte-for-byte in step with listLinearIssuesQuery.
type linearIssueNode struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Priority    int    `json:"priority"`
	BranchName  string `json:"branchName"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
	Labels      struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	State struct {
		Name string `json:"name"`
	} `json:"state"`
}

// linearIssuesResponse wraps a single ListIssues page.
type linearIssuesResponse struct {
	Data struct {
		Issues struct {
			Nodes    []linearIssueNode `json:"nodes"`
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
		} `json:"issues"`
	} `json:"data"`
	Errors []map[string]any `json:"errors"`
}

func (c *LinearClient) ListIssuesByStates(ctx context.Context, states []string) ([]Issue, error) {
	projectSlug, err := c.requireListIssuesConfig()
	if err != nil {
		return nil, err
	}
	nonEmpty := normalizeRequestedStates(states)
	// SPEC §17.3: empty fetch_issues_by_states([]) returns empty without API call.
	if len(nonEmpty) == 0 {
		return nil, nil
	}
	var issues []Issue
	var after any
	maxPages := c.issueMaxPages()
	for page := 1; page <= maxPages; page++ {
		mapped, pageInfo, err := c.fetchLinearIssuesPage(ctx, map[string]any{"projectSlug": projectSlug, "states": nonEmpty, "first": linearIssuePageSize, "after": after})
		if err != nil {
			return nil, err
		}
		issues = append(issues, mapped...)
		nextCursor, hasNext, err := nextLinearIssueCursor(pageInfo, page, maxPages)
		if err != nil {
			return nil, err
		}
		if !hasNext {
			return issues, nil
		}
		after = nextCursor
	}
	return nil, NewError(CategoryIssueListingCapped, fmt.Sprintf("linear pagination exceeded %d pages", maxPages), nil)
}

func nextLinearIssueCursor(pageInfo linearPageInfo, page, maxPages int) (string, bool, error) {
	if !pageInfo.HasNextPage {
		return "", false, nil
	}
	if page >= maxPages {
		return "", false, NewError(CategoryIssueListingCapped, fmt.Sprintf("linear pagination exceeded %d pages", maxPages), nil)
	}
	if pageInfo.EndCursor == "" {
		return "", false, NewError(CategoryLinearMissingEndCursor, "linear pagination missing endCursor", nil)
	}
	return pageInfo.EndCursor, true, nil
}

// requireListIssuesConfig enforces the two ListIssuesByStates preconditions
// and returns the trimmed project slug. It mirrors the original guard order:
// the API key is checked before the project slug.
func (c *LinearClient) requireListIssuesConfig() (string, error) {
	if c.APIKey == "" {
		return "", NewError(CategoryMissingTrackerAPIKey, "Linear API key is required", nil)
	}
	projectSlug := strings.TrimSpace(c.Config.ProjectSlug)
	if projectSlug == "" {
		return "", NewError(CategoryMissingTrackerProjectSlug, "Linear project slug is required", nil)
	}
	return projectSlug, nil
}

func (c *LinearClient) issueMaxPages() int {
	if c != nil && c.Config.PaginationMaxPages > 0 {
		return c.Config.PaginationMaxPages
	}
	return maxLinearIssuePages
}

// normalizeRequestedStates trims each requested state and drops empties.
// The caller keeps the empty short-circuit (SPEC §17.3).
func normalizeRequestedStates(states []string) []string {
	nonEmpty := make([]string, 0, len(states))
	for _, s := range states {
		if t := strings.TrimSpace(s); t != "" {
			nonEmpty = append(nonEmpty, t)
		}
	}
	return nonEmpty
}

// linearPageInfo carries the page-control fields the ListIssuesByStates loop
// inspects after each page so the HasNextPage / EndCursor branches stay
// visible in the caller.
type linearPageInfo struct {
	HasNextPage bool
	EndCursor   string
}

// fetchLinearIssuesPage issues one ListIssues page request, surfaces any
// GraphQL errors, and maps the page's nodes to domain Issues in order. Page
// control (HasNextPage / EndCursor / cursor advance) stays in the caller so
// the early-return semantics remain visible. A page with no nodes yields a nil
// issue slice so the caller's accumulator preserves nil-vs-empty.
func (c *LinearClient) fetchLinearIssuesPage(ctx context.Context, vars map[string]any) ([]Issue, linearPageInfo, error) {
	var out linearIssuesResponse
	if err := c.graphql(ctx, listLinearIssuesQuery, vars, &out); err != nil {
		return nil, linearPageInfo{}, err
	}
	if len(out.Errors) > 0 {
		return nil, linearPageInfo{}, linearGraphQLErrors(out.Errors)
	}
	var issues []Issue
	for _, n := range out.Data.Issues.Nodes {
		iss, err := mapLinearIssueNode(n)
		if err != nil {
			return nil, linearPageInfo{}, err
		}
		issues = append(issues, iss)
	}
	if err := c.attachLinearBlockers(ctx, issues); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, linearPageInfo{}, errors.Join(err, ctxErr)
		}
		if errors.Is(err, ErrRateLimited) {
			return nil, linearPageInfo{}, err
		}
		// Blocker data only gates Todo candidate dispatch. If the auxiliary
		// inverse-relations query fails, keep the primary page and fail Todo
		// candidates closed for this tick; non-Todo consumers ignore BlockedBy.
		log.Printf("event=linear_blocker_listing_failed error=%q detail=\"listing Todo issues fail closed with a placeholder blocker this page\"", err.Error())
		failClosedTodoIssueBlockers(issues)
	}
	pageInfo := linearPageInfo{
		HasNextPage: out.Data.Issues.PageInfo.HasNextPage,
		EndCursor:   out.Data.Issues.PageInfo.EndCursor,
	}
	return issues, pageInfo, nil
}

// mapLinearIssueNode maps one ListIssues node to a domain Issue. createdAt is
// parsed before updatedAt so the first malformed timestamp wins. Blockers are
// not resolved here: attachLinearBlockers fills BlockedBy for the page's
// Todo-state issues in one batched query (#672).
func mapLinearIssueNode(n linearIssueNode) (Issue, error) {
	createdAt, err := parseLinearIssueTime("createdAt", n.CreatedAt)
	if err != nil {
		return Issue{}, err
	}
	updatedAt, err := parseLinearIssueTime("updatedAt", n.UpdatedAt)
	if err != nil {
		return Issue{}, err
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
	return Issue{ID: n.ID, Identifier: n.Identifier, Title: n.Title, Description: n.Description, URL: n.URL, Priority: n.Priority, BranchName: n.BranchName, CreatedAt: createdAt, UpdatedAt: updatedAt, Labels: labels, State: n.State.Name}, nil
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

func failClosedTodoIssueBlockers(issues []Issue) {
	for i := range issues {
		if isTodoState(issues[i].State) {
			issues[i].BlockedBy = []BlockerRef{{}}
		}
	}
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

type linearInverseRelationsPage struct {
	Nodes    *[]linearRelationNode `json:"nodes"`
	PageInfo *struct {
		HasNextPage *bool   `json:"hasNextPage"`
		EndCursor   *string `json:"endCursor"`
	} `json:"pageInfo"`
}

func isTodoState(state string) bool {
	return strings.EqualFold(strings.TrimSpace(state), "Todo")
}

// attachLinearBlockers resolves blocker (inverse-relation) data for the page's
// Todo-state issues in one batched query instead of one query per issue (#672).
// Non-Todo issues never carry blockers, matching the prior per-issue behavior,
// so their BlockedBy stays nil. Pagination of any single issue's
// inverseRelations beyond the first page is preserved deeper in the call chain
// by fetchLinearBlockerChunk -> linearBlockersFromInverseRelations.
func (c *LinearClient) attachLinearBlockers(ctx context.Context, issues []Issue) error {
	ids := make([]string, 0, len(issues))
	for _, iss := range issues {
		if isTodoState(iss.State) {
			ids = append(ids, iss.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	blockers, err := c.linearBlockersForIssues(ctx, ids)
	if err != nil {
		return err
	}
	for i := range issues {
		if isTodoState(issues[i].State) {
			issues[i].BlockedBy = blockers[issues[i].ID]
		}
	}
	return nil
}

// listLinearIssuesInverseRelationsQuery fetches the first inverseRelations page
// for a batch of issue ids in a single request. It is a separate query from
// listLinearIssuesQuery so the candidate-list query's complexity does not grow
// per poll; the inverseRelations page size matches the per-issue overflow query
// in linearBlockersFromInverseRelations.
const listLinearIssuesInverseRelationsQuery = `query ListIssuesInverseRelations($ids: [ID!]!, $first: Int!) {
  issues(filter: { id: { in: $ids } }, first: $first) {
    nodes {
      id
      inverseRelations(first: 50) { nodes { type issue { id identifier state { name } } } pageInfo { hasNextPage endCursor } }
    }
  }
}`

// linearBatchInverseRelationsResponse is the batched first-page inverse-relations
// payload returned by listLinearIssuesInverseRelationsQuery for a chunk of ids.
type linearBatchInverseRelationsResponse struct {
	Data *struct {
		Issues *struct {
			Nodes *[]struct {
				ID               *string                     `json:"id"`
				InverseRelations *linearInverseRelationsPage `json:"inverseRelations"`
			} `json:"nodes"`
		} `json:"issues"`
	} `json:"data"`
	Errors []map[string]any `json:"errors"`
}

// linearBlockersForIssues fetches first-page inverse relations for every id in
// one batched query per linearIssuePageSize chunk, then continues per-issue
// pagination only for ids whose blockers overflow the first page. Every
// requested id is present in the result (empty slice when it has no blockers)
// so callers see the same non-nil empty BlockedBy the per-issue path produced.
func (c *LinearClient) linearBlockersForIssues(ctx context.Context, ids []string) (map[string][]BlockerRef, error) {
	result := make(map[string][]BlockerRef, len(ids))
	for _, id := range ids {
		result[id] = []BlockerRef{}
	}
	for start := 0; start < len(ids); start += linearIssuePageSize {
		end := start + linearIssuePageSize
		if end > len(ids) {
			end = len(ids)
		}
		if err := c.fetchLinearBlockerChunk(ctx, ids[start:end], result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// fetchLinearBlockerChunk runs one batched inverse-relations query for chunk and
// records each returned issue's blockers into result, paginating any single
// issue's overflow pages via linearBlockersFromInverseRelations.
func (c *LinearClient) fetchLinearBlockerChunk(ctx context.Context, chunk []string, result map[string][]BlockerRef) error {
	var out linearBatchInverseRelationsResponse
	if err := c.graphql(ctx, listLinearIssuesInverseRelationsQuery, map[string]any{"ids": chunk, "first": len(chunk)}, &out); err != nil {
		return err
	}
	if len(out.Errors) > 0 {
		return linearGraphQLErrors(out.Errors)
	}
	if out.Data == nil || out.Data.Issues == nil || out.Data.Issues.Nodes == nil {
		return incompleteLinearBlockerResponse("missing data.issues.nodes")
	}
	requested := make(map[string]struct{}, len(chunk))
	for _, id := range chunk {
		requested[id] = struct{}{}
	}
	seen := make(map[string]struct{}, len(chunk))
	for _, n := range *out.Data.Issues.Nodes {
		id, blockers, err := c.linearBlockersForBatchNode(ctx, n.ID, n.InverseRelations, requested, seen)
		if err != nil {
			return err
		}
		result[id] = blockers
		seen[id] = struct{}{}
	}
	if len(seen) != len(requested) {
		return incompleteLinearBlockerResponse("response omitted a requested issue node")
	}
	return nil
}

func (c *LinearClient) linearBlockersForBatchNode(
	ctx context.Context,
	rawID *string,
	page *linearInverseRelationsPage,
	requested, seen map[string]struct{},
) (string, []BlockerRef, error) {
	if rawID == nil || strings.TrimSpace(*rawID) == "" {
		return "", nil, incompleteLinearBlockerResponse("issue node missing id")
	}
	id := strings.TrimSpace(*rawID)
	if _, ok := requested[id]; !ok {
		return "", nil, incompleteLinearBlockerResponse("issue node id was not requested")
	}
	if _, duplicate := seen[id]; duplicate {
		return "", nil, incompleteLinearBlockerResponse("duplicate issue node id")
	}
	nodes, hasNextPage, endCursor, err := linearInverseRelationsValues(page)
	if err != nil {
		return "", nil, err
	}
	blockers, err := c.linearBlockersFromInverseRelations(ctx, id, nodes, hasNextPage, endCursor)
	if err != nil {
		return "", nil, err
	}
	return id, blockers, nil
}

func linearInverseRelationsValues(page *linearInverseRelationsPage) ([]linearRelationNode, bool, string, error) {
	if page == nil || page.Nodes == nil || page.PageInfo == nil || page.PageInfo.HasNextPage == nil {
		return nil, false, "", incompleteLinearBlockerResponse("missing inverseRelations nodes or pageInfo")
	}
	if err := validateLinearRelationNodes(*page.Nodes); err != nil {
		return nil, false, "", err
	}
	endCursor := ""
	if page.PageInfo.EndCursor != nil {
		endCursor = strings.TrimSpace(*page.PageInfo.EndCursor)
	}
	if *page.PageInfo.HasNextPage && endCursor == "" {
		return nil, false, "", incompleteLinearBlockerResponse("paginated inverseRelations missing endCursor")
	}
	return *page.Nodes, *page.PageInfo.HasNextPage, endCursor, nil
}

func validateLinearRelationNodes(nodes []linearRelationNode) error {
	for _, relation := range nodes {
		if strings.TrimSpace(relation.Type) == "" {
			return incompleteLinearBlockerResponse("inverse relation missing type")
		}
		if relation.Type == "blocks" && (strings.TrimSpace(relation.Issue.ID) == "" ||
			strings.TrimSpace(relation.Issue.Identifier) == "" || strings.TrimSpace(relation.Issue.State.Name) == "") {
			return incompleteLinearBlockerResponse("blocking relation missing issue identity or state")
		}
	}
	return nil
}

func incompleteLinearBlockerResponse(detail string) error {
	return NewError(CategoryLinearUnknownPayload, "incomplete Linear blocker response", fmt.Errorf("%w: %s", ErrIssueStateRefreshIncomplete, detail))
}

func (c *LinearClient) linearBlockersFromInverseRelations(ctx context.Context, issueID string, nodes []linearRelationNode, hasNextPage bool, endCursor string) ([]BlockerRef, error) { //nolint:gocognit // baseline (#521)
	if err := validateLinearRelationNodes(nodes); err != nil {
		return nil, err
	}
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
	maxPages := c.issueMaxPages()
	for page := 1; hasNextPage; page++ {
		if page >= maxPages {
			return nil, NewError(CategoryIssueListingCapped, fmt.Sprintf("linear inverse relation pagination exceeded %d pages for issue %s", maxPages, issueID), nil)
		}
		if endCursor == "" {
			return nil, NewError(CategoryLinearMissingEndCursor, fmt.Sprintf("linear inverse relation pagination missing endCursor for issue %s", issueID), nil)
		}
		var out struct {
			Data *struct {
				Issue *struct {
					InverseRelations *linearInverseRelationsPage `json:"inverseRelations"`
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
		if out.Data == nil || out.Data.Issue == nil {
			return nil, incompleteLinearBlockerResponse("missing data.issue")
		}
		nextNodes, nextPage, nextCursor, err := linearInverseRelationsValues(out.Data.Issue.InverseRelations)
		if err != nil {
			return nil, err
		}
		appendBlockers(nextNodes)
		hasNextPage = nextPage
		endCursor = nextCursor
	}
	return blockers, nil
}

// graphql issues a single GraphQL POST against c.BaseURL and decodes the
// response into out. It is unexported because callers should go through one of
// the typed read methods (ListIssuesByStates / FetchIssueStatesByIDs) so the
// request shape and error semantics are consistent.
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
	defer DrainAndClose(resp)
	if err := linearGraphQLResponseError(resp); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := DecodeJSONResponse(resp, out); err != nil {
		return NewError(CategoryLinearUnknownPayload, "decode Linear GraphQL response", err)
	}
	return nil
}

func linearGraphQLResponseError(resp *http.Response) error {
	if resp.StatusCode == http.StatusTooManyRequests {
		return NewRateLimitedError("linear request", resp.StatusCode, resp.Header)
	}
	if resp.StatusCode == http.StatusBadRequest {
		rateLimited, probeErr := linearGraphQLRateLimited(resp)
		if probeErr != nil {
			return NewError(CategoryLinearUnknownPayload, "decode Linear GraphQL error response", probeErr)
		}
		if rateLimited {
			return NewRateLimitedError("linear request", resp.StatusCode, resp.Header)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return NewError(CategoryLinearAPIStatus, fmt.Sprintf("linear request failed: status %d", resp.StatusCode), nil)
	}
	return nil
}

func linearGraphQLRateLimited(resp *http.Response) (bool, error) {
	var payload struct {
		Errors []struct {
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if err := DecodeJSONResponse(resp, &payload); err != nil {
		return false, err
	}
	for _, graphqlErr := range payload.Errors {
		if strings.EqualFold(strings.TrimSpace(graphqlErr.Extensions.Code), "RATELIMITED") {
			return true, nil
		}
	}
	return false, nil
}

func linearGraphQLErrors(errs []map[string]any) error {
	return NewError(CategoryLinearGraphQLErrors, fmt.Sprintf("linear errors: %v", errs), nil)
}
