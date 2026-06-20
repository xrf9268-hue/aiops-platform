package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// ToolCall is the JSON-shaped input accepted by dynamic tools. Tracker
// endpoints are held by orchestrator-side proxies and are not part of this
// public call shape, so an agent cannot redirect orchestrator-held tokens to
// another host.
type ToolCall struct {
	Query       string         `json:"query"`
	Variables   map[string]any `json:"variables,omitempty"`
	IssueNumber int            `json:"issue_number,omitempty"`
	Labels      []string       `json:"labels,omitempty"`
	CloseIssue  bool           `json:"close_issue,omitempty"`
}

// DynamicTool is a client-side tool implemented by the orchestrator and made
// available to an app-server-capable agent session. Tool metadata must never
// include raw tracker tokens; Call closes over orchestrator config instead.
type DynamicTool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Call        func(context.Context, ToolCall) (string, error)
}

type dynamicToolResponse struct {
	Success      bool                     `json:"success"`
	Output       string                   `json:"output"`
	ContentItems []dynamicToolContentItem `json:"contentItems"`
}

type dynamicToolContentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// DynamicToolSet is the runtime tool surface advertised to the coding agent.
type DynamicToolSet struct {
	tools map[string]DynamicTool
}

func (s DynamicToolSet) Lookup(name string) (DynamicTool, bool) {
	tool, ok := s.tools[name]
	return tool, ok
}

