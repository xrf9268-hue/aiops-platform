package gitea

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

func (c *TrackerClient) ListActiveIssues(ctx context.Context) ([]tracker.Issue, error) {
	return c.ListIssuesByStates(ctx, c.Config.ActiveStates)
}

func (c *TrackerClient) ListIssuesByStates(ctx context.Context, states []string) ([]tracker.Issue, error) { //nolint:gocognit // baseline (#521)
	if c.BaseURL == "" || c.Token == "" {
		return nil, fmt.Errorf("GITEA_BASE_URL and Gitea tracker api_key are required")
	}
	if c.Owner == "" || c.Repo == "" {
		return nil, fmt.Errorf("repo.owner and repo.name are required for Gitea tracker polling")
	}
	wantedStates := normalizedStateSet(states)
	if len(wantedStates) == 0 {
		return nil, nil
	}
	// Install a per-poll-tick blocker cache so buildBlockedBy fetches each
	// `Depends on #N` blocker at most once per tick across all source issues (#677).
	ctx = withBlockerCache(ctx)
	labelNames := StateLabelNamesForStates(states, DefaultStateLabelMappings())
	issueState := giteaAPIStateForWorkflowStates(states, c.Config.TerminalStates)

	var out []tracker.Issue
	seenIssues := map[string]struct{}{}
	if len(labelNames) == 0 {
		issues, capped, err := c.listIssuesByStateLabel(ctx, "", issueState, wantedStates, seenIssues)
		if err != nil {
			return nil, err
		}
		if capped {
			return nil, tracker.NewError(
				tracker.CategoryIssueListingCapped,
				fmt.Sprintf("gitea issue listing partial: capped state %q", issueState),
				nil,
			)
		}
		return issues, nil
	}
	var cappedLabels []string
	for _, labelName := range labelNames {
		issues, capped, err := c.listIssuesByStateLabel(ctx, labelName, issueState, wantedStates, seenIssues)
		if err != nil {
			return nil, err
		}
		if capped {
			cappedLabels = append(cappedLabels, labelName)
			continue
		}
		for _, issue := range issues {
			seenIssues[issue.ID] = struct{}{}
		}
		out = append(out, issues...)
	}
	if len(cappedLabels) > 0 {
		return nil, tracker.NewError(
			tracker.CategoryIssueListingCapped,
			fmt.Sprintf("gitea issue listing partial: capped labels %v", cappedLabels),
			nil,
		)
	}
	return out, nil
}

func (c *TrackerClient) listIssuesByStateLabel(ctx context.Context, labelName, issueState string, wantedStates map[string]struct{}, seenIssues map[string]struct{}) ([]tracker.Issue, bool, error) {
	var out []tracker.Issue
	collectionSeen := make(map[string]struct{}, len(seenIssues))
	for id := range seenIssues {
		collectionSeen[id] = struct{}{}
	}
	maxPages := c.IssueMaxPages()
	scope := labelName
	if scope == "" {
		scope = issueState
	}
	for page := 1; page <= maxPages+1; page++ {
		grown, capped, done, err := c.scopePageStep(ctx, labelName, issueState, page, maxPages, scope, wantedStates, collectionSeen, out)
		if err != nil {
			return nil, false, err
		}
		if done {
			return grown, capped, nil
		}
		out = grown
	}
	return nil, true, nil
}

// scopePageStep fetches one page and either ends the collection or grows out.
// It returns done=true with the verbatim return values the parent must pass
// through: the post-cap probe page yields (out|nil, capped) via finishCapProbe;
// a natural end (no next page and a short final batch) yields (out, false); and
// a mid-collection page yields done=false so the parent keeps paging with the
// grown slice. capped is meaningful only when done is true.
func (c *TrackerClient) scopePageStep(ctx context.Context, labelName, issueState string, page, maxPages int, scope string, wantedStates, collectionSeen map[string]struct{}, out []tracker.Issue) (grown []tracker.Issue, capped, done bool, err error) {
	batch, hasNext, err := c.listIssuesPage(ctx, labelName, issueState, page)
	if err != nil {
		return nil, false, false, err
	}
	if page > maxPages {
		out, capped = c.finishCapProbe(out, batch, hasNext, scope, maxPages)
		return out, capped, true, nil
	}
	out, err = c.collectScopePage(ctx, batch, wantedStates, collectionSeen, out)
	if err != nil {
		return nil, false, false, err
	}
	if !hasNext && len(batch) < listIssuesPageSize {
		return out, false, true, nil
	}
	return out, false, false, nil
}

// finishCapProbe owns the post-cap probe page (page > maxPages): an empty,
// terminal probe response confirms the previous pages were the full result, so
// the collected issues are authoritative (capped=false); anything else means
// there were more pages than the cap allows, which records a cap hit and
// returns capped=true with nil issues so reconcile does not treat the partial
// result as authoritative.
func (c *TrackerClient) finishCapProbe(out []tracker.Issue, batch []Issue, hasNext bool, scope string, maxPages int) ([]tracker.Issue, bool) {
	if !hasNext && len(batch) == 0 {
		return out, false
	}
	c.recordPaginationCapHit(scope, maxPages)
	return nil, true
}

