package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	linearWorkpadToolName       = "linear_ai_workpad"
	linearWorkpadMarker         = "<!-- aiops:ai-workpad -->"
	linearWorkpadMaxLookupPages = 100
)

// NewLinearWorkpadTool returns a thin helper over linear_graphql. The helper is
// still agent-side tracker interaction: it only composes deterministic Linear
// GraphQL calls through the token-isolated linear_graphql tool instead of
// adding worker-owned Linear comment writes.
func NewLinearWorkpadTool(linearGraphQL DynamicTool) DynamicTool {
	return DynamicTool{
		Name:        linearWorkpadToolName,
		Description: "Find, create, or update the single AI Workpad Linear comment for an issue by calling the token-isolated linear_graphql tool. Input: {variables:{issueId:string, branch?:string, prUrl?:string, summary?:string, blocker?:string, next?:string}}. No Linear token is exposed.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"variables": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"issueId": map[string]any{"type": "string", "description": "Linear issue identifier or ID to attach the AI Workpad comment to."},
						"branch":  map[string]any{"type": "string", "description": "Current branch name, when known."},
						"prUrl":   map[string]any{"type": "string", "description": "Pull request URL, when known."},
						"summary": map[string]any{"type": "string", "description": "Current run summary or handoff note."},
						"blocker": map[string]any{"type": "string", "description": "Last error or blocker, when any."},
						"next":    map[string]any{"type": "string", "description": "Next action, when known."},
					},
					"required":             []string{"issueId"},
					"additionalProperties": false,
				},
			},
			"required":             []string{"variables"},
			"additionalProperties": false,
		},
		Call: linearWorkpadProxy{linearGraphQL: linearGraphQL}.call,
	}
}

type linearWorkpadProxy struct {
	linearGraphQL DynamicTool
}

type linearWorkpadInput struct {
	IssueID string
	Branch  string
	PRURL   string
	Summary string
	Blocker string
	Next    string
}

type linearToolPayload struct {
	Success bool   `json:"success"`
	Output  string `json:"output"`
}

func (p linearWorkpadProxy) call(ctx context.Context, call ToolCall) (string, error) {
	input := normalizeLinearWorkpadInput(call)
	if strings.TrimSpace(input.IssueID) == "" {
		return dynamicToolFailure(map[string]any{"error": map[string]any{"message": "linear_ai_workpad variables.issueId is required"}})
	}
	if p.linearGraphQL.Call == nil {
		return dynamicToolFailure(map[string]any{"error": map[string]any{"message": "linear_ai_workpad requires the linear_graphql dynamic tool"}})
	}

	commentID, err := p.findLinearWorkpadCommentID(ctx, input.IssueID)
	if err != nil {
		return dynamicToolFailure(map[string]any{"error": map[string]any{"message": "AI Workpad lookup failed", "reason": err.Error()}})
	}

	body := renderLinearWorkpadBody(input)
	var mutation string
	variables := map[string]any{"body": body}
	if commentID != "" {
		mutation = `mutation AIWorkpadUpdate($commentId: String!, $body: String!) {
  commentUpdate(id: $commentId, input: { body: $body }) {
    success
    comment { id }
  }
}`
		variables["commentId"] = commentID
	} else {
		mutation = `mutation AIWorkpadCreate($issueId: String!, $body: String!) {
  commentCreate(input: { issueId: $issueId, body: $body }) {
    success
    comment { id }
  }
}`
		variables["issueId"] = input.IssueID
	}

	mutateResult, err := p.linearGraphQL.Call(ctx, ToolCall{Query: mutation, Variables: variables})
	if err != nil {
		return dynamicToolFailure(map[string]any{"error": map[string]any{"message": "AI Workpad mutation failed", "reason": err.Error()}})
	}
	mutateOutput, ok := decodeSuccessfulToolOutput(mutateResult)
	if !ok {
		return dynamicToolFailure(map[string]any{"error": map[string]any{"message": "AI Workpad mutation returned a failure", "output": mutateResult}})
	}
	if ok, err := linearWorkpadMutationSucceeded(mutateOutput); err != nil {
		return dynamicToolFailure(map[string]any{"error": map[string]any{"message": "AI Workpad mutation response could not be decoded", "reason": err.Error()}})
	} else if !ok {
		return dynamicToolFailure(map[string]any{"error": map[string]any{"message": "AI Workpad mutation did not succeed", "output": mutateOutput}})
	}
	return dynamicToolResult(true, mutateOutput)
}

