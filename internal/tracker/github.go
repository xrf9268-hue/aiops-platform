package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

const (
	githubIssuePageSize = 100
	githubMaxIssuePages = 10
	githubAPIVersion    = "2022-11-28"
)

var githubClaimedIssueRE = regexp.MustCompile(`(?i)\b(?:(?:close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved)|(?:(?:assigned|github)\s+)?issue)\s*:?\s+(?:[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)?#([0-9]+)\b`)

type GitHubClient struct {
	BaseURL string
	Token   string
	Owner   string
	Repo    string
	Config  workflow.TrackerConfig
	HTTP    *http.Client
	Logf    func(format string, args ...any)
	// RequestTimeout caps the wall-clock duration of a single GitHub
	// REST request. Zero falls back to defaultGitHubRequestTimeout.
	// Closes #295: without a per-request bound, a hung api.github.com
	// response (TCP half-open, NLB blackhole, slow server) would wedge
	// the worker's poll loop until the OS keepalive timeout
	// (`tcp_keepalive_time=7200s` default on Linux) and leak goroutines
	// + fds in the meantime.
	RequestTimeout time.Duration

	paginationCapHits atomic.Int64
	issueNumbers      sync.Map // map[string]int — global issue ID → repo issue number, populated by listing
}

// defaultGitHubRequestTimeout bounds a single GitHub REST request when
// the caller does not set GitHubClient.RequestTimeout explicitly.
// 30 s is well above the GitHub API's documented response targets and
// short enough that a wedged connection fails fast in a SPEC §8.1
// minute-scale poll tick.
const defaultGitHubRequestTimeout = 30 * time.Second

func (c *GitHubClient) requestTimeout() time.Duration {
	if c == nil || c.RequestTimeout <= 0 {
		return defaultGitHubRequestTimeout
	}
	return c.RequestTimeout
}

type githubIssue struct {
	ID          int64              `json:"id"`
	Number      int                `json:"number"`
	Title       string             `json:"title"`
	Body        string             `json:"body"`
	HTMLURL     string             `json:"html_url"`
	State       string             `json:"state"`
	CreatedAt   string             `json:"created_at"`
	UpdatedAt   string             `json:"updated_at"`
	Labels      []githubLabel      `json:"labels"`
	PullRequest *githubPullRequest `json:"pull_request,omitempty"`
}

type githubLabel struct {
	Name string `json:"name"`
}

type githubPullRequest struct{}

type githubPullRequestSummary struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
}

func NewGitHubClient(cfg workflow.TrackerConfig, baseURL, owner, repo string) *GitHubClient {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = cfg.Endpoint
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.github.com"
	}
	return &GitHubClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   cfg.APIKey,
		Owner:   owner,
		Repo:    repo,
		Config:  cfg,
		HTTP:    http.DefaultClient,
	}
}

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
	defer func() { _ = resp.Body.Close() }()
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
	defer func() { _ = resp.Body.Close() }()
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

// githubIssueLabels returns the issue's label names lowercased and trimmed
// (SPEC §11.3 normalization), the form the required_labels gate matches against.
func githubIssueLabels(issue githubIssue) []string {
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		if name := strings.ToLower(strings.TrimSpace(label.Name)); name != "" {
			labels = append(labels, name)
		}
	}
	return labels
}

func mapGitHubIssue(issue githubIssue, mappedState string) (Issue, error) {
	id := strconv.FormatInt(issue.ID, 10)
	if id == "0" && issue.Number != 0 {
		id = strconv.Itoa(issue.Number)
	}
	createdAt, err := parseGitHubIssueTime("created_at", issue.CreatedAt)
	if err != nil {
		return Issue{}, err
	}
	updatedAt, err := parseGitHubIssueTime("updated_at", issue.UpdatedAt)
	if err != nil {
		return Issue{}, err
	}
	labels := githubIssueLabels(issue)
	state := strings.TrimSpace(mappedState)
	if state == "" {
		state = strings.TrimSpace(issue.State)
	}
	return Issue{
		ID:          id,
		Identifier:  fmt.Sprintf("#%d", issue.Number),
		Title:       issue.Title,
		Description: issue.Body,
		URL:         issue.HTMLURL,
		State:       state,
		Labels:      labels,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
	}, nil
}

