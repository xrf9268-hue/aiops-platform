package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type LinearClient struct {
	APIKey  string
	BaseURL string
	Config  workflow.TrackerConfig
	HTTP    *http.Client
}

func NewLinearClient(cfg workflow.TrackerConfig) *LinearClient {
	base := "https://api.linear.app/graphql"
	return &LinearClient{APIKey: cfg.APIKey, BaseURL: base, Config: cfg, HTTP: http.DefaultClient}
}

func (c *LinearClient) ListActiveIssues(ctx context.Context) ([]Issue, error) {
	if c.APIKey == "" {
		return nil, fmt.Errorf("Linear API key is required")
	}
	query := `query ListIssues($states: [String!]) {
  issues(filter: { state: { name: { in: $states } } }, first: 50) {
    nodes { id identifier title description url updatedAt state { name } }
  }
}`
	payload := map[string]any{"query": query, "variables": map[string]any{"states": c.Config.ActiveStates}}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("linear query failed: %s", resp.Status)
	}
	var out struct {
		Data struct {
			Issues struct {
				Nodes []struct {
					ID          string `json:"id"`
					Identifier  string `json:"identifier"`
					Title       string `json:"title"`
					Description string `json:"description"`
					URL         string `json:"url"`
					UpdatedAt   string `json:"updatedAt"`
					State       struct {
						Name string `json:"name"`
					} `json:"state"`
				} `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("linear errors: %v", out.Errors)
	}
	issues := make([]Issue, 0, len(out.Data.Issues.Nodes))
	for _, n := range out.Data.Issues.Nodes {
		issues = append(issues, Issue{ID: n.ID, Identifier: n.Identifier, Title: n.Title, Description: n.Description, URL: n.URL, UpdatedAt: n.UpdatedAt, State: n.State.Name})
	}
	return issues, nil
}
