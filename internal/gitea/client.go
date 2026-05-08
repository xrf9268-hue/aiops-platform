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
