package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type giteaIssueLabelsProxy struct {
	token   string
	baseURL string
	owner   string
	repo    string
	http    *http.Client
	// httpTimeout overrides the per-request deadline applied to each Gitea API
	// round trip. Zero uses defaultDynamicToolHTTPTimeout; tests set a tiny
	// value to exercise the timeout path.
	httpTimeout time.Duration
	// currentIssueNumber, activeStates, and terminalStates power the
	// agent-handoff classification mirrored from the Linear tool path (#748):
	// a successful label replace that moves the current issue out of the
	// configured active states is the agent's handoff signal. A zero
	// currentIssueNumber disables classification; the audit still fires.
	currentIssueNumber int
	activeStates       []string
	terminalStates     []string
}

// withCurrentIssueClassification copies the dispatched issue's number and the
// tracker's configured state sets onto the proxy so a successful label replace
// can be classified as a current-issue handoff. Only "#N"-shaped refs are
// trusted (see gitea.IssueNumberFromRef); without one, classification stays
// disabled and the proxy behaves as before.
func (p giteaIssueLabelsProxy) withCurrentIssueClassification(cfg workflow.TrackerConfig, opts dynamicToolOptions) giteaIssueLabelsProxy {
	number, ok := gitea.IssueNumberFromRef(opts.currentIssueID, opts.currentIssueIdentifier)
	if !ok {
		return p
	}
	p.currentIssueNumber = number
	p.activeStates = append([]string(nil), cfg.ActiveStates...)
	p.terminalStates = append([]string(nil), cfg.TerminalStates...)
	return p
}

type giteaIssueLabel struct {
	ID   int64
	Name string
}

