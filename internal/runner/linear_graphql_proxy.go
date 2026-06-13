package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

const defaultLinearGraphQLEndpoint = tracker.DefaultLinearEndpoint

type linearGraphQLProxy struct {
	apiKey  string
	baseURL string
	http    *http.Client
	// httpTimeout overrides the per-request deadline applied to each Linear
	// GraphQL round trip. Zero uses defaultDynamicToolHTTPTimeout; tests set a
	// tiny value to exercise the timeout path.
	httpTimeout time.Duration
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
	allowedMutations  map[string]struct{}
	currentIssueGuard currentIssueMutationGuard
}

// call is the gated entry point exposed to the agent through the
// linear_graphql dynamic tool. It parses the operation kind/name,
// applies the SPEC §15.5 narrowing, fires the optional audit sink, and
// otherwise delegates to callRaw. Subscription operations are rejected
// unconditionally because the runner has no streaming surface for them.
func (p linearGraphQLProxy) call(ctx context.Context, call ToolCall) (string, error) { //nolint:gocognit // baseline (#521)
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
	// The gate keys on the GraphQL operation keyword (`query` /
	// `mutation` / `subscription`), not on the selected root field's
	// type. `query Q { issueDelete(id: "1") { success } }` therefore
	// reaches Linear over HTTP; Linear's server-side schema rejects it
	// because `issueDelete` is a Mutation root field, not a Query root
	// field, so the destructive operation is never executed. Maintaining
	// a client-side denylist of Linear mutation field names would drift
	// every time Linear adds a mutation; we rely on Linear's type system
	// for that defense layer instead. The Authorization header still
	// carries the workspace token on the rejected request, which is
	// broadly comparable to the surface ordinary read queries already
	// expose — but two soft signals differ: Linear records the rejected
	// request in its server-side audit log (an attempted-write signal
	// for operators watching Linear directly), and repeated rejections
	// could plausibly contribute to Linear-side rate limiting on the
	// token. Neither alters the destructive-operation guarantee.
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
		rejection, reject, currentIssueHandoff := p.checkCurrentIssueUpdate(ctx, query, call.Variables)
		if reject {
			if sink := linearGraphQLMutationRejectedSinkFrom(ctx); sink != nil {
				sink(rejection)
			}
			return dynamicToolFailure(map[string]any{
				"error": map[string]any{
					"message":         currentIssueMutationRejectMessage(rejection.Reason),
					"operation_field": rejection.OperationField,
					"reason":          rejection.Reason,
					"found":           rejection.Found,
					"state":           rejection.State,
					"terminal":        rejection.Terminal,
				},
			})
		}
		if currentIssueHandoff.nonActive {
			ctx = withLinearGraphQLCurrentIssueHandoff(ctx, currentIssueHandoff)
		}
	}

	return p.dispatch(ctx, query, op, call.Variables)
}

// callRaw is the harness-internal transport. It does NOT apply the
// SPEC §15.5 mutation gate and is therefore unreachable from
// agent-controlled paths. The linear_ai_workpad helper uses callRaw
// because its mutation text (commentCreate / commentUpdate) is
// composed deterministically by harness code, not by the agent.
//
// Auditing is universal: every successful mutation dispatched through
// dispatch — whether reached via the gated `call` (agent-driven) or
// directly from a harness component like linear_ai_workpad — fires the
// audit sink when one is installed in ctx. The audit event therefore
// captures harness-attributable Linear writes the same way it captures
// agent-driven ones; the per-tool name carried in the runtime-event
// payload disambiguates them for operators (#298).
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
	return p.dispatch(ctx, query, parseLinearGraphQLOperation(query), call.Variables)
}

