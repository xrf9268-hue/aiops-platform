# Run Summary

## Issue

Resolved GitHub issue #151: Linear workflows that omit `tracker.project_slug` should fail during workflow loading instead of failing later on the first Linear poll.

## Changes

- Added loader validation for single-service `tracker.kind: linear` workflows without `tracker.project_slug`.
- Added a focused workflow loader regression test for the missing project slug case.
- Updated existing Linear workflow test fixtures to include `project_slug` where they are not testing missing-field validation.
- Documented the Linear `tracker.project_slug` requirement in `README.md`, `docs/runbooks/local-dev.md`, and the shipped Linear workflow examples.

## Verification

- Red check: applying the new loader test to `HEAD^` failed with `Load returned nil error, want tracker.project_slug requirement for Linear workflow`.
- `go test ./internal/workflow -run TestLoadRejectsLinearWorkflowWithoutProjectSlug -count=1` passed.
- `gofmt -l $(git ls-files '*.go')` produced no output.
- `go mod tidy && git diff --exit-code -- go.mod go.sum` passed.
- `go build ./cmd/worker ./cmd/linear-poller ./cmd/gitea-poller` passed.
- `go test -race -covermode=atomic ./...` was run and failed in `internal/runner` on this Darwin host: two Codex app-server timing tests timed out and sandbox tests expected Linux-only bubblewrap/firejail behavior. No failures were in the changed workflow loader or documentation surface.
- Codex local review returned `{"blocking_findings":[]}`.
- Claude Code local review returned `{"blocking_findings":[]}`.
