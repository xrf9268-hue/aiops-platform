package gitea

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

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

func (c *TrackerClient) ListIssuesByStates(ctx context.Context, states []string) ([]tracker.Issue, error) {
	if c.BaseURL == "" || c.Token == "" {
		return nil, fmt.Errorf("GITEA_BASE_URL and Gitea tracker api_key are required")
	}
	if c.Owner == "" || c.Repo == "" {
		return nil, fmt.Errorf("repo.owner and repo.name are required for Gitea tracker polling")
	}
	wantedStates := normalizedStateSet(states)
	labelNames := StateLabelNamesForStates(states, DefaultStateLabelMappings())
	issueState := giteaAPIStateForWorkflowStates(states, c.Config.TerminalStates)

	var out []tracker.Issue
	for page := 1; page <= listIssuesMaxPages+1; page++ {
		batch, err := c.listIssuesPage(ctx, labelNames, issueState, page)
		if err != nil {
			return nil, err
		}
		if page > listIssuesMaxPages {
			if len(batch) == 0 {
				return out, nil
			}
			return nil, fmt.Errorf("gitea issue pagination exceeded %d pages", listIssuesMaxPages)
		}
		for _, issue := range batch {
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
			out = append(out, tracker.Issue{
				ID:          strconv.FormatInt(issue.ID, 10),
				Identifier:  fmt.Sprintf("#%d", issue.Number),
				Title:       issue.Title,
				Description: issue.Body,
				URL:         issue.HTMLURL,
				State:       state,
				UpdatedAt:   issue.UpdatedAt,
			})
		}
		if len(batch) < listIssuesPageSize {
			return out, nil
		}
	}
	return nil, fmt.Errorf("gitea issue pagination exceeded %d pages", listIssuesMaxPages)
}

func (c *TrackerClient) listIssuesPage(ctx context.Context, labelNames []string, issueState string, page int) ([]Issue, error) {
	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/issues", c.BaseURL, url.PathEscape(c.Owner), url.PathEscape(c.Repo))
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("state", issueState)
	q.Set("type", "issues")
	q.Set("page", strconv.Itoa(page))
	q.Set("limit", strconv.Itoa(listIssuesPageSize))
	if len(labelNames) > 0 {
		q.Set("labels", strings.Join(labelNames, ","))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+c.Token)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("list Gitea issues failed: %s", resp.Status)
	}
	var issues []Issue
	if err := json.NewDecoder(resp.Body).Decode(&issues); err != nil {
		return nil, err
	}
	return issues, nil
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