// collectScopePage folds one page of Gitea issues into out, applying the
// dedup-then-filter-then-include ordering: a duplicate (already in
// collectionSeen) is skipped before any caching/logging so it is neither
// re-cached nor re-logged; an issue with no derivable state or one outside
// wantedStates is dropped before the include-only work; and the issue is marked
// seen only on the include path, after both timestamps parse and right before
// the blocker lookup + append.
func (c *TrackerClient) collectScopePage(ctx context.Context, batch []Issue, wantedStates, collectionSeen map[string]struct{}, out []tracker.Issue) ([]tracker.Issue, error) {
	for _, issue := range batch {
		issueKey := giteaIssueID(issue)
		if _, ok := collectionSeen[issueKey]; ok {
			continue
		}
		c.cacheIssueNumber(issue)
		converted, include, err := c.scopeIssue(ctx, issue, wantedStates)
		if err != nil {
			return nil, err
		}
		if !include {
			continue
		}
		collectionSeen[issueKey] = struct{}{}
		out = append(out, converted)
	}
	return out, nil
}

// scopeIssue derives an issue's workflow state (logging label diagnostics),
// drops it when the state is empty or outside wantedStates, and on the include
// path parses created_at then updated_at (first malformed wins) and builds the
// normalized tracker.Issue including the blocker lookup. include is false for a
// filtered issue; callers must not mark it seen unless include is true.
func (c *TrackerClient) scopeIssue(ctx context.Context, issue Issue, wantedStates map[string]struct{}) (tracker.Issue, bool, error) {
	state, diagnostics := IssueStateFromLabels(issue.Labels, DefaultStateLabelMappings())
	c.logDiagnostics(issue, diagnostics)
	if state == "" {
		return tracker.Issue{}, false, nil
	}
	if len(wantedStates) > 0 {
		if _, ok := wantedStates[strings.ToLower(state)]; !ok {
			return tracker.Issue{}, false, nil
		}
	}
	createdAt, err := parseGiteaIssueTime("created_at", issue.CreatedAt)
	if err != nil {
		return tracker.Issue{}, false, err
	}
	updatedAt, err := parseGiteaIssueTime("updated_at", issue.UpdatedAt)
	if err != nil {
		return tracker.Issue{}, false, err
	}
	return tracker.Issue{
		ID:          giteaIssueID(issue),
		Identifier:  fmt.Sprintf("#%d", issue.Number),
		Title:       issue.Title,
		Description: issue.Body,
		URL:         issue.HTMLURL,
		State:       state,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		Labels:      extractGiteaLabels(issue.Labels),
		BlockedBy:   c.buildBlockedBy(ctx, issue.Body),
		// Priority: Gitea has no native priority field — see
		// dependsOnRegexp comment / SPEC §4.1.1 note. Left at the zero
		// value; dispatch sort treats every Gitea issue as equal priority
		// and falls back to created_at. Operators can opt in to
		// label-driven priority in a follow-up.
	}, true, nil
}

func (c *TrackerClient) recordPaginationCapHit(labelName string, maxPages int) {
	c.paginationCapHits.Add(1)
	if c.Logf != nil {
		c.Logf("gitea issue pagination exceeded %d pages for label %q; failing this tracker listing so reconcile does not treat partial results as authoritative", maxPages, labelName)
	}
}

func (c *TrackerClient) listIssuesPage(ctx context.Context, labelName string, issueState string, page int) ([]Issue, bool, error) {
	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/issues", c.BaseURL, url.PathEscape(c.Owner), url.PathEscape(c.Repo))
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, false, err
	}
	q := u.Query()
	q.Set("state", issueState)
	q.Set("type", "issues")
	q.Set("page", strconv.Itoa(page))
	q.Set("limit", strconv.Itoa(listIssuesPageSize))
	if labelName != "" {
		q.Set("labels", labelName)
	}
	u.RawQuery = q.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, c.requestTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "token "+c.Token)
	client := c.httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer tracker.DrainAndClose(resp)
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, false, tracker.NewRateLimitedError("list Gitea issues", resp.StatusCode, resp.Header)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("list Gitea issues failed: status %d", resp.StatusCode)
	}
	var issues []Issue
	if err := tracker.DecodeJSONResponse(resp, &issues); err != nil {
		return nil, false, fmt.Errorf("decode Gitea issues response: %w", err)
	}
	return issues, hasNextPage(resp.Header.Values("Link")), nil
}

func hasNextPage(linkHeaders []string) bool {
	for _, header := range linkHeaders {
		for _, part := range strings.Split(header, ",") {
			if strings.Contains(part, `rel="next"`) {
				return true
			}
		}
	}
	return false
}
