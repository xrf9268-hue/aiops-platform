package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRuntimeStatusRunbookExampleMatchesHandler parses every ```json fenced
// block under docs/runbooks/runtime-status.md "## JSON shape" and asserts each
// example decodes cleanly into apiStateResponse with
// `json.DisallowUnknownFields`. A field added to the runbook that is not in
// the Go struct (or renamed without updating the doc) fails the build —
// matching the drift-detection requirement from #223.
//
// The test does not pin specific values; the live handler test
// (`TestStateHTTPHandlerReturnsRuntimeStateSnapshot`) covers value semantics.
// This test is purely about schema/key parity between runbook prose and the
// wire format.
func TestRuntimeStatusRunbookExampleMatchesHandler(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "runbooks", "runtime-status.md"))
	if err != nil {
		t.Fatalf("read runbook: %v", err)
	}
	blocks := extractFencedJSONBlocks(string(raw))
	if len(blocks) == 0 {
		t.Fatalf("no ```json blocks found in runbook")
	}

	var stateExampleBlocks int
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
		stateExampleBlocks++

		dec := json.NewDecoder(bytes.NewReader([]byte(block)))
		dec.DisallowUnknownFields()
		var into apiStateResponse
		if err := dec.Decode(&into); err != nil {
			t.Fatalf("runbook block %d failed strict decode into apiStateResponse: %v\n%s", i, err, block)
		}
	}
	if stateExampleBlocks == 0 {
		t.Fatalf("runbook has no /api/v1/state example block (no JSON block contains a `counts` key)")
	}
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
