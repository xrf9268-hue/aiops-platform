package tracker

import (
	"context"
	"encoding/json"
	"errors"
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
// issue number before any list call repopulates the cache. A 404/410 confirms
// absence only when this client previously cached the ID-to-number mapping for
// the current repository; fallback-only not-found responses remain unknown.
// Ambiguous references and failed reads remain unknown, while independent
// current/absent rows survive a partial batch error.
//
// State derivation: if any label on the issue matches a state configured in
// active_states / terminal_states / inactive_states (case-insensitive), the
// matched label is returned (lowercased, matching mapGitHubIssue's
// normalization). Otherwise the issue's open/closed state is returned. This
// matches the GitHub convention where workflow position is encoded as labels.
//
// BlockedBy carries GitHub dependency data from the native GraphQL
// Issue.blockedBy relation for Todo-like states when available, plus the
// in-repo body fallback ("Blocked by #N" / "Depends on #N"). A non-nil empty
// slice means the refresh confirmed no blockers or the state does not need
// blocker hydration; native lookup failure aborts Todo-like refresh so dispatch
// skips this tick, while incomplete native pagination is surfaced as an
// empty-state placeholder so the Todo blocker gate fails closed.
func (c *GitHubClient) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]IssueState, error) {
	return c.FetchIssueStatesByRefs(ctx, IssueRefsFromIDs(issueIDs))
}

// FetchIssueStatesWithoutBlockersByRefs fetches the state/labels needed by the
// runner's per-turn current-issue gate without optional dependency hydration.
// Dispatch-time revalidation still uses FetchIssueStatesByRefs so Todo blocker
// checks remain authoritative before starting new work.
func (c *GitHubClient) FetchIssueStatesWithoutBlockersByRefs(ctx context.Context, issueRefs []IssueRef) (map[string]IssueState, error) {
	return c.fetchIssueStatesByRefs(ctx, issueRefs, false)
}

type githubFetchedIssueState struct {
	issueID string
	issue   githubIssue
	state   string
	labels  []string
}

type githubIssueStateLookup struct {
	outcome IssueStateOutcome
	fetched githubFetchedIssueState
}

type githubIssueStateLookupBatch struct {
	fetched []githubFetchedIssueState
	absent  []string
	errs    []error
	halted  bool
}

func (c *GitHubClient) FetchIssueStatesByRefs(ctx context.Context, issueRefs []IssueRef) (map[string]IssueState, error) {
	return c.fetchIssueStatesByRefs(ctx, issueRefs, true)
}

func (c *GitHubClient) fetchIssueStatesByRefs(ctx context.Context, issueRefs []IssueRef, includeBlockers bool) (map[string]IssueState, error) {
	states, refs := UnknownIssueStatesByRefs(issueRefs)
	if strings.TrimSpace(c.Token) == "" {
		return states, fmt.Errorf("GitHub tracker api_key is required")
	}
	if len(refs) == 0 {
		return states, nil
	}
	if strings.TrimSpace(c.Owner) == "" || strings.TrimSpace(c.Repo) == "" {
		return states, fmt.Errorf("repo.owner and repo.name are required for GitHub tracker polling")
	}
	configuredStates := githubConfiguredStates(c.Config)
	batch := c.lookupGitHubIssueStates(ctx, refs, configuredStates, includeBlockers)
	for _, issueID := range batch.absent {
		states[issueID] = IssueState{Outcome: IssueStateOutcomeAbsent}
	}
	var blockersByID map[string][]BlockerRef
	var blockerErr error
	if !batch.halted {
		blockersByID, blockerErr = c.blockerRefsForFetchedGitHubIssueStates(ctx, batch.fetched, configuredStates, includeBlockers)
	}
	if blockerErr != nil {
		batch.errs = append(batch.errs, blockerErr)
	}
	applyCurrentGitHubIssueStates(states, batch.fetched, blockersByID, includeBlockers, batch.halted || blockerErr != nil)
	return states, errors.Join(batch.errs...)
}

func (c *GitHubClient) lookupGitHubIssueStates(ctx context.Context, refs []IssueRef, configuredStates []string, includeBlockers bool) githubIssueStateLookupBatch {
	batch := githubIssueStateLookupBatch{fetched: make([]githubFetchedIssueState, 0, len(refs))}
	for _, issueRef := range refs {
		lookup, err := c.lookupGitHubIssueState(ctx, issueRef, configuredStates, includeBlockers)
		if err != nil {
			batch.errs = append(batch.errs, err)
			if ShouldStopIssueStateRefresh(ctx, err) {
				batch.halted = true
				break
			}
			continue
		}
		switch lookup.outcome {
		case IssueStateOutcomeAbsent:
			batch.absent = append(batch.absent, issueRef.ID)
		case IssueStateOutcomeCurrent:
			batch.fetched = append(batch.fetched, lookup.fetched)
		}
	}
	return batch
}

