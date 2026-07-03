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

func (c *TrackerClient) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]tracker.IssueState, error) {
	return c.FetchIssueStatesByRefs(ctx, tracker.IssueRefsFromIDs(issueIDs))
}

func (c *TrackerClient) FetchIssueStatesByRefs(ctx context.Context, issueRefs []tracker.IssueRef) (map[string]tracker.IssueState, error) { //nolint:gocognit // baseline (#521)
	if c.BaseURL == "" || c.Token == "" {
		return nil, fmt.Errorf("GITEA_BASE_URL and Gitea tracker api_key are required")
	}
	if c.Owner == "" || c.Repo == "" {
		return nil, fmt.Errorf("repo.owner and repo.name are required for Gitea tracker polling")
	}
	if len(issueRefs) == 0 {
		return map[string]tracker.IssueState{}, nil
	}
	// Install a per-refresh blocker cache so buildBlockedBy fetches each
	// `Depends on #N` blocker at most once across the batch, mirroring the
	// listing path (#677).
	ctx = withBlockerCache(ctx)
	states := make(map[string]tracker.IssueState, len(issueRefs))
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
		if !giteaIssueMatchesRef(issueID, issueRef.Identifier, issue) {
			c.cacheIssueNumber(issue)
			continue
		}
		c.cacheIssueNumber(issue)
		state, diagnostics := IssueStateFromLabels(issue.Labels, DefaultStateLabelMappings())
		c.logDiagnostics(issue, diagnostics)
		if state == "" {
			continue
		}
		// Carry the full label set (SPEC §6.4 required_labels gate) alongside the
		// derived state; extractGiteaLabels lowercases/trims to match the gate.
		// BlockedBy is re-derived from the refreshed body so dispatch-time
		// revalidation can re-apply the SPEC §8.2 Todo blocker gate (#750);
		// the body is authoritative for this adapter, so an absence of
		// `Depends on #N` references is a positive non-nil "no blockers" and
		// transiently-unresolvable references fail closed as open
		// placeholders inside buildBlockedBy.
		blockedBy := c.buildBlockedBy(ctx, issue.Body)
		if blockedBy == nil {
			blockedBy = []tracker.BlockerRef{}
		}
		states[issueID] = tracker.IssueState{State: state, Labels: extractGiteaLabels(issue.Labels), BlockedBy: blockedBy}
	}
	return states, nil
}

func giteaIssueMatchesRef(issueID, identifier string, issue Issue) bool {
	issueID = strings.TrimSpace(issueID)
	if issueID == giteaIssueID(issue) {
		return true
	}
	if !giteaIdentifierMatchesIssueNumber(identifier, issue.Number) {
		return false
	}
	if strings.HasPrefix(issueID, "#") {
		return issueID == fmt.Sprintf("#%d", issue.Number)
	}
	return issueID == giteaIssueNumberID(issue.Number)
}

func giteaIdentifierMatchesIssueNumber(identifier string, issueNumber int) bool {
	if identifierNumber, ok := giteaIssueNumberFromIdentifier(identifier); ok {
		return identifierNumber == issueNumber
	}
	return true
}

func giteaIssueNumberID(issueNumber int) string {
	if issueNumber <= 0 {
		return ""
	}
	return strconv.Itoa(issueNumber)
}

func (c *TrackerClient) issueNumberForStateRefresh(ref tracker.IssueRef) (int, bool) {
	if issueNumber, ok := c.cachedIssueNumber(ref.ID); ok {
		return issueNumber, true
	}
	return IssueNumberFromRef(ref.ID, ref.Identifier)
}

// IssueNumberFromRef derives a Gitea issue number from a tracker issue
// reference without network access. Only "#N"-shaped values are trusted: a
// bare numeric ID is the Gitea-internal int64 id (giteaIssueID prefers it
// over the issue number), so parsing it as an issue number could silently
// target a different issue.
func IssueNumberFromRef(id, identifier string) (int, bool) {
	if issueNumber, ok := giteaIssueNumberFromIdentifier(identifier); ok {
		return issueNumber, true
	}
	if strings.HasPrefix(strings.TrimSpace(id), "#") {
		return giteaIssueNumberFromIdentifier(id)
	}
	return 0, false
}

func (c *TrackerClient) logStateRefreshCacheMiss(ref tracker.IssueRef) {
	if c.Logf == nil {
		return
	}
	c.Logf("gitea issue state refresh skipped uncached issue_id=%q issue_identifier=%q; no repo issue-number fallback available", strings.TrimSpace(ref.ID), strings.TrimSpace(ref.Identifier))
}

func giteaIssueNumberFromIdentifier(identifier string) (int, bool) {
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

func (c *TrackerClient) getIssueByNumber(ctx context.Context, issueNumber int) (Issue, bool, error) {
	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/issues/%d", c.BaseURL, url.PathEscape(c.Owner), url.PathEscape(c.Repo), issueNumber)
	reqCtx, cancel := context.WithTimeout(ctx, c.requestTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Issue{}, false, err
	}
	req.Header.Set("Authorization", "token "+c.Token)
	client := c.httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return Issue{}, false, err
	}
	defer tracker.DrainAndClose(resp)
	if resp.StatusCode == http.StatusNotFound {
		return Issue{}, false, nil
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return Issue{}, false, tracker.NewRateLimitedError(fmt.Sprintf("get Gitea issue #%d", issueNumber), resp.StatusCode, resp.Header)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Issue{}, false, fmt.Errorf("get Gitea issue #%d failed: status %d", issueNumber, resp.StatusCode)
	}
	var issue Issue
	if err := tracker.DecodeJSONResponse(resp, &issue); err != nil {
		return Issue{}, false, fmt.Errorf("decode Gitea issue response: %w", err)
	}
	return issue, true, nil
}
