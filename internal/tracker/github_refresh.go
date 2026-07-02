package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// FetchIssueStatesByIDs implements SPEC §11.1 for the GitHub adapter. GitHub's
// REST API has no `[ID!]` bulk endpoint, so this iterates one
// `GET /repos/{owner}/{repo}/issues/{number}` per ID using the repo issue
// number cached during prior list calls. Ref-aware callers can also provide
// the human identifier (`#123`) so a rebuilt tracker client can query by repo
// issue number before any list call repopulates the cache. Per-ID 404/410
// responses are treated as "issue removed" and silently skipped so a single
// deleted issue does not abort reconciliation for the rest of the running set.
// Other HTTP errors abort the whole call so a transient outage cannot silently
// degrade per-tick state refresh.
//
// State derivation: if any label on the issue matches a state configured in
// active_states / terminal_states / inactive_states (case-insensitive), the
// matched label is returned (lowercased, matching mapGitHubIssue's
// normalization). Otherwise the issue's open/closed state is returned. This
// matches the GitHub convention where workflow position is encoded as labels.
//
// BlockedBy carries GitHub dependency data from the native GraphQL
// Issue.blockedBy relation when available, plus the in-repo body fallback
// ("Blocked by #N" / "Depends on #N"). A non-nil empty slice means the refresh
// confirmed no blockers; unknown or incomplete dependency knowledge is surfaced
// as an empty-state placeholder so the Todo blocker gate fails closed.
func (c *GitHubClient) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]IssueState, error) {
	return c.FetchIssueStatesByRefs(ctx, IssueRefsFromIDs(issueIDs))
}

type githubFetchedIssueState struct {
	issueID string
	issue   githubIssue
	state   string
	labels  []string
}

func (c *GitHubClient) FetchIssueStatesByRefs(ctx context.Context, issueRefs []IssueRef) (map[string]IssueState, error) { //nolint:gocognit // baseline (#521)
	if strings.TrimSpace(c.Token) == "" {
		return nil, fmt.Errorf("GitHub tracker api_key is required")
	}
	if len(issueRefs) == 0 {
		return map[string]IssueState{}, nil
	}
	if strings.TrimSpace(c.Owner) == "" || strings.TrimSpace(c.Repo) == "" {
		return nil, fmt.Errorf("repo.owner and repo.name are required for GitHub tracker polling")
	}
	configuredStates := githubConfiguredStates(c.Config)
	fetched := make([]githubFetchedIssueState, 0, len(issueRefs))
	seen := map[string]struct{}{}
	for _, issueRef := range issueRefs {
		issueID := strings.TrimSpace(issueRef.ID)
		if issueID == "" {
			continue
		}
		if _, ok := seen[issueID]; ok {
			continue
		}
		seen[issueID] = struct{}{}
		issueNumber, ok := c.issueNumberForStateRefresh(issueRef)
		if !ok {
			c.logStateRefreshCacheMiss(issueRef)
			continue
		}
		issue, found, err := c.getIssueByNumber(ctx, issueNumber)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		refreshedID := strconv.FormatInt(issue.ID, 10)
		if !githubFetchedIssueMatchesRef(issueID, issueRef.Identifier, refreshedID, issue.Number) {
			c.cacheIssueNumber(refreshedID, issue.Number)
			continue
		}
		c.cacheIssueNumber(refreshedID, issue.Number)
		if issueNumberID := githubIssueNumberID(issue.Number); issueID == issueNumberID {
			c.cacheIssueNumber(issueID, issue.Number)
		}
		state := githubResolveState(issue, configuredStates)
		if state == "" {
			continue
		}
		fetched = append(fetched, githubFetchedIssueState{
			issueID: issueID,
			issue:   issue,
			state:   state,
			labels:  githubIssueLabels(issue),
		})
	}
	states := make(map[string]IssueState, len(fetched))
	blockersByID := c.blockersForRefreshedGitHubIssues(ctx, fetched, configuredStates)
	for _, row := range fetched {
		blockedBy := blockersByID[row.issueID]
		if blockedBy == nil {
			blockedBy = []BlockerRef{}
		}
		// Carry the full label set (SPEC §6.4 required_labels gate) alongside the
		// resolved state so label removal can stop/release already-claimed work.
		states[row.issueID] = IssueState{State: row.state, Labels: row.labels, BlockedBy: blockedBy}
	}
	return states, nil
}

func (c *GitHubClient) blockersForRefreshedGitHubIssues(ctx context.Context, rows []githubFetchedIssueState, configuredStates []string) map[string][]BlockerRef {
	sources := make([]githubBlockerSource, 0, len(rows))
	for _, row := range rows {
		sources = append(sources, githubBlockerSource{
			IssueID:    row.issueID,
			Identifier: fmt.Sprintf("#%d", row.issue.Number),
			NodeID:     row.issue.NodeID,
			Number:     row.issue.Number,
			Body:       row.issue.Body,
		})
	}
	return c.blockersForGitHubSources(ctx, sources, configuredStates)
}

