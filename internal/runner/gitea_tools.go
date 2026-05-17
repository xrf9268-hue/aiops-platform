package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type giteaIssueLabelsProxy struct {
	token   string
	baseURL string
	owner   string
	repo    string
	http    *http.Client
}

type giteaIssueLabel struct {
	ID   int64
	Name string
}

func (p giteaIssueLabelsProxy) call(ctx context.Context, call ToolCall) (string, error) {
	if call.IssueNumber <= 0 {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "gitea_issue_labels issue_number is required"},
		})
	}
	if len(call.Labels) != 1 {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "gitea_issue_labels labels must contain exactly one aiops/* state label"},
		})
	}
	desiredStateLabels := make([]string, 0, len(call.Labels))
	for _, label := range call.Labels {
		label = strings.TrimSpace(label)
		if label == "" || !strings.HasPrefix(strings.ToLower(label), "aiops/") {
			return dynamicToolFailure(map[string]any{
				"error": map[string]any{"message": "gitea_issue_labels only accepts aiops/* labels"},
			})
		}
		if _, ok := validGiteaStateLabels()[strings.ToLower(label)]; !ok {
			return dynamicToolFailure(map[string]any{
				"error": map[string]any{"message": "gitea_issue_labels label must be one of: aiops/canceled, aiops/done, aiops/human-review, aiops/in-progress, aiops/rework, aiops/todo"},
			})
		}
		desiredStateLabels = append(desiredStateLabels, label)
	}

	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/issues/%d/labels", strings.TrimRight(p.baseURL, "/"), url.PathEscape(p.owner), url.PathEscape(p.repo), call.IssueNumber)
	client := p.http
	if client == nil {
		client = http.DefaultClient
	}
	currentLabels, failure := p.currentIssueLabels(ctx, client, endpoint)
	if failure != "" {
		return failure, nil
	}
	for _, label := range currentLabels {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(label.Name)), "aiops/") && !containsLabelFold(desiredStateLabels, label.Name) {
			if label.ID == 0 {
				return dynamicToolFailure(map[string]any{
					"error": map[string]any{"message": "Gitea label response omitted id for stale aiops label", "label": label.Name},
				})
			}
			if failure := p.deleteIssueLabel(ctx, client, endpoint, label.ID); failure != "" {
				return failure, nil
			}
		}
	}
	labelsToAdd := missingLabels(currentLabels, desiredStateLabels)
	if len(labelsToAdd) == 0 {
		return dynamicToolResult(true, `{"labels":[]}`)
	}
	payload := map[string]any{"labels": labelsToAdd}
	body, err := json.Marshal(payload)
	if err != nil {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "gitea_issue_labels payload could not be encoded", "reason": err.Error()},
		})
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label request could not be built", "reason": err.Error()},
		})
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+strings.TrimSpace(p.token))
	resp, err := client.Do(req)
	if err != nil {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label request failed during transport", "reason": err.Error()},
		})
	}
	defer resp.Body.Close()
	var respBody bytes.Buffer
	_, readErr := respBody.ReadFrom(io.LimitReader(resp.Body, maxLinearGraphQLResponseBytes+1))
	if readErr != nil {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label response body could not be read", "reason": readErr.Error()},
		})
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label request failed", "status": resp.Status, "body": respBody.String()},
		})
	}
	return dynamicToolResult(true, respBody.String())
}