func (p giteaIssueLabelsProxy) call(ctx context.Context, call ToolCall) (string, error) {
	desiredStateLabels, failureResult, failureErr, ok := p.validateDesiredStateLabels(call)
	if !ok {
		return failureResult, failureErr
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
	staleStateLabels := computeStaleStateLabels(currentLabels, desiredStateLabels)
	labelsToAdd := missingLabels(currentLabels, desiredStateLabels)
	result, failure := p.addAndConfirmDesiredLabels(ctx, client, endpoint, labelsToAdd, desiredStateLabels)
	if failure != "" {
		return failure, nil
	}
	if failure := p.deleteStaleStateLabels(ctx, client, endpoint, staleStateLabels); failure != "" {
		return failure, nil
	}
	if len(labelsToAdd) > 0 || len(staleStateLabels) > 0 {
		p.fireMutationAudit(ctx, call.IssueNumber, desiredStateLabels[0], currentLabels)
	}
	return result, nil
}

// fireMutationAudit mirrors the Linear proxy's success-audit contract (#748):
// the shared mutation sink fires only after the full replace (desired-label
// add plus stale-label delete) succeeded AND at least one HTTP write actually
// ran. A partial failure must not fire — a surviving stale active label keeps
// the derived state active (IssueStateFromLabels active-first priority), so
// the issue never left the active set. The success path includes the retry
// that only deletes a stale label left by an earlier partial replace: that
// delete is the write that completes the handoff. A zero-write no-op (label
// already in the desired shape) must not fire: the flip happened elsewhere —
// an earlier audited call, or an operator's manual label edit, which must not
// be attributed to the agent as a handoff.
func (p giteaIssueLabelsProxy) fireMutationAudit(ctx context.Context, issueNumber int, desiredLabel string, preWriteLabels []giteaIssueLabel) {
	if sink := toolMutationSinkFrom(ctx); sink != nil {
		sink(p.classifyLabelMutation(issueNumber, desiredLabel, preWriteLabels))
	}
}

// classifyLabelMutation marks the audit as a current-issue handoff only when
// the replace moved the issue FROM a configured active state TO a non-active
// one. The pre-write gate matters: the Linear path's guard rejects current-
// issue mutations whose refreshed state is not active, so its classification
// implicitly proves the issue left the active set. The Gitea tool has no such
// guard, so without checking the pre-write labels a flip between two
// non-active states (e.g. an operator's manual aiops/done later relabeled
// aiops/human-review by the agent) would be misattributed as an agent handoff
// and mask the operator-owned stop.
func (p giteaIssueLabelsProxy) classifyLabelMutation(issueNumber int, desiredLabel string, preWriteLabels []giteaIssueLabel) ToolMutationAudit {
	audit := ToolMutationAudit{}
	if p.currentIssueNumber <= 0 || issueNumber != p.currentIssueNumber {
		return audit
	}
	preState, _ := gitea.IssueStateFromLabels(stateLabelsForClassification(preWriteLabels), nil)
	if preState == "" || matchStateFold(p.activeStates, preState) == "" {
		return audit
	}
	state, _ := gitea.IssueStateFromLabels([]gitea.Label{{Name: desiredLabel}}, nil)
	if state == "" || matchStateFold(p.activeStates, state) != "" {
		return audit
	}
	audit.CurrentIssueNonActiveStateUpdate = true
	if terminal := matchStateFold(p.terminalStates, state); terminal != "" {
		audit.CurrentIssueTerminalStateUpdate = true
		audit.CurrentIssueTerminalState = terminal
	}
	return audit
}

func stateLabelsForClassification(labels []giteaIssueLabel) []gitea.Label {
	out := make([]gitea.Label, 0, len(labels))
	for _, label := range labels {
		out = append(out, gitea.Label{Name: label.Name})
	}
	return out
}

// matchStateFold returns the configured state entry equal to state
// (case-insensitive, trimmed), or "" when absent. Returning the configured
// entry keeps the audit's terminal-state value sourced from the workflow
// config, matching the Linear guard's resolved-state semantics.
func matchStateFold(states []string, state string) string {
	state = strings.TrimSpace(state)
	if state == "" {
		return ""
	}
	for _, candidate := range states {
		if strings.EqualFold(strings.TrimSpace(candidate), state) {
			return strings.TrimSpace(candidate)
		}
	}
	return ""
}

// validateDesiredStateLabels enforces the tool's input contract: exactly one
// aiops/* state label for a valid issue number. The three guards return the
// (string, error) pair from dynamicToolFailure directly, so the Go error
// component is propagated — unlike the post-HTTP failure paths, which nil it.
func (p giteaIssueLabelsProxy) validateDesiredStateLabels(call ToolCall) (desired []string, failureResult string, failureErr error, ok bool) {
	if call.IssueNumber <= 0 {
		failureResult, failureErr = dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "gitea_issue_labels issue_number is required"},
		})
		return nil, failureResult, failureErr, false
	}
	if len(call.Labels) != 1 {
		failureResult, failureErr = dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "gitea_issue_labels labels must contain exactly one aiops/* state label"},
		})
		return nil, failureResult, failureErr, false
	}
	desiredStateLabels := make([]string, 0, len(call.Labels))
	for _, label := range call.Labels {
		label = strings.TrimSpace(label)
		if label == "" || !strings.HasPrefix(strings.ToLower(label), "aiops/") {
			failureResult, failureErr = dynamicToolFailure(map[string]any{
				"error": map[string]any{"message": "gitea_issue_labels only accepts aiops/* labels"},
			})
			return nil, failureResult, failureErr, false
		}
		if _, ok := validGiteaStateLabels()[strings.ToLower(label)]; !ok {
			failureResult, failureErr = dynamicToolFailure(map[string]any{
				"error": map[string]any{"message": "gitea_issue_labels label must be one of: aiops/canceled, aiops/done, aiops/human-review, aiops/in-progress, aiops/rework, aiops/todo"},
			})
			return nil, failureResult, failureErr, false
		}
		desiredStateLabels = append(desiredStateLabels, label)
	}
	return desiredStateLabels, "", nil, true
}

