package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/orchestrator"
)

// TestRuntimeStatusRunbookExampleMatchesHandler asserts bi-directional schema
// parity between docs/runbooks/runtime-status.md and apiStateResponse:
//
//  1. doc → handler: every ```json fenced block tagged as the /api/v1/state
//     example (identified by the `counts` key) strict-decodes into
//     apiStateResponse with json.DisallowUnknownFields. Catches drift where
//     the doc references a field that does not exist in the wire format.
//
//  2. handler → doc: marshal a fully-populated apiStateResponse (every
//     omitempty field set), then walk the resulting key tree and assert each
//     key path exists in the runbook example. Catches drift where the
//     handler grows a new JSON field that the runbook forgets to mention.
//
// Together these close the "either direction" parity guarantee for #223.
// The test does not pin specific values; live handler value semantics live
// in TestStateHTTPHandlerReturnsRuntimeStateSnapshot.
func TestRuntimeStatusRunbookExampleMatchesHandler(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "runbooks", "runtime-status.md"))
	if err != nil {
		t.Fatalf("read runbook: %v", err)
	}
	blocks := extractFencedJSONBlocks(string(raw))
	if len(blocks) == 0 {
		t.Fatalf("no ```json blocks found in runbook")
	}

	var stateExample map[string]any
	for i, block := range blocks {
		var probe map[string]any
		if err := json.Unmarshal([]byte(block), &probe); err != nil {
			t.Fatalf("runbook block %d is not valid JSON: %v\n%s", i, err, block)
		}
		// Skip /api/v1/refresh and other endpoint blocks. We only drift-check
		// the /api/v1/state shape, identified by presence of `counts`.
		if _, ok := probe["counts"]; !ok {
			continue
		}

		dec := json.NewDecoder(bytes.NewReader([]byte(block)))
		dec.DisallowUnknownFields()
		var into apiStateResponse
		if err := dec.Decode(&into); err != nil {
			t.Fatalf("runbook block %d failed strict decode into apiStateResponse: %v\n%s", i, err, block)
		}
		stateExample = probe
		break
	}
	if stateExample == nil {
		t.Fatalf("runbook has no /api/v1/state example block (no JSON block contains a `counts` key)")
	}

	handlerKeys := flattenJSONKeys(t, fullyPopulatedAPIStateResponseJSON(t))
	docKeys := flattenJSONKeys(t, mustMarshal(t, stateExample))
	var missing []string
	for key := range handlerKeys {
		if _, ok := docKeys[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("apiStateResponse fields not documented in docs/runbooks/runtime-status.md /api/v1/state example:\n  %s\nUpdate the runbook to mention these keys, or remove the field from apiStateResponse if intentional.", strings.Join(missing, "\n  "))
	}
}

// fullyPopulatedAPIStateResponseJSON returns the JSON encoding of an
// apiStateResponse with every field — including every `omitempty` field —
// populated, so json.Marshal cannot omit anything. Used to enumerate the
// complete handler-side schema for drift detection.
func fullyPopulatedAPIStateResponseJSON(t *testing.T) []byte {
	t.Helper()
	startedAt := time.Date(2026, 5, 21, 9, 9, 55, 0, time.UTC)
	blockedAt := time.Date(2026, 5, 20, 6, 5, 38, 0, time.UTC)
	lastCodexAt := time.Date(2026, 5, 21, 9, 10, 0, 0, time.UTC)
	dueAt := time.Date(2026, 5, 21, 9, 11, 0, 0, time.UTC)
	retryAttempt := 1
	resp := apiStateResponse{
		GeneratedAt:                time.Date(2026, 5, 21, 9, 10, 0, 0, time.UTC),
		PollIntervalMs:             15000,
		MaxConcurrentAgents:        4,
		MaxConcurrentAgentsByState: map[string]int{"In Progress": 2},
		Counts: apiStateCounts{
			Running:        1,
			Blocked:        1,
			Retrying:       1,
			Completed:      0,
			Failed:         0,
			CompletedTotal: 12,
			FailedTotal:    3,
		},
		Running: []apiStateRunning{{
			IssueID:           "issue-1",
			Identifier:        "ENG-1",
			StartedAt:         &startedAt,
			RetryAttempt:      &retryAttempt,
			WorkspacePath:     "/var/aiops/workspaces/acme/repo/issue-1",
			LastCodexAt:       &lastCodexAt,
			CodexAppServerPID: 12345,
		}},
		Blocked: []apiStateBlocked{{
			IssueID:           "issue-2",
			Identifier:        "ENG-2",
			State:             "AI Ready",
			BlockedAt:         &blockedAt,
			WorkspacePath:     "/var/aiops/workspaces/acme/repo/issue-2",
			SessionID:         "thread-1-turn-1",
			LastCodexAt:       &lastCodexAt,
			Method:            "mcpServer/elicitation/request",
			Error:             "input required: mcpServer/elicitation/request",
			CodexAppServerPID: 67890,
		}},
		Retrying: []apiStateRetry{{
			IssueID:    "issue-3",
			Identifier: "ENG-3",
			Attempt:    2,
			DueAt:      &dueAt,
			Error:      "retry soon",
		}},
		Completed: []orchestrator.IssueID{"issue-4"},
		Failed:    []orchestrator.IssueID{"issue-5"},
		CodexTotals: apiCodexTotals{
			InputTokens:    100,
			OutputTokens:   200,
			TotalTokens:    300,
			SecondsRunning: 1.5,
		},
		RateLimits: &orchestrator.RateLimitSnapshot{},
	}
	return mustMarshal(t, resp)
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

// flattenJSONKeys decodes raw JSON and returns the set of dotted key paths
// present in it. Object keys are joined with `.`; arrays are walked into
// their first element so per-row keys (e.g. running[].issue_id) appear as
// running.issue_id without an index. Scalar leaves are not added — only
// keys.
func flattenJSONKeys(t *testing.T, raw []byte) map[string]struct{} {
	t.Helper()
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("unmarshal for key flatten: %v\n%s", err, raw)
	}
	out := map[string]struct{}{}
	var walk func(prefix string, node any)
	walk = func(prefix string, node any) {
		switch v := node.(type) {
		case map[string]any:
			for k, child := range v {
				path := k
				if prefix != "" {
					path = prefix + "." + k
				}
				out[path] = struct{}{}
				walk(path, child)
			}
		case []any:
			if len(v) > 0 {
				walk(prefix, v[0])
			}
		}
	}
	walk("", root)
	return out
}

// extractFencedJSONBlocks returns the contents of each ```json fence in src.
// Closing fence is detected as a line consisting only of ``` (possibly
// preceded by whitespace).
func extractFencedJSONBlocks(src string) []string {
	var out []string
	lines := strings.Split(src, "\n")
	var buf strings.Builder
	inJSON := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if !inJSON {
			if trim == "```json" {
				inJSON = true
				buf.Reset()
			}
			continue
		}
		if trim == "```" {
			out = append(out, buf.String())
			inJSON = false
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	return out
}