func applyCurrentGitHubIssueStates(states map[string]IssueState, fetched []githubFetchedIssueState, blockersByID map[string][]BlockerRef, includeBlockers, todoBlockersUnavailable bool) {
	for _, row := range fetched {
		if includeBlockers && githubTodoWorkflowState(row.state) && todoBlockersUnavailable {
			continue
		}
		// Carry the full label set (SPEC §6.4 required_labels gate) alongside the
		// resolved state so label removal can stop/release already-claimed work.
		states[row.issueID] = IssueState{Outcome: IssueStateOutcomeCurrent, State: row.state, Labels: row.labels, BlockedBy: githubIssueStateBlockers(blockersByID, row.issueID, includeBlockers)}
	}
}

func (c *GitHubClient) lookupGitHubIssueState(ctx context.Context, issueRef IssueRef, configuredStates []string, includeBlockers bool) (githubIssueStateLookup, error) {
	issueNumber, ok, cached := c.issueNumberForStateRefresh(issueRef)
	if !ok {
		c.logStateRefreshCacheMiss(issueRef)
		return githubIssueStateLookup{}, nil
	}
	if !githubIssueRefMatchesNumber(issueRef, issueNumber) {
		return githubIssueStateLookup{}, nil
	}
	issue, found, bodyPresent, err := c.getIssueByNumberWithBodyPresence(ctx, issueNumber)
	if err != nil {
		return githubIssueStateLookup{}, err
	}
	if !found {
		if cached {
			return githubIssueStateLookup{outcome: IssueStateOutcomeAbsent}, nil
		}
		return githubIssueStateLookup{}, nil
	}
	return c.classifyFetchedGitHubIssueState(issueRef, issueNumber, issue, bodyPresent, configuredStates, includeBlockers)
}

func (c *GitHubClient) classifyFetchedGitHubIssueState(issueRef IssueRef, issueNumber int, issue githubIssue, bodyPresent bool, configuredStates []string, includeBlockers bool) (githubIssueStateLookup, error) {
	if issue.Number != issueNumber {
		return githubIssueStateLookup{}, nil
	}
	refreshedID := strconv.FormatInt(issue.ID, 10)
	if !githubFetchedIssueMatchesRef(issueRef.ID, issueRef.Identifier, refreshedID, issue.Number) {
		return githubIssueStateLookup{}, nil
	}
	c.cacheIssueNumber(refreshedID, issue.Number)
	if issueNumberID := githubIssueNumberID(issue.Number); issueRef.ID == issueNumberID {
		c.cacheIssueNumber(issueRef.ID, issue.Number)
	}
	state := githubResolveState(issue, configuredStates)
	if state == "" {
		return githubIssueStateLookup{}, nil
	}
	if includeBlockers && githubTodoWorkflowState(state) && !bodyPresent {
		return githubIssueStateLookup{}, fmt.Errorf("%w: GitHub Todo issue response missing body", ErrIssueStateRefreshIncomplete)
	}
	if includeBlockers && githubTodoWorkflowState(state) && strings.TrimSpace(issue.NodeID) == "" {
		return githubIssueStateLookup{}, fmt.Errorf("%w: GitHub Todo issue response missing node_id", ErrIssueStateRefreshIncomplete)
	}
	return githubIssueStateLookup{
		outcome: IssueStateOutcomeCurrent,
		fetched: githubFetchedIssueState{
			issueID: issueRef.ID,
			issue:   issue,
			state:   state,
			labels:  githubIssueLabels(issue),
		},
	}, nil
}

func (c *GitHubClient) blockerRefsForFetchedGitHubIssueStates(ctx context.Context, fetched []githubFetchedIssueState, configuredStates []string, includeBlockers bool) (map[string][]BlockerRef, error) {
	if !includeBlockers {
		return nil, nil
	}
	return c.blockersForRefreshedGitHubIssues(ctx, fetched, configuredStates)
}

func githubIssueStateBlockers(blockersByID map[string][]BlockerRef, issueID string, includeBlockers bool) []BlockerRef {
	if !includeBlockers {
		return nil
	}
	if blockedBy := blockersByID[issueID]; blockedBy != nil {
		return blockedBy
	}
	return []BlockerRef{}
}

func (c *GitHubClient) blockersForRefreshedGitHubIssues(ctx context.Context, rows []githubFetchedIssueState, configuredStates []string) (map[string][]BlockerRef, error) {
	sources := make([]githubBlockerSource, 0, len(rows))
	for _, row := range rows {
		sources = append(sources, githubBlockerSource{
			IssueID:    row.issueID,
			Identifier: fmt.Sprintf("#%d", row.issue.Number),
			State:      row.state,
			NodeID:     row.issue.NodeID,
			Number:     row.issue.Number,
			Body:       row.issue.Body,
		})
	}
	return c.blockersForGitHubSources(ctx, sources, configuredStates, githubBodyBlockerRefresh)
}

