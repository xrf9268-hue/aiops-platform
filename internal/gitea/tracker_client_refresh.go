package gitea

import (
	"context"
	"encoding/json"
	"errors"
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

// FetchIssueStatesByRefs classifies every non-empty requested ID. A 404 is
// confirmed absence only when this client previously cached the ID-to-number
// mapping for the current repository; fallback-only not-found responses stay
// unknown because the human identifier alone does not prove repository scope.
func (c *TrackerClient) FetchIssueStatesByRefs(ctx context.Context, issueRefs []tracker.IssueRef) (map[string]tracker.IssueState, error) { //nolint:gocognit // baseline (#521)
	states, refs := tracker.UnknownIssueStatesByRefs(issueRefs)
	if c.BaseURL == "" || c.Token == "" {
		return states, fmt.Errorf("GITEA_BASE_URL and Gitea tracker api_key are required")
	}
	if c.Owner == "" || c.Repo == "" {
		return states, fmt.Errorf("repo.owner and repo.name are required for Gitea tracker polling")
	}
	if len(refs) == 0 {
		return states, nil
	}
	// Install a per-refresh blocker cache so buildBlockedBy fetches each
	// `Depends on #N` blocker at most once across the batch, mirroring the
	// listing path (#677).
	ctx = withBlockerCache(ctx)
	var refreshErrs []error
	for _, issueRef := range refs {
		issueID := issueRef.ID
		issueNumber, ok, cached := c.issueNumberForStateRefresh(issueRef)
		if !ok {
			c.logStateRefreshCacheMiss(issueRef)
			continue
		}
		if !giteaIssueRefMatchesNumber(issueRef, issueNumber) {
			continue
		}
		issue, found, bodyPresent, err := c.getIssueStateByNumber(ctx, issueNumber)
		if err != nil {
			refreshErrs = append(refreshErrs, err)
			if tracker.ShouldStopIssueStateRefresh(ctx, err) {
				break
			}
			continue
		}
		if !found {
			if cached {
				states[issueID] = tracker.IssueState{Outcome: tracker.IssueStateOutcomeAbsent}
			}
			continue
		}
		if issue.Number != issueNumber {
			continue
		}
		if !giteaIssueMatchesRef(issueID, issueRef.Identifier, issue) {
			continue
		}
		c.cacheIssueNumber(issue)
		state, diagnostics := IssueStateFromLabels(issue.Labels, DefaultStateLabelMappings())
		c.logDiagnostics(issue, diagnostics)
		if strings.EqualFold(strings.TrimSpace(state), "Todo") && !bodyPresent {
			refreshErrs = append(refreshErrs, fmt.Errorf("%w: Gitea Todo issue response missing body", tracker.ErrIssueStateRefreshIncomplete))
			continue
		}
		if state == "" {
			if stateDiagnosticsContain(diagnostics, "unknown_aiops_label") {
				continue
			}
			// A matching issue with no aiops/* workflow label is authoritatively
			// outside this adapter's workflow and must release continuations.
			states[issueID] = tracker.IssueState{Outcome: tracker.IssueStateOutcomeAbsent}
			continue
		}
		refreshed, err := c.buildRefreshedIssueState(ctx, issue, state)
		if err != nil {
			refreshErrs = append(refreshErrs, err)
			if tracker.ShouldStopIssueStateRefresh(ctx, err) {
				break
			}
			continue
		}
		states[issueID] = refreshed
	}
	return states, errors.Join(refreshErrs...)
}

// buildRefreshedIssueState carries the full normalized label set and refreshed
// blockers onto an authoritative Current row. The issue body is authoritative
// for this adapter, so no `Depends on #N` references becomes a positive non-nil
// "no blockers" result; transient resolution failures become open placeholders
// inside buildBlockedBy.
func (c *TrackerClient) buildRefreshedIssueState(ctx context.Context, issue Issue, state string) (tracker.IssueState, error) {
	blockedBy, err := c.buildBlockedBy(ctx, issue.Body, blockerLookupHaltRefresh)
	if err != nil {
		return tracker.IssueState{}, err
	}
	if blockedBy == nil {
		blockedBy = []tracker.BlockerRef{}
	}
	return tracker.IssueState{
		Outcome:   tracker.IssueStateOutcomeCurrent,
		State:     state,
		Labels:    extractGiteaLabels(issue.Labels),
		BlockedBy: blockedBy,
	}, nil
}

func stateDiagnosticsContain(diagnostics []StateDiagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func giteaIssueMatchesRef(issueID, identifier string, issue Issue) bool {
	issueID = strings.TrimSpace(issueID)
	if !giteaIdentifierMatchesIssueNumber(identifier, issue.Number) {
		return false
	}
	if issueID == giteaIssueID(issue) {
		return true
	}
	if strings.HasPrefix(issueID, "#") {
		return issueID == fmt.Sprintf("#%d", issue.Number)
	}
	return issueID == giteaIssueNumberID(issue.Number)
}

func giteaIssueRefMatchesNumber(ref tracker.IssueRef, issueNumber int) bool {
	identifier := strings.TrimSpace(ref.Identifier)
	if identifier != "" {
		got, ok := giteaIssueNumberFromIdentifier(identifier)
		if !ok || got != issueNumber {
			return false
		}
	}
	id := strings.TrimSpace(ref.ID)
	if !strings.HasPrefix(id, "#") {
		return true
	}
	got, ok := giteaIssueNumberFromIdentifier(id)
	return ok && got == issueNumber
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

func (c *TrackerClient) issueNumberForStateRefresh(ref tracker.IssueRef) (int, bool, bool) {
	if issueNumber, ok := c.cachedIssueNumber(ref.ID); ok {
		return issueNumber, true, true
	}
	issueNumber, ok := IssueNumberFromRef(ref.ID, ref.Identifier)
	return issueNumber, ok, false
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
	issue, found, _, err := c.getIssueByNumberWithMetadata(ctx, issueNumber)
	return issue, found, err
}

func (c *TrackerClient) getIssueStateByNumber(ctx context.Context, issueNumber int) (Issue, bool, bool, error) {
	return c.getIssueByNumberWithMetadata(ctx, issueNumber)
}

func (c *TrackerClient) getIssueByNumberWithMetadata(ctx context.Context, issueNumber int) (Issue, bool, bool, error) {
	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/issues/%d", c.BaseURL, url.PathEscape(c.Owner), url.PathEscape(c.Repo), issueNumber)
	reqCtx, cancel := context.WithTimeout(ctx, c.requestTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Issue{}, false, false, err
	}
	req.Header.Set("Authorization", "token "+c.Token)
	client := c.httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return Issue{}, false, false, err
	}
	defer tracker.DrainAndClose(resp)
	if resp.StatusCode == http.StatusNotFound {
		return Issue{}, false, false, nil
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return Issue{}, false, false, tracker.NewRateLimitedError(fmt.Sprintf("get Gitea issue #%d", issueNumber), resp.StatusCode, resp.Header)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Issue{}, false, false, fmt.Errorf("get Gitea issue #%d failed: status %d", issueNumber, resp.StatusCode)
	}
	issue, bodyPresent, err := decodeGiteaIssueStateResponse(resp)
	if err != nil {
		return Issue{}, false, false, err
	}
	return issue, true, bodyPresent, nil
}

func decodeGiteaIssueStateResponse(resp *http.Response) (Issue, bool, error) {
	var raw json.RawMessage
	if err := tracker.DecodeJSONResponse(resp, &raw); err != nil {
		return Issue{}, false, fmt.Errorf("decode Gitea issue response: %w", err)
	}
	var presence struct {
		ID     *int64  `json:"id"`
		Number *int    `json:"number"`
		Body   *string `json:"body"`
		Labels *[]struct {
			Name *string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(raw, &presence); err != nil {
		return Issue{}, false, fmt.Errorf("decode Gitea issue response: %w", err)
	}
	if presence.ID == nil || *presence.ID <= 0 || presence.Number == nil || *presence.Number <= 0 || presence.Labels == nil {
		return Issue{}, false, fmt.Errorf("%w: Gitea issue response has incomplete id, number, or labels", tracker.ErrIssueStateRefreshIncomplete)
	}
	for _, label := range *presence.Labels {
		if label.Name == nil || strings.TrimSpace(*label.Name) == "" {
			return Issue{}, false, fmt.Errorf("%w: Gitea issue label missing non-empty name", tracker.ErrIssueStateRefreshIncomplete)
		}
	}
	var issue Issue
	if err := json.Unmarshal(raw, &issue); err != nil {
		return Issue{}, false, fmt.Errorf("decode Gitea issue response: %w", err)
	}
	return issue, presence.Body != nil, nil
}
