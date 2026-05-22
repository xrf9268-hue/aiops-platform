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

func ListIssuesMaxPages() int {
	return listIssuesMaxPages
}

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
		HTTP:    http.DefaultClient,
	}
}

func (c *TrackerClient) ListActiveIssues(ctx context.Context) ([]tracker.Issue, error) {
	return c.ListIssuesByStates(ctx, c.Config.ActiveStates)
}

// PaginationCapHits returns how often this client observed more than
// listIssuesMaxPages of Gitea issue results for a label-scoped listing.
func (c *TrackerClient) PaginationCapHits() int64 {
	return c.paginationCapHits.Load()
}

func (c *TrackerClient) ListIssuesByStates(ctx context.Context, states []string) ([]tracker.Issue, error) {
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
	labelNames := StateLabelNamesForStates(states, DefaultStateLabelMappings())
	issueState := giteaAPIStateForWorkflowStates(states, c.Config.TerminalStates)

	var out []tracker.Issue
	seenIssues := map[string]struct{}{}
	if len(labelNames) == 0 {
		return c.listIssuesByStateLabel(ctx, "", issueState, wantedStates, seenIssues)
	}
	for _, labelName := range labelNames {
		issues, err := c.listIssuesByStateLabel(ctx, labelName, issueState, wantedStates, seenIssues)
		if err != nil {
			return nil, err
		}
		out = append(out, issues...)
	}
	return out, nil
}

func (c *TrackerClient) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error) {
	if c.BaseURL == "" || c.Token == "" {
		return nil, fmt.Errorf("GITEA_BASE_URL and Gitea tracker api_key are required")
	}
	if c.Owner == "" || c.Repo == "" {
		return nil, fmt.Errorf("repo.owner and repo.name are required for Gitea tracker polling")
	}
	if len(issueIDs) == 0 {
		return map[string]string{}, nil
	}
	states := make(map[string]string, len(issueIDs))
	seen := map[string]struct{}{}
	for _, issueID := range issueIDs {
		issueID = strings.TrimSpace(issueID)
		if issueID == "" {
			continue
		}
		if _, ok := seen[issueID]; ok {
			continue
		}
		seen[issueID] = struct{}{}
		issueNumber, ok := c.cachedIssueNumber(issueID)
		if !ok {
			continue
		}
		issue, found, err := c.getIssueByNumber(ctx, issueNumber)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if refreshedID := giteaIssueID(issue); refreshedID != issueID {
			c.cacheIssueNumber(issue)
			continue
		}
		c.cacheIssueNumber(issue)
		state, diagnostics := IssueStateFromLabels(issue.Labels, DefaultStateLabelMappings())
		c.logDiagnostics(issue, diagnostics)
		if state == "" {
			continue
		}
		states[issueID] = state
	}
	return states, nil
}

func (c *TrackerClient) listIssuesByStateLabel(ctx context.Context, labelName, issueState string, wantedStates map[string]struct{}, seenIssues map[string]struct{}) ([]tracker.Issue, error) {
	var out []tracker.Issue
	for page := 1; page <= listIssuesMaxPages+1; page++ {
		batch, hasNext, err := c.listIssuesPage(ctx, labelName, issueState, page)
		if err != nil {
			return nil, err
		}
		if page > listIssuesMaxPages {
			if !hasNext && len(batch) == 0 {
				return out, nil
			}
			c.recordPaginationCapHit(labelName)
			return nil, fmt.Errorf("gitea issue pagination exceeded %d pages for label %q", listIssuesMaxPages, labelName)
		}
		for _, issue := range batch {
			issueKey := giteaIssueID(issue)
			if _, ok := seenIssues[issueKey]; ok {
				continue
			}
			c.cacheIssueNumber(issue)
			state, diagnostics := IssueStateFromLabels(issue.Labels, DefaultStateLabelMappings())
			c.logDiagnostics(issue, diagnostics)
			if state == "" {
				continue
			}
			if len(wantedStates) > 0 {
				if _, ok := wantedStates[strings.ToLower(state)]; !ok {
					continue
				}
			}
			createdAt, err := parseGiteaIssueTime("created_at", issue.CreatedAt)
			if err != nil {
				return nil, err
			}
			updatedAt, err := parseGiteaIssueTime("updated_at", issue.UpdatedAt)
			if err != nil {
				return nil, err
			}
			seenIssues[issueKey] = struct{}{}
			out = append(out, tracker.Issue{
				ID:          issueKey,
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
			})
		}
		if !hasNext && len(batch) < listIssuesPageSize {
			return out, nil
		}
	}
	return nil, fmt.Errorf("gitea issue pagination exceeded %d pages", listIssuesMaxPages)
}

func (c *TrackerClient) recordPaginationCapHit(labelName string) {
	c.paginationCapHits.Add(1)
	if c.Logf != nil {
		c.Logf("gitea issue pagination exceeded %d pages for label %q; aborting this tracker poll to avoid acting on a truncated result set", listIssuesMaxPages, labelName)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Issue{}, false, err
	}
	req.Header.Set("Authorization", "token "+c.Token)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return Issue{}, false, err
	}
	defer resp.Body.Close()
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "token "+c.Token)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
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

// buildBlockedBy parses `Depends on #N` references from issue.Body and looks
// up each blocker's current workflow state via a follow-up Gitea fetch so the
// §8.2 Todo blocker rule can compare against the blocker's State. Lookup
// failures are silently skipped (best-effort): a missing or deleted blocker
// drops out of the list rather than aborting candidate enumeration. Lookups
// are O(distinct refs) per source issue; this is acceptable for typical
// workflows (0–3 blockers per issue).
func (c *TrackerClient) buildBlockedBy(ctx context.Context, body string) []tracker.BlockerRef {
	matches := dependsOnRegexp.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}
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
		issue, found, err := c.getIssueByNumber(ctx, n)
		if err != nil || !found {
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
