package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// ToolCall is the JSON-shaped input accepted by the dynamic linear_graphql
// tool. Query and Variables are agent-controlled. The Linear endpoint is held
// by the orchestrator-side proxy and is not part of this public call shape, so
// an agent cannot redirect the orchestrator-held token to another host.
type ToolCall struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// DynamicTool is a client-side tool implemented by the orchestrator and made
// available to an app-server-capable agent session. Tool metadata must never
// include raw tracker tokens; Call closes over orchestrator config instead.
type DynamicTool struct {
	Name        string
	Description string
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

// DynamicToolsForWorkflow builds the SPEC §10.5 client-side tool surface for
// the runner. linear_graphql is advertised only when Linear auth is configured;
// the token stays captured in this process and is never copied into tool
// metadata, agent environment, prompt text, or the GraphQL JSON payload.
func DynamicToolsForWorkflow(wf workflow.Workflow) DynamicToolSet {
	tools := DynamicToolSet{tools: map[string]DynamicTool{}}
	trackerCfg := wf.Config.Tracker
	if strings.EqualFold(trackerCfg.Kind, "linear") && trackerCfg.APIKey != "" {
		client := linearGraphQLProxy{
			apiKey:  trackerCfg.APIKey,
			baseURL: defaultLinearGraphQLEndpoint,
			http:    http.DefaultClient,
		}
		tools.tools["linear_graphql"] = DynamicTool{
			Name:        "linear_graphql",
			Description: "Execute a raw Linear GraphQL query or mutation using the orchestrator-configured Linear auth. Input: {query:string, variables?:object}. The Linear API token is never exposed to the agent process.",
			Call:        client.call,
		}
	}
	return tools
}

const (
	defaultLinearGraphQLEndpoint  = "https://api.linear.app/graphql"
	maxLinearGraphQLResponseBytes = 1 << 20
)

type linearGraphQLProxy struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

func (p linearGraphQLProxy) call(ctx context.Context, call ToolCall) (string, error) {
	query := strings.TrimSpace(call.Query)
	if query == "" {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{
				"message": "linear_graphql query is required",
			},
		})
	}
	if countGraphQLOperations(query) > 1 {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{
				"message": "linear_graphql query must contain exactly one operation",
			},
		})
	}
	endpoint := p.baseURL
	if endpoint == "" {
		endpoint = defaultLinearGraphQLEndpoint
	}

	payload := map[string]any{"query": query}
	if call.Variables != nil {
		payload["variables"] = call.Variables
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{
				"message": "`linear_graphql.variables` must be a JSON object that can be encoded as GraphQL variables.",
				"reason":  err.Error(),
			},
		})
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{
				"message": "Linear GraphQL request could not be built.",
				"reason":  err.Error(),
			},
		})
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", p.apiKey)

	httpClient := p.http
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{
				"message": "Linear GraphQL request failed during transport.",
				"reason":  err.Error(),
			},
		})
	}
	defer resp.Body.Close()
	var respBody bytes.Buffer
	n, err := respBody.ReadFrom(io.LimitReader(resp.Body, maxLinearGraphQLResponseBytes+1))
	if err != nil {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{
				"message": "Linear GraphQL response body could not be read.",
				"reason":  err.Error(),
			},
		})
	}
	if n > maxLinearGraphQLResponseBytes {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{
				"message": "Linear GraphQL response exceeded maximum size.",
				"limit":   maxLinearGraphQLResponseBytes,
			},
		})
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{
				"message": "Linear GraphQL request failed before receiving a successful response.",
				"status":  resp.Status,
				"body":    respBody.String(),
			},
		})
	}
	return linearGraphQLToolResponse(respBody.Bytes())
}

func countGraphQLOperations(query string) int {
	count := 0
	depth := 0
	parenDepth := 0
	operationHeader := false
	for i := 0; i < len(query); {
		ch := query[i]
		switch ch {
		case '#':
			for i < len(query) && query[i] != '\n' && query[i] != '\r' {
				i++
			}
			continue
		case '"':
			if strings.HasPrefix(query[i:], `"""`) {
				i += 3
				for i < len(query) && !strings.HasPrefix(query[i:], `"""`) {
					i++
				}
				if i < len(query) {
					i += 3
				}
				continue
			}
			i++
			for i < len(query) {
				if query[i] == '\\' {
					i += 2
					continue
				}
				if query[i] == '"' {
					i++
					break
				}
				i++
			}
			continue
		case '{':
			depth++
			operationHeader = false
			i++
			continue
		case '}':
			if depth > 0 {
				depth--
			}
			i++
			continue
		case '(':
			parenDepth++
			i++
			continue
		case ')':
			if parenDepth > 0 {
				parenDepth--
			}
			i++
			continue
		case '\n', '\r':
			i++
			continue
		case ' ', '\t', ',':
			i++
			continue
		case '$':
			i++
			for i < len(query) && isGraphQLNameContinue(query[i]) {
				i++
			}
			continue
		}

		if depth == 0 && parenDepth == 0 && isGraphQLNameStart(ch) {
			start := i
			i++
			for i < len(query) && isGraphQLNameContinue(query[i]) {
				i++
			}
			name := query[start:i]
			if !operationHeader {
				switch name {
				case "query", "mutation", "subscription":
					count++
					operationHeader = true
				}
			}
			continue
		}

		i++
	}
	return count
}

func isGraphQLNameStart(ch byte) bool {
	return ch == '_' || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z')
}

func isGraphQLNameContinue(ch byte) bool {
	return isGraphQLNameStart(ch) || (ch >= '0' && ch <= '9')
}

func linearGraphQLToolResponse(body []byte) (string, error) {
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{
				"message": "Linear GraphQL response was not valid JSON.",
				"reason":  err.Error(),
				"body":    string(body),
			},
		})
	}
	success := true
	if errors, ok := decoded["errors"].([]any); ok && len(errors) > 0 {
		success = false
	}
	return dynamicToolResult(success, string(body))
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