func parseGitHubIssueTime(field, value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse GitHub issue %s %q: %w", field, value, err)
	}
	return parsed, nil
}

func githubIssueQueryForState(state string) (issueState, label, mappedState string) {
	state = strings.TrimSpace(state)
	switch strings.ToLower(state) {
	case "open", "closed", "all":
		return strings.ToLower(state), "", strings.ToLower(state)
	default:
		return "open", state, state
	}
}

func githubStatesMayIncludeOpenIssues(states []string) bool {
	for _, state := range states {
		issueState, _, _ := githubIssueQueryForState(state)
		if issueState == "open" || issueState == "all" {
			return true
		}
	}
	return false
}

func githubIssueQueryRequiresCompleteClaims(issueState string) bool {
	issueState = strings.ToLower(strings.TrimSpace(issueState))
	return issueState == "open" || issueState == "all"
}

func githubIssueCollectionScope(issueState, label string) string {
	if strings.TrimSpace(label) != "" {
		return label
	}
	return issueState
}

func githubClaimedIssueNumbers(text string) []int {
	matches := githubClaimedIssueRE.FindAllStringSubmatch(text, -1)
	out := make([]int, 0, len(matches))
	seen := map[int]struct{}{}
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		number, err := strconv.Atoi(match[1])
		if err != nil || number == 0 {
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

func nonEmptyGitHubStates(states []string) []string {
	out := make([]string, 0, len(states))
	seen := map[string]struct{}{}
	for _, state := range states {
		state = strings.TrimSpace(state)
		if state == "" {
			continue
		}
		key := strings.ToLower(state)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, state)
	}
	return out
}

// githubRateLimited reports whether resp is one of GitHub's documented
// rate-limit responses. Primary and secondary limits surface as 403 as well
// as 429 (REST API docs), distinguished from ordinary permission 403s by an
// exhausted X-RateLimit-Remaining or a Retry-After header — a plain 403
// stays a generic status error so auth misconfiguration is not misreported
// as throttling.
func githubRateLimited(resp *http.Response) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if resp.StatusCode != http.StatusForbidden {
		return false
	}
	return strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")) == "0" ||
		strings.TrimSpace(resp.Header.Get("Retry-After")) != ""
}

func githubHasNextPage(linkHeaders []string) bool {
	for _, header := range linkHeaders {
		for _, part := range strings.Split(header, ",") {
			if strings.Contains(part, `rel="next"`) {
				return true
			}
		}
	}
	return false
}

func (c *GitHubClient) recordPaginationCapHit(label string, maxPages int) {
	c.paginationCapHits.Add(1)
	if c.Logf != nil {
		c.Logf("github pagination exceeded %d pages for label/state %q; failing this tracker listing so reconcile does not treat partial results as authoritative", maxPages, label)
	}
}

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
// BlockedBy stays nil (#750 documented gap): this adapter models no issue
// dependencies — its listing path never populates Issue.BlockedBy either —
// so the refresh has no blocker knowledge to supply and consumers keep
// their listing-time blocker verdict (which is likewise always empty here).
func (c *GitHubClient) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]IssueState, error) {
	return c.FetchIssueStatesByRefs(ctx, IssueRefsFromIDs(issueIDs))
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
	states := make(map[string]IssueState, len(issueRefs))
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
		// Carry the full label set (SPEC §6.4 required_labels gate) alongside the
		// resolved state so label removal can stop/release already-claimed work.
		states[issueID] = IssueState{State: state, Labels: githubIssueLabels(issue)}
	}
	return states, nil
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
	defer func() { _ = resp.Body.Close() }()
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