// dispatch is the shared transport for the gated and ungated entry
// points. Both `call` and `callRaw` parse the operation once and hand
// the result here so the parser does not run twice per request. The
// audit-sink fire lives here so harness-driven and agent-driven
// mutations share a single source of truth.
func (p linearGraphQLProxy) dispatch(ctx context.Context, query string, op linearGraphQLOperation, variables map[string]any) (string, error) { //nolint:gocognit // baseline (#521)
	body, err := linearGraphQLRequestBody(query, variables)
	if err != nil {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{
				"message": "`linear_graphql.variables` must be a JSON object that can be encoded as GraphQL variables.",
				"reason":  err.Error(),
			},
		})
	}

	reqCtx, cancel := context.WithTimeout(ctx, dynamicToolRequestTimeout(p.httpTimeout))
	defer cancel()

	req, err := p.linearGraphQLRequest(reqCtx, body)
	if err != nil {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{
				"message": "Linear GraphQL request could not be built.",
				"reason":  err.Error(),
			},
		})
	}

	resp, err := p.linearHTTPClient().Do(req)
	if err != nil {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{
				"message": "Linear GraphQL request failed during transport.",
				"reason":  err.Error(),
			},
		})
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := readDynamicToolResponseBody(resp.Body)
	if err != nil {
		if errors.Is(err, errDynamicToolResponseTooLarge) {
			return dynamicToolFailure(map[string]any{
				"error": map[string]any{
					"message": "Linear GraphQL response exceeded maximum size.",
					"limit":   maxLinearGraphQLResponseBytes,
				},
			})
		}
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{
				"message": "Linear GraphQL response body could not be read.",
				"reason":  err.Error(),
			},
		})
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{
				"message": "Linear GraphQL request failed before receiving a successful response.",
				"status":  resp.Status,
				"body":    sanitizeToolErrorBody(respBody, p.apiKey),
			},
		})
	}
	result, toolErr := linearGraphQLToolResponse(respBody, p.apiKey)
	if toolErr == nil && op.Kind == linearGraphQLOperationMutation && toolResultSucceeded(result) {
		if p.currentIssueGuard.postOperatorTerminalStop(ctx) {
			if sink := linearGraphQLPostStopMutationSinkFrom(ctx); sink != nil {
				sink(op.FieldName)
			}
		} else if sink := toolMutationSinkFrom(ctx); sink != nil {
			sink(ToolMutationAudit{
				OperationField:                   op.FieldName,
				CurrentIssueNonActiveStateUpdate: linearGraphQLCurrentIssueHandoffFrom(ctx),
				CurrentIssueTerminalStateUpdate:  linearGraphQLCurrentIssueTerminalHandoffFrom(ctx),
				CurrentIssueTerminalState:        linearGraphQLCurrentIssueTerminalHandoffStateFrom(ctx),
			})
		}
	}
	return result, toolErr
}

func linearGraphQLRequestBody(query string, variables map[string]any) ([]byte, error) {
	payload := map[string]any{"query": query}
	if variables != nil {
		payload["variables"] = variables
	}
	return json.Marshal(payload)
}

func (p linearGraphQLProxy) linearGraphQLRequest(ctx context.Context, body []byte) (*http.Request, error) {
	endpoint := linearGraphQLEndpoint(p.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", linearAuthorizationHeader(p.apiKey))
	return req, nil
}

func linearGraphQLEndpointFromConfig(cfg workflow.TrackerConfig) string {
	return linearGraphQLEndpoint(cfg.Endpoint)
}

func linearGraphQLEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return defaultLinearGraphQLEndpoint
	}
	return endpoint
}

func (p linearGraphQLProxy) linearHTTPClient() *http.Client {
	if p.http != nil {
		return p.http
	}
	return http.DefaultClient
}

func linearAuthorizationHeader(apiKey string) string {
	return strings.TrimSpace(apiKey)
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

// linearGraphQLToolResponse wraps a 2xx Linear body into a tool result. secret
// is the workspace token: it is redacted from every body surfaced to the agent
// — the valid-JSON success/GraphQL-errors payload and the unparseable-body
// diagnostic alike — so a proxy that echoes the Authorization value in a 200
// response cannot leak the credential past the token-isolation boundary
// (#76/#298). The unparseable body is also truncated; a success body is the
// agent's requested payload and is preserved in full (still bounded by the
// readDynamicToolResponseBody size cap).
func linearGraphQLToolResponse(body []byte, secret string) (string, error) {
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return dynamicToolFailure(map[string]any{
			"error": map[string]any{
				"message": "Linear GraphQL response was not valid JSON.",
				"reason":  err.Error(),
				"body":    sanitizeToolErrorBody(body, secret),
			},
		})
	}
	success := true
	if errors, ok := decoded["errors"].([]any); ok && len(errors) > 0 {
		success = false
	}
	return dynamicToolResult(success, redactToolSecrets(string(body), secret))
}