func (p giteaIssueLabelsProxy) currentIssueLabels(ctx context.Context, client *http.Client, endpoint string) ([]giteaIssueLabel, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		failure, _ := dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label read request could not be built", "reason": err.Error()},
		})
		return nil, failure
	}
	req.Header.Set("Authorization", "token "+strings.TrimSpace(p.token))
	resp, err := client.Do(req)
	if err != nil {
		failure, _ := dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label read request failed during transport", "reason": err.Error()},
		})
		return nil, failure
	}
	defer resp.Body.Close()
	var respBody bytes.Buffer
	_, readErr := respBody.ReadFrom(io.LimitReader(resp.Body, maxLinearGraphQLResponseBytes+1))
	if readErr != nil {
		failure, _ := dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label read response body could not be read", "reason": readErr.Error()},
		})
		return nil, failure
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure, _ := dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label read request failed", "status": resp.Status, "body": respBody.String()},
		})
		return nil, failure
	}
	var labels []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(respBody.Bytes(), &labels); err != nil {
		failure, _ := dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label read response body could not be decoded", "reason": err.Error()},
		})
		return nil, failure
	}
	out := make([]giteaIssueLabel, 0, len(labels))
	for _, label := range labels {
		if strings.TrimSpace(label.Name) != "" {
			out = append(out, giteaIssueLabel{ID: label.ID, Name: label.Name})
		}
	}
	return out, ""
}

func (p giteaIssueLabelsProxy) deleteIssueLabel(ctx context.Context, client *http.Client, endpoint string, labelID int64) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, fmt.Sprintf("%s/%d", endpoint, labelID), nil)
	if err != nil {
		failure, _ := dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label delete request could not be built", "reason": err.Error()},
		})
		return failure
	}
	req.Header.Set("Authorization", "token "+strings.TrimSpace(p.token))
	resp, err := client.Do(req)
	if err != nil {
		failure, _ := dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label delete request failed during transport", "reason": err.Error()},
		})
		return failure
	}
	defer resp.Body.Close()
	var respBody bytes.Buffer
	_, readErr := respBody.ReadFrom(io.LimitReader(resp.Body, maxLinearGraphQLResponseBytes+1))
	if readErr != nil {
		failure, _ := dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label delete response body could not be read", "reason": readErr.Error()},
		})
		return failure
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure, _ := dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label delete request failed", "status": resp.Status, "body": respBody.String()},
		})
		return failure
	}
	return ""
}

func validGiteaStateLabels() map[string]struct{} {
	return map[string]struct{}{
		"aiops/canceled":     {},
		"aiops/done":         {},
		"aiops/human-review": {},
		"aiops/in-progress":  {},
		"aiops/rework":       {},
		"aiops/todo":         {},
	}
}

func replaceAIOpsLabels(currentLabels, desiredStateLabels []string) []string {
	labels := make([]string, 0, len(currentLabels)+len(desiredStateLabels))
	seen := make(map[string]struct{}, len(currentLabels)+len(desiredStateLabels))
	for _, label := range currentLabels {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" || strings.HasPrefix(strings.ToLower(trimmed), "aiops/") {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		labels = append(labels, trimmed)
	}
	for _, label := range desiredStateLabels {
		trimmed := strings.TrimSpace(label)
		key := strings.ToLower(trimmed)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		labels = append(labels, trimmed)
	}
	return labels
}

func containsLabelFold(labels []string, label string) bool {
	want := strings.ToLower(strings.TrimSpace(label))
	for _, candidate := range labels {
		if strings.ToLower(strings.TrimSpace(candidate)) == want {
			return true
		}
	}
	return false
}

func missingLabels(currentLabels []giteaIssueLabel, desiredLabels []string) []string {
	out := make([]string, 0, len(desiredLabels))
	for _, desired := range desiredLabels {
		if !containsIssueLabelFold(currentLabels, desired) {
			out = append(out, strings.TrimSpace(desired))
		}
	}
	return out
}

func containsIssueLabelFold(labels []giteaIssueLabel, label string) bool {
	want := strings.ToLower(strings.TrimSpace(label))
	for _, candidate := range labels {
		if strings.ToLower(strings.TrimSpace(candidate.Name)) == want {
			return true
		}
	}
	return false
}

func giteaBaseURLFromTracker(cfg workflow.TrackerConfig) string {
	if cfg.ProjectSlug != "" {
		return strings.TrimRight(cfg.ProjectSlug, "/")
	}
	return strings.TrimRight(os.Getenv("GITEA_BASE_URL"), "/")
}