func githubFetchedIssueMatchesRef(issueID, identifier, refreshedID string, issueNumber int) bool {
	issueID = strings.TrimSpace(issueID)
	if issueID == refreshedID {
		return true
	}
	if !githubIdentifierMatchesIssueNumber(identifier, issueNumber) {
		return false
	}
	if strings.HasPrefix(issueID, "#") {
		return issueID == fmt.Sprintf("#%d", issueNumber)
	}
	return issueID == githubIssueNumberID(issueNumber)
}

func githubIdentifierMatchesIssueNumber(identifier string, issueNumber int) bool {
	if identifierNumber, ok := githubIssueNumberFromIdentifier(identifier); ok {
		return identifierNumber == issueNumber
	}
	return true
}

func githubIssueNumberID(issueNumber int) string {
	if issueNumber <= 0 {
		return ""
	}
	return strconv.Itoa(issueNumber)
}

func (c *GitHubClient) issueNumberForStateRefresh(ref IssueRef) (int, bool) {
	if issueNumber, ok := c.cachedIssueNumber(ref.ID); ok {
		return issueNumber, true
	}
	if issueNumber, ok := githubIssueNumberFromIdentifier(ref.Identifier); ok {
		return issueNumber, true
	}
	if strings.HasPrefix(strings.TrimSpace(ref.ID), "#") {
		return githubIssueNumberFromIdentifier(ref.ID)
	}
	return 0, false
}

func (c *GitHubClient) logStateRefreshCacheMiss(ref IssueRef) {
	if c.Logf == nil {
		return
	}
	c.Logf("github issue state refresh skipped uncached issue_id=%q issue_identifier=%q; no repo issue-number fallback available", strings.TrimSpace(ref.ID), strings.TrimSpace(ref.Identifier))
}

func githubIssueNumberFromIdentifier(identifier string) (int, bool) {
	identifier = strings.TrimSpace(identifier)
	if !strings.HasPrefix(identifier, "#") {
		return 0, false
	}
	number, err := strconv.Atoi(strings.TrimPrefix(identifier, "#"))
	if err != nil || number <= 0 {
		return 0, false
	}
	return number, true
}

func (c *GitHubClient) getIssueByNumber(ctx context.Context, issueNumber int) (githubIssue, bool, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/issues/%d", c.BaseURL, url.PathEscape(c.Owner), url.PathEscape(c.Repo), issueNumber)
	reqCtx, cancel := context.WithTimeout(ctx, c.requestTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return githubIssue{}, false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return githubIssue{}, false, err
	}
	defer DrainAndClose(resp)
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return githubIssue{}, false, nil
	}
	if githubRateLimited(resp) {
		return githubIssue{}, false, NewRateLimitedError(fmt.Sprintf("get GitHub issue #%d", issueNumber), resp.StatusCode, resp.Header)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return githubIssue{}, false, fmt.Errorf("get GitHub issue #%d failed: status %d", issueNumber, resp.StatusCode)
	}
	var issue githubIssue
	if err := json.NewDecoder(resp.Body).Decode(&issue); err != nil {
		return githubIssue{}, false, err
	}
	return issue, true, nil
}

func (c *GitHubClient) cacheIssueNumber(issueID string, issueNumber int) {
	if strings.TrimSpace(issueID) == "" || issueNumber <= 0 {
		return
	}
	c.issueNumbers.Store(issueID, issueNumber)
}

func (c *GitHubClient) cachedIssueNumber(issueID string) (int, bool) {
	got, ok := c.issueNumbers.Load(issueID)
	if !ok {
		return 0, false
	}
	issueNumber, ok := got.(int)
	return issueNumber, ok && issueNumber > 0
}

func githubConfiguredStates(cfg workflow.TrackerConfig) []string {
	out := make([]string, 0, len(cfg.ActiveStates)+len(cfg.TerminalStates)+len(cfg.InactiveStates))
	seen := map[string]struct{}{}
	for _, group := range [][]string{cfg.ActiveStates, cfg.TerminalStates, cfg.InactiveStates} {
		for _, state := range group {
			state = strings.ToLower(strings.TrimSpace(state))
			if state == "" {
				continue
			}
			if _, ok := seen[state]; ok {
				continue
			}
			seen[state] = struct{}{}
			out = append(out, state)
		}
	}
	return out
}

func githubResolveState(issue githubIssue, configured []string) string {
	labelSet := make(map[string]struct{}, len(issue.Labels))
	for _, label := range issue.Labels {
		name := strings.ToLower(strings.TrimSpace(label.Name))
		if name != "" {
			labelSet[name] = struct{}{}
		}
	}
	for _, state := range configured {
		if _, ok := labelSet[state]; ok {
			return state
		}
	}
	return strings.ToLower(strings.TrimSpace(issue.State))
}
