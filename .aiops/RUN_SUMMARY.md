# Run Summary

Issue: #122, `linear_graphql` rejected valid single-operation GraphQL documents when they also defined fragments.

## Changes

- Added a focused regression test for `countGraphQLOperations` covering:
  - named query plus fragment definition
  - mutation plus fragment definition
  - anonymous operation plus fragment definition
  - fragment definition with directive input-object arguments
  - multiple named operations still counting as multiple operations
- Updated the operation counter so a top-level `fragment` definition is treated as a document definition header without incrementing the operation count. This prevents the fragment selection set from being misclassified as a second anonymous operation, including when fragment directives contain input-object braces before the selection set.

## Why

The Symphony `linear_graphql` tool contract requires exactly one GraphQL operation per tool call. GraphQL fragment definitions are reusable document definitions, not operations, so a document with one operation and one or more fragments should pass the local single-operation guard.

## Verification

- `go test ./internal/runner -run TestCountGraphQLOperationsIgnoresFragmentDefinitions -count=1` failed before the production change with counts of `2` for fragment-bearing single-operation documents.
- `go test ./internal/runner -run TestCountGraphQLOperationsIgnoresFragmentDefinitions/fragment_directive_input_object -count=1` failed before the directive brace handling change with count `2`.
- `go test ./internal/runner -run TestCountGraphQLOperationsIgnoresFragmentDefinitions -count=1` passed after the parser fix.
- `gofmt -l $(git ls-files '*.go')` passed with no output.
- `go mod tidy && git diff --exit-code -- go.mod go.sum` passed.
- `go build ./cmd/worker ./cmd/linear-poller ./cmd/gitea-poller` passed.
- `go test -race -covermode=atomic ./...` was run. It failed in `internal/runner` on this macOS host due existing environment-sensitive failures: Linux sandbox backend tests require `bubblewrap`/`firejail`, and `TestCodexAppServerRunnerServerRequestsRefreshStallClock` exceeded its 100 ms stall budget by about 2 ms.

Local review gates are recorded in the PR body.
