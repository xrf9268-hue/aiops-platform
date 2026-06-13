package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (c *GitHubClient) ListActiveIssues(ctx context.Context) ([]Issue, error) {
	return c.ListIssuesByStates(ctx, c.Config.ActiveStates)
}

func (c *GitHubClient) PaginationCapHits() int64 {
	return c.paginationCapHits.Load()
}

func (c *GitHubClient) issueMaxPages() int {
	if c != nil && c.Config.PaginationMaxPages > 0 {
		return c.Config.PaginationMaxPages
	}
	return githubMaxIssuePages
}

func (c *GitHubClient) ListIssuesByStates(ctx context.Context, states []string) ([]Issue, error) { //nolint:gocognit // baseline (#521)
	if strings.TrimSpace(c.Token) == "" {
		return nil, fmt.Errorf("GitHub tracker api_key is required")
	}
	if strings.TrimSpace(c.Owner) == "" || strings.TrimSpace(c.Repo) == "" {
		return nil, fmt.Errorf("repo.owner and repo.name are required for GitHub tracker polling")
	}
	stateFilters := nonEmptyGitHubStates(states)
	if len(stateFilters) == 0 {
		return nil, nil
	}
	claimedIssueNumbers := map[int]struct{}{}
	claimsCapped := false
	if githubStatesMayIncludeOpenIssues(stateFilters) {
		var err error
		claimedIssueNumbers, claimsCapped, err = c.openPullRequestClaimedIssueNumbers(ctx)
		if err != nil {
			return nil, err
		}
	}
	seen := map[string]struct{}{}
	var out []Issue
	var cappedScopes []string
	for _, state := range stateFilters {
		issueState, label, mappedState := githubIssueQueryForState(state)
		scope := githubIssueCollectionScope(issueState, label)
		if claimsCapped && githubIssueQueryRequiresCompleteClaims(issueState) {
			if c.Logf != nil {
				c.Logf("github open pull request pagination exceeded configured cap; skipping issue collection %q to avoid dispatching already-claimed issues", scope)
			}
			cappedScopes = append(cappedScopes, scope)
			continue
		}
		issues, capped, err := c.listIssuesForState(ctx, issueState, label, mappedState, seen, claimedIssueNumbers)
		if err != nil {
			return nil, err
		}
		if capped {
			cappedScopes = append(cappedScopes, scope)
			continue
		}
		for _, issue := range issues {
			seen[issue.ID] = struct{}{}
		}
		out = append(out, issues...)
	}
	if len(cappedScopes) > 0 {
		return nil, NewError(
			CategoryIssueListingCapped,
			fmt.Sprintf("github issue listing partial: capped collections %v", cappedScopes),
			nil,
		)
	}
	return out, nil
}

func (c *GitHubClient) listIssuesForState(ctx context.Context, issueState, label, mappedState string, seen map[string]struct{}, claimedIssueNumbers map[int]struct{}) ([]Issue, bool, error) {
	var out []Issue
	collectionSeen := newGitHubCollectionSeen(seen)
	maxPages := c.issueMaxPages()
	scope := githubIssueCollectionScope(issueState, label)
	for page := 1; page <= maxPages+1; page++ {
		batch, hasNext, err := c.listIssuesPage(ctx, issueState, label, page)
		if err != nil {
			return nil, false, err
		}
		if page > maxPages {
			return c.finishGitHubCapProbe(out, batch, hasNext, scope, maxPages)
		}
		out, err = c.collectIssuesFromPage(batch, mappedState, claimedIssueNumbers, collectionSeen, out)
		if err != nil {
			return nil, false, err
		}
		if !hasNext && len(batch) < githubIssuePageSize {
			return out, false, nil
		}
	}
	return nil, true, nil
}

// newGitHubCollectionSeen seeds a per-collection dedup set from the shared
// cross-state seen set, copying every key so a global mapped.ID already
// collected in an earlier state is not re-collected here.
func newGitHubCollectionSeen(seen map[string]struct{}) map[string]struct{} {
	collectionSeen := make(map[string]struct{}, len(seen))
	for id := range seen {
		collectionSeen[id] = struct{}{}
	}
	return collectionSeen
}

// finishGitHubCapProbe handles the post-cap probe page (page > maxPages): an
// empty, no-next probe means the collection fit exactly within the cap and the
// accumulated out is returned; any remaining content records a cap hit and
// signals capped with a nil issue slice so partial results are not treated as
// authoritative.
func (c *GitHubClient) finishGitHubCapProbe(out []Issue, batch []githubIssue, hasNext bool, scope string, maxPages int) ([]Issue, bool, error) {
	if !hasNext && len(batch) == 0 {
		return out, false, nil
	}
	c.recordPaginationCapHit(scope, maxPages)
	return nil, true, nil
}

