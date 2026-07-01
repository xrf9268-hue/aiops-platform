package tracker

import (
	"net/http"
	"os"
	"regexp"
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
	issueMetadata     sync.Map // map[string]githubIssueMetadata — global issue ID → body/node metadata for blocker hydration
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
	NodeID      string             `json:"node_id"`
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

type githubIssueMetadata struct {
	NodeID string
	Number int
	Body   string
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

// NewGitHubClientFromEnv builds the GitHub tracker client with the base URL
// resolved exactly as the worker dispatch does: tracker.endpoint first, then
// the GITHUB_API_BASE_URL environment variable, then the constructor's
// api.github.com default. Shared by cmd/worker and internal/doctor so the
// doctor preflight can never drift from the poll loop's resolution (PR #801
// drift class).
func NewGitHubClientFromEnv(cfg workflow.TrackerConfig, owner, repo string) *GitHubClient {
	baseURL := cfg.Endpoint
	if baseURL == "" {
		baseURL = os.Getenv("GITHUB_API_BASE_URL")
	}
	return NewGitHubClient(cfg, baseURL, owner, repo)
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
