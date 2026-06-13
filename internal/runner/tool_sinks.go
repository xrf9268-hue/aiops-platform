package runner

import (
	"context"
	"strings"
)

// toolMutationSinkKey carries the audit-event callback through tracker
// mutation tool calls (linear_graphql, linear_ai_workpad, gitea_issue_labels).
// handleDynamicToolCall installs the sink before invoking the tool; the proxy
// fires it once a mutation round-trip succeeds. The key is unexported so only
// runner code can write or read it — agent-controlled paths cannot forge a
// bypass.
type dynamicToolContextKey int

const (
	toolMutationSinkKey dynamicToolContextKey = iota + 1
	linearGraphQLMutationRejectedSinkKey
	linearGraphQLPostStopMutationSinkKey
	linearGraphQLCurrentIssueHandoffKey
	linearGraphQLCurrentIssueTerminalHandoffKey
)

// ToolMutationAudit is the non-secret audit fact emitted once a tracker
// mutation succeeds — by the linear_graphql/linear_ai_workpad proxy and the
// gitea_issue_labels proxy alike, so both trackers share one handoff
// classification taxonomy (#748). It deliberately excludes query text,
// variables, and state IDs; current-issue handoff fields are parsed
// classifications only.
type ToolMutationAudit struct {
	OperationField                   string
	CurrentIssueNonActiveStateUpdate bool
	CurrentIssueTerminalStateUpdate  bool
	CurrentIssueTerminalState        string
}

// ToolMutationSink is the audit callback invoked once per successful
// mutation dispatched through an agent-visible tracker mutation tool
// (linear_graphql, linear_ai_workpad, gitea_issue_labels). Implementations
// must NOT echo the query body or variables — the design intent is operator
// visibility of WHICH mutation ran, not WHAT values were attached.
type ToolMutationSink func(ToolMutationAudit)

type linearGraphQLMutationFieldSink func(operationName string)

type linearGraphQLMutationRejected struct {
	OperationField string `json:"operation_field,omitempty"`
	Reason         string `json:"reason"`
	Found          bool   `json:"found"`
	State          string `json:"state,omitempty"`
	Terminal       bool   `json:"terminal"`
}

type linearGraphQLMutationRejectedSink func(linearGraphQLMutationRejected)

// WithToolMutationSink returns a context that the tracker mutation proxies
// consult to surface successful mutations as audit events. The sink is
// invoked only after the mutation actually succeeded; transport failures,
// Linear-reported GraphQL errors, and partial Gitea label replaces do not
// fire the audit.
func WithToolMutationSink(ctx context.Context, sink ToolMutationSink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, toolMutationSinkKey, sink)
}

func toolMutationSinkFrom(ctx context.Context) ToolMutationSink {
	sink, _ := ctx.Value(toolMutationSinkKey).(ToolMutationSink)
	return sink
}

func WithLinearGraphQLMutationRejectedSink(ctx context.Context, sink linearGraphQLMutationRejectedSink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, linearGraphQLMutationRejectedSinkKey, sink)
}

func linearGraphQLMutationRejectedSinkFrom(ctx context.Context) linearGraphQLMutationRejectedSink {
	sink, _ := ctx.Value(linearGraphQLMutationRejectedSinkKey).(linearGraphQLMutationRejectedSink)
	return sink
}

func WithLinearGraphQLPostStopMutationSink(ctx context.Context, sink linearGraphQLMutationFieldSink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, linearGraphQLPostStopMutationSinkKey, sink)
}

func linearGraphQLPostStopMutationSinkFrom(ctx context.Context) linearGraphQLMutationFieldSink {
	sink, _ := ctx.Value(linearGraphQLPostStopMutationSinkKey).(linearGraphQLMutationFieldSink)
	return sink
}

func withLinearGraphQLCurrentIssueHandoff(ctx context.Context, handoff currentIssueHandoffClassification) context.Context {
	ctx = context.WithValue(ctx, linearGraphQLCurrentIssueHandoffKey, true)
	if state := strings.TrimSpace(handoff.terminalState); state != "" {
		ctx = context.WithValue(ctx, linearGraphQLCurrentIssueTerminalHandoffKey, state)
	}
	return ctx
}

func linearGraphQLCurrentIssueHandoffFrom(ctx context.Context) bool {
	v, _ := ctx.Value(linearGraphQLCurrentIssueHandoffKey).(bool)
	return v
}

func linearGraphQLCurrentIssueTerminalHandoffFrom(ctx context.Context) bool {
	return linearGraphQLCurrentIssueTerminalHandoffStateFrom(ctx) != ""
}

func linearGraphQLCurrentIssueTerminalHandoffStateFrom(ctx context.Context) string {
	v, _ := ctx.Value(linearGraphQLCurrentIssueTerminalHandoffKey).(string)
	return strings.TrimSpace(v)
}
