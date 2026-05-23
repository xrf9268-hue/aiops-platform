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

// linearGraphQLOperationKind names the GraphQL top-level operation kinds
// the runner discriminates between when applying the SPEC §15.5 mutation
// gate. Subscriptions are recognized so the gate can reject them with a
// typed error instead of letting them slip past the read-only check (the
// runner has no streaming surface for them either).
type linearGraphQLOperationKind string

const (
	linearGraphQLOperationQuery        linearGraphQLOperationKind = "query"
	linearGraphQLOperationMutation     linearGraphQLOperationKind = "mutation"
	linearGraphQLOperationSubscription linearGraphQLOperationKind = "subscription"
)

// linearGraphQLOperation is the minimal parse the gate needs: the operation
// kind plus the first selected top-level field name (e.g. "issueDelete",
// "commentCreate"). FieldName is empty when the parser could not isolate a
// single top-level field; in that case the gate falls back to denying
// mutations with an "unidentified mutation" reason rather than guessing.
type linearGraphQLOperation struct {
	Kind      linearGraphQLOperationKind
	FieldName string
}

// linearGraphQLMutationSinkKey carries the audit-event callback through the
// linear_graphql tool call. handleDynamicToolCall installs the sink before
// invoking the tool; the proxy fires it once a mutation HTTP round-trip
// returns a non-error response. The key is unexported so only runner code
// can write or read it — agent-controlled paths cannot forge a bypass.
type linearGraphQLContextKey int

const linearGraphQLMutationSinkKey linearGraphQLContextKey = 1

// LinearGraphQLMutationSink is the audit callback invoked once per
// successful mutation dispatched through the agent-visible linear_graphql
// tool. Implementations must NOT echo the query body or variables — the
// design intent is operator visibility of WHICH mutation ran, not WHAT
// values were attached.
type LinearGraphQLMutationSink func(operationName string)

// WithLinearGraphQLMutationSink returns a context that the linear_graphql
// tool consults to surface successful mutations as audit events. The sink
// is invoked only after the HTTP response status is 2xx; transport failures
// and Linear-reported GraphQL errors do not fire the audit.
func WithLinearGraphQLMutationSink(ctx context.Context, sink LinearGraphQLMutationSink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, linearGraphQLMutationSinkKey, sink)
}

func linearGraphQLMutationSinkFrom(ctx context.Context) LinearGraphQLMutationSink {
	sink, _ := ctx.Value(linearGraphQLMutationSinkKey).(LinearGraphQLMutationSink)
	return sink
}

