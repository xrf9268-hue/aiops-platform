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

	paginationCapHits atomic.Int64
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
		baseURL = cfg.BaseURL
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

func (c *GitHubClient) ListIssuesByStates(ctx context.Context, states []string) ([]Issue, error) {
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
	if githubStatesMayIncludeOpenIssues(stateFilters) {
		var err error
		claimedIssueNumbers, err = c.openPullRequestClaimedIssueNumbers(ctx)
		if err != nil {
			return nil, err
		}
	}
	seen := map[string]struct{}{}
	var out []Issue
	for _, state := range stateFilters {
		issueState, label, mappedState := githubIssueQueryForState(state)
		issues, err := c.listIssuesForState(ctx, issueState, label, mappedState, seen, claimedIssueNumbers)
		if err != nil {
			return nil, err
		}
		out = append(out, issues...)
	}
	return out, nil
}

func (c *GitHubClient) listIssuesForState(ctx context.Context, issueState, label, mappedState string, seen map[string]struct{}, claimedIssueNumbers map[int]struct{}) ([]Issue, error) {
	var out []Issue
	for page := 1; page <= githubMaxIssuePages+1; page++ {
		batch, hasNext, err := c.listIssuesPage(ctx, issueState, label, page)
		if err != nil {
			return nil, err
		}
		if page > githubMaxIssuePages {
			if !hasNext && len(batch) == 0 {
				return out, nil
			}
			c.recordPaginationCapHit(label)
			return nil, fmt.Errorf("github issue pagination exceeded %d pages for label/state %q", githubMaxIssuePages, label)
		}
		for _, issue := range batch {
			if issue.PullRequest != nil {
				continue
			}
			if _, claimed := claimedIssueNumbers[issue.Number]; claimed && strings.EqualFold(strings.TrimSpace(issue.State), "open") {
				continue
			}
			mapped, err := mapGitHubIssue(issue, mappedState)
			if err != nil {
				return nil, err
			}
			if mapped.ID == "" {
				continue
			}
			if _, ok := seen[mapped.ID]; ok {
				continue
			}
			seen[mapped.ID] = struct{}{}
			out = append(out, mapped)
		}
		if !hasNext && len(batch) < githubIssuePageSize {
			return out, nil
		}
	}
	return nil, fmt.Errorf("github issue pagination exceeded %d pages", githubMaxIssuePages)
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
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
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("list GitHub issues failed: %s", resp.Status)
	}
	var issues []githubIssue
	if err := json.NewDecoder(resp.Body).Decode(&issues); err != nil {
		return nil, false, err
	}
	return issues, githubHasNextPage(resp.Header.Values("Link")), nil
}

func (c *GitHubClient) openPullRequestClaimedIssueNumbers(ctx context.Context) (map[int]struct{}, error) {
	out := map[int]struct{}{}
	for page := 1; page <= githubMaxIssuePages; page++ {
		pulls, hasNext, err := c.listOpenPullRequestsPage(ctx, page)
		if err != nil {
			return nil, err
		}
		for _, pull := range pulls {
			if !strings.EqualFold(strings.TrimSpace(pull.State), "open") {
				continue
			}
			for _, issueNumber := range githubClaimedIssueNumbers(pull.Title + "\n" + pull.Body) {
				out[issueNumber] = struct{}{}
			}
		}
		if page == githubMaxIssuePages {
			if hasNext {
				c.recordPaginationCapHit("open pull requests")
				return nil, fmt.Errorf("github open pull request pagination exceeded %d pages", githubMaxIssuePages)
			}
			return out, nil
		}
		if !hasNext && len(pulls) < githubIssuePageSize {
			return out, nil
		}
	}
	return out, nil
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
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
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("list GitHub pull requests failed: %s", resp.Status)
	}
	var pulls []githubPullRequestSummary
	if err := json.NewDecoder(resp.Body).Decode(&pulls); err != nil {
		return nil, false, err
	}
	return pulls, githubHasNextPage(resp.Header.Values("Link")), nil
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
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		if name := strings.TrimSpace(label.Name); name != "" {
			labels = append(labels, name)
		}
	}
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

func (c *GitHubClient) recordPaginationCapHit(label string) {
	c.paginationCapHits.Add(1)
	if c.Logf != nil {
		c.Logf("github pagination exceeded %d pages for label/state %q; aborting this tracker poll to avoid acting on a truncated result set", githubMaxIssuePages, label)
	}
}
