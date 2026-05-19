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
	staleStateLabels := make([]giteaIssueLabel, 0, len(currentLabels))
	for _, label := range currentLabels {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(label.Name)), "aiops/") && !containsLabelFold(desiredStateLabels, label.Name) {
			staleStateLabels = append(staleStateLabels, label)
		}
	}
	labelsToAdd := missingLabels(currentLabels, desiredStateLabels)
	var result string
	if len(labelsToAdd) > 0 {
		var failure string
		result, failure = p.addIssueLabels(ctx, client, endpoint, labelsToAdd)
		if failure != "" {
			return failure, nil
		}
		confirmedLabels, confirmationFailure := p.decodeIssueLabelsFromToolResult(result, "Gitea label add response")
		if confirmationFailure != "" {
			return confirmationFailure, nil
		}
		for _, desired := range desiredStateLabels {
			if !containsIssueLabelFold(confirmedLabels, desired) {
				return dynamicToolFailure(map[string]any{
					"error": map[string]any{"message": "Gitea label add response did not include desired aiops label", "label": desired},
				})
			}
		}
	} else {
		result, _ = dynamicToolResult(true, `{"labels":[]}`)
	}
	for _, label := range staleStateLabels {
		if label.ID == 0 {
			return dynamicToolFailure(map[string]any{
				"error": map[string]any{"message": "Gitea label response omitted id for stale aiops label", "label": label.Name},
			})
		}
		if failure := p.deleteIssueLabel(ctx, client, endpoint, label.ID); failure != "" {
			return failure, nil
		}
	}
	return result, nil
}

func (p giteaIssueLabelsProxy) addIssueLabels(ctx context.Context, client *http.Client, endpoint string, labels []string) (string, string) {
	payload := map[string]any{"labels": labels}
	body, err := json.Marshal(payload)
	if err != nil {
		failure, _ := dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "gitea_issue_labels payload could not be encoded", "reason": err.Error()},
		})
		return "", failure
	}
	_, respBody, failure := p.doGiteaRequest(ctx, client, http.MethodPost, endpoint, body)
	if failure != "" {
		return "", failure
	}
	result, _ := dynamicToolResult(true, string(respBody))
	return result, ""
}

func (p giteaIssueLabelsProxy) currentIssueLabels(ctx context.Context, client *http.Client, endpoint string) ([]giteaIssueLabel, string) {
	_, respBody, failure := p.doGiteaRequest(ctx, client, http.MethodGet, endpoint, nil)
	if failure != "" {
		return nil, failure
	}
	var labels []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(respBody, &labels); err != nil {
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
	status, _, failure := p.doGiteaRequest(ctx, client, http.MethodDelete, fmt.Sprintf("%s/%d", endpoint, labelID), nil)
	if status == http.StatusNotFound {
		return ""
	}
	return failure
}

func (p giteaIssueLabelsProxy) doGiteaRequest(ctx context.Context, client *http.Client, method, endpoint string, body []byte) (int, []byte, string) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		failure, _ := dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label request could not be built", "reason": err.Error()},
		})
		return 0, nil, failure
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "token "+strings.TrimSpace(p.token))
	resp, err := client.Do(req)
	if err != nil {
		failure, _ := dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label request failed during transport", "reason": err.Error()},
		})
		return 0, nil, failure
	}
	defer resp.Body.Close()
	var respBody bytes.Buffer
	_, readErr := respBody.ReadFrom(io.LimitReader(resp.Body, maxLinearGraphQLResponseBytes+1))
	if readErr != nil {
		failure, _ := dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label response body could not be read", "reason": readErr.Error()},
		})
		return resp.StatusCode, respBody.Bytes(), failure
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure, _ := dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label request failed", "status": resp.Status, "body": respBody.String()},
		})
		return resp.StatusCode, respBody.Bytes(), failure
	}
	return resp.StatusCode, respBody.Bytes(), ""
}

func (p giteaIssueLabelsProxy) decodeIssueLabelsFromToolResult(result, source string) ([]giteaIssueLabel, string) {
	var envelope struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal([]byte(result), &envelope); err != nil {
		failure, _ := dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": source + " envelope could not be decoded", "reason": err.Error()},
		})
		return nil, failure
	}
	var rawLabels []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(envelope.Output), &rawLabels); err != nil {
		var payload struct {
			Labels []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"labels"`
		}
		if objectErr := json.Unmarshal([]byte(envelope.Output), &payload); objectErr != nil {
			failure, _ := dynamicToolFailure(map[string]any{
				"error": map[string]any{"message": source + " body could not be decoded", "reason": err.Error(), "body": envelope.Output},
			})
			return nil, failure
		}
		rawLabels = payload.Labels
	}
	out := make([]giteaIssueLabel, 0, len(rawLabels))
	for _, label := range rawLabels {
		if strings.TrimSpace(label.Name) != "" {
			out = append(out, giteaIssueLabel{ID: label.ID, Name: label.Name})
		}
	}
	return out, ""
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
