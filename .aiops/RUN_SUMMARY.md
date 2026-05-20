# Run Summary

Issue: #151, Linear workflows missing `tracker.project_slug` should fail during workflow loading instead of first poll.

Changes made:
- Added load-time validation for `tracker.kind: linear` so single-service workflows require `tracker.project_slug`.
- Kept the multi-service routing extension intact: a top-level slug may be omitted only when each service route provides its own `services[].tracker.project_slug`.
- Added regression tests for both the single-service missing-slug case and the service-route missing-slug case.
- Updated Linear workflow fixtures, shipped workflow examples, README, and the local-dev runbook to document the requirement and service-route exception.

Verification:
- Confirmed the new single-service missing-slug test failed before production code changes.
- `go test ./internal/workflow -run 'TestLoadRejectsLinear(TrackerWithoutProjectSlug|ServiceRouteWithoutAnyProjectSlug|ServiceWithoutExplicitRoute|ServiceOnlyWorkflow)' -count=1`
- `go test ./internal/workflow ./internal/orchestrator ./internal/worker -count=1`
- `gofmt -l $(git ls-files '*.go')`
- `go mod tidy && git diff --exit-code -- go.mod go.sum`
- `go build ./cmd/worker ./cmd/linear-poller ./cmd/gitea-poller`
- `go test -race -covermode=atomic ./...` was run and still fails only in `internal/runner` on this macOS host: Codex app-server timing/read-timeout tests and Linux sandbox backend tests that return `requires linux host OS; current host OS is darwin`.
- Codex local review: `{"blocking_findings":[]}` after fixes.
- Claude Code local review: `{"blocking_findings":[]}` after fixes.