func githubFetchedIssueMatchesRef(issueID, identifier, refreshedID string, issueNumber int) bool {
	issueID = strings.TrimSpace(issueID)
	if !githubIdentifierMatchesIssueNumber(identifier, issueNumber) {
		return false
	}
	if issueID == refreshedID {
		return true
	}
	if strings.HasPrefix(issueID, "#") {
		return issueID == fmt.Sprintf("#%d", issueNumber)
	}
	return issueID == githubIssueNumberID(issueNumber)
}

func githubIssueRefMatchesNumber(ref IssueRef, issueNumber int) bool {
	identifier := strings.TrimSpace(ref.Identifier)
	if identifier != "" {
		got, ok := githubIssueNumberFromIdentifier(identifier)
		if !ok || got != issueNumber {
			return false
		}
	}
	id := strings.TrimSpace(ref.ID)
	if !strings.HasPrefix(id, "#") {
		return true
	}
	got, ok := githubIssueNumberFromIdentifier(id)
	return ok && got == issueNumber
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

func (c *GitHubClient) issueNumberForStateRefresh(ref IssueRef) (int, bool, bool) {
	if issueNumber, ok := c.cachedIssueNumber(ref.ID); ok {
		return issueNumber, true, true
	}
	if issueNumber, ok := githubIssueNumberFromIdentifier(ref.Identifier); ok {
		return issueNumber, true, false
	}
	if strings.HasPrefix(strings.TrimSpace(ref.ID), "#") {
		issueNumber, ok := githubIssueNumberFromIdentifier(ref.ID)
		return issueNumber, ok, false
	}
	return 0, false, false
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
	issue, found, _, err := c.getIssueByNumberWithBodyPresence(ctx, issueNumber)
	return issue, found, err
}

func (c *GitHubClient) getIssueByNumberWithBodyPresence(ctx context.Context, issueNumber int) (githubIssue, bool, bool, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/issues/%d", c.BaseURL, url.PathEscape(c.Owner), url.PathEscape(c.Repo), issueNumber)
	reqCtx, cancel := context.WithTimeout(ctx, c.requestTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return githubIssue{}, false, false, err
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
		return githubIssue{}, false, false, err
	}
	defer DrainAndClose(resp)
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return githubIssue{}, false, false, nil
	}
	if githubRateLimited(resp) {
		return githubIssue{}, false, false, NewRateLimitedError(fmt.Sprintf("get GitHub issue #%d", issueNumber), resp.StatusCode, resp.Header)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return githubIssue{}, false, false, fmt.Errorf("get GitHub issue #%d failed: status %d", issueNumber, resp.StatusCode)
	}
	issue, bodyPresent, err := decodeGitHubIssueStateResponse(resp)
	if err != nil {
		return githubIssue{}, false, false, err
	}
	return issue, true, bodyPresent, nil
}

func decodeGitHubIssueStateResponse(resp *http.Response) (githubIssue, bool, error) {
	var raw json.RawMessage
	if err := DecodeJSONResponse(resp, &raw); err != nil {
		return githubIssue{}, false, fmt.Errorf("decode GitHub issue response: %w", err)
	}
	var presence struct {
		ID     *int64          `json:"id"`
		Number *int            `json:"number"`
		State  *string         `json:"state"`
		Body   json.RawMessage `json:"body"`
		Labels *[]struct {
			Name *string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(raw, &presence); err != nil {
		return githubIssue{}, false, fmt.Errorf("decode GitHub issue response: %w", err)
	}
	if presence.ID == nil || *presence.ID <= 0 || presence.Number == nil || *presence.Number <= 0 ||
		presence.State == nil || strings.TrimSpace(*presence.State) == "" || presence.Labels == nil {
		return githubIssue{}, false, fmt.Errorf("%w: GitHub issue response has incomplete id, number, state, or labels", ErrIssueStateRefreshIncomplete)
	}
	for _, label := range *presence.Labels {
		if label.Name == nil || strings.TrimSpace(*label.Name) == "" {
			return githubIssue{}, false, fmt.Errorf("%w: GitHub issue label missing non-empty name", ErrIssueStateRefreshIncomplete)
		}
	}
	var issue githubIssue
	if err := json.Unmarshal(raw, &issue); err != nil {
		return githubIssue{}, false, fmt.Errorf("decode GitHub issue response: %w", err)
	}
	return issue, len(presence.Body) > 0, nil
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
