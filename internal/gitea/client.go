package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// defaultGiteaRequestTimeout bounds a single Gitea HTTP request when the
// caller does not set Client.RequestTimeout explicitly. SPEC §8.1's
// poll-tick cadence is minute-scale, so a 30 s per-request ceiling
// catches hung-but-not-yet-RST connections (the failure mode #295
// described — TCP half-open, NLB blackhole, slow server) well before
// the OS-level keepalive RTO trips.
const defaultGiteaRequestTimeout = 30 * time.Second

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// RequestTimeout caps the wall-clock duration of a single Gitea
	// request. Zero falls back to defaultGiteaRequestTimeout. Closes
	// #295: without a per-request bound, a hung upstream would wedge
	// the worker's poll loop until the OS keepalive timeout (typically
	// tcp_keepalive_time=7200s on Linux) and leak goroutines + fds in
	// the meantime.
	RequestTimeout time.Duration
}

func (c Client) requestTimeout() time.Duration {
	if c.RequestTimeout > 0 {
		return c.RequestTimeout
	}
	return defaultGiteaRequestTimeout
}

type CreatePullRequestInput struct {
	Owner string
	Repo  string
	Title string
	Body  string
	Head  string
	Base  string
	// Draft, when true, asks Gitea to open the pull request as a draft.
	//
	// Gitea's CreatePullRequestOption has NO `draft` field — verified against
	// Gitea release/v1.26 source (modules/structs/pull.go). Draft state is
	// derived exclusively from a Work-In-Progress title prefix
	// (`WIP:` / `[WIP]` by default; configurable via Gitea's
	// `setting.Repository.PullRequest.WorkInProgressPrefixes`). When Draft is
	// true and the input Title does not already begin with a recognized WIP
	// prefix, this client prepends `WIP: ` before sending the request.
	Draft bool
}

type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Title   string `json:"title"`
	// Draft is populated from the Gitea response. Older Gitea versions
	// (≤ 1.21.x) omit the field, so callers should treat false as
	// "either not draft or unknown" on those versions.
	Draft bool `json:"draft"`
}

// wipPrefixes mirrors Gitea's default
// `setting.Repository.PullRequest.WorkInProgressPrefixes`. We do not read
// the actual Gitea config; if an operator has customized the prefix list
// the worker still uses these defaults, which is acceptable because the
// match in Gitea is a superset (Gitea will recognize "WIP: " regardless
// of whether it has been removed from the config — and in practice it
// has not been).
var wipPrefixes = []string{"WIP:", "[WIP]"}

// hasWIPPrefix reports whether title already starts with one of Gitea's
// recognized Work-In-Progress prefixes (case-insensitive, matching
// Gitea's util.AsciiEqualFold behavior).
func hasWIPPrefix(title string) bool {
	upper := strings.ToUpper(title)
	for _, p := range wipPrefixes {
		if strings.HasPrefix(upper, strings.ToUpper(p)) {
			return true
		}
	}
	return false
}

type FindOpenPullRequestInput struct {
	Owner string
	Repo  string
	// Head is the source branch ref (e.g. "ai/tsk_42") to match against the
	// `head.ref` of each open pull request. Gitea's list endpoint does not
	// reliably support filtering by head across versions, so we filter
	// client-side after pulling each page.
	Head string
}

// listPullsPageSize is the page size we ask Gitea for when scanning open PRs
// looking for one whose head ref matches a given work branch. Gitea's default
// page size is 50; we cap pagination at listPullsMaxPages so a misconfigured
// server cannot make us walk forever.
const (
	listPullsPageSize = 50
	listPullsMaxPages = 10
)

// FindOpenPullRequest returns the first open pull request whose head ref
// equals in.Head, or (nil, nil) when no open PR exists for that branch. It is
// used by the worker to make PR handoff idempotent: if a previous attempt for
// the same task has already opened a PR for the work branch, retries reuse it
// instead of asking Gitea to create a duplicate.
func (c Client) FindOpenPullRequest(ctx context.Context, in FindOpenPullRequestInput) (*PullRequest, error) { //nolint:gocognit // baseline (#521)
	if c.BaseURL == "" || c.Token == "" {
		return nil, fmt.Errorf("GITEA_BASE_URL and GITEA_TOKEN are required")
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	base := strings.TrimRight(c.BaseURL, "/") + fmt.Sprintf("/api/v1/repos/%s/%s/pulls", url.PathEscape(in.Owner), url.PathEscape(in.Repo))
	for page := 1; page <= listPullsMaxPages; page++ {
		u := fmt.Sprintf("%s?state=open&page=%d&limit=%d", base, page, listPullsPageSize)
		reqCtx, cancel := context.WithTimeout(ctx, c.requestTimeout())
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
		if err != nil {
			cancel()
			return nil, err
		}
		req.Header.Set("Authorization", "token "+c.Token)
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			return nil, err
		}
		var batch []struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
			Title   string `json:"title"`
			Head    struct {
				Ref string `json:"ref"`
			} `json:"head"`
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			_ = resp.Body.Close()
			cancel()
			return nil, fmt.Errorf("list pull requests failed: %s", resp.Status)
		}
		err = json.NewDecoder(resp.Body).Decode(&batch)
		_ = resp.Body.Close()
		cancel()
		if err != nil {
			return nil, err
		}
		for _, p := range batch {
			if p.Head.Ref == in.Head {
				return &PullRequest{Number: p.Number, HTMLURL: p.HTMLURL, Title: p.Title}, nil
			}
		}
		if len(batch) < listPullsPageSize {
			return nil, nil
		}
	}
	return nil, nil
}

func (c Client) CreatePullRequest(ctx context.Context, in CreatePullRequestInput) (*PullRequest, error) {
	if c.BaseURL == "" || c.Token == "" {
		return nil, fmt.Errorf("GITEA_BASE_URL and GITEA_TOKEN are required")
	}
	title := in.Title
	if in.Draft && !hasWIPPrefix(title) {
		title = "WIP: " + title
	}
	payload := map[string]any{
		"title": title,
		"body":  in.Body,
		"head":  in.Head,
		"base":  in.Base,
	}
	b, _ := json.Marshal(payload)
	endpoint := strings.TrimRight(c.BaseURL, "/") + fmt.Sprintf("/api/v1/repos/%s/%s/pulls", url.PathEscape(in.Owner), url.PathEscape(in.Repo))
	reqCtx, cancel := context.WithTimeout(ctx, c.requestTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+c.Token)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("create pull request failed: %s", resp.Status)
	}
	var pr PullRequest
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	return &pr, nil
}