func (p linearWorkpadProxy) findLinearWorkpadCommentID(ctx context.Context, issueID string) (string, error) {
	var after string
	for pageCount := 0; pageCount < linearWorkpadMaxLookupPages; pageCount++ {
		variables := map[string]any{"issueId": issueID}
		if after != "" {
			variables["after"] = after
		}
		findResult, err := p.linearGraphQL.Call(ctx, ToolCall{
			Query: `query AIWorkpadFind($issueId: String!, $after: String) {
  issue(id: $issueId) {
    comments(first: 50, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes { id body }
    }
  }
}`,
			Variables: variables,
		})
		if err != nil {
			return "", err
		}
		findOutput, ok := decodeSuccessfulToolOutput(findResult)
		if !ok {
			return "", fmt.Errorf("lookup returned a failure: %s", findResult)
		}

		page, err := decodeLinearWorkpadCommentPage(findOutput)
		if err != nil {
			return "", fmt.Errorf("lookup response could not be decoded: %w", err)
		}
		for _, node := range page.Nodes {
			if strings.Contains(node.Body, linearWorkpadMarker) {
				return node.ID, nil
			}
		}
		if !page.HasNextPage || strings.TrimSpace(page.EndCursor) == "" {
			return "", nil
		}
		nextCursor := strings.TrimSpace(page.EndCursor)
		if nextCursor == after {
			return "", fmt.Errorf("lookup pagination did not advance after cursor %q", nextCursor)
		}
		after = nextCursor
	}
	return "", fmt.Errorf("lookup exceeded %d pages", linearWorkpadMaxLookupPages)
}

func normalizeLinearWorkpadInput(call ToolCall) linearWorkpadInput {
	vars := call.Variables
	if nested, ok := vars["variables"].(map[string]any); ok {
		vars = nested
	}
	return linearWorkpadInput{
		IssueID: stringVariable(vars, "issueId"),
		Branch:  stringVariable(vars, "branch"),
		PRURL:   stringVariable(vars, "prUrl"),
		Summary: stringVariable(vars, "summary"),
		Blocker: stringVariable(vars, "blocker"),
		Next:    stringVariable(vars, "next"),
	}
}

func stringVariable(vars map[string]any, name string) string {
	if vars == nil {
		return ""
	}
	value, _ := vars[name].(string)
	return strings.TrimSpace(value)
}

func decodeSuccessfulToolOutput(result string) (string, bool) {
	var payload linearToolPayload
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		return "", false
	}
	return payload.Output, payload.Success
}

func linearWorkpadMutationSucceeded(output string) (bool, error) {
	var payload struct {
		Data struct {
			CommentCreate *struct {
				Success bool `json:"success"`
			} `json:"commentCreate"`
			CommentUpdate *struct {
				Success bool `json:"success"`
			} `json:"commentUpdate"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		return false, err
	}
	if payload.Data.CommentCreate != nil {
		return payload.Data.CommentCreate.Success, nil
	}
	if payload.Data.CommentUpdate != nil {
		return payload.Data.CommentUpdate.Success, nil
	}
	return false, nil
}

type linearWorkpadCommentPage struct {
	HasNextPage bool
	EndCursor   string
	Nodes       []struct {
		ID   string `json:"id"`
		Body string `json:"body"`
	}
}

func decodeLinearWorkpadCommentPage(output string) (linearWorkpadCommentPage, error) {
	var payload struct {
		Data struct {
			Issue struct {
				Comments struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						ID   string `json:"id"`
						Body string `json:"body"`
					} `json:"nodes"`
				} `json:"comments"`
			} `json:"issue"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		return linearWorkpadCommentPage{}, err
	}
	return linearWorkpadCommentPage{
		HasNextPage: payload.Data.Issue.Comments.PageInfo.HasNextPage,
		EndCursor:   payload.Data.Issue.Comments.PageInfo.EndCursor,
		Nodes:       payload.Data.Issue.Comments.Nodes,
	}, nil
}

func renderLinearWorkpadBody(input linearWorkpadInput) string {
	return fmt.Sprintf(`%s
# AI Workpad

- Current branch: %s
- Pull request: %s
- Run summary: %s
- Last error/blocker: %s
- Next action: %s
`, linearWorkpadMarker, workpadValue(input.Branch), workpadValue(input.PRURL), workpadValue(input.Summary), workpadValue(input.Blocker), workpadValue(input.Next))
}

func workpadValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}
