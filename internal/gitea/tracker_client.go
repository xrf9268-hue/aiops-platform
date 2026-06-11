package gitea

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

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

const (
	listIssuesPageSize = 50
	listIssuesMaxPages = 20
)

// Issue is the subset of Gitea's issue JSON used by the tracker reader.
type Issue struct {
	ID        int64   `json:"id"`
	Number    int     `json:"number"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	HTMLURL   string  `json:"html_url"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	Labels    []Label `json:"labels"`
}

// TrackerClient is the Gitea issue reader used by pollers/reconciliation. It
// intentionally exposes no label mutation methods; Gitea writes belong on the
// agent-side dynamic tool surface per SPEC §1.
type TrackerClient struct {
	BaseURL string
	Token   string
	Owner   string
	Repo    string
	Config  workflow.TrackerConfig
	HTTP    *http.Client
	Logf    func(format string, args ...any)
	// RequestTimeout caps the wall-clock duration of a single Gitea tracker
	// request. Zero falls back to defaultGiteaRequestTimeout.
	RequestTimeout time.Duration

	issueNumbers sync.Map

	paginationCapHits atomic.Int64
}

func NewTrackerClient(cfg workflow.TrackerConfig, baseURL, owner, repo string) *TrackerClient {
	return &TrackerClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   cfg.APIKey,
		Owner:   owner,
		Repo:    repo,
		Config:  cfg,
	}
}

func (c *TrackerClient) requestTimeout() time.Duration {
	if c != nil && c.RequestTimeout > 0 {
		return c.RequestTimeout
	}
	return defaultGiteaRequestTimeout
}

func (c *TrackerClient) httpClient() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: c.requestTimeout()}
}

func (c *TrackerClient) ListActiveIssues(ctx context.Context) ([]tracker.Issue, error) {
	return c.ListIssuesByStates(ctx, c.Config.ActiveStates)
}

// PaginationCapHits returns how often this client observed more than
// listIssuesMaxPages of Gitea issue results for a label-scoped listing.
func (c *TrackerClient) PaginationCapHits() int64 {
	return c.paginationCapHits.Load()
}

func (c *TrackerClient) IssueMaxPages() int {
	if c != nil && c.Config.PaginationMaxPages > 0 {
		return c.Config.PaginationMaxPages
	}
	return listIssuesMaxPages
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
		states[issueID] = tracker.IssueState{State: state, Labels: extractGiteaLabels(issue.Labels)}
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

func parseGiteaIssueTime(field, value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse Gitea issue %s %q: %w", field, value, err)
	}
	return parsed, nil
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return Issue{}, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Issue{}, false, fmt.Errorf("get Gitea issue #%d failed: %s", issueNumber, resp.Status)
	}
	var issue Issue
	if err := json.NewDecoder(resp.Body).Decode(&issue); err != nil {
		return Issue{}, false, err
	}
	return issue, true, nil
}

func (c *TrackerClient) cacheIssueNumber(issue Issue) {
	issueID := giteaIssueID(issue)
	if issueID == "" || issue.Number <= 0 {
		return
	}
	c.issueNumbers.Store(issueID, issue.Number)
}

func (c *TrackerClient) cachedIssueNumber(issueID string) (int, bool) {
	got, ok := c.issueNumbers.Load(issueID)
	if !ok {
		return 0, false
	}
	issueNumber, ok := got.(int)
	return issueNumber, ok && issueNumber > 0
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("list Gitea issues failed: %s", resp.Status)
	}
	var issues []Issue
	if err := json.NewDecoder(resp.Body).Decode(&issues); err != nil {
		return nil, false, err
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

// dependsOnRegexp matches the documented Gitea cross-reference syntax
// `Depends on #N` (case-insensitive, anywhere in body) per SPEC §11.3.
// Gitea has no native priority field, so `Priority` on the normalized Issue
// stays zero for this tracker; the dispatch sort falls back to created_at
// per §8.2. Operators who want label-driven priority can wire it as a
// follow-up.
var dependsOnRegexp = regexp.MustCompile(`(?i)depends on #(\d+)`)

// extractGiteaLabels returns lowercased label names per SPEC §11.3
// normalization. The Gitea label payload (`Label.Name`) is already deserialized
// by IssueStateFromLabels; we just project it onto the cross-tracker Issue
// shape.
func extractGiteaLabels(labels []Label) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		name := strings.TrimSpace(l.Name)
		if name == "" {
			continue
		}
		out = append(out, strings.ToLower(name))
	}
	return out
}

// giteaTrackerContextKey scopes context values set by this package so they
// cannot collide with keys from other packages.
type giteaTrackerContextKey int