// ToolCall is the JSON-shaped input accepted by the dynamic linear_graphql
// tool. Query and Variables are agent-controlled. The Linear endpoint is held
// by the orchestrator-side proxy and is not part of this public call shape, so
// an agent cannot redirect the orchestrator-held token to another host.
type ToolCall struct {
	Query       string         `json:"query"`
	Variables   map[string]any `json:"variables,omitempty"`
	IssueNumber int            `json:"issue_number,omitempty"`
	Labels      []string       `json:"labels,omitempty"`
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

// DynamicToolsForWorkflow builds the SPEC §10.5 client-side tool surface for
// the runner. linear_graphql is advertised only when Linear auth is configured;
// the token stays captured in this process and is never copied into tool
// metadata, agent environment, prompt text, or the GraphQL JSON payload.
func DynamicToolsForWorkflow(wf workflow.Workflow) DynamicToolSet {
	tools := DynamicToolSet{tools: map[string]DynamicTool{}}
	trackerCfg := wf.Config.Tracker
	if strings.EqualFold(trackerCfg.Kind, "linear") && trackerCfg.APIKey != "" {
		client := linearGraphQLProxy{
			apiKey:           trackerCfg.APIKey,
			baseURL:          defaultLinearGraphQLEndpoint,
			http:             http.DefaultClient,
			allowMutations:   wf.Config.Codex.LinearGraphQL.AllowMutations,
			allowedMutations: linearGraphQLAllowSet(wf.Config.Codex.LinearGraphQL.AllowedMutations),
		}
		tools.tools["linear_graphql"] = DynamicTool{
			Name:        "linear_graphql",
			Description: linearGraphQLToolDescription(client),
			InputSchema: map[string]any{
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
			},
			Call: client.call,
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
	if strings.EqualFold(trackerCfg.Kind, "gitea") && trackerCfg.APIKey != "" && wf.Config.Repo.Owner != "" && wf.Config.Repo.Name != "" && giteaBaseURLFromTracker(trackerCfg) != "" {
		client := giteaIssueLabelsProxy{
			token:   trackerCfg.APIKey,
			baseURL: giteaBaseURLFromTracker(trackerCfg),
			owner:   wf.Config.Repo.Owner,
			repo:    wf.Config.Repo.Name,
			http:    http.DefaultClient,
		}
		tools.tools["gitea_issue_labels"] = DynamicTool{
			Name:        "gitea_issue_labels",
			Description: "Replace the aiops/* state label on one Gitea issue using orchestrator-configured Gitea auth. Input: {issue_number:number, labels:string[]} with exactly one aiops/* label. The Gitea API token is never exposed to the agent process.",
			InputSchema: map[string]any{
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
				},
				"required":             []string{"issue_number", "labels"},
				"additionalProperties": false,
			},
			Call: client.call,
		}
	}
	return tools
}

const maxLinearGraphQLResponseBytes = 1 << 20

var defaultLinearGraphQLEndpoint = "https://api.linear.app/graphql"

type linearGraphQLProxy struct {
	apiKey  string
	baseURL string
	http    *http.Client
	// allowMutations toggles the SPEC §15.5 harness narrowing (#298).
	// When false (the zero value), the proxy refuses every mutation the
	// agent submits before any HTTP request is built. Harness-internal
	// callers reach the underlying transport through callRaw, which
	// does not consult these fields.
	allowMutations bool
	// allowedMutations, when non-empty, narrows accepted mutations to
	// the listed top-level Mutation root field names (e.g.
	// "issueUpdate"). nil/empty means "any mutation" — only meaningful
	// while allowMutations is true.
	allowedMutations map[string]struct{}
}

// call is the gated entry point exposed to the agent through the
// linear_graphql dynamic tool. It parses the operation kind/name,
// applies the SPEC §15.5 narrowing, fires the optional audit sink, and
// otherwise delegates to callRaw. Subscription operations are rejected
// unconditionally because the runner has no streaming surface for them.
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
	op := parseLinearGraphQLOperation(query)
	switch op.Kind {
	case linearGraphQLOperationSubscription:
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{
				"message": "linear_graphql does not accept subscription operations; submit a query or mutation instead",
			},
		})
	case linearGraphQLOperationMutation:
		if !p.allowMutations {
			return dynamicToolFailure(map[string]any{
				"error": map[string]any{
					"message":          "linear_graphql mutations are disabled by this workflow; set codex.linear_graphql.allow_mutations: true in WORKFLOW.md to opt in (SPEC §15.5)",
					"operation":        string(op.Kind),
					"operation_field":  op.FieldName,
					"workflow_setting": "codex.linear_graphql.allow_mutations",
				},
			})
		}
		if len(p.allowedMutations) > 0 {
			if op.FieldName == "" {
				return dynamicToolFailure(map[string]any{
					"error": map[string]any{
						"message":          "linear_graphql could not identify the top-level mutation field; rewrite the mutation so its selection set starts with a single Mutation root field",
						"workflow_setting": "codex.linear_graphql.allowed_mutations",
					},
				})
			}
			if _, ok := p.allowedMutations[op.FieldName]; !ok {
				return dynamicToolFailure(map[string]any{
					"error": map[string]any{
						"message":          "linear_graphql mutation is not in the workflow's allowed_mutations list",
						"operation_field":  op.FieldName,
						"workflow_setting": "codex.linear_graphql.allowed_mutations",
					},
				})
			}
		}
	}

	result, err := p.callRaw(ctx, call)
	if err != nil {
		return result, err
	}
	if op.Kind == linearGraphQLOperationMutation && toolResultSucceeded(result) {
		if sink := linearGraphQLMutationSinkFrom(ctx); sink != nil {
			sink(op.FieldName)
		}
	}
	return result, nil
}