func (s DynamicToolSet) Names() []string {
	names := make([]string, 0, len(s.tools))
	for name := range s.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type DynamicToolOption func(*dynamicToolOptions)

type dynamicToolOptions struct {
	currentIssueID                 string
	currentIssueIdentifier         string
	currentIssueRefresher          IssueStateRefresher
	currentIssueOperatorStopLookup OperatorTerminalStopLookup
}

func WithCurrentIssueToolGuard(issueID, issueIdentifier string, refresher IssueStateRefresher) DynamicToolOption {
	return func(opts *dynamicToolOptions) {
		opts.currentIssueID = strings.TrimSpace(issueID)
		opts.currentIssueIdentifier = strings.TrimSpace(issueIdentifier)
		opts.currentIssueRefresher = refresher
	}
}

func WithCurrentIssueOperatorTerminalStopLookup(lookup OperatorTerminalStopLookup) DynamicToolOption {
	return func(opts *dynamicToolOptions) {
		opts.currentIssueOperatorStopLookup = lookup
	}
}

// DynamicToolsForWorkflow builds the SPEC §10.5 client-side tool surface for
// the runner. linear_graphql is advertised only when Linear auth is configured;
// the token stays captured in this process and is never copied into tool
// metadata, agent environment, prompt text, or the GraphQL JSON payload.
func DynamicToolsForWorkflow(wf workflow.Workflow, toolOptions ...DynamicToolOption) DynamicToolSet {
	opts := dynamicToolOptions{}
	for _, apply := range toolOptions {
		if apply != nil {
			apply(&opts)
		}
	}
	tools := DynamicToolSet{tools: map[string]DynamicTool{}}
	trackerCfg := wf.Config.Tracker
	if strings.EqualFold(trackerCfg.Kind, "linear") && trackerCfg.APIKey != "" {
		client := linearGraphQLProxy{
			apiKey:           trackerCfg.APIKey,
			baseURL:          linearGraphQLEndpointFromConfig(trackerCfg),
			http:             http.DefaultClient,
			allowMutations:   wf.Config.Codex.LinearGraphQL.AllowMutations,
			allowedMutations: linearGraphQLAllowSet(wf.Config.Codex.LinearGraphQL.AllowedMutations),
		}
		if guard, ok := currentIssueGuardFromOptions(opts, trackerCfg); ok {
			client.currentIssueGuard = guard
		}
		tools.tools["linear_graphql"] = DynamicTool{
			Name:        "linear_graphql",
			Description: linearGraphQLToolDescription(client),
			InputSchema: linearGraphQLInputSchema(),
			Call:        client.call,
		}
		// The AI Workpad helper composes a fixed mutation around the
		// token-isolated proxy; it must skip the agent-visible gate
		// because the mutation text is harness-owned, not agent-supplied.
		// Passing a separate DynamicTool whose Call routes through the
		// ungated callRaw method keeps tests that hand-build mocks of the
		// linear_graphql tool unchanged while denying the agent any path
		// to reach callRaw directly.
		harnessTool := DynamicTool{
			Name: "linear_graphql",
			Call: client.callRaw,
		}
		tools.tools["linear_ai_workpad"] = NewLinearWorkpadTool(harnessTool)
	}
	giteaBaseURL := giteaBaseURLFromTracker(trackerCfg)
	if strings.EqualFold(trackerCfg.Kind, "gitea") && trackerCfg.APIKey != "" && wf.Config.Repo.Owner != "" && wf.Config.Repo.Name != "" && giteaBaseURL != "" {
		client := giteaIssueLabelsProxy{
			token:   trackerCfg.APIKey,
			baseURL: giteaBaseURL,
			owner:   wf.Config.Repo.Owner,
			repo:    wf.Config.Repo.Name,
			http:    http.DefaultClient,
		}.withCurrentIssueClassification(trackerCfg, opts)
		tools.tools["gitea_issue_labels"] = DynamicTool{
			Name:        "gitea_issue_labels",
			Description: "Replace the aiops/* state label on one Gitea issue using orchestrator-configured Gitea auth. Input: {issue_number:number, labels:string[], close_issue?:boolean} with exactly one aiops/* label. close_issue is accepted only for the current issue with a configured terminal state label, and closes that issue in the same tool call. The Gitea API token is never exposed to the agent process.",
			InputSchema: giteaIssueLabelsInputSchema(),
			Call:        client.call,
		}
	}
	return tools
}

func currentIssueGuardFromOptions(opts dynamicToolOptions, cfg workflow.TrackerConfig) (currentIssueMutationGuard, bool) {
	if opts.currentIssueID == "" || (opts.currentIssueRefresher == nil && opts.currentIssueOperatorStopLookup == nil) {
		return currentIssueMutationGuard{}, false
	}
	return currentIssueMutationGuard{
		issueID:                    opts.currentIssueID,
		issueIdentifier:            opts.currentIssueIdentifier,
		activeStates:               append([]string(nil), cfg.ActiveStates...),
		terminalStates:             append([]string(nil), cfg.TerminalStates...),
		teamKey:                    cfg.TeamKey,
		refresh:                    opts.currentIssueRefresher,
		operatorTerminalStopLookup: opts.currentIssueOperatorStopLookup,
		activeCache:                &workflowStateIDCache{},
		terminalCache:              &workflowStateIDCache{},
	}, true
}

func linearGraphQLInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "A single Linear GraphQL query or mutation.",
			},
			"variables": map[string]any{
				"type":                 "object",
				"description":          "Optional GraphQL variables object.",
				"additionalProperties": true,
			},
		},
		"required":             []string{"query"},
		"additionalProperties": false,
	}
}

func giteaIssueLabelsInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"issue_number": map[string]any{
				"type":        "integer",
				"description": "Gitea issue number whose aiops/* state labels should be replaced.",
			},
			"labels": map[string]any{
				"type":        "array",
				"description": "Complete desired aiops/* state label set for the issue; exactly one label is accepted.",
				"items":       map[string]any{"type": "string"},
				"minItems":    1,
				"maxItems":    1,
			},
			"close_issue": map[string]any{
				"type":        "boolean",
				"description": "When true, close the current Gitea issue after replacing it with a configured terminal aiops/* state label.",
			},
		},
		"required":             []string{"issue_number", "labels"},
		"additionalProperties": false,
	}
}