const blockerCacheContextKey giteaTrackerContextKey = iota

// blockerCacheEntry memoizes one getIssueByNumber result so a `Depends on #N`
// blocker referenced by multiple source issues is fetched at most once per poll
// tick (#677). Only definitive results — a successful fetch or a 404 not-found —
// are cached; a transient fetch error is deliberately not cached so a later
// source issue referencing the same blocker can still retry it within the tick,
// preserving the original best-effort skip behavior without dropping a blocker
// on a transient error.
type blockerCacheEntry struct {
	issue Issue
	found bool
}

// withBlockerCache installs a fresh per-poll-tick blocker memoization cache on
// ctx. ListIssuesByStates calls it once per tick so buildBlockedBy dedupes
// blocker fetches across every source issue in the tick; the cache lives only
// for that context, so each blocker's state is re-read on the next tick.
func withBlockerCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, blockerCacheContextKey, map[int]blockerCacheEntry{})
}

func blockerCacheFrom(ctx context.Context) map[int]blockerCacheEntry {
	cache, _ := ctx.Value(blockerCacheContextKey).(map[int]blockerCacheEntry)
	return cache
}

// cachedIssueByNumber fetches blocker issue n via getIssueByNumber at most once
// per poll tick. A successful fetch and a definitive 404 are memoized; a
// transient error returns a miss without caching so a later source issue can
// retry. When no per-tick cache is installed (non-poll callers), it falls back
// to a direct fetch.
func (c *TrackerClient) cachedIssueByNumber(ctx context.Context, cache map[int]blockerCacheEntry, n int) (Issue, bool) {
	if cache != nil {
		if entry, ok := cache[n]; ok {
			return entry.issue, entry.found
		}
	}
	issue, found, err := c.getIssueByNumber(ctx, n)
	if err != nil {
		return Issue{}, false
	}
	if cache != nil {
		cache[n] = blockerCacheEntry{issue: issue, found: found}
	}
	return issue, found
}

// buildBlockedBy parses `Depends on #N` references from issue.Body and looks
// up each blocker's current workflow state via a follow-up Gitea fetch so the
// §8.2 Todo blocker rule can compare against the blocker's State. Lookup
// failures are silently skipped (best-effort): a missing or deleted blocker
// drops out of the list rather than aborting candidate enumeration. The
// per-poll-tick blocker cache (installed by ListIssuesByStates) ensures each
// distinct blocker is fetched at most once per tick across all source issues,
// instead of O(distinct refs) per source issue (#677).
func (c *TrackerClient) buildBlockedBy(ctx context.Context, body string) []tracker.BlockerRef {
	matches := dependsOnRegexp.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}
	cache := blockerCacheFrom(ctx)
	seen := map[int]struct{}{}
	var out []tracker.BlockerRef
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		issue, found := c.cachedIssueByNumber(ctx, cache, n)
		if !found {
			continue
		}
		state, _ := IssueStateFromLabels(issue.Labels, DefaultStateLabelMappings())
		out = append(out, tracker.BlockerRef{
			ID:         giteaIssueID(issue),
			Identifier: fmt.Sprintf("#%d", issue.Number),
			State:      state,
		})
	}
	return out
}

func giteaIssueID(issue Issue) string {
	if issue.ID != 0 {
		return strconv.FormatInt(issue.ID, 10)
	}
	if issue.Number != 0 {
		return strconv.Itoa(issue.Number)
	}
	return ""
}

func giteaAPIStateForWorkflowStates(states, terminalStateNames []string) string {
	terminalStates := normalizedStateSet(terminalStateNames)
	if len(terminalStates) == 0 {
		terminalStates = normalizedStateSet(workflow.DefaultConfig().Tracker.TerminalStates)
	}
	for _, state := range states {
		if _, ok := terminalStates[strings.ToLower(strings.TrimSpace(state))]; ok {
			return "all"
		}
	}
	return "open"
}

func (c *TrackerClient) logDiagnostics(issue Issue, diagnostics []StateDiagnostic) {
	if c.Logf == nil {
		return
	}
	identifier := issue.Number
	for _, diagnostic := range diagnostics {
		c.Logf("gitea issue #%d label diagnostic %s: %s", identifier, diagnostic.Code, diagnostic.Message)
	}
}

func normalizedStateSet(states []string) map[string]struct{} {
	set := make(map[string]struct{}, len(states))
	for _, state := range states {
		state = strings.ToLower(strings.TrimSpace(state))
		if state != "" {
			set[state] = struct{}{}
		}
	}
	return set
}
