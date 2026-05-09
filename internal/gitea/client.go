package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

type CreatePullRequestInput struct {
	Owner string
	Repo  string
	Title string
	Body  string
	Head  string
	Base  string
	// Draft, when true, asks Gitea to open the pull request as a draft.
	// Gitea ≥ 1.18 supports the `draft` field on POST /repos/{owner}/{repo}/pulls.
	// When false (the default) the field is omitted so older Gitea versions
	// keep their existing behavior.
	Draft bool
}

type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Title   string `json:"title"`
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
func (c Client) FindOpenPullRequest(ctx context.Context, in FindOpenPullRequestInput) (*PullRequest, error) {
	if c.BaseURL == "" || c.Token == "" {
		return nil, fmt.Errorf("GITEA_BASE_URL and GITEA_TOKEN are required")
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	base := strings.TrimRight(c.BaseURL, "/") + fmt.Sprintf("/api/v1/repos/%s/%s/pulls", in.Owner, in.Repo)
	for page := 1; page <= listPullsMaxPages; page++ {
		u := fmt.Sprintf("%s?state=open&page=%d&limit=%d", base, page, listPullsPageSize)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "token "+c.Token)
		resp, err := client.Do(req)
		if err != nil {
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
			resp.Body.Close()
			return nil, fmt.Errorf("list pull requests failed: %s", resp.Status)
		}
		err = json.NewDecoder(resp.Body).Decode(&batch)
		resp.Body.Close()
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
	payload := map[string]any{
		"title": in.Title,
		"body":  in.Body,
		"head":  in.Head,
		"base":  in.Base,
	}
	if in.Draft {
		payload["draft"] = true
	}
	b, _ := json.Marshal(payload)
	url := strings.TrimRight(c.BaseURL, "/") + fmt.Sprintf("/api/v1/repos/%s/%s/pulls", in.Owner, in.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
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
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("create pull request failed: %s", resp.Status)
	}
	var pr PullRequest
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	return &pr, nil
}