// computeStaleStateLabels selects the aiops/* labels currently on the issue
// that are not the desired state label and therefore must be removed.
func computeStaleStateLabels(currentLabels []giteaIssueLabel, desiredStateLabels []string) []giteaIssueLabel {
	staleStateLabels := make([]giteaIssueLabel, 0, len(currentLabels))
	for _, label := range currentLabels {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(label.Name)), "aiops/") && !containsLabelFold(desiredStateLabels, label.Name) {
			staleStateLabels = append(staleStateLabels, label)
		}
	}
	return staleStateLabels
}

// addAndConfirmDesiredLabels adds the missing desired labels and confirms the
// add response actually contains them; when nothing needs adding it returns the
// synthetic empty-label result. failure is "" on success.
func (p giteaIssueLabelsProxy) addAndConfirmDesiredLabels(ctx context.Context, client *http.Client, endpoint string, labelsToAdd, desiredStateLabels []string) (result string, failure string) {
	if len(labelsToAdd) > 0 {
		result, failure = p.addIssueLabels(ctx, client, endpoint, labelsToAdd)
		if failure != "" {
			return "", failure
		}
		confirmedLabels, confirmationFailure := p.decodeIssueLabelsFromToolResult(result, "Gitea label add response")
		if confirmationFailure != "" {
			return "", confirmationFailure
		}
		for _, desired := range desiredStateLabels {
			if !containsIssueLabelFold(confirmedLabels, desired) {
				failure, _ = dynamicToolFailure(map[string]any{
					"error": map[string]any{"message": "Gitea label add response did not include desired aiops label", "label": desired},
				})
				return "", failure
			}
		}
		return result, ""
	}
	result, _ = dynamicToolResult(true, `{"labels":[]}`)
	return result, ""
}

// deleteStaleStateLabels removes each stale aiops/* label, failing if Gitea
// omitted a label's id. failure is "" on success.
func (p giteaIssueLabelsProxy) deleteStaleStateLabels(ctx context.Context, client *http.Client, endpoint string, staleStateLabels []giteaIssueLabel) string {
	for _, label := range staleStateLabels {
		if label.ID == 0 {
			failure, _ := dynamicToolFailure(map[string]any{
				"error": map[string]any{"message": "Gitea label response omitted id for stale aiops label", "label": label.Name},
			})
			return failure
		}
		if failure := p.deleteIssueLabel(ctx, client, endpoint, label.ID); failure != "" {
			return failure
		}
	}
	return ""
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
	// Redact the token from the 2xx body too: SPEC token isolation (#76/#298)
	// holds regardless of status, so a Gitea/proxy that echoes the
	// Authorization value in a success response must not reach the agent.
	result, _ := dynamicToolResult(true, redactToolSecrets(string(respBody), p.token))
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
	ctx, cancel := context.WithTimeout(ctx, dynamicToolRequestTimeout(p.httpTimeout))
	defer cancel()
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
	defer func() { _ = resp.Body.Close() }()
	respBody, readErr := readDynamicToolResponseBody(resp.Body)
	if readErr != nil {
		if errors.Is(readErr, errDynamicToolResponseTooLarge) {
			failure, _ := dynamicToolFailure(map[string]any{
				"error": map[string]any{"message": "Gitea label response exceeded maximum size", "limit": maxLinearGraphQLResponseBytes},
			})
			return resp.StatusCode, nil, failure
		}
		failure, _ := dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label response body could not be read", "reason": readErr.Error()},
		})
		return resp.StatusCode, nil, failure
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure, _ := dynamicToolFailure(map[string]any{
			"error": map[string]any{"message": "Gitea label request failed", "status": resp.Status, "body": sanitizeToolErrorBody(respBody, p.token)},
		})
		return resp.StatusCode, respBody, failure
	}
	return resp.StatusCode, respBody, ""
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
	return gitea.BaseURLFromTrackerConfig(cfg, os.Getenv("GITEA_BASE_URL"))
}