// callRaw is the harness-internal transport. It does NOT apply the
// SPEC §15.5 mutation gate and is therefore unreachable from
// agent-controlled paths. The linear_ai_workpad helper uses callRaw
// because its mutation text (commentCreate / commentUpdate) is
// composed deterministically by harness code, not by the agent.
func (p linearGraphQLProxy) callRaw(ctx context.Context, call ToolCall) (string, error) {
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
	req.Header.Set("Authorization", linearAuthorizationHeader(p.apiKey))

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

func linearAuthorizationHeader(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if strings.HasPrefix(strings.ToLower(apiKey), "bearer ") {
		return apiKey
	}
	return "Bearer " + apiKey
}

// parseLinearGraphQLOperation inspects the agent-supplied query string and
// returns the operation kind plus the first selected top-level field name.
// The parser is structural — it reuses the lexer state machine from
// countGraphQLOperations and stops at the first non-fragment operation
// header — so it is robust against the same comment/string/parameter
// shapes the count check already handles. It does NOT validate that the
// query is semantically well-formed Linear GraphQL; that is Linear's job
// once the request is dispatched.
func parseLinearGraphQLOperation(query string) linearGraphQLOperation {
	op := linearGraphQLOperation{Kind: linearGraphQLOperationQuery}
	depth := 0
	parenDepth := 0
	// expecting describes what the next `{` at depth 0 opens:
	//   "operation" — the body of the operation we just identified
	//   "fragment"  — the body of a fragment definition (skip it)
	//   ""          — neither yet; an unannounced `{` becomes a
	//                  shorthand query
	expecting := ""
	headerSelectionRoot := false
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
			if depth == 0 && parenDepth == 0 {
				switch expecting {
				case "operation":
					headerSelectionRoot = true
				case "fragment":
					// Entering a fragment body; not the operation
					// we are looking for. Leave headerSelectionRoot
					// false so its inner field name doesn't get
					// captured.
					headerSelectionRoot = false
				default:
					// Shorthand `{ ... }` query — no operation
					// keyword was ever encountered. Kind stays
					// query (the zero value).
					if op.FieldName == "" {
						headerSelectionRoot = true
					}
				}
				expecting = ""
			}
			depth++
			i++
			continue
		case '}':
			if depth > 0 {
				depth--
			}
			if depth == 0 {
				// Closing one top-level body resets the state so a
				// trailing `mutation { ... }` after a leading
				// `fragment F on T { id }` is still parsed.
				headerSelectionRoot = false
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
		case '\n', '\r', ' ', '\t', ',':
			i++
			continue
		case '$':
			i++
			for i < len(query) && isGraphQLNameContinue(query[i]) {
				i++
			}
			continue
		}

		if isGraphQLNameStart(ch) {
			start := i
			i++
			for i < len(query) && isGraphQLNameContinue(query[i]) {
				i++
			}
			name := query[start:i]
			switch {
			case depth == 0 && parenDepth == 0 && expecting == "":
				// Operation/fragment headers can appear here. Once an
				// operation has been identified (op.FieldName set
				// below), we stop overriding op.Kind so subsequent
				// operations don't shadow the one whose body we
				// already entered.
				switch name {
				case "query":
					if op.FieldName == "" {
						op.Kind = linearGraphQLOperationQuery
					}
					expecting = "operation"
				case "mutation":
					if op.FieldName == "" {
						op.Kind = linearGraphQLOperationMutation
					}
					expecting = "operation"
				case "subscription":
					if op.FieldName == "" {
						op.Kind = linearGraphQLOperationSubscription
					}
					expecting = "operation"
				case "fragment":
					expecting = "fragment"
				}
			case headerSelectionRoot && depth == 1 && parenDepth == 0:
				if op.FieldName == "" {
					op.FieldName = name
				}
			}
			continue
		}

		i++
	}
	return op
}

// linearGraphQLAllowSet hashes the operator-declared mutation allow-list
// for O(1) lookup. Returns nil for empty input so callers can distinguish
// "no restriction" from "explicit empty list" (the loader rejects the
// latter combination anyway, but the predicate stays cheap).
func linearGraphQLAllowSet(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		set[trimmed] = struct{}{}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// linearGraphQLToolDescription builds the tool description advertised to
// the agent. The description visibly reflects the workflow's narrowing so
// the agent knows up-front whether mutations are permitted instead of
// learning by trial and error.
func linearGraphQLToolDescription(p linearGraphQLProxy) string {
	base := "Execute a Linear GraphQL query using the orchestrator-configured Linear auth. Input: {query:string, variables?:object}. The Linear API token is never exposed to the agent process."
	switch {
	case !p.allowMutations:
		return base + " Mutations are disabled by this workflow; submit queries only."
	case len(p.allowedMutations) > 0:
		names := make([]string, 0, len(p.allowedMutations))
		for name := range p.allowedMutations {
			names = append(names, name)
		}
		sort.Strings(names)
		return base + " Mutations are restricted to: " + strings.Join(names, ", ") + "."
	default:
		return base + " Mutations are permitted."
	}
}

// toolResultSucceeded returns true when result is a dynamicToolResponse
// JSON envelope whose success field is true. Used by the gated call path
// to fire the audit sink only after Linear actually accepted the
// mutation, not on transport errors or Linear-reported GraphQL errors.
func toolResultSucceeded(result string) bool {
	var envelope dynamicToolResponse
	if err := json.Unmarshal([]byte(result), &envelope); err != nil {
		return false
	}
	return envelope.Success
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
			if depth == 0 && parenDepth == 0 {
				if !operationHeader {
					count++
				}
				operationHeader = false
			}
			depth++
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
				case "fragment":
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