// collectIssuesFromPage folds one issue batch into out, delegating the
// per-issue filter/map/dedup decision to collectIssueFromBatch. A mapping
// error aborts the fold and discards accumulated work. out is passed in and
// the grown slice returned.
func (c *GitHubClient) collectIssuesFromPage(batch []githubIssue, mappedState string, claimedIssueNumbers map[int]struct{}, collectionSeen map[string]struct{}, out []Issue) ([]Issue, error) {
	for _, issue := range batch {
		mapped, collect, err := c.collectIssueFromBatch(issue, mappedState, claimedIssueNumbers, collectionSeen)
		if err != nil {
			return nil, err
		}
		if collect {
			out = append(out, mapped)
		}
	}
	return out, nil
}

// collectIssueFromBatch applies the single-issue collection rules: it drops PRs
// disguised as issues, skips issues claimed by an open PR while they are still
// open, surfaces mapping errors, skips empty-ID issues, caches the issue number
// for every mapped issue with a non-empty ID before the dedup check, and
// deduplicates by global mapped.ID against (and into) collectionSeen. It
// returns the mapped issue and whether the caller should collect it.
func (c *GitHubClient) collectIssueFromBatch(issue githubIssue, mappedState string, claimedIssueNumbers map[int]struct{}, collectionSeen map[string]struct{}) (Issue, bool, error) {
	if issue.PullRequest != nil {
		return Issue{}, false, nil
	}
	if _, claimed := claimedIssueNumbers[issue.Number]; claimed && strings.EqualFold(strings.TrimSpace(issue.State), "open") {
		return Issue{}, false, nil
	}
	mapped, err := mapGitHubIssue(issue, mappedState)
	if err != nil {
		return Issue{}, false, err
	}
	if mapped.ID == "" {
		return Issue{}, false, nil
	}
	c.cacheIssueNumber(mapped.ID, issue.Number)
	if _, ok := collectionSeen[mapped.ID]; ok {
		return Issue{}, false, nil
	}
	collectionSeen[mapped.ID] = struct{}{}
	return mapped, true, nil
}

func (c *GitHubClient) listIssuesPage(ctx context.Context, issueState, label string, page int) ([]githubIssue, bool, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/issues", c.BaseURL, url.PathEscape(c.Owner), url.PathEscape(c.Repo))
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, false, err
	}
	q := u.Query()
	q.Set("state", issueState)
	q.Set("per_page", strconv.Itoa(githubIssuePageSize))
	q.Set("page", strconv.Itoa(page))
	if label != "" {
		q.Set("labels", label)
	}
	u.RawQuery = q.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, c.requestTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, false, err
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
		return nil, false, err
	}
	defer DrainAndClose(resp)
	if githubRateLimited(resp) {
		return nil, false, NewRateLimitedError("list GitHub issues", resp.StatusCode, resp.Header)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("list GitHub issues failed: status %d", resp.StatusCode)
	}
	var issues []githubIssue
	if err := json.NewDecoder(resp.Body).Decode(&issues); err != nil {
		return nil, false, err
	}
	return issues, githubHasNextPage(resp.Header.Values("Link")), nil
}

func (c *GitHubClient) openPullRequestClaimedIssueNumbers(ctx context.Context) (map[int]struct{}, bool, error) { //nolint:gocognit // baseline (#521)
	out := map[int]struct{}{}
	maxPages := c.issueMaxPages()
	for page := 1; page <= maxPages+1; page++ {
		pulls, hasNext, err := c.listOpenPullRequestsPage(ctx, page)
		if err != nil {
			return nil, false, err
		}
		if page > maxPages {
			if !hasNext && len(pulls) == 0 {
				return out, false, nil
			}
			c.recordPaginationCapHit("open pull requests", maxPages)
			return out, true, nil
		}
		for _, pull := range pulls {
			if !strings.EqualFold(strings.TrimSpace(pull.State), "open") {
				continue
			}
			for _, issueNumber := range githubClaimedIssueNumbers(pull.Title + "\n" + pull.Body) {
				out[issueNumber] = struct{}{}
			}
		}
		if !hasNext && len(pulls) < githubIssuePageSize {
			return out, false, nil
		}
	}
	return out, false, nil
}

func (c *GitHubClient) listOpenPullRequestsPage(ctx context.Context, page int) ([]githubPullRequestSummary, bool, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls", c.BaseURL, url.PathEscape(c.Owner), url.PathEscape(c.Repo))
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, false, err
	}
	q := u.Query()
	q.Set("state", "open")
	q.Set("per_page", strconv.Itoa(githubIssuePageSize))
	q.Set("page", strconv.Itoa(page))
	u.RawQuery = q.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, c.requestTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, false, err
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
		return nil, false, err
	}
	defer DrainAndClose(resp)
	if githubRateLimited(resp) {
		return nil, false, NewRateLimitedError("list GitHub pull requests", resp.StatusCode, resp.Header)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("list GitHub pull requests failed: status %d", resp.StatusCode)
	}
	var pulls []githubPullRequestSummary
	if err := json.NewDecoder(resp.Body).Decode(&pulls); err != nil {
		return nil, false, err
	}
	return pulls, githubHasNextPage(resp.Header.Values("Link")), nil
}
