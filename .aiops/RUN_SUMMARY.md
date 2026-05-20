# Run Summary

## Issue

Resolved GitHub issue #151: Linear workflows could omit `tracker.project_slug` and only fail later when the Linear client tried to poll candidate issues.

## Changes

- Added load-time validation for single-service Linear workflows: `tracker.kind: linear` now requires `tracker.project_slug` unless the workflow uses service-specific Linear routing.
- Added a focused loader regression test covering the missing `tracker.project_slug` error and its operator-facing field/path guidance.
- Updated valid Linear test fixtures to include `project_slug`.
- Documented the Linear `tracker.project_slug` requirement in `README.md`, `docs/runbooks/local-dev.md`, and `examples/WORKFLOW.md`.

## Verification

- `go test ./internal/workflow -run TestLoadRejectsLinearWorkflowWithoutProjectSlug -count=1`
- `go test ./internal/workflow ./cmd/worker -count=1`
- `go test ./internal/orchestrator ./internal/worker ./internal/workflow ./cmd/worker -count=1`
- `gofmt -l $(git ls-files '*.go')`
- `go mod tidy && git diff --exit-code -- go.mod go.sum`
- `go build ./cmd/worker ./cmd/linear-poller ./cmd/gitea-poller`
- `go test -race -covermode=atomic $(go list ./... | grep -v '^github.com/xrf9268-hue/aiops-platform/internal/runner$')`
- Codex JSON local review: `{"blocking_findings":[]}`
- Claude Code JSON local review: `{"blocking_findings":[]}`

`go test -race -covermode=atomic ./...` was run. It still fails in the unrelated `internal/runner` package on this macOS host due to the existing Darwin sandbox test expectations for bubblewrap/firejail. The packages touched by this change and the non-runner package set passed with the targeted commands above.
