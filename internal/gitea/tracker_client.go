package gitea

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

const (
	listIssuesPageSize = 50
	listIssuesMaxPages = 20
)

// defaultGiteaRequestTimeout bounds a single Gitea HTTP request when the
// caller does not set TrackerClient.RequestTimeout explicitly. SPEC §8.1's
// poll-tick cadence is minute-scale, so a 30 s per-request ceiling
// catches hung-but-not-yet-RST connections (the failure mode #295
// described — TCP half-open, NLB blackhole, slow server) well before
// the OS-level keepalive RTO trips.
const defaultGiteaRequestTimeout = 30 * time.Second

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
