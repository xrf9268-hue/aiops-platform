# RUN_SUMMARY for Issue #127

## What changed
- Removed dead code `replaceAIOpsLabels` from `internal/runner/gitea_tools.go`.
- Kept the active label update path intact (`giteaIssueLabelsProxy.call` continues to use incremental add/delete semantics via `addIssueLabels`, `currentIssueLabels`, `deleteIssueLabel`, and helper checks).

## Why
- Issue #127 identified `replaceAIOpsLabels` as an unreachable leftover from an earlier full-replacement label strategy and requested either deletion or explicit reintegration.
- No call sites reference it anywhere in the tree, so keeping it added unnecessary, security-adjacent audit surface for a token-bearing path without behavior.

## Verification
### Focused checks
- `go test ./internal/runner -run 'TestGiteaIssueLabels' -count=1`
  - Passed.

### Required project commands
- `gofmt -l $(git ls-files '*.go')`
  - No output.
- `go mod tidy && git diff --exit-code -- go.mod go.sum`
  - Passed.
- `go test -race -covermode=atomic ./...`
  - Failed in this environment due existing unrelated baseline issues (e.g., `internal/runner` sandbox/timeout tests, plus a pre-existing `cmd/worker` path-resolution expectation mismatch).
- `go build ./cmd/worker ./cmd/linear-poller ./cmd/gitea-poller`
  - Passed.

### Local review gates
- Codex review (`codex exec --ephemeral ...`) returned: `{"blocking_findings":[]}`.
- Claude review (`claude -p ...`) returned JSON with `.structured_output.blocking_findings = []`.