const maxLinearGraphQLResponseBytes = 1 << 20

// defaultDynamicToolHTTPTimeout bounds a single dynamic-tool HTTP round trip
// (linear_graphql, gitea_issue_labels). The surrounding agent-turn context is
// far larger, so without a tight per-request deadline a hung tracker endpoint
// would stall the whole turn — exactly the failure class the repo's "all
// external I/O is timeout-bounded" rule exists to prevent (#287/#405). A proxy
// may override via its httpTimeout field; zero means use this default.
const defaultDynamicToolHTTPTimeout = 30 * time.Second

func dynamicToolRequestTimeout(override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	return defaultDynamicToolHTTPTimeout
}

// maxToolErrorBodyRunes caps how much of a diagnostic response body (a non-2xx
// status or an unparseable success body) is surfaced in an agent-facing tool
// result. A valid success body is the agent's requested payload and is returned
// in full (still bounded by maxLinearGraphQLResponseBytes); a diagnostic body
// is truncated to keep a large error page from flooding agent context.
const maxToolErrorBodyRunes = 2048

// redactToolSecrets replaces every occurrence of each non-empty secret with a
// placeholder. EVERY response body placed in an agent-facing tool result —
// success, failure, or unparseable — passes through it, so a tracker server or
// a fronting proxy that echoes the Authorization value cannot leak the
// credential to the agent, which SPEC token isolation (#76/#298) forbids
// regardless of HTTP status. Real tracker tokens are long, high-entropy
// strings, so a match means an actual echo, not legitimate response content.
func redactToolSecrets(s string, secrets ...string) string {
	for _, secret := range secrets {
		if secret = strings.TrimSpace(secret); secret != "" {
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
	}
	return s
}

// sanitizeToolErrorBody redacts secrets and then truncates to
// maxToolErrorBodyRunes. It is for diagnostic bodies (non-2xx or unparseable)
// that are surfaced only to help the agent understand a failure, so unlike a
// success body they are bounded to keep a large error page from flooding agent
// context. Redaction runs before truncation so a token straddling the
// truncation boundary cannot leave an un-redacted head fragment behind.
func sanitizeToolErrorBody(body []byte, secrets ...string) string {
	s := redactToolSecrets(string(body), secrets...)
	if runes := []rune(s); len(runes) > maxToolErrorBodyRunes {
		return string(runes[:maxToolErrorBodyRunes]) + "…[truncated]"
	}
	return s
}

var errDynamicToolResponseTooLarge = errors.New("dynamic tool response exceeded maximum size")

// readDynamicToolResponseBody reads up to maxLinearGraphQLResponseBytes from r,
// returning errDynamicToolResponseTooLarge when the cap is exceeded so the
// caller can fail loud instead of silently truncating an oversized body into a
// tool result. Shared by the linear_graphql and gitea_issue_labels proxies so
// the cap is enforced in exactly one place.
func readDynamicToolResponseBody(r io.Reader) ([]byte, error) {
	var body bytes.Buffer
	n, err := body.ReadFrom(io.LimitReader(r, maxLinearGraphQLResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if n > maxLinearGraphQLResponseBytes {
		return nil, fmt.Errorf("%w: %d", errDynamicToolResponseTooLarge, maxLinearGraphQLResponseBytes)
	}
	return body.Bytes(), nil
}

func dynamicToolFailure(payload any) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(fmt.Sprintf(`{"error":{"message":"Linear GraphQL tool execution failed.","reason":%q}}`, err.Error()))
	}
	return dynamicToolResult(false, string(body))
}

func dynamicToolResult(success bool, output string) (string, error) {
	body, err := json.Marshal(dynamicToolResponse{
		Success: success,
		Output:  output,
		ContentItems: []dynamicToolContentItem{{
			Type: "inputText",
			Text: output,
		}},
	})
	if err != nil {
		return "", err
	}
	return string(body), nil
}
