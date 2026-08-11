package tracker

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	githubprofile "github.com/xrf9268-hue/aiops-platform/internal/trackerprofile/github"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

const (
	githubIssuePageSize = 100
	githubMaxIssuePages = 10
	githubAPIVersion    = "2022-11-28"
)

var githubClaimedIssueRE = regexp.MustCompile(`(?i)\b(?:(?:close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved)|(?:(?:assigned|github)\s+)?issue)\s*:?\s+(?:[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)?#([0-9]+)\b`)

type GitHubClient struct {
	BaseURL            string
	Token              string
	Owner              string
	Repo               string
	PaginationMaxPages int
	Config             workflow.TrackerConfig
	HTTP               *http.Client
	Logf               func(format string, args ...any)
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
// resolved exactly as the worker dispatch does: tracker.provider.api_url first, then
// the GITHUB_API_BASE_URL environment variable, then the constructor's
// api.github.com default. Shared by cmd/worker and internal/doctor so the
// doctor preflight can never drift from the poll loop's resolution (PR #801
// drift class).
func NewGitHubClientFromEnv(cfg workflow.TrackerConfig, owner, repo string) *GitHubClient {
	profile := githubProfileForConfig(cfg, owner, repo)
	if profile != nil {
		return NewGitHubClient(cfg, profile.APIURL, profile.Owner, profile.Repo)
	}
	return NewGitHubClient(cfg, os.Getenv("GITHUB_API_BASE_URL"), owner, repo)
}

func NewGitHubClient(cfg workflow.TrackerConfig, baseURL, owner, repo string) *GitHubClient {
	profile := githubProfileForConfig(cfg, owner, repo)
	if strings.TrimSpace(baseURL) == "" {
		baseURL = profile.APIURL
	}
	return &GitHubClient{
		BaseURL:            strings.TrimRight(baseURL, "/"),
		Token:              profile.Token,
		Owner:              profile.Owner,
		Repo:               profile.Repo,
		PaginationMaxPages: profile.PaginationMaxPages,
		Config:             cfg,
		HTTP:               http.DefaultClient,
	}
}

func githubProfileForConfig(cfg workflow.TrackerConfig, owner, repo string) *githubprofile.Profile {
	if profile, ok := cfg.ProviderProfile().(*githubprofile.Profile); ok {
		return profile
	}
	profile, err := githubprofile.Parse(cfg.Provider, owner, repo, os.LookupEnv)
	if err == nil {
		return profile
	}
	return &githubprofile.Profile{APIURL: githubprofile.DefaultAPIURL, Owner: owner, Repo: repo, PaginationMaxPages: githubMaxIssuePages}
}
