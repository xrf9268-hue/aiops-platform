package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

const (
	githubBlockedByPageSize        = 100
	githubBlockedBySourceChunkSize = 25
)

var githubBlockedByIssueRE = regexp.MustCompile(`(?i)\b(?:blocked by|depends on)\s+#([0-9]+)\b`)

type githubBlockerSource struct {
	IssueID    string
	Identifier string
	State      string
	NodeID     string
	Number     int
	Body       string
}

type githubNativeBlockers struct {
	Blockers   []BlockerRef
	Incomplete bool
}

type githubGraphQLBlockersResponse struct {
	Data *struct {
		Nodes *[]githubGraphQLIssueNode `json:"nodes"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type githubGraphQLIssueNode struct {
	ID        *string                 `json:"id"`
	Number    *int                    `json:"number"`
	BlockedBy *githubGraphQLBlockedBy `json:"blockedBy"`
}

type githubGraphQLBlockedBy struct {
	Nodes    *[]githubGraphQLBlockerNode `json:"nodes"`
	PageInfo *struct {
		HasNextPage *bool `json:"hasNextPage"`
	} `json:"pageInfo"`
}

type githubGraphQLBlockerNode struct {
	ID         *string `json:"id"`
	DatabaseID *int64  `json:"databaseId"`
	Number     *int    `json:"number"`
	State      *string `json:"state"`
	Labels     *struct {
		Nodes *[]githubGraphQLLabel `json:"nodes"`
	} `json:"labels"`
}

type githubGraphQLLabel struct {
	Name *string `json:"name"`
}

type githubBodyBlockerResolution struct {
	Ref   BlockerRef
	Found bool
}

type githubBodyBlockerLookupPolicy uint8

const (
	githubBodyBlockerBestEffort githubBodyBlockerLookupPolicy = iota
	githubBodyBlockerRefresh
)

func (c *GitHubClient) attachGitHubBlockersToIssues(ctx context.Context, issues []Issue) error {
	if len(issues) == 0 {
		return nil
	}
	sources := make([]githubBlockerSource, 0, len(issues))
	for i := range issues {
		meta, ok := c.cachedIssueMetadata(issues[i].ID)
		if !ok {
			issues[i].BlockedBy = []BlockerRef{}
			continue
		}
		sources = append(sources, githubBlockerSource{
			IssueID:    issues[i].ID,
			Identifier: issues[i].Identifier,
			State:      issues[i].State,
			NodeID:     meta.NodeID,
			Number:     meta.Number,
			Body:       meta.Body,
		})
	}
	blockers, err := c.blockersForGitHubSources(ctx, sources, githubConfiguredStates(c.Config), githubBodyBlockerBestEffort)
	if err != nil {
		return err
	}
	for i := range issues {
		if blockedBy, ok := blockers[issues[i].ID]; ok {
			issues[i].BlockedBy = blockedBy
			continue
		}
		if issues[i].BlockedBy == nil {
			issues[i].BlockedBy = []BlockerRef{}
		}
	}
	return nil
}

func (c *GitHubClient) blockersForGitHubSources(ctx context.Context, sources []githubBlockerSource, configuredStates []string, bodyLookupPolicy githubBodyBlockerLookupPolicy) (map[string][]BlockerRef, error) {
	out := make(map[string][]BlockerRef, len(sources))
	hydrationSources := githubBlockerHydrationSources(sources)
	native, nativeErr := c.nativeGitHubBlockers(ctx, hydrationSources, configuredStates)
	if nativeErr != nil {
		if c.Logf != nil {
			c.Logf("github blockedBy lookup failed; skipping unverified blocker state for this tick: %v", nativeErr)
		}
		return nil, nativeErr
	}
	for _, source := range sources {
		blockedBy, err := c.blockersForGitHubSource(ctx, source, native, configuredStates, bodyLookupPolicy)
		if err != nil {
			return nil, err
		}
		out[source.IssueID] = blockedBy
	}
	return out, nil
}

func (c *GitHubClient) blockersForGitHubSource(ctx context.Context, source githubBlockerSource, native map[string]githubNativeBlockers, configuredStates []string, bodyLookupPolicy githubBodyBlockerLookupPolicy) ([]BlockerRef, error) {
	if !githubBlockerSourceNeedsHydration(source) {
		return []BlockerRef{}, nil
	}
	blockedBy, err := nativeGitHubBlockersForSource(source, native)
	if err != nil {
		return nil, err
	}
	bodyBlockers, err := c.bodyGitHubBlockers(ctx, source.Body, configuredStates, githubBlockerNumbers(blockedBy), bodyLookupPolicy)
	if err != nil {
		return nil, err
	}
	for _, blocker := range bodyBlockers {
		blockedBy = appendGitHubBlocker(blockedBy, blocker)
	}
	if blockedBy == nil {
		return []BlockerRef{}, nil
	}
	return blockedBy, nil
}

func githubBlockerHydrationSources(sources []githubBlockerSource) []githubBlockerSource {
	hydrationSources := make([]githubBlockerSource, 0, len(sources))
	for _, source := range sources {
		if githubBlockerSourceNeedsHydration(source) {
			hydrationSources = append(hydrationSources, source)
		}
	}
	return hydrationSources
}

func githubBlockerSourceNeedsHydration(source githubBlockerSource) bool {
	return githubTodoWorkflowState(source.State)
}

func githubTodoWorkflowState(state string) bool {
	state = strings.ToLower(strings.TrimSpace(state))
	return state == "todo" || strings.HasSuffix(state, ":todo") || strings.HasSuffix(state, "/todo")
}

func nativeGitHubBlockersForSource(source githubBlockerSource, native map[string]githubNativeBlockers) ([]BlockerRef, error) {
	if source.NodeID == "" {
		return []BlockerRef{}, nil
	}
	got, ok := native[source.NodeID]
	if !ok {
		return nil, fmt.Errorf("github blockedBy lookup missing issue node %q for %s", source.NodeID, source.Identifier)
	}
	blockedBy := append([]BlockerRef(nil), got.Blockers...)
	if got.Incomplete {
		blockedBy = appendGitHubBlocker(blockedBy, unknownGitHubBlocker(source, "native blocker pagination incomplete"))
	}
	return blockedBy, nil
}

func (c *GitHubClient) nativeGitHubBlockers(ctx context.Context, sources []githubBlockerSource, configuredStates []string) (map[string]githubNativeBlockers, error) {
	nodeIDs := githubBlockerNodeIDs(sources)
	if len(nodeIDs) == 0 {
		return map[string]githubNativeBlockers{}, nil
	}
	sourceNumbers, err := githubBlockerSourceNumbers(sources)
	if err != nil {
		return nil, err
	}
	out := make(map[string]githubNativeBlockers, len(nodeIDs))
	for start := 0; start < len(nodeIDs); start += githubBlockedBySourceChunkSize {
		end := min(start+githubBlockedBySourceChunkSize, len(nodeIDs))
		blockersByID, err := c.nativeGitHubBlockerChunk(ctx, nodeIDs[start:end], sourceNumbers, configuredStates)
		if err != nil {
			return nil, err
		}
		for id, blockers := range blockersByID {
			out[id] = blockers
		}
	}
	return out, nil
}

func (c *GitHubClient) nativeGitHubBlockerChunk(ctx context.Context, nodeIDs []string, sourceNumbers map[string]int, configuredStates []string) (map[string]githubNativeBlockers, error) {
	decoded, err := c.fetchNativeGitHubBlockers(ctx, nodeIDs)
	if err != nil {
		return nil, err
	}
	expectedNumbers := make(map[string]int, len(nodeIDs))
	for _, id := range nodeIDs {
		expectedNumbers[id] = sourceNumbers[id]
	}
	return graphQLNativeBlockers(decoded, expectedNumbers, configuredStates)
}

func githubBlockerNodeIDs(sources []githubBlockerSource) []string {
	nodeIDs := make([]string, 0, len(sources))
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		nodeID := strings.TrimSpace(source.NodeID)
		if nodeID == "" {
			continue
		}
		if _, duplicate := seen[nodeID]; duplicate {
			continue
		}
		seen[nodeID] = struct{}{}
		nodeIDs = append(nodeIDs, nodeID)
	}
	return nodeIDs
}

func githubBlockerSourceNumbers(sources []githubBlockerSource) (map[string]int, error) {
	out := make(map[string]int, len(sources))
	for _, source := range sources {
		nodeID := strings.TrimSpace(source.NodeID)
		if nodeID == "" {
			continue
		}
		if source.Number <= 0 {
			return nil, incompleteGitHubBlockerResponse("source issue has invalid number")
		}
		if number, exists := out[nodeID]; exists && number != source.Number {
			return nil, incompleteGitHubBlockerResponse("source node id maps to conflicting numbers")
		}
		out[nodeID] = source.Number
	}
	return out, nil
}

func (c *GitHubClient) fetchNativeGitHubBlockers(ctx context.Context, nodeIDs []string) (githubGraphQLBlockersResponse, error) {
	var payload bytes.Buffer
	reqBody := map[string]any{
		"query": `query GitHubIssueBlockers($ids: [ID!]!, $first: Int!) {
  nodes(ids: $ids) {
    ... on Issue {
      id
      number
      blockedBy(first: $first) {
        nodes {
          id
          databaseId
          number
          state
          labels(first: 100) { nodes { name } }
        }
        pageInfo { hasNextPage }
      }
    }
  }
}`,
		"variables": map[string]any{"ids": nodeIDs, "first": githubBlockedByPageSize},
	}
	if err := json.NewEncoder(&payload).Encode(reqBody); err != nil {
		return githubGraphQLBlockersResponse{}, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, c.requestTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.graphqlEndpoint(), &payload)
	if err != nil {
		return githubGraphQLBlockersResponse{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return githubGraphQLBlockersResponse{}, err
	}
	defer DrainAndClose(resp)
	if githubRateLimited(resp) {
		return githubGraphQLBlockersResponse{}, NewRateLimitedError("fetch GitHub issue blockers", resp.StatusCode, resp.Header)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return githubGraphQLBlockersResponse{}, fmt.Errorf("fetch GitHub issue blockers failed: status %d", resp.StatusCode)
	}
	var decoded githubGraphQLBlockersResponse
	if err := DecodeJSONResponse(resp, &decoded); err != nil {
		return githubGraphQLBlockersResponse{}, fmt.Errorf("decode GitHub blockers response: %w", err)
	}
	if len(decoded.Errors) > 0 {
		return githubGraphQLBlockersResponse{}, fmt.Errorf("fetch GitHub issue blockers failed: %s", decoded.Errors[0].Message)
	}
	return decoded, nil
}

func graphQLNativeBlockers(decoded githubGraphQLBlockersResponse, expectedNumbers map[string]int, configuredStates []string) (map[string]githubNativeBlockers, error) {
	if decoded.Data == nil || decoded.Data.Nodes == nil {
		return nil, incompleteGitHubBlockerResponse("missing data.nodes")
	}
	out := make(map[string]githubNativeBlockers, len(*decoded.Data.Nodes))
	for _, node := range *decoded.Data.Nodes {
		id, blockers, err := githubNativeBlockersFromGraphQLNode(node, expectedNumbers, configuredStates)
		if err != nil {
			return nil, err
		}
		if _, duplicate := out[id]; duplicate {
			return nil, incompleteGitHubBlockerResponse("duplicate issue node id")
		}
		out[id] = blockers
	}
	if len(out) != len(expectedNumbers) {
		return nil, incompleteGitHubBlockerResponse("response omitted a requested source node")
	}
	return out, nil
}

func githubNativeBlockersFromGraphQLNode(node githubGraphQLIssueNode, expectedNumbers map[string]int, configuredStates []string) (string, githubNativeBlockers, error) {
	if err := validateGitHubGraphQLIssueNode(node); err != nil {
		return "", githubNativeBlockers{}, err
	}
	id := strings.TrimSpace(*node.ID)
	expectedNumber, requested := expectedNumbers[id]
	if !requested {
		return "", githubNativeBlockers{}, incompleteGitHubBlockerResponse("response contains an unrequested source node id")
	}
	if *node.Number != expectedNumber {
		return "", githubNativeBlockers{}, incompleteGitHubBlockerResponse("source node number does not match requested issue")
	}
	blockers := make([]BlockerRef, 0, len(*node.BlockedBy.Nodes))
	for _, blocker := range *node.BlockedBy.Nodes {
		blockers = appendGitHubBlocker(blockers, githubGraphQLBlockerRef(blocker, configuredStates))
	}
	return id, githubNativeBlockers{
		Blockers:   blockers,
		Incomplete: *node.BlockedBy.PageInfo.HasNextPage,
	}, nil
}

func validateGitHubGraphQLIssueNode(node githubGraphQLIssueNode) error {
	if err := validateGitHubGraphQLIssueNodeShape(node); err != nil {
		return err
	}
	for _, blocker := range *node.BlockedBy.Nodes {
		if err := validateGitHubGraphQLBlockerNode(blocker); err != nil {
			return err
		}
	}
	return nil
}

func validateGitHubGraphQLIssueNodeShape(node githubGraphQLIssueNode) error {
	if node.ID == nil || strings.TrimSpace(*node.ID) == "" || node.Number == nil || *node.Number <= 0 {
		return incompleteGitHubBlockerResponse("issue node missing identity or blockedBy fields")
	}
	if node.BlockedBy == nil || node.BlockedBy.Nodes == nil || node.BlockedBy.PageInfo == nil || node.BlockedBy.PageInfo.HasNextPage == nil {
		return incompleteGitHubBlockerResponse("issue node missing identity or blockedBy fields")
	}
	return nil
}

func validateGitHubGraphQLBlockerNode(blocker githubGraphQLBlockerNode) error {
	if blocker.ID == nil || strings.TrimSpace(*blocker.ID) == "" || blocker.Number == nil || *blocker.Number <= 0 ||
		blocker.State == nil || strings.TrimSpace(*blocker.State) == "" {
		return incompleteGitHubBlockerResponse("blocker node missing identity, state, or labels")
	}
	if blocker.Labels == nil || blocker.Labels.Nodes == nil {
		return incompleteGitHubBlockerResponse("blocker node missing identity, state, or labels")
	}
	for _, label := range *blocker.Labels.Nodes {
		if label.Name == nil || strings.TrimSpace(*label.Name) == "" {
			return incompleteGitHubBlockerResponse("blocker label missing non-empty name")
		}
	}
	return nil
}

func incompleteGitHubBlockerResponse(detail string) error {
	return fmt.Errorf("%w: GitHub blockedBy response %s", ErrIssueStateRefreshIncomplete, detail)
}

func (c *GitHubClient) bodyGitHubBlockers(ctx context.Context, body string, configuredStates []string, skip map[int]struct{}, policy githubBodyBlockerLookupPolicy) ([]BlockerRef, error) {
	numbers := githubBlockedByIssueNumbers(body)
	if len(numbers) == 0 {
		return nil, nil
	}
	out := make([]BlockerRef, 0, len(numbers))
	for _, number := range numbers {
		if _, ok := skip[number]; ok {
			continue
		}
		resolved, err := c.resolveBodyGitHubBlocker(ctx, number, configuredStates, policy)
		if err != nil {
			return nil, err
		}
		if resolved.Found {
			out = appendGitHubBlocker(out, resolved.Ref)
		}
	}
	return out, nil
}

func githubBlockerNumbers(blockers []BlockerRef) map[int]struct{} {
	out := map[int]struct{}{}
	for _, blocker := range blockers {
		if number, ok := githubIssueNumberFromIdentifier(blocker.Identifier); ok {
			out[number] = struct{}{}
		}
	}
	return out
}

func (c *GitHubClient) resolveBodyGitHubBlocker(ctx context.Context, issueNumber int, configuredStates []string, policy githubBodyBlockerLookupPolicy) (githubBodyBlockerResolution, error) {
	issue, found, lookupErr := c.getIssueByNumber(ctx, issueNumber)
	if lookupErr != nil {
		return c.bodyGitHubBlockerLookupFailure(ctx, issueNumber, lookupErr, policy)
	}
	if !found {
		return githubBodyBlockerResolution{}, nil
	}
	if issue.Number != issueNumber {
		if c.Logf != nil {
			c.Logf("github body blocker lookup for #%d returned issue #%d; failing closed with the requested blocker placeholder", issueNumber, issue.Number)
		}
		return githubBodyBlockerPlaceholder(issueNumber), nil
	}
	id := strconv.FormatInt(issue.ID, 10)
	c.cacheIssueNumber(id, issue.Number)
	c.cacheIssueMetadata(id, issue)
	return githubBodyBlockerResolution{
		Ref: BlockerRef{
			ID:         id,
			Identifier: fmt.Sprintf("#%d", issue.Number),
			State:      githubResolveState(issue, configuredStates),
		},
		Found: true,
	}, nil
}

func (c *GitHubClient) bodyGitHubBlockerLookupFailure(ctx context.Context, issueNumber int, lookupErr error, policy githubBodyBlockerLookupPolicy) (githubBodyBlockerResolution, error) {
	if policy == githubBodyBlockerBestEffort {
		if c.Logf != nil {
			c.Logf("github body blocker lookup for #%d failed; omitting unverified fallback blocker this tick: %v", issueNumber, lookupErr)
		}
		return githubBodyBlockerResolution{}, nil
	}
	if ShouldStopIssueStateRefresh(ctx, lookupErr) {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(lookupErr, ctxErr) {
			lookupErr = errors.Join(lookupErr, ctxErr)
		}
		return githubBodyBlockerResolution{}, lookupErr
	}
	if c.Logf != nil {
		c.Logf("github body blocker lookup for #%d failed; failing closed with an open placeholder this tick: %v", issueNumber, lookupErr)
	}
	return githubBodyBlockerPlaceholder(issueNumber), nil
}

func githubBodyBlockerPlaceholder(issueNumber int) githubBodyBlockerResolution {
	return githubBodyBlockerResolution{
		Ref:   BlockerRef{Identifier: fmt.Sprintf("#%d", issueNumber)},
		Found: true,
	}
}

func githubGraphQLBlockerRef(blocker githubGraphQLBlockerNode, configuredStates []string) BlockerRef {
	labels := make([]githubLabel, 0, len(*blocker.Labels.Nodes))
	for _, label := range *blocker.Labels.Nodes {
		labels = append(labels, githubLabel{Name: *label.Name})
	}
	id := strings.TrimSpace(*blocker.ID)
	if blocker.DatabaseID != nil && *blocker.DatabaseID > 0 {
		id = strconv.FormatInt(*blocker.DatabaseID, 10)
	}
	return BlockerRef{
		ID:         id,
		Identifier: fmt.Sprintf("#%d", *blocker.Number),
		State: githubResolveState(githubIssue{
			Number: *blocker.Number,
			State:  *blocker.State,
			Labels: labels,
		}, configuredStates),
	}
}

func githubBlockedByIssueNumbers(body string) []int {
	matches := githubBlockedByIssueRE.FindAllStringSubmatch(body, -1)
	out := make([]int, 0, len(matches))
	seen := map[int]struct{}{}
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		number, err := strconv.Atoi(match[1])
		if err != nil || number <= 0 {
			continue
		}
		if _, ok := seen[number]; ok {
			continue
		}
		seen[number] = struct{}{}
		out = append(out, number)
	}
	return out
}

func appendGitHubBlocker(blockers []BlockerRef, blocker BlockerRef) []BlockerRef {
	key := blocker.Identifier
	if strings.TrimSpace(key) == "" {
		key = blocker.ID
	}
	for _, existing := range blockers {
		existingKey := existing.Identifier
		if strings.TrimSpace(existingKey) == "" {
			existingKey = existing.ID
		}
		if existingKey == key {
			return blockers
		}
	}
	return append(blockers, blocker)
}

func unknownGitHubBlocker(source githubBlockerSource, _ string) BlockerRef {
	identifier := strings.TrimSpace(source.Identifier)
	if identifier == "" && source.Number > 0 {
		identifier = fmt.Sprintf("#%d", source.Number)
	}
	return BlockerRef{Identifier: identifier, State: ""}
}

func (c *GitHubClient) graphqlEndpoint() string {
	base := strings.TrimRight(c.BaseURL, "/")
	if strings.EqualFold(base, "https://api.github.com") {
		return base + "/graphql"
	}
	if strings.HasSuffix(base, "/api/v3") {
		return strings.TrimSuffix(base, "/api/v3") + "/api/graphql"
	}
	return base + "/graphql"
}

func (c *GitHubClient) cacheIssueMetadata(issueID string, issue githubIssue) {
	if strings.TrimSpace(issueID) == "" {
		return
	}
	c.issueMetadata.Store(issueID, githubIssueMetadata{
		NodeID: strings.TrimSpace(issue.NodeID),
		Number: issue.Number,
		Body:   issue.Body,
	})
}

func (c *GitHubClient) cachedIssueMetadata(issueID string) (githubIssueMetadata, bool) {
	got, ok := c.issueMetadata.Load(issueID)
	if !ok {
		return githubIssueMetadata{}, false
	}
	meta, ok := got.(githubIssueMetadata)
	return meta, ok
}
