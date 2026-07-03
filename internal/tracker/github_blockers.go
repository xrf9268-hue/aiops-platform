package tracker

import (
	"bytes"
	"context"
	"encoding/json"
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
	Data struct {
		Nodes []githubGraphQLIssueNode `json:"nodes"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type githubGraphQLIssueNode struct {
	ID        string `json:"id"`
	Number    int    `json:"number"`
	BlockedBy struct {
		Nodes    []githubGraphQLBlockerNode `json:"nodes"`
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
	} `json:"blockedBy"`
}

type githubGraphQLBlockerNode struct {
	ID         string `json:"id"`
	DatabaseID int64  `json:"databaseId"`
	Number     int    `json:"number"`
	State      string `json:"state"`
	Labels     struct {
		Nodes []githubGraphQLLabel `json:"nodes"`
	} `json:"labels"`
}

type githubGraphQLLabel struct {
	Name string `json:"name"`
}

type githubBodyBlockerResolution struct {
	Ref   BlockerRef
	Found bool
}

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
	blockers, err := c.blockersForGitHubSources(ctx, sources, githubConfiguredStates(c.Config))
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

func (c *GitHubClient) blockersForGitHubSources(ctx context.Context, sources []githubBlockerSource, configuredStates []string) (map[string][]BlockerRef, error) {
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
		blockedBy, err := c.blockersForGitHubSource(ctx, source, native, configuredStates)
		if err != nil {
			return nil, err
		}
		out[source.IssueID] = blockedBy
	}
	return out, nil
}

func (c *GitHubClient) blockersForGitHubSource(ctx context.Context, source githubBlockerSource, native map[string]githubNativeBlockers, configuredStates []string) ([]BlockerRef, error) {
	if !githubBlockerSourceNeedsHydration(source) {
		return []BlockerRef{}, nil
	}
	blockedBy, err := nativeGitHubBlockersForSource(source, native)
	if err != nil {
		return nil, err
	}
	for _, blocker := range c.bodyGitHubBlockers(ctx, source.Body, configuredStates, githubBlockerNumbers(blockedBy)) {
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
	out := make(map[string]githubNativeBlockers, len(nodeIDs))
	for start := 0; start < len(nodeIDs); start += githubBlockedBySourceChunkSize {
		end := start + githubBlockedBySourceChunkSize
		if end > len(nodeIDs) {
			end = len(nodeIDs)
		}
		decoded, err := c.fetchNativeGitHubBlockers(ctx, nodeIDs[start:end])
		if err != nil {
			return nil, err
		}
		for id, blockers := range graphQLNativeBlockers(decoded, configuredStates) {
			out[id] = blockers
		}
	}
	return out, nil
}

func githubBlockerNodeIDs(sources []githubBlockerSource) []string {
	nodeIDs := make([]string, 0, len(sources))
	for _, source := range sources {
		if nodeID := strings.TrimSpace(source.NodeID); nodeID != "" {
			nodeIDs = append(nodeIDs, nodeID)
		}
	}
	return nodeIDs
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

func graphQLNativeBlockers(decoded githubGraphQLBlockersResponse, configuredStates []string) map[string]githubNativeBlockers {
	out := make(map[string]githubNativeBlockers, len(decoded.Data.Nodes))
	for _, node := range decoded.Data.Nodes {
		blockers := make([]BlockerRef, 0, len(node.BlockedBy.Nodes))
		for _, blocker := range node.BlockedBy.Nodes {
			blockers = appendGitHubBlocker(blockers, githubGraphQLBlockerRef(blocker, configuredStates))
		}
		out[node.ID] = githubNativeBlockers{
			Blockers:   blockers,
			Incomplete: node.BlockedBy.PageInfo.HasNextPage,
		}
	}
	return out
}

func (c *GitHubClient) bodyGitHubBlockers(ctx context.Context, body string, configuredStates []string, skip map[int]struct{}) []BlockerRef {
	numbers := githubBlockedByIssueNumbers(body)
	if len(numbers) == 0 {
		return nil
	}
	out := make([]BlockerRef, 0, len(numbers))
	cache := make(map[int]githubBodyBlockerResolution, len(numbers))
	for _, number := range numbers {
		if _, ok := skip[number]; ok {
			continue
		}
		resolved, ok := cache[number]
		if !ok {
			resolved = c.resolveBodyGitHubBlocker(ctx, number, configuredStates)
			cache[number] = resolved
		}
		if resolved.Found {
			out = appendGitHubBlocker(out, resolved.Ref)
		}
	}
	return out
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

func (c *GitHubClient) resolveBodyGitHubBlocker(ctx context.Context, issueNumber int, configuredStates []string) githubBodyBlockerResolution {
	issue, found, err := c.getIssueByNumber(ctx, issueNumber)
	if err != nil {
		if c.Logf != nil {
			c.Logf("github body blocker lookup for #%d failed; omitting unverified fallback blocker this tick: %v", issueNumber, err)
		}
		return githubBodyBlockerResolution{}
	}
	if !found {
		return githubBodyBlockerResolution{}
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
	}
}

func githubGraphQLBlockerRef(blocker githubGraphQLBlockerNode, configuredStates []string) BlockerRef {
	labels := make([]githubLabel, 0, len(blocker.Labels.Nodes))
	for _, label := range blocker.Labels.Nodes {
		labels = append(labels, githubLabel(label))
	}
	id := strings.TrimSpace(blocker.ID)
	if blocker.DatabaseID > 0 {
		id = strconv.FormatInt(blocker.DatabaseID, 10)
	}
	return BlockerRef{
		ID:         id,
		Identifier: fmt.Sprintf("#%d", blocker.Number),
		State: githubResolveState(githubIssue{
			Number: blocker.Number,
			State:  blocker.State,
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
